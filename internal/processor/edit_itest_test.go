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

// Integration tests for the single-edit decision path (ADR-0010): IT-010–IT-019,
// IT-025–IT-027 and IT-042 of the operation-editing test contract. Edits are
// registered through the shared ledger core (api.Insert) so the rows carry exactly
// what production writes, then decided through the processor's own entry points.

const editOwner int64 = 1

var (
	t1 = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 8, 22, 15, 30, 0, 0, time.UTC) // same UTC day as t1
	t3 = time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)   // three days later
)

// opState is the full current projection of an operations row plus its xmin, so
// a test can assert "unchanged" column by column and by tuple identity.
type opState struct {
	status     string
	accountID  int64
	amount     int64
	effective  time.Time
	revision   int32
	revisedAt  *time.Time
	editOf     *int64
	isDelete   bool
	expected   *int32
	confirmed  *time.Time
	reason     *string
	registered time.Time
	deletedBy  *int64
	deletedAt  *time.Time
	xmin       string
}

func readOpState(t *testing.T, pool *pgxpool.Pool, id int64) opState {
	t.Helper()
	var s opState
	err := pool.QueryRow(context.Background(),
		`SELECT status, account_id, amount, effective_at, revision, revised_at, edit_of, is_delete, expected_revision,
		        confirmed_at, invalidation_reason, registered_at, deleted_by, deleted_at, xmin::text
		   FROM operations WHERE id = $1`, id).
		Scan(&s.status, &s.accountID, &s.amount, &s.effective, &s.revision, &s.revisedAt, &s.editOf, &s.isDelete, &s.expected,
			&s.confirmed, &s.reason, &s.registered, &s.deletedBy, &s.deletedAt, &s.xmin)
	if err != nil {
		t.Fatalf("read op %d: %v", id, err)
	}
	return s
}

// rowJSON renders the whole operations row as JSON plus its xmin, for
// "unchanged, column by column and by tuple identity" assertions.
func rowJSON(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var row string
	if err := pool.QueryRow(context.Background(),
		`SELECT xmin::text || ' ' || row_to_json(o)::text FROM operations o WHERE id = $1`, id).Scan(&row); err != nil {
		t.Fatalf("row_to_json %d: %v", id, err)
	}
	return row
}

// revisionRow mirrors one operation_revisions row.
type revisionRow struct {
	revision     int32
	accountID    int64
	amount       int64
	effective    time.Time
	recordedAt   time.Time
	supersededBy int64
	supersededAt time.Time
}

func readRevisions(t *testing.T, pool *pgxpool.Pool, opID int64) []revisionRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT revision, account_id, amount, effective_at, recorded_at, superseded_by, superseded_at
		   FROM operation_revisions WHERE operation_id = $1 ORDER BY revision`, opID)
	if err != nil {
		t.Fatalf("read revisions of %d: %v", opID, err)
	}
	defer rows.Close()
	var out []revisionRow
	for rows.Next() {
		var r revisionRow
		if err := rows.Scan(&r.revision, &r.accountID, &r.amount, &r.effective, &r.recordedAt, &r.supersededBy, &r.supersededAt); err != nil {
			t.Fatalf("scan revision: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate revisions: %v", err)
	}
	return out
}

// confirmSingle registers and decides one regular operation, returning its id.
func confirmSingle(t *testing.T, p *Processor, pool *pgxpool.Pool, acct, amount int64, when time.Time) int64 {
	t.Helper()
	op := insertPendingSingle(t, pool, acct, amount, when)
	must(t, p.processSingle(context.Background(), op))
	if s := readOpState(t, pool, op.ID); s.status != string(model.OpConfirmed) {
		t.Fatalf("op %d status = %s, want CONFIRMED", op.ID, s.status)
	}
	return op.ID
}

// registerEdit registers one edit item through the shared ledger core (so the row
// carries the resolved full state exactly as production writes it) and returns
// the pendingOp the drain would fetch for it.
func registerEdit(t *testing.T, pool *pgxpool.Pool, key int, item api.InsertOp) pendingOp {
	t.Helper()
	ctx := context.Background()
	item.OwnerID = editOwner
	var id int64
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, err := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: fmt.Sprintf("00000000-0000-4000-8000-%012d", key),
			Operations:     []api.InsertOp{item},
		})
		if err != nil {
			return err
		}
		id = res.Operations[0].ID
		return nil
	})
	if err != nil {
		t.Fatalf("register edit: %v", err)
	}
	return fetchPendingOp(t, pool, id)
}

// fetchPendingOp reads one PENDING row back into the pendingOp shape the
// dispatcher builds from fetchPendingWork.
func fetchPendingOp(t *testing.T, pool *pgxpool.Pool, id int64) pendingOp {
	t.Helper()
	op := pendingOp{ID: id}
	if err := pool.QueryRow(context.Background(),
		`SELECT account_id, amount, effective_at, edit_of, is_delete, expected_revision FROM operations WHERE id = $1`, id).
		Scan(&op.AccountID, &op.Amount, &op.EffectiveAt, &op.EditOf, &op.IsDelete, &op.ExpectedRevision); err != nil {
		t.Fatalf("read pending op %d: %v", id, err)
	}
	return op
}

// drainAll runs one leader drain (spec §8.1) with a large batch.
func drainAll(t *testing.T, p *Processor) {
	t.Helper()
	if err := p.drain(context.Background(), 200); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// assertSnapshotsRecomputed checks every snapshot row of an account against a
// recomputation from CONFIRMED rows: the cumulative sum of amounts whose
// effective_at UTC day is on or before the snapshot day (spec §8.4). Edit and
// delete rows are never CONFIRMED and a DELETED row is no longer CONFIRMED, so
// none of them enters the sum.
func assertSnapshotsRecomputed(t *testing.T, pool *pgxpool.Pool, acct int64) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT day::text, balance FROM balance_snapshots WHERE account_id = $1 ORDER BY day`, acct)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	type snap struct {
		day string
		bal int64
	}
	var snaps []snap
	for rows.Next() {
		var s snap
		if err := rows.Scan(&s.day, &s.bal); err != nil {
			rows.Close()
			t.Fatalf("scan snapshot: %v", err)
		}
		snaps = append(snaps, s)
	}
	rows.Close()
	for _, s := range snaps {
		var want int64
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(sum(amount), 0) FROM operations
			  WHERE account_id = $1 AND status = 'CONFIRMED'
			    AND (effective_at AT TIME ZONE 'UTC')::date <= $2::date`, acct, s.day).Scan(&want); err != nil {
			t.Fatalf("recompute %s: %v", s.day, err)
		}
		if s.bal != want {
			t.Fatalf("snapshot %s for account %d = %d, recomputed from CONFIRMED rows = %d", s.day, acct, s.bal, want)
		}
	}
}

// snapshotXmins returns xmin per snapshot day, to assert rows were not rewritten.
func snapshotXmins(t *testing.T, pool *pgxpool.Pool, acct int64) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT day::text, xmin::text FROM balance_snapshots WHERE account_id = $1`, acct)
	if err != nil {
		t.Fatalf("read snapshot xmins: %v", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var day, xmin string
		if err := rows.Scan(&day, &xmin); err != nil {
			t.Fatalf("scan snapshot xmin: %v", err)
		}
		out[day] = xmin
	}
	return out
}

// snapshotRowsObserved reads the balancedb_snapshot_rows_touched histogram's
// sample count and sum from the registry, so a test can assert the value of a
// single new observation as a delta.
func snapshotRowsObserved(t *testing.T, m *obs.Metrics) (count uint64, sum float64) {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "balancedb_snapshot_rows_touched" {
			h := f.GetMetric()[0].GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	return 0, 0
}

// counterValue reads one plain counter from the registry (0 if absent).
func counterValue(t *testing.T, m *obs.Metrics, name string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// decisionCount reads balancedb_decisions_total for one (kind, outcome).
func decisionCount(t *testing.T, m *obs.Metrics, kind, outcome string) float64 {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "balancedb_decisions_total" {
			continue
		}
		for _, mt := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range mt.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["kind"] == kind && labels["outcome"] == outcome {
				return mt.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// IT-010: the golden apply. Target {cash −1500} CONFIRMED, edit to −1200 → the
// edit row is APPLIED, the target reads −1200 at revision 2 with revised_at set,
// the balance moved by +300, exactly one history row {41, 1, cash, −1500, T1,
// recorded_at = registered_at, superseded_by = edit id} exists, and the outcome
// NOTIFY op:<edit id> is delivered on commit. The decision counter gains
// kind=edit outcome=applied.
func TestEditApply(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	before := readOpState(t, pool, target)

	// Observe the outcome channel on a dedicated connection.
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN outcomes`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})
	if edit.EditOf == nil || *edit.EditOf != target || edit.Amount != -1200 || edit.AccountID != cash || !edit.EffectiveAt.Equal(t1) {
		t.Fatalf("registered edit row = %+v, want resolved full state", edit)
	}
	must(t, p.processSingle(ctx, edit))

	e := readOpState(t, pool, edit.ID)
	if e.status != string(model.OpApplied) || e.confirmed == nil {
		t.Fatalf("edit status=%s confirmed_at=%v, want APPLIED with a decision instant", e.status, e.confirmed)
	}
	after := readOpState(t, pool, target)
	if after.status != string(model.OpConfirmed) || after.amount != -1200 || after.revision != 2 || after.revisedAt == nil {
		t.Fatalf("target after apply = %+v, want CONFIRMED/-1200/revision 2/revised_at set", after)
	}
	if after.accountID != cash || !after.effective.Equal(t1) || !after.registered.Equal(before.registered) || after.confirmed == nil || !after.confirmed.Equal(*before.confirmed) {
		t.Fatalf("target immutable columns changed: before=%+v after=%+v", before, after)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1200 {
		t.Fatalf("balance = %d, want -1200 (moved by +300)", bal)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 1 {
		t.Fatalf("history rows = %d, want 1", len(revs))
	}
	r := revs[0]
	if r.revision != 1 || r.accountID != cash || r.amount != -1500 || !r.effective.Equal(t1) || r.supersededBy != edit.ID {
		t.Fatalf("history row = %+v, want {1, cash, -1500, t1, superseded_by %d}", r, edit.ID)
	}
	if !r.recordedAt.Equal(before.registered) {
		t.Fatalf("recorded_at = %v, want the target's registered_at %v", r.recordedAt, before.registered)
	}
	if !r.supersededAt.Equal(*after.revisedAt) {
		t.Fatalf("superseded_at = %v, want the target's new revised_at %v", r.supersededAt, *after.revisedAt)
	}

	// The outcomes channel is per database, not per schema (ADR-0002), so other
	// packages' itests running in parallel may ring it too: skip foreign payloads.
	nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	want := fmt.Sprintf("op:%d", edit.ID)
	for {
		n, err := conn.Conn().WaitForNotification(nctx)
		if err != nil {
			t.Fatalf("outcome notify %q not received: %v", want, err)
		}
		if n.Payload == want {
			break
		}
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeApplied); got != 1 {
		t.Fatalf("decisions{edit,applied} = %v, want 1", got)
	}
	assertSnapshotsRecomputed(t, pool, cash)
}

// IT-011: effective_at moved across a snapshot day. With confirmed operations on
// the old day, days between, and the new day, the moved operation leaves its old
// day and lands on the new one; every snapshot equals the recomputation from
// CONFIRMED rows.
func TestEditMoveAcrossDays(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", nil, nil)
	target := confirmSingle(t, p, pool, cash, 500, t1)                                   // Aug 22
	confirmSingle(t, p, pool, cash, 40, time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC))   // Aug 23
	confirmSingle(t, p, pool, cash, 60, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))   // Aug 24
	confirmSingle(t, p, pool, cash, 7, t3)                                               // Aug 25
	confirmSingle(t, p, pool, cash, 1000, time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)) // later

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, EffectiveAt: t3})
	must(t, p.processSingle(context.Background(), edit))

	s := readOpState(t, pool, target)
	if s.status != "CONFIRMED" || !s.effective.Equal(t3) || s.amount != 500 || s.revision != 2 {
		t.Fatalf("target = %+v, want moved to t3 at revision 2 with amount kept", s)
	}
	for _, tc := range []struct {
		day  string
		want int64
	}{
		{"2026-08-22", 0}, {"2026-08-23", 40}, {"2026-08-24", 100}, {"2026-08-25", 607}, {"2026-08-30", 1607},
	} {
		if b, ok := snapshotBalance(t, pool, cash, tc.day); !ok || b != tc.want {
			t.Fatalf("snapshot %s = %d (ok=%v), want %d", tc.day, b, ok, tc.want)
		}
	}
	assertSnapshotsRecomputed(t, pool, cash)
	if bal, _ := readAccount(t, pool, cash); bal != 1607 {
		t.Fatalf("final balance = %d, want 1607 (unchanged by a time move)", bal)
	}
}

// IT-012: account moved to a new external id. The ledger creates the account on
// demand at registration; the apply moves −1500 off cash (+1500) and onto wallet
// (−1500), with snapshots on both accounts.
func TestEditMoveAccount(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, ExternalID: "wallet"})
	var wallet int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM accounts WHERE owner_id = $1 AND external_id = 'wallet'`, editOwner).Scan(&wallet); err != nil {
		t.Fatalf("wallet account not created on demand: %v", err)
	}
	if edit.AccountID != wallet || edit.Amount != -1500 || !edit.EffectiveAt.Equal(t1) {
		t.Fatalf("edit row = %+v, want wallet/-1500/t1", edit)
	}
	must(t, p.processSingle(context.Background(), edit))

	s := readOpState(t, pool, target)
	if s.accountID != wallet || s.amount != -1500 || s.revision != 2 {
		t.Fatalf("target = %+v, want on wallet at revision 2", s)
	}
	if bal, ver := readAccount(t, pool, cash); bal != 0 || ver != 2 {
		t.Fatalf("cash balance=%d version=%d, want 0/2", bal, ver)
	}
	if bal, ver := readAccount(t, pool, wallet); bal != -1500 || ver != 1 {
		t.Fatalf("wallet balance=%d version=%d, want -1500/1", bal, ver)
	}
	if b, ok := snapshotBalance(t, pool, cash, "2026-08-22"); !ok || b != 0 {
		t.Fatalf("cash snapshot = %d (ok=%v), want 0", b, ok)
	}
	if b, ok := snapshotBalance(t, pool, wallet, "2026-08-22"); !ok || b != -1500 {
		t.Fatalf("wallet snapshot = %d (ok=%v), want -1500", b, ok)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 1 || revs[0].accountID != cash {
		t.Fatalf("history = %+v, want one row on cash", revs)
	}
	assertSnapshotsRecomputed(t, pool, cash)
	assertSnapshotsRecomputed(t, pool, wallet)
}

// IT-013: a no-op edit (the same amount re-sent) is APPLIED and recorded as a
// revision with identical values; balance and snapshots unchanged.
func TestEditNoOpRecordsRevision(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	xmins := snapshotXmins(t, pool, cash)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1500})
	must(t, p.processSingle(context.Background(), edit))

	if e := readOpState(t, pool, edit.ID); e.status != string(model.OpApplied) {
		t.Fatalf("edit status = %s, want APPLIED", e.status)
	}
	s := readOpState(t, pool, target)
	if s.revision != 2 || s.amount != -1500 || s.accountID != cash || !s.effective.Equal(t1) {
		t.Fatalf("target = %+v, want revision 2 with identical values", s)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 1 || revs[0].amount != -1500 || revs[0].accountID != cash || !revs[0].effective.Equal(t1) {
		t.Fatalf("history = %+v, want one identical row", revs)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1500 {
		t.Fatalf("balance = %d, want -1500 (unchanged)", bal)
	}
	if got := snapshotXmins(t, pool, cash); fmt.Sprint(got) != fmt.Sprint(xmins) {
		t.Fatalf("snapshot rows rewritten by a zero-delta edit: before=%v after=%v", xmins, got)
	}
}

// IT-014: editing the original of a confirmed reversal leaves the reversal row
// untouched — every column and its xmin (N7: reversal_of is inert metadata).
func TestEditOriginalLeavesReversalUntouched(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", nil, nil)
	original := confirmSingle(t, p, pool, cash, -1500, t1)
	var reversal int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO operations (account_id, amount, effective_at, reversal_of) VALUES ($1, 1500, $2, $3) RETURNING id`,
		cash, t1, original).Scan(&reversal); err != nil {
		t.Fatalf("insert reversal: %v", err)
	}
	must(t, p.processSingle(context.Background(), fetchPendingOp(t, pool, reversal)))
	rowBefore := rowJSON(t, pool, reversal)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &original, Amount: -1200})
	must(t, p.processSingle(context.Background(), edit))

	if rowAfter := rowJSON(t, pool, reversal); rowBefore != rowAfter {
		t.Fatalf("reversal row changed:\n before %s\n after  %s", rowBefore, rowAfter)
	}
	if s := readOpState(t, pool, original); s.amount != -1200 || s.revision != 2 {
		t.Fatalf("original = %+v, want -1200 at revision 2", s)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 300 {
		t.Fatalf("balance = %d, want 300 (-1200 + 1500)", bal)
	}
}

// IT-015: an edit whose net breaks min is INVALID with
// LIMIT_VIOLATED{cash, min, shortfall}; the target keeps revision 1 and its
// values, no history row, no balance or snapshot change, no version bump.
func TestEditLimitViolationRejects(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	cash := createAccount(t, pool, editOwner, "cash", ptr(0), nil)
	confirmSingle(t, p, pool, cash, 2000, t1)
	target := confirmSingle(t, p, pool, cash, -1500, t2) // balance 500
	before := readOpState(t, pool, target)
	xmins := snapshotXmins(t, pool, cash)
	_, verBefore := readAccount(t, pool, cash)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -2100}) // net -600 → -100 < 0
	must(t, p.processSingle(context.Background(), edit))

	e := readOpState(t, pool, edit.ID)
	if e.status != string(model.OpInvalid) || e.reason == nil || e.confirmed == nil {
		t.Fatalf("edit = %+v, want INVALID with a reason and a decision instant", e)
	}
	rej, err := model.ParseRejection(*e.reason)
	if err != nil {
		t.Fatalf("parse rejection: %v", err)
	}
	if rej.Code != model.ReasonLimitViolated || rej.Account != "cash" || rej.LimitSide != model.LimitMin || rej.Shortfall != 100 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/cash/min/100", rej)
	}
	after := readOpState(t, pool, target)
	if after.revision != 1 || after.amount != before.amount || after.xmin != before.xmin {
		t.Fatalf("target changed by a rejected edit: before=%+v after=%+v", before, after)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 0 {
		t.Fatalf("history rows = %d, want none", len(revs))
	}
	if bal, ver := readAccount(t, pool, cash); bal != 500 || ver != verBefore {
		t.Fatalf("balance=%d version=%d, want 500/%d (untouched)", bal, ver, verBefore)
	}
	if got := snapshotXmins(t, pool, cash); fmt.Sprint(got) != fmt.Sprint(xmins) {
		t.Fatalf("snapshots rewritten by a rejected edit")
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeInvalid); got != 1 {
		t.Fatalf("decisions{edit,invalid} = %v, want 1", got)
	}
}

// IT-016: limits tightened after registration and before decision are the limits
// the edit is validated against.
func TestEditValidatesAgainstLimitsAtDecision(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", nil, ptr(10_000))
	target := confirmSingle(t, p, pool, cash, 100, t1)

	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: 900}) // would pass max 10 000
	if _, err := pool.Exec(context.Background(), `UPDATE accounts SET max_balance = 500, version = version + 1 WHERE id = $1`, cash); err != nil {
		t.Fatalf("tighten limits: %v", err)
	}
	must(t, p.processSingle(context.Background(), edit))

	e := readOpState(t, pool, edit.ID)
	if e.status != string(model.OpInvalid) || e.reason == nil {
		t.Fatalf("edit = %+v, want INVALID under the tightened max", e)
	}
	rej, _ := model.ParseRejection(*e.reason)
	if rej.Code != model.ReasonLimitViolated || rej.LimitSide != model.LimitMax || rej.Shortfall != 400 {
		t.Fatalf("rejection = %+v, want LIMIT_VIOLATED/max/400 (100-100+900 = 900 vs 500)", rej)
	}
	if s := readOpState(t, pool, target); s.amount != 100 || s.revision != 1 {
		t.Fatalf("target changed: %+v", s)
	}
}

// IT-017: two edits of one target registered back to back. Drained in one
// batch, the first applies and the second computes its delta against the
// first's result (same-transaction visibility): −1500 → −1200 → −1000 leaves the
// balance at −1000, the target at revision 3, and two dense history rows whose
// recorded_at chain through revised_at.
func TestEditSequentialEditsChain(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)

	e1 := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})
	e2 := registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -1000})
	drainAll(t, p)

	for _, id := range []int64{e1.ID, e2.ID} {
		if s := readOpState(t, pool, id); s.status != string(model.OpApplied) {
			t.Fatalf("edit %d status = %s, want APPLIED", id, s.status)
		}
	}
	s := readOpState(t, pool, target)
	if s.amount != -1000 || s.revision != 3 {
		t.Fatalf("target = %+v, want -1000 at revision 3", s)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1000 {
		t.Fatalf("balance = %d, want -1000 (+300 then +200)", bal)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 2 || revs[0].revision != 1 || revs[1].revision != 2 {
		t.Fatalf("history = %+v, want revisions 1 and 2", revs)
	}
	if revs[0].amount != -1500 || revs[0].supersededBy != e1.ID || revs[1].amount != -1200 || revs[1].supersededBy != e2.ID {
		t.Fatalf("history = %+v, want {-1500 by e1}, {-1200 by e2}", revs)
	}
	if !revs[1].recordedAt.Equal(revs[0].supersededAt) {
		t.Fatalf("revision 2 recorded_at %v != revision 1 superseded_at %v", revs[1].recordedAt, revs[0].supersededAt)
	}
	if !s.revisedAt.Equal(revs[1].supersededAt) {
		t.Fatalf("current revised_at %v != revision 2 superseded_at %v", s.revisedAt, revs[1].supersededAt)
	}
	assertSnapshotsRecomputed(t, pool, cash)
}

// IT-018 / IT-019: expected_revision. Equal to the current revision → applied;
// stale → INVALID STALE_REVISION{target, expected, actual} and the target
// untouched; two unguarded edits both apply, in id order.
func TestEditExpectedRevision(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)

	// IT-018: guard matches.
	guarded := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200, ExpectedRevision: ptr32(1)})
	must(t, p.processSingle(context.Background(), guarded))
	if s := readOpState(t, pool, guarded.ID); s.status != string(model.OpApplied) {
		t.Fatalf("guarded edit status = %s, want APPLIED", s.status)
	}
	if s := readOpState(t, pool, target); s.revision != 2 {
		t.Fatalf("target revision = %d, want 2", s.revision)
	}

	// IT-019: stale guard (still expecting revision 1).
	stale := registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -1100, ExpectedRevision: ptr32(1)})
	must(t, p.processSingle(context.Background(), stale))
	e := readOpState(t, pool, stale.ID)
	if e.status != string(model.OpInvalid) || e.reason == nil {
		t.Fatalf("stale edit = %+v, want INVALID", e)
	}
	rej, err := model.ParseRejection(*e.reason)
	if err != nil {
		t.Fatalf("parse rejection: %v", err)
	}
	if rej.Code != model.ReasonStaleRevision || rej.OperationID == nil || *rej.OperationID != target ||
		rej.ExpectedRevision == nil || *rej.ExpectedRevision != 1 || rej.ActualRevision == nil || *rej.ActualRevision != 2 {
		t.Fatalf("rejection = %+v, want STALE_REVISION{%d, 1, 2}", rej, target)
	}
	if rej.Account != "" || rej.LimitSide != "" || rej.Shortfall != 0 {
		t.Fatalf("STALE_REVISION must not carry limit detail: %+v", rej)
	}
	if s := readOpState(t, pool, target); s.revision != 2 || s.amount != -1200 {
		t.Fatalf("target changed by a stale edit: %+v", s)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 1 {
		t.Fatalf("history rows = %d, want 1", len(revs))
	}

	// Two unguarded edits both apply, in id order (last writer wins by id).
	u1 := registerEdit(t, pool, 3, api.InsertOp{EditOf: &target, Amount: -1000})
	u2 := registerEdit(t, pool, 4, api.InsertOp{EditOf: &target, Amount: -900})
	drainAll(t, p)
	for _, id := range []int64{u1.ID, u2.ID} {
		if s := readOpState(t, pool, id); s.status != string(model.OpApplied) {
			t.Fatalf("unguarded edit %d status = %s, want APPLIED", id, s.status)
		}
	}
	s := readOpState(t, pool, target)
	if s.amount != -900 || s.revision != 4 {
		t.Fatalf("target = %+v, want -900 at revision 4", s)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 3 || revs[1].amount != -1200 || revs[1].supersededBy != u1.ID || revs[2].amount != -1000 || revs[2].supersededBy != u2.ID {
		t.Fatalf("history = %+v, want -1500 / -1200 by u1 / -1000 by u2", revs)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -900 {
		t.Fatalf("balance = %d, want -900", bal)
	}
}

// IT-025: revision CAS miss. The afterTargetRead seam bumps the target's
// revision from a second connection between the target read and the CAS →
// rowcount 0 → errGuardMiss, batch rolled back: edit still PENDING, no history
// row, balance untouched. The next cycle re-decides against the new revision.
func TestEditRevisionCASMiss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})

	p.afterTargetRead = func() {
		if _, err := pool.Exec(context.Background(),
			`UPDATE operations SET revision = revision + 1 WHERE id = $1`, target); err != nil {
			t.Errorf("hook revision bump: %v", err)
		}
	}
	if err := p.processSingle(context.Background(), edit); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a moved revision, got %v", err)
	}
	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpPending) {
		t.Fatalf("edit status = %s, want still PENDING after the CAS miss", s.status)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 0 {
		t.Fatalf("history rows = %d after a rolled-back apply, want 0", len(revs))
	}
	if bal, ver := readAccount(t, pool, cash); bal != -1500 || ver != 1 {
		t.Fatalf("balance=%d version=%d, want -1500/1 (untouched)", bal, ver)
	}
	if s := readOpState(t, pool, target); s.revision != 2 || s.amount != -1500 {
		t.Fatalf("target = %+v, want only the hook's revision bump", s)
	}

	// Clean retry next cycle: reads revision 2, CAS succeeds, history records 2.
	p.afterTargetRead = nil
	must(t, p.processSingle(context.Background(), edit))
	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpApplied) {
		t.Fatalf("edit status = %s on clean retry, want APPLIED", s.status)
	}
	s := readOpState(t, pool, target)
	if s.revision != 3 || s.amount != -1200 {
		t.Fatalf("target = %+v, want -1200 at revision 3", s)
	}
	revs := readRevisions(t, pool, target)
	if len(revs) != 1 || revs[0].revision != 2 || revs[0].amount != -1500 {
		t.Fatalf("history = %+v, want one row superseding revision 2", revs)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1200 {
		t.Fatalf("balance = %d, want -1200", bal)
	}
}

// IT-026: a zombie leader (lease tampered from under it) deciding an edit trips
// Guard 1 — nothing is written: the edit stays PENDING, the target keeps its row,
// no history, no balance change.
func TestEditZombieLeaderWritesNothing(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	target := confirmSingle(t, p, pool, cash, -1500, t1)
	edit := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})
	before := rowJSON(t, pool, target)

	if _, err := pool.Exec(context.Background(), `UPDATE leader_lease SET owner = NULL, lease_until = NULL`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := p.processSingle(context.Background(), edit); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss from a zombie leader, got %v", err)
	}
	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpPending) {
		t.Fatalf("edit status = %s, want PENDING", s.status)
	}
	if after := rowJSON(t, pool, target); after != before {
		t.Fatalf("target changed by a zombie:\n before %s\n after  %s", before, after)
	}
	if revs := readRevisions(t, pool, target); len(revs) != 0 {
		t.Fatalf("history rows = %d, want 0", len(revs))
	}
	if bal, ver := readAccount(t, pool, cash); bal != -1500 || ver != 1 {
		t.Fatalf("balance=%d version=%d, want -1500/1", bal, ver)
	}

	// A proper leader then decides it cleanly.
	leader := leaderProcessor(t, pool)
	must(t, leader.processSingle(context.Background(), edit))
	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpApplied) {
		t.Fatalf("edit status = %s after a real leader, want APPLIED", s.status)
	}
}

// IT-027: snapshot coalescing counts. On an account with 30 later snapshot days,
// a same-day amount edit touches 31 rows (one apply: the day plus 30 cascaded);
// the same edit made cross-day reports the two-apply count (old day + 30, new
// day + fewer); a zero-delta same-day time move reports 0 and leaves every
// snapshot row's xmin unchanged. Every snapshot equals the recomputation from
// CONFIRMED rows throughout.
func TestEditSnapshotCoalescingCounts(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	cash := createAccount(t, pool, editOwner, "cash", nil, nil)
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	target := confirmSingle(t, p, pool, cash, -1500, base)
	for d := 1; d <= 30; d++ {
		confirmSingle(t, p, pool, cash, 10, base.AddDate(0, 0, d))
	}

	observe := func(edit pendingOp) float64 {
		t.Helper()
		c0, s0 := snapshotRowsObserved(t, m)
		must(t, p.processSingle(context.Background(), edit))
		c1, s1 := snapshotRowsObserved(t, m)
		if c1 != c0+1 {
			t.Fatalf("snapshot-rows observations grew by %d, want exactly 1 per edit", c1-c0)
		}
		assertSnapshotsRecomputed(t, pool, cash)
		return s1 - s0
	}

	// Same day, amount change: one apply → 1 + 30 rows.
	sameDay := registerEdit(t, pool, 1, api.InsertOp{EditOf: &target, Amount: -1200})
	if got := observe(sameDay); got != 31 {
		t.Fatalf("same-day amount edit touched %v snapshot rows, want 31 (one apply)", got)
	}

	// Cross-day: move to day 10 with the amount changed → −old at day 0 (31 rows)
	// plus +new at day 10 (1 + 20 later rows) = 52, the two-apply form.
	crossDay := registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -1000, EffectiveAt: base.AddDate(0, 0, 10)})
	if got := observe(crossDay); got != 52 {
		t.Fatalf("cross-day edit touched %v snapshot rows, want 52 (two applies)", got)
	}

	// Zero delta, same day (a time move within day 10): no snapshot write at all.
	xmins := snapshotXmins(t, pool, cash)
	timeMove := registerEdit(t, pool, 3, api.InsertOp{EditOf: &target, EffectiveAt: base.AddDate(0, 0, 10).Add(5 * time.Hour)})
	if got := observe(timeMove); got != 0 {
		t.Fatalf("zero-delta same-day move touched %v snapshot rows, want 0", got)
	}
	if got := snapshotXmins(t, pool, cash); fmt.Sprint(got) != fmt.Sprint(xmins) {
		t.Fatalf("zero-delta move rewrote snapshot rows")
	}
	if s := readOpState(t, pool, target); s.revision != 4 || !s.effective.Equal(base.AddDate(0, 0, 10).Add(5*time.Hour)) {
		t.Fatalf("target = %+v, want revision 4 at the moved instant", s)
	}
}

// IT-042: ADR-0008 schedule with deferral. The target is inserted in an open host
// transaction and an edit of it is attempted from a second transaction: the edit
// row's FK on edit_of cannot see the uncommitted target, so the insert is refused
// (23503) until the target commits — through the insertion path an edit can never
// become visible before its target, and a work query that sees the edit has the
// target ahead of it in id order. The "target still PENDING at decision" window
// therefore only opens if a batch is handed the edit without having decided the
// target first; the schedule forces exactly that: the unit is deferred (errDeferred
// swallowed, debug log, counter +1, row still PENDING, the drain cursor moved past
// it), the batch commits without it, and the next cycle's drain decides target
// then edit in id order.
func TestEditDeferredWhileTargetPending(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))

	// Host transaction H1 registers the target and holds it open.
	h1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin h1: %v", err)
	}
	defer h1.Rollback(ctx)
	res, err := api.Insert(ctx, h1, api.InsertRequest{
		IdempotencyKey: "00000000-0000-4000-8000-000000000101",
		Operations:     []api.InsertOp{{OwnerID: editOwner, ExternalID: "cash", Amount: -1500, EffectiveAt: t1}},
	})
	if err != nil {
		t.Fatalf("insert target in h1: %v", err)
	}
	target := res.Operations[0].ID

	// A second transaction cannot reference the uncommitted target: the FK refuses
	// the edit row outright, so an edit is never visible before its target.
	insertEdit := func() (int64, error) {
		var id int64
		err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO operations (account_id, amount, effective_at, edit_of) VALUES ($1, -1200, $2, $3) RETURNING id`,
				cash, t1, target).Scan(&id)
		})
		return id, err
	}
	if _, err := insertEdit(); err == nil || !strings.Contains(err.Error(), "operations_edit_of_fkey") {
		t.Fatalf("edit of an uncommitted target: err=%v, want the edit_of FK violation", err)
	}
	if err := h1.Commit(ctx); err != nil {
		t.Fatalf("commit h1: %v", err)
	}
	edit, err := insertEdit()
	if err != nil {
		t.Fatalf("edit insert after the target committed: %v", err)
	}
	if edit <= target {
		t.Fatalf("edit id %d should be above target id %d", edit, target)
	}

	// The schedule: a batch that sees the edit while the target is still PENDING.
	w := fetchPendingOp(t, pool, edit)
	deferredUpTo, err := p.processBatch(ctx, []workRow{{
		ID: w.ID, AccountID: w.AccountID, Amount: w.Amount, EffectiveAt: w.EffectiveAt,
		EditOf: w.EditOf, ExpectedRevision: w.ExpectedRevision,
	}})
	if err != nil {
		t.Fatalf("processBatch with a deferred edit returned %v, want nil (batch continues)", err)
	}
	if deferredUpTo != edit {
		t.Fatalf("deferred cursor = %d, want the edit id %d", deferredUpTo, edit)
	}
	if s := readOpState(t, pool, edit); s.status != string(model.OpPending) {
		t.Fatalf("deferred edit status = %s, want PENDING", s.status)
	}
	if s := readOpState(t, pool, target); s.status != string(model.OpPending) || s.revision != 1 {
		t.Fatalf("target touched by a deferral: %+v", s)
	}
	if got := counterValue(t, m, "balancedb_edit_deferrals_total"); got != 1 {
		t.Fatalf("edit_deferrals_total = %v, want 1", got)
	}
	if got := decisionCount(t, m, obs.KindEdit, obs.OutcomeApplied) + decisionCount(t, m, obs.KindEdit, obs.OutcomeInvalid); got != 0 {
		t.Fatalf("a deferral counted as a decision: %v", got)
	}

	// Next cycle: a full drain decides the target first (id order), then the edit.
	drainAll(t, p)
	if s := readOpState(t, pool, target); s.status != string(model.OpConfirmed) || s.amount != -1200 || s.revision != 2 {
		t.Fatalf("target after the next cycle = %+v, want CONFIRMED/-1200/revision 2", s)
	}
	if s := readOpState(t, pool, edit); s.status != string(model.OpApplied) {
		t.Fatalf("edit after the next cycle = %s, want APPLIED", s.status)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1200 {
		t.Fatalf("balance = %d, want -1200", bal)
	}
	if got := counterValue(t, m, "balancedb_edit_deferrals_total"); got != 1 {
		t.Fatalf("edit_deferrals_total = %v after the clean cycle, want still 1", got)
	}
}

// The drain cursor: with batch_size 1 and a deferred edit at the head of the
// queue, the work behind it is still decided in the same drain (no starvation).
func TestDrainCursorSkipsDeferredHead(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))

	// Build the queue by hand: a PENDING target, a PENDING edit of it, and a regular
	// operation behind them. Starting the cursor past the target makes the first
	// batch the edit alone, so its target read finds PENDING and the unit defers.
	target := insertPendingSingle(t, pool, cash, -1500, t1)
	var edit int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, edit_of) VALUES ($1, -1200, $2, $3) RETURNING id`,
		cash, t1, target.ID).Scan(&edit); err != nil {
		t.Fatalf("insert edit: %v", err)
	}
	later := insertPendingSingle(t, pool, cash, 700, t2)

	// Batch size 1 starting past the target: the first batch is the edit alone
	// (deferred), the cursor moves, the second batch decides `later`.
	var afterID int64 = target.ID
	for i := 0; i < 3; i++ {
		work, err := p.fetchPending(ctx, 1, afterID)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(work) == 0 {
			break
		}
		deferredUpTo, err := p.processBatch(ctx, work)
		if err != nil {
			t.Fatalf("processBatch: %v", err)
		}
		if deferredUpTo > afterID {
			afterID = deferredUpTo
		}
	}
	if s := readOpState(t, pool, edit); s.status != string(model.OpPending) {
		t.Fatalf("edit status = %s, want PENDING (deferred)", s.status)
	}
	if s := readOpState(t, pool, later.ID); s.status != string(model.OpConfirmed) {
		t.Fatalf("operation behind a deferred edit = %s, want CONFIRMED in the same drain", s.status)
	}
	// A real drain from the start then settles everything in id order.
	drainAll(t, p)
	if s := readOpState(t, pool, target.ID); s.status != string(model.OpConfirmed) || s.amount != -1200 {
		t.Fatalf("target = %+v, want CONFIRMED at -1200", s)
	}
	if s := readOpState(t, pool, edit); s.status != string(model.OpApplied) {
		t.Fatalf("edit = %s, want APPLIED", s.status)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -500 {
		t.Fatalf("balance = %d, want -500", bal)
	}
}

// Safety Invariant 3 (static half): no SQL in this package updates or deletes
// operation_revisions — the table is append-only by construction.
func TestNoRevisionMutationSQL(t *testing.T) {
	for name, sql := range map[string]string{
		"selectEditTarget": selectEditTarget, "guardFlipApplied": guardFlipApplied, "guardFlipEditInvalid": guardFlipEditInvalid,
		"insertRevision": insertRevision, "guardRevisionCAS": guardRevisionCAS,
		"guardFlipConfirmed": guardFlipConfirmed, "guardFlipInvalid": guardFlipInvalid,
		"guardAccountCAS": guardAccountCAS, "guardFlipLegsConfirmed": guardFlipLegsConfirmed,
		"guardFlipLegsInvalid": guardFlipLegsInvalid, "guardFlipTxCommitted": guardFlipTxCommitted,
		"guardFlipTxRejected": guardFlipTxRejected, "guardFlipEditLegsApplied": guardFlipEditLegsApplied,
		"guardFlipEditLegsInvalid": guardFlipEditLegsInvalid, "selectGroupLegs": selectGroupLegs,
		"guardDeleteCAS": guardDeleteCAS, "fetchPendingWork": fetchPendingWork,
	} {
		up := strings.ToUpper(sql)
		if strings.Contains(up, "OPERATION_REVISIONS") && !strings.HasPrefix(strings.TrimSpace(up), "INSERT") {
			t.Fatalf("%s touches operation_revisions with a non-INSERT statement:\n%s", name, sql)
		}
	}
}
