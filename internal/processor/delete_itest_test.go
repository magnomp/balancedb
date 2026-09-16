//go:build itest

package processor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
)

// Integration tests for the single-delete decision path (ADR-0011): IT-010–IT-012,
// IT-014–IT-019, IT-040–IT-043 and IT-045 of the operation-deletion test contract.
// Deletes are registered through the shared ledger core (api.Insert) so the rows
// carry exactly what production writes, then decided through the processor's own
// entry points.

// registerDelete registers one delete item through the shared ledger core and
// returns the pendingOp the drain would fetch for it.
func registerDelete(t *testing.T, pool *pgxpool.Pool, key int, target int64, expected *int32) pendingOp {
	t.Helper()
	return registerEdit(t, pool, key, api.InsertOp{DeleteOf: &target, ExpectedRevision: expected})
}

// countRows returns the result of a count(*) query.
func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// snapshotBalances returns balance per snapshot day of an account.
func snapshotBalances(t *testing.T, pool *pgxpool.Pool, acct int64) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT day::text, balance FROM balance_snapshots WHERE account_id = $1`, acct)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var day string
		var bal int64
		if err := rows.Scan(&day, &bal); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		out[day] = bal
	}
	return out
}

// requireRejection parses the row's invalidation_reason and checks the code and
// the named operation.
func requireRejection(t *testing.T, s opState, code model.ReasonCode, target int64) model.Rejection {
	t.Helper()
	if s.status != string(model.OpInvalid) || s.reason == nil || s.confirmed == nil {
		t.Fatalf("row = %+v, want INVALID with a reason and a decision instant", s)
	}
	rej, err := model.ParseRejection(*s.reason)
	if err != nil {
		t.Fatalf("parse rejection: %v", err)
	}
	if rej.Code != code {
		t.Fatalf("rejection = %+v, want code %s", rej, code)
	}
	if code != model.ReasonLimitViolated && (rej.OperationID == nil || *rej.OperationID != target) {
		t.Fatalf("rejection = %+v, want operation_id %d", rej, target)
	}
	return rej
}

// IT-010 / IT-011: the golden apply. Target {cash +1500 on Aug 22} CONFIRMED with
// later confirmed days; delete it → the delete row is APPLIED with confirmed_at,
// the target is DELETED with deleted_by = delete id and deleted_at = the delete's
// decision instant, its revision, revised_at, values and immutable columns intact;
// the balance moved by −1500 and accounts.version by +1; operation_revisions and
// operations row counts unchanged (nothing physically deleted); every snapshot on
// and after Aug 22 moved by exactly −1500 (one apply, observed once); NOTIFY
// op:<delete id> delivered; decisions{delete,applied} = 1.
func TestDeleteApply(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	confirmSingle(t, p, pool, cash, 200, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)) // before
	target := confirmSingle(t, p, pool, cash, 1500, t1)                                 // Aug 22
	confirmSingle(t, p, pool, cash, -300, t3)                                           // Aug 25
	confirmSingle(t, p, pool, cash, 40, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))  // later
	before := readOpState(t, pool, target)
	snapsBefore := snapshotBalances(t, pool, cash)
	_, verBefore := readAccount(t, pool, cash)
	opsBefore := countRows(t, pool, `SELECT count(*) FROM operations`)
	revsBefore := countRows(t, pool, `SELECT count(*) FROM operation_revisions`)

	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN outcomes`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	del := registerDelete(t, pool, 1, target, nil)
	if !del.isDelete() || *del.EditOf != target || del.AccountID != cash || del.Amount != 1500 || !del.EffectiveAt.Equal(t1) {
		t.Fatalf("registered delete row = %+v, want is_delete with the target's informational copy", del)
	}
	c0, s0 := snapshotRowsObserved(t, m)
	must(t, p.processSingle(ctx, del))

	d := readOpState(t, pool, del.ID)
	if d.status != string(model.OpApplied) || d.confirmed == nil || d.deletedBy != nil || d.deletedAt != nil {
		t.Fatalf("delete row = %+v, want APPLIED with a decision instant and no deletion stamps of its own", d)
	}
	after := readOpState(t, pool, target)
	if after.status != string(model.OpDeleted) {
		t.Fatalf("target status = %s, want DELETED", after.status)
	}
	if after.deletedBy == nil || *after.deletedBy != del.ID || after.deletedAt == nil || !after.deletedAt.Equal(*d.confirmed) {
		t.Fatalf("target deletion stamps = by %v at %v, want by %d at the delete's confirmed_at %v", after.deletedBy, after.deletedAt, del.ID, d.confirmed)
	}
	if after.amount != before.amount || after.accountID != before.accountID || !after.effective.Equal(before.effective) ||
		after.revision != before.revision || after.revisedAt != nil || !after.registered.Equal(before.registered) ||
		after.confirmed == nil || !after.confirmed.Equal(*before.confirmed) || after.editOf != nil || after.isDelete {
		t.Fatalf("target last values changed by the delete: before=%+v after=%+v", before, after)
	}
	if bal, ver := readAccount(t, pool, cash); bal != 200-300+40 || ver != verBefore+1 {
		t.Fatalf("cash balance=%d version=%d, want -60 (moved by -1500) / %d", bal, ver, verBefore+1)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations`); n != opsBefore+1 {
		t.Fatalf("operations rows = %d, want %d (the delete row only; nothing physically deleted)", n, opsBefore+1)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operation_revisions`); n != revsBefore {
		t.Fatalf("operation_revisions rows = %d, want %d unchanged (a delete appends no revision)", n, revsBefore)
	}

	// Snapshots: days on/after Aug 22 moved by exactly −1500, earlier days
	// untouched; one observation for the one apply; recomputation from CONFIRMED
	// rows (which now excludes the DELETED target) holds.
	snapsAfter := snapshotBalances(t, pool, cash)
	if len(snapsAfter) != len(snapsBefore) {
		t.Fatalf("snapshot day count changed: %d → %d", len(snapsBefore), len(snapsAfter))
	}
	for day, was := range snapsBefore {
		want := was
		if day >= "2026-08-22" {
			want = was - 1500
		}
		if snapsAfter[day] != want {
			t.Fatalf("snapshot %s = %d, want %d (before %d)", day, snapsAfter[day], want, was)
		}
	}
	c1, s1 := snapshotRowsObserved(t, m)
	if c1 != c0+1 || s1-s0 != 3 {
		t.Fatalf("snapshot observations grew by %d rows sum %v, want exactly one apply touching 3 rows (Aug 22, 25, 30)", c1-c0, s1-s0)
	}
	assertSnapshotsRecomputed(t, pool, cash)

	nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	want := fmt.Sprintf("op:%d", del.ID)
	for {
		n, err := conn.Conn().WaitForNotification(nctx)
		if err != nil {
			t.Fatalf("outcome notify %q not received: %v", want, err)
		}
		if n.Payload == want {
			break
		}
	}
	if got := decisionCount(t, m, obs.KindDelete, obs.OutcomeApplied); got != 1 {
		t.Fatalf("decisions{delete,applied} = %v, want 1", got)
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeApplied); got != 0 {
		t.Fatalf("a delete counted as an edit: decisions{edit,applied} = %v", got)
	}
}

// IT-012: delete after edit removes the revision-2 values. 41 {cash −1500} edited
// to −1200 (revision 2), then deleted → the balance moves by +1200, the row reads
// −1200 at revision 2 DELETED, history still has exactly the one edit row.
func TestDeleteAfterEditRemovesCurrentValues(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	must(t, p.processSingle(context.Background(), registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})))
	if bal, _ := readAccount(t, pool, cash); bal != -1200 {
		t.Fatalf("balance after edit = %d, want -1200", bal)
	}

	must(t, p.processSingle(context.Background(), registerDelete(t, pool, 2, target, nil)))

	s := readOpState(t, pool, target)
	if s.status != string(model.OpDeleted) || s.amount != -1200 || s.revision != 2 || s.deletedBy == nil {
		t.Fatalf("target = %+v, want DELETED at -1200 / revision 2", s)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0 (moved by +1200, the edited amount)", bal)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 1 || revs[0].amount != -1500 {
		t.Fatalf("history = %+v, want exactly the one edit row", revs)
	}
	assertSnapshotsRecomputed(t, pool, cash)
}

// IT-014: reversal links are inert (N7). 41 has a CONFIRMED reversal 45; deleting
// 41 leaves 45 untouched (every column, xmin) and deleting 45 leaves 41
// untouched; idx_ops_reversal still blocks a second live reversal of 41 because
// the DELETED reversal is not INVALID.
func TestDeleteLeavesReversalLinkUntouched(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", nil, nil)
	original := confirmSingle(t, p, pool, cash, -1500, t1)
	var reversal int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, reversal_of) VALUES ($1, 1500, $2, $3) RETURNING id`,
		cash, t1, original).Scan(&reversal); err != nil {
		t.Fatalf("insert reversal: %v", err)
	}
	must(t, p.processSingle(ctx, fetchPendingOp(t, pool, reversal)))
	reversalBefore := rowJSON(t, pool, reversal)

	must(t, p.processSingle(ctx, registerDelete(t, pool, 1, original, nil)))
	if s := readOpState(t, pool, original); s.status != string(model.OpDeleted) {
		t.Fatalf("original = %s, want DELETED", s.status)
	}
	if got := rowJSON(t, pool, reversal); got != reversalBefore {
		t.Fatalf("reversal row changed by deleting its original:\n before %s\n after  %s", reversalBefore, got)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 1500 {
		t.Fatalf("balance = %d, want 1500 (reversal alone)", bal)
	}

	originalBefore := rowJSON(t, pool, original)
	must(t, p.processSingle(ctx, registerDelete(t, pool, 2, reversal, nil)))
	if s := readOpState(t, pool, reversal); s.status != string(model.OpDeleted) {
		t.Fatalf("reversal = %s, want DELETED", s.status)
	}
	if got := rowJSON(t, pool, original); got != originalBefore {
		t.Fatalf("original row changed by deleting its reversal:\n before %s\n after  %s", originalBefore, got)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0", bal)
	}

	// A DELETED reversal is still "live" for idx_ops_reversal (status <> 'INVALID').
	_, err := pool.Exec(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, reversal_of) VALUES ($1, 1500, $2, $3)`, cash, t1, original)
	if err == nil || !strings.Contains(err.Error(), "idx_ops_reversal") {
		t.Fatalf("second reversal of a deleted-reversal original: err=%v, want the idx_ops_reversal unique violation", err)
	}
}

// IT-015: LIMIT_VIOLATED on removal. cash min 0; credit 41 +1500 then a −500
// spend leave 1000; deleting 41 would land at −500 → the delete is INVALID with
// LIMIT_VIOLATED{cash, min, 500} and confirmed_at stamped; 41 stays CONFIRMED
// (xmin unchanged), balance 1000, no snapshot or version write;
// decisions{delete,invalid} = 1.
func TestDeleteLimitViolationRejects(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	cash := createAccount(t, pool, editOwner, "cash", ptr(0), nil)
	target := confirmSingle(t, p, pool, cash, 1500, t1)
	confirmSingle(t, p, pool, cash, -500, t2)
	before := readOpState(t, pool, target)
	xmins := snapshotXmins(t, pool, cash)
	_, verBefore := readAccount(t, pool, cash)

	del := registerDelete(t, pool, 1, target, nil)
	must(t, p.processSingle(context.Background(), del))

	rej := requireRejection(t, readOpState(t, pool, del.ID), model.ReasonLimitViolated, target)
	if rej.Account != "cash" || rej.LimitSide != model.LimitMin || rej.Shortfall != 500 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/cash/min/500", rej)
	}
	after := readOpState(t, pool, target)
	if after.status != string(model.OpConfirmed) || after.xmin != before.xmin || after.deletedBy != nil {
		t.Fatalf("target changed by a rejected delete: before=%+v after=%+v", before, after)
	}
	if bal, ver := readAccount(t, pool, cash); bal != 1000 || ver != verBefore {
		t.Fatalf("balance=%d version=%d, want 1000/%d (untouched)", bal, ver, verBefore)
	}
	if got := snapshotXmins(t, pool, cash); fmt.Sprint(got) != fmt.Sprint(xmins) {
		t.Fatalf("snapshots rewritten by a rejected delete")
	}
	if got := decisionCount(t, m, obs.KindDelete, obs.OutcomeInvalid); got != 1 {
		t.Fatalf("decisions{delete,invalid} = %v, want 1", got)
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeInvalid); got != 0 {
		t.Fatalf("a rejected delete counted as an edit: %v", got)
	}
}

// IT-016: limits in force at processing time. The delete is registered while the
// removal would pass; the min is then raised so it violates → rejected.
func TestDeleteValidatesAgainstLimitsAtDecision(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), nil)
	target := confirmSingle(t, p, pool, cash, 1500, t1) // balance 1500

	del := registerDelete(t, pool, 1, target, nil) // removal → 0, passes min -10 000
	if _, err := pool.Exec(context.Background(), `UPDATE accounts SET min_balance = 100, version = version + 1 WHERE id = $1`, cash); err != nil {
		t.Fatalf("tighten limits: %v", err)
	}
	must(t, p.processSingle(context.Background(), del))

	rej := requireRejection(t, readOpState(t, pool, del.ID), model.ReasonLimitViolated, target)
	if rej.LimitSide != model.LimitMin || rej.Shortfall != 100 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/min/100 (1500-1500 = 0 vs min 100)", rej)
	}
	if s := readOpState(t, pool, target); s.status != string(model.OpConfirmed) {
		t.Fatalf("target changed: %+v", s)
	}
}

// IT-017: values at decision time. An edit (id 60) and an unguarded delete (id
// 61) of 41 registered back to back and drained in one batch: the edit applies
// first (id order) and the delete then removes the *edited* amount — not the
// informational copy the delete row took at submission.
func TestDeleteAfterEditInSameBatchRemovesEditedValues(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})
	del := registerDelete(t, pool, 2, target, nil)
	if del.Amount != -1500 {
		t.Fatalf("delete row copied %d at submission, want -1500 (the pre-edit value)", del.Amount)
	}
	drainAll(t, p)

	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpApplied) {
		t.Fatalf("edit = %s, want APPLIED", s.status)
	}
	if s := readOpState(t, pool, del.ID); s.status != string(model.OpApplied) {
		t.Fatalf("delete = %s, want APPLIED", s.status)
	}
	s := readOpState(t, pool, target)
	if s.status != string(model.OpDeleted) || s.amount != -1200 || s.revision != 2 || s.deletedBy == nil || *s.deletedBy != del.ID {
		t.Fatalf("target = %+v, want DELETED at -1200 / revision 2 by %d", s, del.ID)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0 (-1500 → -1200 → removed)", bal)
	}
	assertSnapshotsRecomputed(t, pool, cash)
}

// IT-018: expected_revision. 41 at revision 3 (two applied edits):
// expected_revision=2 → INVALID STALE_REVISION{41, 2, 3} with the target
// untouched; expected_revision=3 → APPLIED, target DELETED at revision 3.
func TestDeleteExpectedRevision(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	must(t, p.processSingle(context.Background(), registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})))
	must(t, p.processSingle(context.Background(), registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -1000})))
	before := readOpState(t, pool, target)
	if before.revision != 3 {
		t.Fatalf("target revision = %d, want 3", before.revision)
	}

	stale := registerDelete(t, pool, 3, target, ptr32(2))
	must(t, p.processSingle(context.Background(), stale))
	rej := requireRejection(t, readOpState(t, pool, stale.ID), model.ReasonStaleRevision, target)
	if rej.ExpectedRevision == nil || *rej.ExpectedRevision != 2 || rej.ActualRevision == nil || *rej.ActualRevision != 3 {
		t.Fatalf("rejection = %+v, want STALE_REVISION{%d, 2, 3}", rej, target)
	}
	if after := readOpState(t, pool, target); after.xmin != before.xmin || after.status != string(model.OpConfirmed) {
		t.Fatalf("target touched by a stale delete: %+v", after)
	}

	guarded := registerDelete(t, pool, 4, target, ptr32(3))
	must(t, p.processSingle(context.Background(), guarded))
	if s := readOpState(t, pool, guarded.ID); s.status != string(model.OpApplied) {
		t.Fatalf("guarded delete = %s, want APPLIED", s.status)
	}
	s := readOpState(t, pool, target)
	if s.status != string(model.OpDeleted) || s.revision != 3 || s.amount != -1000 || s.deletedBy == nil || *s.deletedBy != guarded.ID {
		t.Fatalf("target = %+v, want DELETED at revision 3 / -1000 by %d", s, guarded.ID)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 2 {
		t.Fatalf("history rows = %d, want 2 (a delete appends none)", len(revs))
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0", bal)
	}
}

// IT-019: delete vs edit of one target in the same batch, decided in id order.
// Delete (id 60) then unguarded edit (id 61) → 60 applies (41 DELETED) and 61 is
// INVALID TARGET_NOT_EDITABLE{41}; the reversed order (edit first, then delete —
// IT-017) applies both, the delete removing the edited values.
func TestDeleteThenEditSameBatchOrdering(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)

	del := registerDelete(t, pool, 1, target, nil)
	edit := registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -1200})
	if edit.ID <= del.ID {
		t.Fatalf("edit id %d must follow delete id %d", edit.ID, del.ID)
	}
	drainAll(t, p)

	if s := readOpState(t, pool, del.ID); s.status != string(model.OpApplied) {
		t.Fatalf("delete = %s, want APPLIED", s.status)
	}
	requireRejection(t, readOpState(t, pool, edit.ID), model.ReasonTargetNotEditable, target)
	s := readOpState(t, pool, target)
	if s.status != string(model.OpDeleted) || s.amount != -1500 || s.revision != 1 || s.deletedBy == nil || *s.deletedBy != del.ID {
		t.Fatalf("target = %+v, want DELETED at -1500 / revision 1 by %d", s, del.ID)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 0 {
		t.Fatalf("history rows = %d, want 0", len(revs))
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0", bal)
	}
}

// IT-040: deferral. A delete row whose target is still PENDING is handed to a
// batch (the ADR-0008 schedule: the insertion path never lets a delete become
// visible before its target, so the window only opens when a batch sees the
// delete without having decided the target): the unit is deferred — errDeferred
// swallowed, counted on the shared balancedb_edit_deferrals_total, row still
// PENDING, the drain cursor moved past it — the batch commits without it, later
// work in the same drain is still decided, and the next cycle's drain decides
// target then delete in id order.
func TestDeleteDeferredWhileTargetPending(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))

	// Build the queue by hand: a PENDING target, a PENDING delete of it, and a
	// regular operation behind them.
	target := insertPendingSingle(t, pool, cash, -1500, t1)
	var del int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, edit_of, is_delete) VALUES ($1, -1500, $2, $3, TRUE) RETURNING id`,
		cash, t1, target.ID).Scan(&del); err != nil {
		t.Fatalf("insert delete: %v", err)
	}
	later := insertPendingSingle(t, pool, cash, 700, t2)

	// The schedule: a batch that sees the delete (and the work behind it) while
	// the target is still PENDING.
	w := fetchPendingOp(t, pool, del)
	if !w.isDelete() {
		t.Fatalf("fetched row is not a delete: %+v", w)
	}
	deferredUpTo, err := p.processBatch(ctx, []workRow{
		{ID: w.ID, AccountID: w.AccountID, Amount: w.Amount, EffectiveAt: w.EffectiveAt, EditOf: w.EditOf, IsDelete: w.IsDelete, ExpectedRevision: w.ExpectedRevision},
		{ID: later.ID, AccountID: later.AccountID, Amount: later.Amount, EffectiveAt: later.EffectiveAt},
	})
	if err != nil {
		t.Fatalf("processBatch with a deferred delete returned %v, want nil (batch continues)", err)
	}
	if deferredUpTo != del {
		t.Fatalf("deferred cursor = %d, want the delete id %d", deferredUpTo, del)
	}
	if s := readOpState(t, pool, del); s.status != string(model.OpPending) {
		t.Fatalf("deferred delete status = %s, want PENDING", s.status)
	}
	if s := readOpState(t, pool, target.ID); s.status != string(model.OpPending) || s.deletedBy != nil {
		t.Fatalf("target touched by a deferral: %+v", s)
	}
	if s := readOpState(t, pool, later.ID); s.status != string(model.OpConfirmed) {
		t.Fatalf("work behind the deferred delete = %s, want CONFIRMED in the same batch", s.status)
	}
	if got := counterValue(t, m, "balancedb_edit_deferrals_total"); got != 1 {
		t.Fatalf("edit_deferrals_total = %v, want 1 (shared counter)", got)
	}
	if got := decisionCount(t, m, obs.KindDelete, obs.OutcomeApplied) + decisionCount(t, m, obs.KindDelete, obs.OutcomeInvalid); got != 0 {
		t.Fatalf("a deferral counted as a delete decision: %v", got)
	}

	// Next cycle: a full drain decides the target first (id order), then the delete.
	drainAll(t, p)
	if s := readOpState(t, pool, target.ID); s.status != string(model.OpDeleted) || s.deletedBy == nil || *s.deletedBy != del {
		t.Fatalf("target after the next cycle = %+v, want DELETED by %d", s, del)
	}
	if s := readOpState(t, pool, del); s.status != string(model.OpApplied) {
		t.Fatalf("delete after the next cycle = %s, want APPLIED", s.status)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 700 {
		t.Fatalf("balance = %d, want 700 (-1500 confirmed then removed, +700)", bal)
	}
	if got := counterValue(t, m, "balancedb_edit_deferrals_total"); got != 1 {
		t.Fatalf("edit_deferrals_total = %v after the clean cycle, want still 1", got)
	}
	assertSnapshotsRecomputed(t, pool, cash)
}

// IT-041: a delete of an INVALID target is INVALID with TARGET_NOT_EDITABLE{41};
// the target row is untouched.
func TestDeleteInvalidTargetRejects(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(0), nil)
	rejected := insertPendingSingle(t, pool, cash, -1500, t1)
	must(t, p.processSingle(context.Background(), rejected))
	before := rowJSON(t, pool, rejected.ID)
	if s := readOpState(t, pool, rejected.ID); s.status != string(model.OpInvalid) {
		t.Fatalf("target = %s, want INVALID", s.status)
	}

	del := registerDelete(t, pool, 1, rejected.ID, nil)
	must(t, p.processSingle(context.Background(), del))
	requireRejection(t, readOpState(t, pool, del.ID), model.ReasonTargetNotEditable, rejected.ID)
	if got := rowJSON(t, pool, rejected.ID); got != before {
		t.Fatalf("INVALID target changed:\n before %s\n after  %s", before, got)
	}
}

// IT-042: a DELETED target. A second delete and an edit of DELETED 41 are both
// registered (the ledger refuses nothing at submission — user decision) and both
// decided INVALID TARGET_NOT_EDITABLE{41}; 41 is unchanged column by column and
// by tuple identity (deleted_by still the first delete, revision unchanged). A
// group with an edit_of leg on the DELETED target is REJECTED as a whole with the
// same reason, its regular leg INVALID, nothing applied.
func TestDeletedTargetRejectsEditsAndDeletes(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	first := registerDelete(t, pool, 1, target, nil)
	must(t, p.processSingle(ctx, first))
	deleted := readOpState(t, pool, target)
	if deleted.status != string(model.OpDeleted) || deleted.deletedBy == nil || *deleted.deletedBy != first.ID {
		t.Fatalf("target = %+v, want DELETED by %d", deleted, first.ID)
	}
	rowBefore := rowJSON(t, pool, target)
	_, verBefore := readAccount(t, pool, cash)

	second := registerDelete(t, pool, 2, target, nil)
	edit := registerEdit(t, pool, 3, api.InsertOp{EditOf: &target, Amount: -1200})
	guarded := registerDelete(t, pool, 4, target, ptr32(1)) // even a matching guard: status rules first
	drainAll(t, p)

	for _, id := range []int64{second.ID, edit.ID, guarded.ID} {
		rej := requireRejection(t, readOpState(t, pool, id), model.ReasonTargetNotEditable, target)
		if rej.ExpectedRevision != nil || rej.ActualRevision != nil {
			t.Fatalf("row %d: TARGET_NOT_EDITABLE must not carry revisions: %+v", id, rej)
		}
	}
	if got := rowJSON(t, pool, target); got != rowBefore {
		t.Fatalf("DELETED target changed:\n before %s\n after  %s", rowBefore, got)
	}
	if bal, ver := readAccount(t, pool, cash); bal != 0 || ver != verBefore {
		t.Fatalf("balance=%d version=%d, want 0/%d (untouched)", bal, ver, verBefore)
	}
	if got := decisionCount(t, m, obs.KindDelete, obs.OutcomeInvalid); got != 2 {
		t.Fatalf("decisions{delete,invalid} = %v, want 2", got)
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeInvalid); got != 1 {
		t.Fatalf("decisions{edit,invalid} = %v, want 1", got)
	}

	// Grouped edit leg on the DELETED target: the whole group is REJECTED.
	var txID int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, err := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: "00000000-0000-4000-8000-000000000105",
			Operations: []api.InsertOp{
				{OwnerID: editOwner, EditOf: &target, Amount: -1100},
				{OwnerID: editOwner, ExternalID: "cash", Amount: 300, EffectiveAt: t2},
			},
		})
		if err != nil {
			return err
		}
		txID = *res.TransactionID
		return nil
	})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	must(t, p.processGroup(ctx, txID))
	var txStatus string
	var reason *string
	if err := pool.QueryRow(ctx, `SELECT status, reject_reason FROM transactions WHERE id = $1`, txID).Scan(&txStatus, &reason); err != nil {
		t.Fatalf("read transaction: %v", err)
	}
	if txStatus != string(model.TxRejected) || reason == nil {
		t.Fatalf("group = %s (%v), want REJECTED with a reason", txStatus, reason)
	}
	rej, err := model.ParseRejection(*reason)
	if err != nil || rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != target {
		t.Fatalf("group reason = %+v (%v), want TARGET_NOT_EDITABLE{%d}", rej, err, target)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations WHERE transaction_id = $1 AND status = 'INVALID'`, txID); n != 2 {
		t.Fatalf("INVALID legs = %d, want 2", n)
	}
	if got := rowJSON(t, pool, target); got != rowBefore {
		t.Fatalf("DELETED target changed by a rejected group:\n before %s\n after  %s", rowBefore, got)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("balance = %d, want 0", bal)
	}
}

// IT-043: a reversal of a DELETED operation is an ordinary operation (N7): it
// registers and is decided on its own limits — CONFIRMED here — and the DELETED
// row is untouched.
func TestReversalOfDeletedIsOrdinary(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	must(t, p.processSingle(ctx, registerDelete(t, pool, 1, target, nil)))
	rowBefore := rowJSON(t, pool, target)

	var reversal int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, err := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: "00000000-0000-4000-8000-000000000106",
			Operations:     []api.InsertOp{{OwnerID: editOwner, ExternalID: "cash", Amount: 1500, EffectiveAt: t1, ReversalOf: &target}},
		})
		if err != nil {
			return err
		}
		reversal = res.Operations[0].ID
		return nil
	})
	if err != nil {
		t.Fatalf("register reversal of a DELETED operation: %v", err)
	}
	drainAll(t, p)

	if s := readOpState(t, pool, reversal); s.status != string(model.OpConfirmed) {
		t.Fatalf("reversal = %+v, want CONFIRMED as an ordinary operation", s)
	}
	if got := rowJSON(t, pool, target); got != rowBefore {
		t.Fatalf("DELETED original changed by its reversal:\n before %s\n after  %s", rowBefore, got)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 1500 {
		t.Fatalf("balance = %d, want 1500 (deleted -1500 leaves 0; reversal +1500)", bal)
	}
}

// IT-045: delete CAS miss. The afterTargetRead seam bumps 41's revision from a
// second connection between the target read and the CAS → rowcount 0 →
// errGuardMiss, batch rolled back: the delete still PENDING, 41 CONFIRMED with
// no deletion stamps, balance untouched. A seam that instead flips 41 DELETED
// (a concurrent delete apply) misses the same way. The clean retry applies.
func TestDeleteCASMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	del := registerDelete(t, pool, 1, target, nil)

	p.afterTargetRead = func() {
		if _, err := pool.Exec(ctx, `UPDATE operations SET revision = revision + 1 WHERE id = $1`, target); err != nil {
			t.Errorf("hook revision bump: %v", err)
		}
	}
	if err := p.processSingle(ctx, del); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a moved revision, got %v", err)
	}
	if s := readOpState(t, pool, del.ID); s.status != string(model.OpPending) {
		t.Fatalf("delete status = %s, want still PENDING after the CAS miss", s.status)
	}
	if s := readOpState(t, pool, target); s.status != string(model.OpConfirmed) || s.deletedBy != nil || s.deletedAt != nil || s.revision != 2 {
		t.Fatalf("target = %+v, want CONFIRMED at revision 2 (the hook's bump only)", s)
	}
	if bal, ver := readAccount(t, pool, cash); bal != -1500 || ver != 1 {
		t.Fatalf("balance=%d version=%d, want -1500/1 (untouched)", bal, ver)
	}

	// A concurrent delete apply (target already DELETED) misses the same way.
	p.afterTargetRead = func() {
		if _, err := pool.Exec(ctx,
			`UPDATE operations SET status = 'DELETED', deleted_by = $2, deleted_at = now() WHERE id = $1`, target, del.ID); err != nil {
			t.Errorf("hook concurrent delete: %v", err)
		}
	}
	if err := p.processSingle(ctx, del); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a concurrently deleted target, got %v", err)
	}
	if s := readOpState(t, pool, del.ID); s.status != string(model.OpPending) {
		t.Fatalf("delete status = %s, want still PENDING", s.status)
	}
	if bal, ver := readAccount(t, pool, cash); bal != -1500 || ver != 1 {
		t.Fatalf("balance=%d version=%d after the second miss, want -1500/1", bal, ver)
	}
	// Undo the hook's stand-in so the clean retry meets a CONFIRMED target.
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'CONFIRMED', deleted_by = NULL, deleted_at = NULL WHERE id = $1`, target); err != nil {
		t.Fatalf("restore target: %v", err)
	}

	// Clean retry next cycle: reads revision 2, CAS succeeds.
	p.afterTargetRead = nil
	must(t, p.processSingle(ctx, del))
	if s := readOpState(t, pool, del.ID); s.status != string(model.OpApplied) {
		t.Fatalf("delete status = %s on clean retry, want APPLIED", s.status)
	}
	s := readOpState(t, pool, target)
	if s.status != string(model.OpDeleted) || s.revision != 2 || s.deletedBy == nil || *s.deletedBy != del.ID {
		t.Fatalf("target = %+v, want DELETED at revision 2 by %d", s, del.ID)
	}
	if bal, ver := readAccount(t, pool, cash); bal != 0 || ver != 2 {
		t.Fatalf("balance=%d version=%d, want 0/2", bal, ver)
	}
}

// A zombie leader (lease tampered from under it) deciding a delete trips Guard 1
// — nothing is written; a proper leader then decides it cleanly.
func TestDeleteZombieLeaderWritesNothing(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	del := registerDelete(t, pool, 1, target, nil)
	before := rowJSON(t, pool, target)

	if _, err := pool.Exec(ctx, `UPDATE leader_lease SET owner = NULL, lease_until = NULL`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := p.processSingle(ctx, del); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss from a zombie leader, got %v", err)
	}
	if s := readOpState(t, pool, del.ID); s.status != string(model.OpPending) {
		t.Fatalf("delete status = %s, want PENDING", s.status)
	}
	if after := rowJSON(t, pool, target); after != before {
		t.Fatalf("target changed by a zombie:\n before %s\n after  %s", before, after)
	}

	leader := leaderProcessor(t, pool)
	must(t, leader.processSingle(ctx, del))
	if s := readOpState(t, pool, target); s.status != string(model.OpDeleted) {
		t.Fatalf("target = %s after a real leader, want DELETED", s.status)
	}
}
