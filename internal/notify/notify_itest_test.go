//go:build itest

// Package notify internal itests: they exercise the real LISTEN outcomes
// connection against TEST_DATABASE_URL. Run via `make itest`.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
)

func quietLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// notifyOutcome emits a NOTIFY outcomes with the given payload from a pool
// connection (a different backend than the notifier's listen connection).
func notifyOutcome(t *testing.T, pool *pgxpool.Pool, payload string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "SELECT pg_notify('outcomes', $1)", payload); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
}

// waitArmed blocks until the notifier reports a non-zero backend pid (its listen
// connection is up) or the deadline passes.
func waitArmed(t *testing.T, n *Notifier) uint32 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pid := n.BackendPID(); pid != 0 {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("notifier did not arm within 3s")
	return 0
}

// recv waits for a signal on ch or fails after d.
func recv(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestNotifierDeliversAndReconnects proves the two core behaviors of the outcome
// fan-out: a registered waiter receives its notification, and after the listen
// connection is killed mid-flight the notifier reconnects, re-issues LISTEN, and
// resumes delivery (reconnect-with-resubscribe, ADR-0002).
func TestNotifierDeliversAndReconnects(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := New(pool, quietLogger(t))
	n.ReconnectDelay = 50 * time.Millisecond
	go func() { _ = n.Run(ctx) }()

	pid := waitArmed(t, n)

	key := fmt.Sprintf("op:%d", rand.Int63())
	ch, cancelWaiter := n.Register(key)
	defer cancelWaiter()

	// Fast path: notification reaches the waiter.
	notifyOutcome(t, pool, key)
	recv(t, ch, 2*time.Second, "first notification")

	// Kill the listen connection's backend; the notifier must reconnect.
	if _, err := pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("pg_terminate_backend: %v", err)
	}

	// Wait for a fresh (non-zero) listen connection.
	deadline := time.Now().Add(3 * time.Second)
	var newPID uint32
	for time.Now().Before(deadline) {
		newPID = n.BackendPID()
		if newPID != 0 && newPID != pid {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if newPID == 0 {
		t.Fatal("notifier did not re-arm after connection kill")
	}

	// Resubscribe proven: a notification after reconnect still reaches the same
	// (still-registered) waiter.
	notifyOutcome(t, pool, key)
	recv(t, ch, 2*time.Second, "post-reconnect notification")
}

// TestNotifierCoalescesAndScopesByKey proves dispatch is keyed: a waiter only wakes
// for its own key, and repeated notifications never block the dispatcher (the size-1
// buffer coalesces).
func TestNotifierCoalescesAndScopesByKey(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := New(pool, quietLogger(t))
	go func() { _ = n.Run(ctx) }()
	waitArmed(t, n)

	mine := fmt.Sprintf("op:%d", rand.Int63())
	other := fmt.Sprintf("tx:%d", rand.Int63())
	ch, cancelWaiter := n.Register(mine)
	defer cancelWaiter()

	// A notification for a different key must not wake this waiter.
	notifyOutcome(t, pool, other)
	select {
	case <-ch:
		t.Fatal("waiter woke for a foreign key")
	case <-time.After(300 * time.Millisecond):
	}

	// Several notifications for our key coalesce into a still-deliverable signal.
	for i := 0; i < 5; i++ {
		notifyOutcome(t, pool, mine)
	}
	recv(t, ch, 2*time.Second, "own-key notification")

	// After cancel, the waiter is unregistered and no longer dispatched to.
	cancelWaiter()
	if _, ok := n.snapshotWaiters()[mine]; ok {
		t.Fatal("waiter still registered after cancel")
	}
}

// snapshotWaiters is a tiny test accessor for the registry key set.
func (n *Notifier) snapshotWaiters() map[string]int {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]int, len(n.waiters))
	for k, set := range n.waiters {
		out[k] = len(set)
	}
	return out
}
