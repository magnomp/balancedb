//go:build itest

// Package processor internal itests: they call the unexported single-op path and
// loop helpers directly and set the test seams, so they live in package processor
// rather than processor_test. Run via `make itest` against TEST_DATABASE_URL.
package processor

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/model"
)

const testTTL = 30 * time.Second

// leaderProcessor builds a processor whose lease it has already acquired, so
// Guard 1 passes in processSingle.
func leaderProcessor(t *testing.T, pool *pgxpool.Pool) *Processor {
	t.Helper()
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	if ok, err := l.Acquire(context.Background(), testTTL); err != nil || !ok {
		t.Fatalf("acquire lease: ok=%v err=%v", ok, err)
	}
	return New(pool, l, slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError})))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

// createAccount inserts an account with the given (nullable) limits and returns
// its id.
func createAccount(t *testing.T, pool *pgxpool.Pool, owner int64, ext string, minB, maxB *int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (owner_id, external_id, min_balance, max_balance) VALUES ($1,$2,$3,$4) RETURNING id`,
		owner, ext, minB, maxB).Scan(&id)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	return id
}

// insertPendingSingle writes a PENDING single operation directly (no doorbell) and
// returns the pendingOp the drain would fetch for it.
func insertPendingSingle(t *testing.T, pool *pgxpool.Pool, acctID, amount int64, when time.Time) pendingOp {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO operations (account_id, amount, effective_at) VALUES ($1,$2,$3) RETURNING id`,
		acctID, amount, when).Scan(&id)
	if err != nil {
		t.Fatalf("insert pending single: %v", err)
	}
	return pendingOp{ID: id, AccountID: acctID, Amount: amount, EffectiveAt: when}
}

func readAccount(t *testing.T, pool *pgxpool.Pool, id int64) (balance, version int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT confirmed_balance, version FROM accounts WHERE id = $1`, id).Scan(&balance, &version); err != nil {
		t.Fatalf("read account: %v", err)
	}
	return
}

type opRow struct {
	status      string
	confirmedAt *time.Time
	reason      *string
}

func readOp(t *testing.T, pool *pgxpool.Pool, id int64) opRow {
	t.Helper()
	var r opRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, confirmed_at, invalidation_reason FROM operations WHERE id = $1`, id).
		Scan(&r.status, &r.confirmedAt, &r.reason); err != nil {
		t.Fatalf("read op: %v", err)
	}
	return r
}

// snapshotBalance returns the snapshot balance for an account on a UTC day, and
// whether the row exists.
func snapshotBalance(t *testing.T, pool *pgxpool.Pool, acctID int64, day string) (int64, bool) {
	t.Helper()
	var bal int64
	err := pool.QueryRow(context.Background(),
		`SELECT balance FROM balance_snapshots WHERE account_id = $1 AND day = $2::date`, acctID, day).Scan(&bal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	return bal, true
}

func setConfig(t *testing.T, pool *pgxpool.Pool, loopMs, ttlMs int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE config SET loop_interval_ms = $1, lease_ttl_ms = $2`, loopMs, ttlMs); err != nil {
		t.Fatalf("set config: %v", err)
	}
}

func ptr(v int64) *int64 { return &v }

// TestProcessSingleAccept covers the accept path: balance, version bump, snapshot
// row, status, confirmed_at.
func TestProcessSingleAccept(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(0), ptr(1000))
	when := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	op := insertPendingSingle(t, pool, acct, 500, when)

	if err := p.processSingle(context.Background(), op); err != nil {
		t.Fatalf("processSingle: %v", err)
	}

	row := readOp(t, pool, op.ID)
	if row.status != string(model.OpConfirmed) {
		t.Fatalf("status = %q, want CONFIRMED", row.status)
	}
	if row.confirmedAt == nil {
		t.Fatalf("confirmed_at is null")
	}
	bal, ver := readAccount(t, pool, acct)
	if bal != 500 || ver != 1 {
		t.Fatalf("account balance=%d version=%d, want 500/1", bal, ver)
	}
	if b, ok := snapshotBalance(t, pool, acct, "2026-08-22"); !ok || b != 500 {
		t.Fatalf("snapshot 2026-08-22 = %d (ok=%v), want 500", b, ok)
	}
}

// TestProcessSingleReject covers the reject path: INVALID + machine-readable
// reason detail, no balance/version change, no snapshot.
func TestProcessSingleReject(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(0), ptr(100))
	when := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	op := insertPendingSingle(t, pool, acct, 500, when)

	if err := p.processSingle(context.Background(), op); err != nil {
		t.Fatalf("processSingle: %v", err)
	}

	row := readOp(t, pool, op.ID)
	if row.status != string(model.OpInvalid) {
		t.Fatalf("status = %q, want INVALID", row.status)
	}
	if row.confirmedAt != nil {
		t.Fatalf("confirmed_at should be null on reject")
	}
	if row.reason == nil {
		t.Fatalf("invalidation_reason is null")
	}
	rej, err := model.ParseRejection(*row.reason)
	if err != nil {
		t.Fatalf("parse rejection: %v", err)
	}
	if rej.Code != model.ReasonLimitViolated || rej.Account != "w" || rej.LimitSide != model.LimitMax || rej.Shortfall != 400 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/w/max/400", rej)
	}
	if bal, ver := readAccount(t, pool, acct); bal != 0 || ver != 0 {
		t.Fatalf("account balance=%d version=%d, want 0/0 (no change)", bal, ver)
	}
	if _, ok := snapshotBalance(t, pool, acct, "2026-08-22"); ok {
		t.Fatalf("reject must not write a snapshot")
	}
}

// TestProcessSingleUnboundedLimits covers NULL (unbounded) limits: large positive
// and negative amounts both accept.
func TestProcessSingleUnboundedLimits(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", nil, nil)
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	for _, amt := range []int64{1_000_000_000, -5_000_000_000} {
		op := insertPendingSingle(t, pool, acct, amt, when)
		if err := p.processSingle(context.Background(), op); err != nil {
			t.Fatalf("processSingle(%d): %v", amt, err)
		}
	}
	if bal, ver := readAccount(t, pool, acct); bal != -4_000_000_000 || ver != 2 {
		t.Fatalf("balance=%d version=%d, want -4000000000/2", bal, ver)
	}
}

// TestProcessSingleBackdatedCascade confirms operations on later days first, then
// backdates one — the new day is seeded from the prior snapshot and every existing
// later day gains the amount (spec §8.4).
func TestProcessSingleBackdatedCascade(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(-1_000_000), ptr(1_000_000))

	d1 := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	// Forward order first: D2 then D3.
	must(t, p.processSingle(context.Background(), insertPendingSingle(t, pool, acct, 100, d2)))
	must(t, p.processSingle(context.Background(), insertPendingSingle(t, pool, acct, 50, d3)))
	// Now backdate D1 — must cascade into D2 and D3.
	must(t, p.processSingle(context.Background(), insertPendingSingle(t, pool, acct, 30, d1)))

	for _, tc := range []struct {
		day  string
		want int64
	}{
		{"2026-08-10", 30},
		{"2026-08-20", 130},
		{"2026-08-25", 180},
	} {
		if b, ok := snapshotBalance(t, pool, acct, tc.day); !ok || b != tc.want {
			t.Fatalf("snapshot %s = %d (ok=%v), want %d", tc.day, b, ok, tc.want)
		}
	}
	if bal, _ := readAccount(t, pool, acct); bal != 180 {
		t.Fatalf("final balance = %d, want 180", bal)
	}
}

// TestProcessSingleFutureDated covers N5: a future-dated operation enters the
// FINAL balance, and a later same-account present-day operation cascades forward
// into the future snapshot.
func TestProcessSingleFutureDated(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(-1_000_000), ptr(1_000_000))

	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	today := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	must(t, p.processSingle(context.Background(), insertPendingSingle(t, pool, acct, 200, future)))
	if bal, _ := readAccount(t, pool, acct); bal != 200 {
		t.Fatalf("final balance after future-dated = %d, want 200 (N5)", bal)
	}
	// A present-day op cascades forward into the 2030 snapshot.
	must(t, p.processSingle(context.Background(), insertPendingSingle(t, pool, acct, 50, today)))

	if b, ok := snapshotBalance(t, pool, acct, "2026-08-22"); !ok || b != 50 {
		t.Fatalf("today snapshot = %d (ok=%v), want 50", b, ok)
	}
	if b, ok := snapshotBalance(t, pool, acct, "2030-01-01"); !ok || b != 250 {
		t.Fatalf("2030 snapshot = %d (ok=%v), want 250", b, ok)
	}
	if bal, _ := readAccount(t, pool, acct); bal != 250 {
		t.Fatalf("final balance = %d, want 250", bal)
	}
}

// TestGuard1LeaseFenceMiss: a processor that does not hold the lease trips Guard 1
// — rollback, no state change — and a proper leader then decides it cleanly.
func TestGuard1LeaseFenceMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	acct := createAccount(t, pool, 1, "w", ptr(0), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	op := insertPendingSingle(t, pool, acct, 100, when)

	// A processor whose lease was never acquired: its owner is not in leader_lease.
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	nonLeader := New(pool, l, slog.Default())
	if err := nonLeader.processSingle(context.Background(), op); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss, got %v", err)
	}
	if row := readOp(t, pool, op.ID); row.status != string(model.OpPending) {
		t.Fatalf("op status = %q, want still PENDING after guard 1 miss", row.status)
	}
	if bal, _ := readAccount(t, pool, acct); bal != 0 {
		t.Fatalf("balance changed on guard 1 miss: %d", bal)
	}

	// Clean retry by a real leader.
	leader := leaderProcessor(t, pool)
	must(t, leader.processSingle(context.Background(), op))
	if row := readOp(t, pool, op.ID); row.status != string(model.OpConfirmed) {
		t.Fatalf("op status = %q, want CONFIRMED on clean retry", row.status)
	}
	if bal, _ := readAccount(t, pool, acct); bal != 100 {
		t.Fatalf("balance = %d, want 100 after clean retry", bal)
	}
}

// TestGuard2StatusFlipMiss: re-processing an already-decided operation trips Guard
// 2 (status no longer PENDING) — no double application.
func TestGuard2StatusFlipMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(0), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	op := insertPendingSingle(t, pool, acct, 100, when)

	must(t, p.processSingle(context.Background(), op))
	// Second pass: the op is CONFIRMED, so Guard 2's WHERE status='PENDING' matches
	// nothing → rollback, no second balance application.
	if err := p.processSingle(context.Background(), op); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on re-process, got %v", err)
	}
	if bal, ver := readAccount(t, pool, acct); bal != 100 || ver != 1 {
		t.Fatalf("balance=%d version=%d, want 100/1 (no double apply)", bal, ver)
	}
}

// TestGuard3VersionCASMiss: a concurrent account-version bump between the account
// read and the CAS trips Guard 3 (stale version) — rollback, then a clean retry
// next cycle succeeds against the new version.
func TestGuard3VersionCASMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	acct := createAccount(t, pool, 1, "w", ptr(0), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	op := insertPendingSingle(t, pool, acct, 100, when)

	// Inject a committed version bump right after processSingle reads the account,
	// so its Guard 3 CAS (WHERE version = read_version) matches 0 rows.
	p.afterAccountRead = func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE accounts SET version = version + 1 WHERE id = $1`, acct); err != nil {
			t.Errorf("hook version bump: %v", err)
		}
	}
	if err := p.processSingle(context.Background(), op); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on stale version, got %v", err)
	}
	if row := readOp(t, pool, op.ID); row.status != string(model.OpPending) {
		t.Fatalf("op status = %q, want still PENDING after guard 3 miss", row.status)
	}
	if bal, ver := readAccount(t, pool, acct); bal != 0 || ver != 1 {
		t.Fatalf("balance=%d version=%d, want 0/1 (only the hook's bump)", bal, ver)
	}

	// Clean retry next cycle (hook cleared): reads version 1, CAS succeeds.
	p.afterAccountRead = nil
	must(t, p.processSingle(context.Background(), op))
	if row := readOp(t, pool, op.ID); row.status != string(model.OpConfirmed) {
		t.Fatalf("op status = %q, want CONFIRMED on clean retry", row.status)
	}
	if bal, ver := readAccount(t, pool, acct); bal != 100 || ver != 2 {
		t.Fatalf("balance=%d version=%d, want 100/2 after clean retry", bal, ver)
	}
}

// TestDoorbellWakeup: with a long loop_interval, an insert against an idle leader
// is decided in well under one interval because the doorbell wakes it early.
func TestDoorbellWakeup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	setConfig(t, pool, 60_000, 60_000) // 60s loop, 60s TTL
	createAccount(t, pool, 1, "w", ptr(-1_000_000), ptr(1_000_000))

	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	p := New(pool, l, slog.Default())

	idle := make(chan struct{}, 1)
	p.enteredIdleWait = func() {
		select {
		case idle <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, idle, 5*time.Second, "leader to enter idle wait")

	// Insert via the real path so the doorbell rings.
	opID := insertViaAPI(t, pool, 1, "w", 250)
	start := time.Now()
	waitConfirmed(t, pool, opID, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("doorbell wakeup took %v, want well under the 60s interval", elapsed)
	}
}

// TestDoorbellLossTimedWakeup: with the doorbell suppressed (the op is written
// directly, ringing nothing), the timed wakeup still decides it within one
// interval.
func TestDoorbellLossTimedWakeup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	const loopMs = 1000
	setConfig(t, pool, loopMs, 15_000)
	acct := createAccount(t, pool, 1, "w", ptr(-1_000_000), ptr(1_000_000))

	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	p := New(pool, l, slog.Default())

	idle := make(chan struct{}, 1)
	p.enteredIdleWait = func() {
		select {
		case idle <- struct{}{}:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = p.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, idle, 5*time.Second, "leader to enter idle wait")

	// Write a PENDING op directly — no NOTIFY. Only the timed wakeup can find it.
	op := insertPendingSingle(t, pool, acct, 77, time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC))
	start := time.Now()
	waitConfirmed(t, pool, op.ID, 5*time.Second)
	// Timed wakeup fires at deadline = min(loop_interval, ttl/2) = one interval;
	// allow a generous multiple for CI scheduling jitter.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timed wakeup took %v, want within a small multiple of the %dms interval", elapsed, loopMs)
	}
}

// --- helpers used only by the loop timing tests ---

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("processSingle: %v", err)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitConfirmed(t *testing.T, pool *pgxpool.Pool, opID int64, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if readOp(t, pool, opID).status == string(model.OpConfirmed) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("op %d not CONFIRMED within %v", opID, d)
}

// insertViaAPI inserts a single through api.Insert (which rings the doorbell) and
// returns the new operation id.
func insertViaAPI(t *testing.T, pool *pgxpool.Pool, owner int64, ext string, amount int64) int64 {
	t.Helper()
	ctx := context.Background()
	var opID int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, err := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: "00000000-0000-4000-8000-000000000abc",
			Operations: []api.InsertOp{{
				OwnerID: owner, ExternalID: ext, Amount: amount,
				EffectiveAt: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
			}},
		})
		if err != nil {
			return err
		}
		opID = res.Operations[0].ID
		return nil
	})
	if err != nil {
		t.Fatalf("insert via api: %v", err)
	}
	return opID
}
