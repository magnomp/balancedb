//go:build itest

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/notify"
)

func quietAPILogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(apiTestWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type apiTestWriter struct{ t *testing.T }

func (w apiTestWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// fakeNotifier is a controllable outcomeWaiter for the wait-path tests. onRegister,
// when set, runs synchronously inside Register (before the wait path's immediate
// status check) — used to model a decision that lands at/just-before registration.
// The returned channel never signals by default, so tests drive resolution via the
// poll ticker, the deadline, or onRegister.
type fakeNotifier struct {
	mu         sync.Mutex
	registered []string
	onRegister func(key string)
	ch         chan struct{}
}

func (f *fakeNotifier) Register(key string) (<-chan struct{}, func()) {
	f.mu.Lock()
	f.registered = append(f.registered, key)
	if f.ch == nil {
		f.ch = make(chan struct{})
	}
	ch := f.ch
	f.mu.Unlock()
	if f.onRegister != nil {
		f.onRegister(key)
	}
	return ch, func() {}
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registered)
}

// waitServer builds a Server over a throwaway schema with the given notifier and
// poll interval, served via httptest.
func waitServer(t *testing.T, notifier outcomeWaiter, poll time.Duration) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewSchema(t)
	srv := NewServer(pool, nil, notifier)
	srv.pollInterval = poll
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool
}

// decideSingle flips a single operation to a terminal status directly (a test
// shortcut past the processor guards — these tests exercise the API wait mechanics,
// not the decision engine).
func decideSingle(t *testing.T, pool *pgxpool.Pool, opID int64, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE operations SET status=$2, confirmed_at=now() WHERE id=$1`, opID, status)
	if err != nil {
		t.Fatalf("decide op %d: %v", opID, err)
	}
}

func pendingOpID(t *testing.T, pool *pgxpool.Pool, acctID int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`SELECT id FROM operations WHERE account_id=$1 AND status='PENDING' ORDER BY id DESC LIMIT 1`,
		acctID).Scan(&id)
	if err != nil {
		t.Fatalf("find pending op: %v", err)
	}
	return id
}

func singleBody(ext string, waitMs int) string {
	return fmt.Sprintf(`{"operations":[{"account":%q,"amount":100,"effective_at":"2026-01-01T00:00:00Z"}],"wait_ms":%d}`, ext, waitMs)
}

// TestWaitDecidedBeforeRegistration: the decision commits at/before the waiter
// registers. The immediate status check (after Register, before any wait) must catch
// it and return 200 — no notification, no poll, no lost wakeup. The poll interval is
// huge so only the immediate check can resolve it.
func TestWaitDecidedBeforeRegistration(t *testing.T) {
	fake := &fakeNotifier{}
	ts, pool := waitServer(t, fake, time.Hour)
	seedAccount(t, pool, 7, "acct", nil, nil, 0)

	// onRegister decides the just-inserted op, keyed by 'op:<id>'.
	fake.onRegister = func(key string) {
		var id int64
		if _, err := fmt.Sscanf(key, "op:%d", &id); err != nil {
			t.Errorf("parse key %q: %v", key, err)
			return
		}
		decideSingle(t, pool, id, "CONFIRMED")
	}

	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(1), singleBody("acct", 5000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 (decided), got %d: %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	ops := m["operations"].([]any)
	if got := ops[0].(map[string]any)["status"]; got != "CONFIRMED" {
		t.Fatalf("want CONFIRMED, got %v", got)
	}
}

// TestWaitExpiryReturns202: an undecided op with a short budget expires and returns
// 202 with the current (PENDING) state. Poll interval is huge so the deadline is the
// only thing that can fire.
func TestWaitExpiryReturns202(t *testing.T) {
	ts, pool := waitServer(t, nil, time.Hour)
	seedAccount(t, pool, 7, "acct", nil, nil, 0)

	start := time.Now()
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(1), singleBody("acct", 200))
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 (expiry), got %d: %s", resp.StatusCode, b)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("returned too early (%s); should have waited ~200ms", elapsed)
	}
	m := decode(t, b)
	ops := m["operations"].([]any)
	if got := ops[0].(map[string]any)["status"]; got != "PENDING" {
		t.Fatalf("want PENDING on expiry, got %v", got)
	}
}

// TestWaitCappedByAPIMaxWait: a huge wait_ms is capped by api_max_wait_ms from the
// config row, so the call still returns promptly (202).
func TestWaitCappedByAPIMaxWait(t *testing.T) {
	ts, pool := waitServer(t, nil, time.Hour)
	seedAccount(t, pool, 7, "acct", nil, nil, 0)
	if _, err := pool.Exec(context.Background(), `UPDATE config SET api_max_wait_ms=150`); err != nil {
		t.Fatalf("set api_max_wait_ms: %v", err)
	}

	start := time.Now()
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(1), singleBody("acct", 60000))
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", resp.StatusCode, b)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cap not applied: waited %s for a 60s budget capped at 150ms", elapsed)
	}
}

// TestWaitPollFallbackAfterListenKilled: with the notifier's listen connection
// killed (and reconnect held off), the outcome NOTIFY is lost — the status-poll
// fallback must still resolve the wait to 200. Uses a REAL notifier.
func TestWaitPollFallbackAfterListenKilled(t *testing.T) {
	pool := dbtest.NewSchema(t)
	acctID := seedAccount(t, pool, 7, "acct", nil, nil, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := notify.New(pool, quietAPILogger(t))
	n.ReconnectDelay = time.Hour // stay down after the kill
	go func() { _ = n.Run(ctx) }()

	// Wait until armed, capture the listen backend pid.
	var pid uint32
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pid = n.BackendPID(); pid != 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("notifier did not arm")
	}

	srv := NewServer(pool, nil, n)
	srv.pollInterval = 100 * time.Millisecond
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	type result struct {
		code int
		body []byte
	}
	done := make(chan result, 1)
	go func() {
		resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(1), singleBody("acct", 10000))
		done <- result{resp.StatusCode, b}
	}()

	// Find the op the handler inserted, kill the listen connection, wait for the
	// notifier to observe the drop (pid → 0), then decide + emit the (now lost)
	// NOTIFY. Only the poll fallback can resolve the wait.
	var opID int64
	findDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(findDeadline) {
		var id int64
		err := pool.QueryRow(ctx, `SELECT id FROM operations WHERE account_id=$1 AND status='PENDING' ORDER BY id DESC LIMIT 1`, acctID).Scan(&id)
		if err == nil {
			opID = id
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if opID == 0 {
		t.Fatal("handler did not insert the pending op")
	}

	if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("terminate listen backend: %v", err)
	}
	for i := 0; i < 200 && n.BackendPID() != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if n.BackendPID() != 0 {
		t.Fatal("notifier unexpectedly re-armed; poll-fallback would not be isolated")
	}

	decideSingle(t, pool, opID, "CONFIRMED")
	if _, err := pool.Exec(ctx, `SELECT pg_notify('outcomes', $1)`, fmt.Sprintf("op:%d", opID)); err != nil {
		t.Fatalf("emit (lost) notify: %v", err)
	}

	select {
	case r := <-done:
		if r.code != http.StatusOK {
			t.Fatalf("want 200 via poll fallback, got %d: %s", r.code, r.body)
		}
		m := decode(t, r.body)
		ops := m["operations"].([]any)
		if got := ops[0].(map[string]any)["status"]; got != "CONFIRMED" {
			t.Fatalf("want CONFIRMED, got %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not resolve via poll fallback within 5s")
	}
}

// TestWaitZeroNeverTouchesNotify: wait_ms = 0 returns 202 immediately and never
// registers a waiter (the notify machinery is strictly opt-in, ADR-0002).
func TestWaitZeroNeverTouchesNotify(t *testing.T) {
	fake := &fakeNotifier{}
	ts, pool := waitServer(t, fake, time.Hour)
	seedAccount(t, pool, 7, "acct", nil, nil, 0)

	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(1), singleBody("acct", 0))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", resp.StatusCode, b)
	}
	if fake.count() != 0 {
		t.Fatalf("wait_ms=0 registered %d waiter(s); must be 0", fake.count())
	}
	m := decode(t, b)
	ops := m["operations"].([]any)
	if got := ops[0].(map[string]any)["status"]; got != "PENDING" {
		t.Fatalf("want PENDING (fire-and-forget), got %v", got)
	}
}
