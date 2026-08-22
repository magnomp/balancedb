//go:build itest

// External test package so it may import processor (which imports api) without an
// import cycle: this is the full bidirectional low-latency path of ADR-0002 — the
// insert doorbell wakes the real leader, the deciding commit's NOTIFY wakes the
// waiting API request.
package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/notify"
	"github.com/magnomp/balancedb/internal/processor"
)

type e2eWriter struct{ t *testing.T }

func (w e2eWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// TestWaitEndToEndDoorbellAndNotify runs a real processor and a real notifier and
// issues a synchronous insert. loop_interval is raised to 10s so the ONLY way the
// wait can resolve quickly is the doorbell (insert → leader wakeup) plus the outcome
// NOTIFY (decision → waiter wakeup): resolving well under one loop_interval proves
// the fast path, not a timed fallback.
func TestWaitEndToEndDoorbellAndNotify(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()

	// Raise loop_interval so a missed doorbell would cost seconds; keep ttl so
	// ttl/2 (the other idle-deadline term) also stays large.
	if _, err := pool.Exec(ctx, `UPDATE config SET loop_interval_ms=10000, lease_ttl_ms=30000`); err != nil {
		t.Fatalf("set config: %v", err)
	}

	// Account with generous limits so the single is CONFIRMED.
	var acctID int64
	if err := pool.QueryRow(ctx, `INSERT INTO accounts (owner_id, external_id) VALUES ($1,$2) RETURNING id`, 7, "acct").Scan(&acctID); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(e2eWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))

	// Real notifier.
	nctx, ncancel := context.WithCancel(ctx)
	defer ncancel()
	n := notify.New(pool, logger)
	go func() { _ = n.Run(nctx) }()

	// Real processor loop.
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	proc := processor.New(pool, l, logger)
	procDone := make(chan struct{})
	go func() { _ = proc.Run(pctx); close(procDone) }()

	// Wait until a leader holds the lease (so its doorbell listener is armed).
	waitLeader(t, pool)
	// Small grace so the leader has drained-empty and armed LISTEN work_available.
	time.Sleep(300 * time.Millisecond)

	// Wait until both the notifier and processor doorbell listeners are up.
	waitNotifierArmed(t, n)

	srv := api.NewServer(pool, nil, n)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := fmt.Sprintf(`{"operations":[{"account":%q,"amount":500,"effective_at":"2026-01-01T00:00:00Z"}],"wait_ms":9000}`, "acct")

	start := time.Now()
	resp, b := post(t, ts, "7", "00000000-0000-4000-8000-000000000001", body)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 (synchronous outcome), got %d: %s", resp.StatusCode, b)
	}
	if !strings.Contains(string(b), `"CONFIRMED"`) {
		t.Fatalf("want CONFIRMED outcome, got: %s", b)
	}
	// loop_interval is 10s; the fast path must resolve in a small fraction of it.
	if elapsed > 3*time.Second {
		t.Fatalf("resolved in %s with loop_interval=10s; the doorbell+notify fast path did not fire", elapsed)
	}
	t.Logf("end-to-end synchronous insert resolved in %s (loop_interval=10s)", elapsed)

	pcancel()
	<-procDone
}

func waitLeader(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var held bool
		err := pool.QueryRow(context.Background(),
			`SELECT owner IS NOT NULL AND lease_until > now() FROM leader_lease`).Scan(&held)
		if err == nil && held {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no leader elected within 5s")
}

func waitNotifierArmed(t *testing.T, n *notify.Notifier) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n.BackendPID() != 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("notifier did not arm within 3s")
}

func post(t *testing.T, ts *httptest.Server, owner, idem, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/transactions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Owner-Id", owner)
	req.Header.Set("Idempotency-Key", idem)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, b
}
