//go:build itest

package processor

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/model"
)

// legSpec describes one leg to insert into a pending group.
type legSpec struct {
	acctID int64
	amount int64
	when   time.Time
}

// insertPendingGroup writes a PENDING transaction and its legs directly (no
// doorbell) and returns the transaction id and the leg operation ids in order.
func insertPendingGroup(t *testing.T, pool *pgxpool.Pool, legs []legSpec) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	var txID int64
	err := pool.QueryRow(ctx,
		`INSERT INTO transactions (idempotency_key, payload_hash, op_count)
		 VALUES (gen_random_uuid(), $1, $2) RETURNING id`,
		[]byte("test-payload"), len(legs)).Scan(&txID)
	if err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	legIDs := make([]int64, len(legs))
	for i, l := range legs {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO operations (account_id, amount, effective_at, transaction_id)
			 VALUES ($1,$2,$3,$4) RETURNING id`,
			l.acctID, l.amount, l.when, txID).Scan(&id); err != nil {
			t.Fatalf("insert leg %d: %v", i, err)
		}
		legIDs[i] = id
	}
	return txID, legIDs
}

type txRow struct {
	status    string
	reason    *string
	decidedAt *time.Time
}

func readTx(t *testing.T, pool *pgxpool.Pool, id int64) txRow {
	t.Helper()
	var r txRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status, reject_reason, decided_at FROM transactions WHERE id = $1`, id).
		Scan(&r.status, &r.reason, &r.decidedAt); err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	return r
}

// TestProcessGroupCommit: a multi-account group commits atomically — every leg
// CONFIRMED, transaction COMMITTED, every account's net applied with a version
// bump, a snapshot per leg.
func TestProcessGroupCommit(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	b := createAccount(t, pool, 1, "b", ptr(-1000), ptr(1000))
	c := createAccount(t, pool, 1, "c", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	txID, legIDs := insertPendingGroup(t, pool, []legSpec{
		{a, 100, when}, {b, -40, when}, {c, 25, when},
	})
	if err := p.processGroup(context.Background(), txID); err != nil {
		t.Fatalf("processGroup: %v", err)
	}

	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) || r.decidedAt == nil {
		t.Fatalf("tx = %+v, want COMMITTED with decided_at", r)
	}
	for _, id := range legIDs {
		if row := readOp(t, pool, id); row.status != string(model.OpConfirmed) || row.confirmedAt == nil {
			t.Fatalf("leg %d = %+v, want CONFIRMED", id, row)
		}
	}
	for _, tc := range []struct {
		id             int64
		wantBal        int64
		wantVer        int64
		wantSnapshotEq int64
	}{{a, 100, 1, 100}, {b, -40, 1, -40}, {c, 25, 1, 25}} {
		if bal, ver := readAccount(t, pool, tc.id); bal != tc.wantBal || ver != tc.wantVer {
			t.Fatalf("account %d balance=%d version=%d, want %d/%d", tc.id, bal, ver, tc.wantBal, tc.wantVer)
		}
		if s, ok := snapshotBalance(t, pool, tc.id, "2026-08-22"); !ok || s != tc.wantSnapshotEq {
			t.Fatalf("account %d snapshot = %d (ok=%v), want %d", tc.id, s, ok, tc.wantSnapshotEq)
		}
	}
}

// TestProcessGroupOneLegFailsRejectsWhole: one offending leg rejects the whole
// group with the offending account + shortfall; no leg is CONFIRMED, no balance
// moves, no snapshot is written (G2 all-or-nothing).
func TestProcessGroupOneLegFailsRejectsWhole(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	b := createAccount(t, pool, 1, "b", ptr(0), ptr(50)) // b cannot exceed 50
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	txID, legIDs := insertPendingGroup(t, pool, []legSpec{{a, 100, when}, {b, 90, when}})
	if err := p.processGroup(context.Background(), txID); err != nil {
		t.Fatalf("processGroup: %v", err)
	}

	r := readTx(t, pool, txID)
	if r.status != string(model.TxRejected) || r.reason == nil {
		t.Fatalf("tx = %+v, want REJECTED with reason", r)
	}
	rej, err := model.ParseRejection(*r.reason)
	if err != nil {
		t.Fatalf("parse rejection: %v", err)
	}
	if rej.Code != model.ReasonLimitViolated || rej.Account != "b" || rej.LimitSide != model.LimitMax || rej.Shortfall != 40 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/b/max/40", rej)
	}
	for _, id := range legIDs {
		if row := readOp(t, pool, id); row.status != string(model.OpInvalid) {
			t.Fatalf("leg %d status = %q, want INVALID", id, row.status)
		}
	}
	for _, id := range []int64{a, b} {
		if bal, ver := readAccount(t, pool, id); bal != 0 || ver != 0 {
			t.Fatalf("account %d balance=%d version=%d, want 0/0 (no change)", id, bal, ver)
		}
		if _, ok := snapshotBalance(t, pool, id, "2026-08-22"); ok {
			t.Fatalf("account %d: reject must not write a snapshot", id)
		}
	}
}

// TestProcessGroupNetPerAccount: two legs on the same account net before validation
// and application. +100 alone exceeds max 80; net +70 is within it → commit, one
// version bump, two snapshot deltas summing to the net.
func TestProcessGroupNetPerAccount(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(80))
	d1 := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	txID, _ := insertPendingGroup(t, pool, []legSpec{{a, 100, d1}, {a, -30, d2}})
	if err := p.processGroup(context.Background(), txID); err != nil {
		t.Fatalf("processGroup: %v", err)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) {
		t.Fatalf("tx status = %q, want COMMITTED (net +70 within max 80)", r.status)
	}
	if bal, ver := readAccount(t, pool, a); bal != 70 || ver != 1 {
		t.Fatalf("account balance=%d version=%d, want 70/1 (net applied once)", bal, ver)
	}
	// Snapshot deltas per leg: +100 on d1, then -30 on d2 → d1=100, d2=70.
	if s, ok := snapshotBalance(t, pool, a, "2026-08-20"); !ok || s != 100 {
		t.Fatalf("d1 snapshot = %d (ok=%v), want 100", s, ok)
	}
	if s, ok := snapshotBalance(t, pool, a, "2026-08-22"); !ok || s != 70 {
		t.Fatalf("d2 snapshot = %d (ok=%v), want 70", s, ok)
	}
}

// TestProcessGroupSkipByStatus: re-processing an already-decided group is a no-op
// (skip-by-status) and does not double-apply.
func TestProcessGroupSkipByStatus(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	txID, _ := insertPendingGroup(t, pool, []legSpec{{a, 100, when}, {a, 50, when}})
	must(t, p.processGroup(context.Background(), txID))
	if bal, ver := readAccount(t, pool, a); bal != 150 || ver != 1 {
		t.Fatalf("after first commit balance=%d version=%d, want 150/1", bal, ver)
	}
	// Second pass: transaction is COMMITTED → skip, no error, no double-apply.
	must(t, p.processGroup(context.Background(), txID))
	if bal, ver := readAccount(t, pool, a); bal != 150 || ver != 1 {
		t.Fatalf("re-process changed state: balance=%d version=%d, want 150/1", bal, ver)
	}
}

// TestProcessGroupGuard1Miss: a non-leader tripping Guard 1 leaves the whole group
// PENDING; a real leader then commits it cleanly.
func TestProcessGroupGuard1Miss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	txID, legIDs := insertPendingGroup(t, pool, []legSpec{{a, 100, when}, {a, 50, when}})

	nonLeader := newNonLeader(t, pool)
	if err := nonLeader.processGroup(context.Background(), txID); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss, got %v", err)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxPending) {
		t.Fatalf("tx status = %q, want still PENDING after guard 1 miss", r.status)
	}
	for _, id := range legIDs {
		if row := readOp(t, pool, id); row.status != string(model.OpPending) {
			t.Fatalf("leg %d status = %q, want still PENDING", id, row.status)
		}
	}

	leader := leaderProcessor(t, pool)
	must(t, leader.processGroup(context.Background(), txID))
	if bal := mustBalance(t, pool, a); bal != 150 {
		t.Fatalf("balance = %d, want 150 after clean retry", bal)
	}
}

// TestProcessGroupGuard3Miss: a concurrent version bump on one involved account
// between the group's account read and its CAS trips Guard 3 — whole-group
// rollback, nothing applied — and a clean retry commits against the new version.
func TestProcessGroupGuard3Miss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	b := createAccount(t, pool, 1, "b", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	txID, legIDs := insertPendingGroup(t, pool, []legSpec{{a, 100, when}, {b, 50, when}})

	p.afterAccountRead = func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE accounts SET version = version + 1 WHERE id = $1`, b); err != nil {
			t.Errorf("hook version bump: %v", err)
		}
	}
	if err := p.processGroup(context.Background(), txID); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on stale version, got %v", err)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxPending) {
		t.Fatalf("tx status = %q, want still PENDING after guard 3 miss", r.status)
	}
	for _, id := range legIDs {
		if row := readOp(t, pool, id); row.status != string(model.OpPending) {
			t.Fatalf("leg %d status = %q, want still PENDING", id, row.status)
		}
	}
	if bal := mustBalance(t, pool, a); bal != 0 {
		t.Fatalf("account a balance = %d, want 0 (rolled back)", bal)
	}

	p.afterAccountRead = nil
	must(t, p.processGroup(context.Background(), txID))
	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) {
		t.Fatalf("tx status = %q, want COMMITTED on clean retry", r.status)
	}
	if mustBalance(t, pool, a) != 100 || mustBalance(t, pool, b) != 50 {
		t.Fatalf("balances a=%d b=%d, want 100/50", mustBalance(t, pool, a), mustBalance(t, pool, b))
	}
}

// mustBalance reads just the confirmed balance.
func mustBalance(t *testing.T, pool *pgxpool.Pool, id int64) int64 {
	t.Helper()
	bal, _ := readAccount(t, pool, id)
	return bal
}

// TestProcessBatchMixedCommitsTogether: a batch of interleaved singles and a group
// is decided in one pass, each unit's guards evaluated per decision. Verifies the
// §8.1 dispatch (group decided at its first leg, later legs skipped) and §8.5
// batching over one transaction.
func TestProcessBatchMixedCommitsTogether(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	b := createAccount(t, pool, 1, "b", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	s1 := insertPendingSingle(t, pool, a, 10, when)
	txID, legIDs := insertPendingGroup(t, pool, []legSpec{{a, 100, when}, {b, -40, when}})
	s2 := insertPendingSingle(t, pool, b, 5, when)

	// One drain pass with a batch large enough to hold everything.
	if err := p.drain(context.Background(), 10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if readOp(t, pool, s1.ID).status != string(model.OpConfirmed) {
		t.Fatalf("single s1 not confirmed")
	}
	if readOp(t, pool, s2.ID).status != string(model.OpConfirmed) {
		t.Fatalf("single s2 not confirmed")
	}
	if readTx(t, pool, txID).status != string(model.TxCommitted) {
		t.Fatalf("group not committed")
	}
	for _, id := range legIDs {
		if readOp(t, pool, id).status != string(model.OpConfirmed) {
			t.Fatalf("leg %d not confirmed", id)
		}
	}
	// a: +10 (s1) +100 (leg) = 110; b: -40 (leg) +5 (s2) = -35.
	if bal := mustBalance(t, pool, a); bal != 110 {
		t.Fatalf("account a = %d, want 110", bal)
	}
	if bal := mustBalance(t, pool, b); bal != -35 {
		t.Fatalf("account b = %d, want -35", bal)
	}
}

// TestProcessBatchWholeBatchRollbackOnGuardMiss: if any decision in a batch trips a
// guard, the whole batch rolls back (nothing decided) and reprocessing after the
// racing condition clears yields the correct final state (spec §8.5, idempotent by
// construction).
func TestProcessBatchWholeBatchRollbackOnGuardMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	a := createAccount(t, pool, 1, "a", ptr(-1000), ptr(1000))
	when := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

	s1 := insertPendingSingle(t, pool, a, 10, when)
	s2 := insertPendingSingle(t, pool, a, 20, when)
	s3 := insertPendingSingle(t, pool, a, 30, when)

	// Trip Guard 3 on the FIRST decision once: the hook bumps the version between
	// the account read and the CAS, then disarms itself. The whole batch rolls back.
	fired := false
	p.afterAccountRead = func() {
		if fired {
			return
		}
		fired = true
		if _, err := pool.Exec(context.Background(),
			`UPDATE accounts SET version = version + 1 WHERE id = $1`, a); err != nil {
			t.Errorf("hook version bump: %v", err)
		}
	}

	if err := p.drain(context.Background(), 10); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss from the batch, got %v", err)
	}
	// Nothing decided: all three still PENDING, only the hook's version bump landed.
	for _, op := range []pendingOp{s1, s2, s3} {
		if readOp(t, pool, op.ID).status != string(model.OpPending) {
			t.Fatalf("op %d decided despite whole-batch rollback", op.ID)
		}
	}
	if _, ver := readAccount(t, pool, a); ver != 1 {
		t.Fatalf("version = %d, want 1 (only the hook)", ver)
	}

	// Reprocess cleanly (hook already disarmed after firing once).
	must(t, p.drain(context.Background(), 10))
	for _, op := range []pendingOp{s1, s2, s3} {
		if readOp(t, pool, op.ID).status != string(model.OpConfirmed) {
			t.Fatalf("op %d not CONFIRMED after clean reprocess", op.ID)
		}
	}
	if bal, ver := readAccount(t, pool, a); bal != 60 || ver != 4 {
		t.Fatalf("balance=%d version=%d, want 60/4 (hook bump + 3 decisions)", bal, ver)
	}
}

// newNonLeader builds a processor whose lease was never acquired (its owner is not
// in leader_lease), so Guard 1 always misses.
func newNonLeader(t *testing.T, pool *pgxpool.Pool) *Processor {
	t.Helper()
	l, err := lease.New(pool)
	if err != nil {
		t.Fatalf("lease.New: %v", err)
	}
	return New(pool, l, slog.Default())
}
