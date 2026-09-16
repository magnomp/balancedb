//go:build itest

package processor

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
)

// Integration tests for the group edit path (ADR-0010, spec §8.3 with edit legs):
// IT-020–IT-024 of the operation-editing test contract plus the whole-group
// deferral. Groups are registered through the shared ledger core (api.Insert) so
// the rows carry exactly what production writes, then decided through the
// processor's own entry points.

// registerGroup registers one group (owner editOwner) through the shared ledger
// core and returns its transaction id and leg ids in registration order.
func registerGroup(t *testing.T, pool *pgxpool.Pool, key int, items ...api.InsertOp) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	for i := range items {
		items[i].OwnerID = editOwner
	}
	var (
		txID int64
		legs []int64
	)
	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		res, err := api.Insert(ctx, tx, api.InsertRequest{
			IdempotencyKey: fmt.Sprintf("00000000-0000-4000-8000-%012d", key),
			Operations:     items,
		})
		if err != nil {
			return err
		}
		txID = *res.TransactionID
		for _, o := range res.Operations {
			legs = append(legs, o.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	return txID, legs
}

// IT-020: a group of two edits → both edit legs APPLIED, the transaction
// COMMITTED, one history row per target, and each involved account's net applied
// once (its version bumps by exactly one — Guard 3 once per account, not per
// leg). NOTIFY tx:<id> is delivered on commit.
func TestGroupOfTwoEditsApplies(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	a := confirmSingle(t, p, pool, cash, -1500, t1)
	b := confirmSingle(t, p, pool, cash, 800, t2)
	bal0, ver0 := readAccount(t, pool, cash)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN outcomes`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	txID, legs := registerGroup(t, pool, 1,
		api.InsertOp{EditOf: &a, Amount: -1200},
		api.InsertOp{EditOf: &b, Amount: 900, EffectiveAt: t3},
	)
	must(t, p.processGroup(ctx, txID))

	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) || r.decidedAt == nil {
		t.Fatalf("tx = %+v, want COMMITTED", r)
	}
	for _, id := range legs {
		if s := readOpState(t, pool, id); s.status != string(model.OpApplied) || s.confirmed == nil {
			t.Fatalf("edit leg %d = %+v, want APPLIED with a decision instant", id, s)
		}
	}
	sa, sb := readOpState(t, pool, a), readOpState(t, pool, b)
	if sa.amount != -1200 || sa.revision != 2 || sa.revisedAt == nil || sa.status != string(model.OpConfirmed) {
		t.Fatalf("target a = %+v, want -1200 at revision 2", sa)
	}
	if sb.amount != 900 || !sb.effective.Equal(t3) || sb.revision != 2 || sb.status != string(model.OpConfirmed) {
		t.Fatalf("target b = %+v, want 900 at %v revision 2", sb, t3)
	}
	ra, rb := readRevisions(t, pool, a), readRevisions(t, pool, b)
	if len(ra) != 1 || ra[0].revision != 1 || ra[0].amount != -1500 || ra[0].supersededBy != legs[0] || !ra[0].recordedAt.Equal(sa.registered) {
		t.Fatalf("history of a = %+v", ra)
	}
	if len(rb) != 1 || rb[0].revision != 1 || rb[0].amount != 800 || !rb[0].effective.Equal(t2) || rb[0].supersededBy != legs[1] {
		t.Fatalf("history of b = %+v", rb)
	}
	// Net on cash: +1500 −1200 −800 +900 = +400, applied once.
	bal, ver := readAccount(t, pool, cash)
	if bal != bal0+400 || ver != ver0+1 {
		t.Fatalf("cash balance/version = %d/%d, want %d/%d (one CAS for the whole group)", bal, ver, bal0+400, ver0+1)
	}
	assertSnapshotsRecomputed(t, pool, cash)

	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("wait for outcome: %v", err)
	}
	for n.Payload != fmt.Sprintf("tx:%d", txID) {
		if n, err = conn.Conn().WaitForNotification(ctx); err != nil {
			t.Fatalf("wait for outcome: %v", err)
		}
	}
	if got := decisionCount(t, m, obs.KindGroup, obs.OutcomeCommitted); got != 1 {
		t.Fatalf("decisions{group,committed} = %v, want 1", got)
	}
}

// IT-021: a mixed group (edit + new operation on the same account) is validated
// on its net: cash at min 20 with balance 100; the edit alone (+100 → +5, net −95)
// would breach, the new +30 leg lifts the net to −65 → 35, so the group commits —
// the new operation CONFIRMED, the edit APPLIED, the target at revision 2.
func TestMixedGroupNetsEditAndNewOp(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(20), nil)
	target := confirmSingle(t, p, pool, cash, 100, t1)

	txID, legs := registerGroup(t, pool, 1,
		api.InsertOp{EditOf: &target, Amount: 5},
		api.InsertOp{ExternalID: "cash", Amount: 30, EffectiveAt: t2},
	)
	must(t, p.processGroup(ctx, txID))

	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) {
		t.Fatalf("tx = %+v, want COMMITTED", r)
	}
	if s := readOpState(t, pool, legs[0]); s.status != string(model.OpApplied) {
		t.Fatalf("edit leg = %s, want APPLIED", s.status)
	}
	if s := readOpState(t, pool, legs[1]); s.status != string(model.OpConfirmed) || s.revision != 1 {
		t.Fatalf("new leg = %+v, want CONFIRMED at revision 1", s)
	}
	if s := readOpState(t, pool, target); s.amount != 5 || s.revision != 2 {
		t.Fatalf("target = %+v, want +5 at revision 2", s)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 35 {
		t.Fatalf("balance = %d, want 35", bal)
	}
	assertSnapshotsRecomputed(t, pool, cash)

	// A lone edit with nothing to net against is decided on its own: +5 → −50
	// (net −55) would leave −20 < 20, so it is INVALID and the target stays.
	edit := registerEdit(t, pool, 2, api.InsertOp{EditOf: &target, Amount: -50})
	must(t, p.processSingle(ctx, edit))
	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpInvalid) {
		t.Fatalf("lone edit = %s, want INVALID", s.status)
	}
	if s := readOpState(t, pool, target); s.amount != 5 || s.revision != 2 {
		t.Fatalf("target after the rejected edit = %+v, want unchanged +5 at revision 2", s)
	}
}

// IT-022: a group with one violating item is REJECTED as a whole — every leg
// INVALID with the same rejection (regular and edit legs alike), every target
// unchanged column by column and by tuple identity, no history row, no balance
// or snapshot write. Three shapes of violation: a limit breach, a target that
// ended INVALID (TARGET_NOT_EDITABLE) and a stale expected_revision.
func TestGroupOneItemViolatesRejectsWhole(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(0), nil)
	wallet := createAccount(t, pool, editOwner, "wallet", nil, nil)
	a := confirmSingle(t, p, pool, cash, 100, t1)
	b := confirmSingle(t, p, pool, wallet, 50, t1)
	bad := insertPendingSingle(t, pool, cash, -500, t1) // rejected: 100 − 500 < 0
	must(t, p.processSingle(ctx, bad))
	if s := readOpState(t, pool, bad.ID); s.status != string(model.OpInvalid) {
		t.Fatalf("setup: op %d = %s, want INVALID", bad.ID, s.status)
	}
	key := 10

	check := func(name string, wantCode model.ReasonCode, items ...api.InsertOp) *model.Rejection {
		t.Helper()
		before := map[int64]string{a: rowJSON(t, pool, a), b: rowJSON(t, pool, b), bad.ID: rowJSON(t, pool, bad.ID)}
		xmins := snapshotXmins(t, pool, cash)
		balCash, verCash := readAccount(t, pool, cash)
		balWallet, verWallet := readAccount(t, pool, wallet)

		key++
		txID, legs := registerGroup(t, pool, key, items...)
		must(t, p.processGroup(ctx, txID))

		r := readTx(t, pool, txID)
		if r.status != string(model.TxRejected) || r.reason == nil {
			t.Fatalf("%s: tx = %+v, want REJECTED with a reason", name, r)
		}
		rej, err := model.ParseRejection(*r.reason)
		if err != nil || rej.Code != wantCode {
			t.Fatalf("%s: tx reason = %s (%v), want %s", name, *r.reason, err, wantCode)
		}
		for _, id := range legs {
			s := readOpState(t, pool, id)
			if s.status != string(model.OpInvalid) || s.reason == nil || *s.reason != *r.reason {
				t.Fatalf("%s: leg %d = %+v, want INVALID with the group's reason %s", name, id, s, *r.reason)
			}
			if s.editOf != nil && s.confirmed == nil {
				t.Fatalf("%s: rejected edit leg %d has no decision instant", name, id)
			}
		}
		for id, row := range before {
			if got := rowJSON(t, pool, id); got != row {
				t.Fatalf("%s: target %d changed:\n before %s\n after  %s", name, id, row, got)
			}
			if revs := readRevisions(t, pool, id); len(revs) != 0 {
				t.Fatalf("%s: target %d gained history rows %+v", name, id, revs)
			}
		}
		if bc, vc := readAccount(t, pool, cash); bc != balCash || vc != verCash {
			t.Fatalf("%s: cash moved %d/%d → %d/%d", name, balCash, verCash, bc, vc)
		}
		if bw, vw := readAccount(t, pool, wallet); bw != balWallet || vw != verWallet {
			t.Fatalf("%s: wallet moved %d/%d → %d/%d", name, balWallet, verWallet, bw, vw)
		}
		if got := snapshotXmins(t, pool, cash); len(got) != len(xmins) {
			t.Fatalf("%s: snapshot rows changed", name)
		} else {
			for day, x := range xmins {
				if got[day] != x {
					t.Fatalf("%s: snapshot %s rewritten", name, day)
				}
			}
		}
		return &rej
	}

	// Limit breach: the edit of a (+100 → +10, net −90) plus a new −20 → −10 < 0.
	rej := check("limit", model.ReasonLimitViolated,
		api.InsertOp{EditOf: &a, Amount: 10},
		api.InsertOp{ExternalID: "cash", Amount: -20, EffectiveAt: t2},
		api.InsertOp{EditOf: &b, Amount: 60},
	)
	if rej.Account != "cash" || rej.LimitSide != model.LimitMin || rej.Shortfall != 10 {
		t.Fatalf("limit rejection = %+v, want cash/min/10", *rej)
	}

	// Target not editable: a valid edit of b grouped with an edit of the INVALID op.
	rej = check("not-editable", model.ReasonTargetNotEditable,
		api.InsertOp{EditOf: &b, Amount: 60},
		api.InsertOp{EditOf: &bad.ID, Amount: -1},
	)
	if rej.OperationID == nil || *rej.OperationID != bad.ID || rej.ExpectedRevision != nil {
		t.Fatalf("not-editable rejection = %+v, want TARGET_NOT_EDITABLE{%d}", *rej, bad.ID)
	}

	// Stale revision: a is at revision 1; expected 2 rejects the whole group.
	rej = check("stale", model.ReasonStaleRevision,
		api.InsertOp{ExternalID: "wallet", Amount: 5, EffectiveAt: t2},
		api.InsertOp{EditOf: &a, Amount: 10, ExpectedRevision: ptr32(2)},
	)
	if rej.OperationID == nil || *rej.OperationID != a || rej.ExpectedRevision == nil || *rej.ExpectedRevision != 2 || rej.ActualRevision == nil || *rej.ActualRevision != 1 {
		t.Fatalf("stale rejection = %+v, want STALE_REVISION{%d, 2, 1}", *rej, a)
	}
}

// IT-023: editing one leg of a COMMITTED group changes only that leg — the group
// stays COMMITTED with its op_count, the sibling leg and the transaction row are
// untouched, and the edited leg keeps its transaction_id.
func TestEditOneLegOfCommittedGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	createAccount(t, pool, editOwner, "wallet", ptr(-10_000), ptr(10_000))

	txID, legs := registerGroup(t, pool, 1,
		api.InsertOp{ExternalID: "cash", Amount: -700, EffectiveAt: t1},
		api.InsertOp{ExternalID: "wallet", Amount: 700, EffectiveAt: t1},
	)
	must(t, p.processGroup(ctx, txID))
	txBefore := readTx(t, pool, txID)
	sibling := rowJSON(t, pool, legs[1])
	var opCount int
	if err := pool.QueryRow(ctx, `SELECT op_count FROM transactions WHERE id = $1`, txID).Scan(&opCount); err != nil {
		t.Fatalf("op_count: %v", err)
	}

	edit := registerEdit(t, pool, 2, api.InsertOp{EditOf: &legs[0], Amount: -650})
	must(t, p.processSingle(ctx, edit))

	if s := readOpState(t, pool, edit.ID); s.status != string(model.OpApplied) {
		t.Fatalf("edit = %s, want APPLIED", s.status)
	}
	leg := readOpState(t, pool, legs[0])
	if leg.amount != -650 || leg.revision != 2 || leg.status != string(model.OpConfirmed) {
		t.Fatalf("edited leg = %+v, want -650 at revision 2, still CONFIRMED", leg)
	}
	var legTx *int64
	if err := pool.QueryRow(ctx, `SELECT transaction_id FROM operations WHERE id = $1`, legs[0]).Scan(&legTx); err != nil || legTx == nil || *legTx != txID {
		t.Fatalf("edited leg transaction_id = %v (%v), want %d", legTx, err, txID)
	}
	if got := rowJSON(t, pool, legs[1]); got != sibling {
		t.Fatalf("sibling leg changed:\n before %s\n after  %s", sibling, got)
	}
	txAfter := readTx(t, pool, txID)
	var opCountAfter int
	if err := pool.QueryRow(ctx, `SELECT op_count FROM transactions WHERE id = $1`, txID).Scan(&opCountAfter); err != nil {
		t.Fatalf("op_count: %v", err)
	}
	if txAfter.status != string(model.TxCommitted) || opCountAfter != opCount || !txAfter.decidedAt.Equal(*txBefore.decidedAt) {
		t.Fatalf("group after the edit = %+v op_count %d, want unchanged COMMITTED/%d", txAfter, opCountAfter, opCount)
	}
}

// IT-024: an edit of a leg of a REJECTED group is INVALID with
// TARGET_NOT_EDITABLE{leg id}; the leg and its group are untouched.
func TestEditLegOfRejectedGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	createAccount(t, pool, editOwner, "cash", ptr(0), nil)
	createAccount(t, pool, editOwner, "wallet", nil, nil)

	txID, legs := registerGroup(t, pool, 1,
		api.InsertOp{ExternalID: "cash", Amount: -700, EffectiveAt: t1}, // cash 0 − 700 < 0
		api.InsertOp{ExternalID: "wallet", Amount: 700, EffectiveAt: t1},
	)
	must(t, p.processGroup(ctx, txID))
	if r := readTx(t, pool, txID); r.status != string(model.TxRejected) {
		t.Fatalf("setup: tx = %+v, want REJECTED", r)
	}
	legBefore := rowJSON(t, pool, legs[1])

	edit := registerEdit(t, pool, 2, api.InsertOp{EditOf: &legs[1], Amount: 5})
	must(t, p.processSingle(ctx, edit))

	s := readOpState(t, pool, edit.ID)
	if s.status != string(model.OpInvalid) || s.reason == nil {
		t.Fatalf("edit = %+v, want INVALID with a reason", s)
	}
	rej, err := model.ParseRejection(*s.reason)
	if err != nil || rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != legs[1] {
		t.Fatalf("edit rejection = %+v (%v), want TARGET_NOT_EDITABLE{%d}", rej, err, legs[1])
	}
	if got := rowJSON(t, pool, legs[1]); got != legBefore {
		t.Fatalf("rejected leg changed:\n before %s\n after  %s", legBefore, got)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxRejected) {
		t.Fatalf("group = %+v, want still REJECTED", r)
	}
}

// A group with an edit leg whose target is still PENDING is deferred as a whole
// (spec §8.2/§8.3 for edits): every leg stays PENDING, the transaction stays
// PENDING, the deferral is counted once and the drain cursor advances past every
// leg of the group; the next cycle decides target and group in id order. Like
// the single-edit deferral it is unreachable through real inserts (the edit_of
// FK refuses an uncommitted target), so the schedule hands processBatch the
// group's legs alone.
func TestGroupDeferredWhileTargetPending(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))

	target := insertPendingSingle(t, pool, cash, -1500, t1)
	var txID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO transactions (idempotency_key, payload_hash, op_count) VALUES (gen_random_uuid(), 'x', 2) RETURNING id`).Scan(&txID); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}
	var editLeg, newLeg int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, transaction_id, edit_of) VALUES ($1, -1200, $2, $3, $4) RETURNING id`,
		cash, t1, txID, target.ID).Scan(&editLeg); err != nil {
		t.Fatalf("insert edit leg: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO operations (account_id, amount, effective_at, transaction_id) VALUES ($1, 100, $2, $3) RETURNING id`,
		cash, t2, txID).Scan(&newLeg); err != nil {
		t.Fatalf("insert new leg: %v", err)
	}

	// The schedule: a batch that sees the group's legs while the target is PENDING.
	work, err := p.fetchPending(ctx, 10, target.ID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(work) != 2 {
		t.Fatalf("fetched %d rows past the target, want the 2 legs", len(work))
	}
	deferredUpTo, err := p.processBatch(ctx, work)
	if err != nil {
		t.Fatalf("processBatch with a deferred group returned %v, want nil", err)
	}
	if deferredUpTo != newLeg {
		t.Fatalf("deferred cursor = %d, want the group's last leg %d", deferredUpTo, newLeg)
	}
	for _, id := range []int64{target.ID, editLeg, newLeg} {
		if s := readOpState(t, pool, id); s.status != string(model.OpPending) {
			t.Fatalf("op %d = %s after the deferral, want PENDING", id, s.status)
		}
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxPending) {
		t.Fatalf("tx = %+v after the deferral, want PENDING", r)
	}
	if got := counterValue(t, m, "balancedb_edit_deferrals_total"); got != 1 {
		t.Fatalf("edit_deferrals_total = %v, want 1 (once per deferred group)", got)
	}

	// Next cycle: target first (id order), then the group.
	drainAll(t, p)
	if s := readOpState(t, pool, target.ID); s.status != string(model.OpConfirmed) || s.amount != -1200 || s.revision != 2 {
		t.Fatalf("target = %+v, want CONFIRMED/-1200/revision 2", s)
	}
	if s := readOpState(t, pool, editLeg); s.status != string(model.OpApplied) {
		t.Fatalf("edit leg = %s, want APPLIED", s.status)
	}
	if s := readOpState(t, pool, newLeg); s.status != string(model.OpConfirmed) {
		t.Fatalf("new leg = %s, want CONFIRMED", s.status)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) {
		t.Fatalf("tx = %+v, want COMMITTED", r)
	}
	if bal, _ := readAccount(t, pool, cash); bal != -1100 {
		t.Fatalf("balance = %d, want -1100", bal)
	}
}
