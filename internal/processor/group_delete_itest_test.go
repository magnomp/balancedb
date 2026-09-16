//go:build itest

package processor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
)

// Integration tests for the group path with delete legs (ADR-0011, spec §8.3):
// IT-020–IT-024 and IT-046 of the operation-deletion test contract, plus the
// grouped delete-leg half of IT-042. Groups are registered through the shared
// ledger core (api.Insert) so the rows carry exactly what production writes,
// then decided through the processor's own entry points.

// mixedFixture is the _dx.md mixed group's world: cash holds a (−1500 at t1)
// and b (−800 at t2); wallet holds d, confirmed +250 at t1 and since edited to
// +200 (revision 2). Balances: cash −2300, wallet +200.
type mixedFixture struct {
	cash, wallet int64
	a, b, d      int64
}

// seedMixed confirms the fixture. walletMin is wallet's min limit (nil =
// unbounded) so a test can make the delete of d violate.
func seedMixed(t *testing.T, p *Processor, pool *pgxpool.Pool, walletMin *int64) mixedFixture {
	t.Helper()
	ctx := context.Background()
	f := mixedFixture{
		cash:   createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000)),
		wallet: createAccount(t, pool, editOwner, "wallet", walletMin, ptr(10_000)),
	}
	f.a = confirmSingle(t, p, pool, f.cash, -1500, t1)
	f.b = confirmSingle(t, p, pool, f.cash, -800, t2)
	f.d = confirmSingle(t, p, pool, f.wallet, 250, t1)
	edit := registerEdit(t, pool, 900, api.InsertOp{EditOf: &f.d, Amount: 200})
	must(t, p.processSingle(ctx, edit))
	if s := readOpState(t, pool, f.d); s.status != string(model.OpConfirmed) || s.amount != 200 || s.revision != 2 {
		t.Fatalf("setup: d = %+v, want +200 at revision 2", s)
	}
	if bal, _ := readAccount(t, pool, f.cash); bal != -2300 {
		t.Fatalf("setup: cash = %d, want -2300", bal)
	}
	if bal, _ := readAccount(t, pool, f.wallet); bal != 200 {
		t.Fatalf("setup: wallet = %d, want 200", bal)
	}
	return f
}

// mixedItems is the _dx.md group: [delete a, delete d expected 2, edit b −700,
// new cash +300]. Virtual legs: +1500 (cash), −200 (wallet), +800 −700 (cash),
// +300 (cash) → cash +1900, wallet −200.
func (f mixedFixture) mixedItems() []api.InsertOp {
	return []api.InsertOp{
		{DeleteOf: &f.a},
		{DeleteOf: &f.d, ExpectedRevision: ptr32(2)},
		{EditOf: &f.b, Amount: -700},
		{ExternalID: "cash", Amount: 300, EffectiveAt: t3},
	}
}

// IT-020: the mixed group commits as one unit — both delete legs and the edit
// leg APPLIED with a decision instant, the new leg CONFIRMED, the transaction
// COMMITTED; a and d DELETED (deleted_by = their leg, deleted_at = the leg's
// decision instant, last values and revision frozen, nothing appended); b at
// revision 2 with one history row; each account's balance moved by its net
// under exactly one Guard 3 CAS (version +1 each); one snapshot apply per
// virtual unit; NOTIFY tx:<id>; one group decision, no single-delete decision.
func TestMixedGroupWithDeletesCommits(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	f := seedMixed(t, p, pool, nil)
	aBefore, dBefore := readOpState(t, pool, f.a), readOpState(t, pool, f.d)
	_, cashVer := readAccount(t, pool, f.cash)
	_, walletVer := readAccount(t, pool, f.wallet)
	revsBefore := countRows(t, pool, `SELECT count(*) FROM operation_revisions`)
	opsBefore := countRows(t, pool, `SELECT count(*) FROM operations`)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN outcomes`); err != nil {
		t.Fatalf("listen: %v", err)
	}

	txID, legs := registerGroup(t, pool, 1, f.mixedItems()...)
	c0, _ := snapshotRowsObserved(t, m)
	must(t, p.processGroup(ctx, txID))

	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) || r.decidedAt == nil {
		t.Fatalf("tx = %+v, want COMMITTED", r)
	}
	var decided []opState
	for i, id := range legs[:3] {
		s := readOpState(t, pool, id)
		if s.status != string(model.OpApplied) || s.confirmed == nil || s.deletedBy != nil || s.deletedAt != nil {
			t.Fatalf("edit-class leg %d (#%d) = %+v, want APPLIED with a decision instant and no stamps of its own", id, i, s)
		}
		decided = append(decided, s)
	}
	if s := readOpState(t, pool, legs[3]); s.status != string(model.OpConfirmed) || s.revision != 1 || s.amount != 300 {
		t.Fatalf("new leg = %+v, want CONFIRMED +300 at revision 1", s)
	}
	if !decided[0].isDelete || !decided[1].isDelete || decided[2].isDelete {
		t.Fatalf("leg kinds = %v/%v/%v, want delete/delete/edit", decided[0].isDelete, decided[1].isDelete, decided[2].isDelete)
	}

	// The delete targets: DELETED by their leg, frozen otherwise.
	for i, tc := range []struct {
		id     int64
		before opState
		leg    int64
		legRow opState
	}{{f.a, aBefore, legs[0], decided[0]}, {f.d, dBefore, legs[1], decided[1]}} {
		after := readOpState(t, pool, tc.id)
		if after.status != string(model.OpDeleted) || after.deletedBy == nil || *after.deletedBy != tc.leg ||
			after.deletedAt == nil || !after.deletedAt.Equal(*tc.legRow.confirmed) {
			t.Fatalf("target #%d (%d) = %+v, want DELETED by leg %d at its decision instant %v", i, tc.id, after, tc.leg, tc.legRow.confirmed)
		}
		if after.amount != tc.before.amount || after.accountID != tc.before.accountID || !after.effective.Equal(tc.before.effective) ||
			after.revision != tc.before.revision || !after.registered.Equal(tc.before.registered) {
			t.Fatalf("target #%d last values changed: before=%+v after=%+v", i, tc.before, after)
		}
	}
	if s := readOpState(t, pool, f.d); s.revision != 2 || s.amount != 200 {
		t.Fatalf("d = %+v, want the revision-2 values frozen", s)
	}
	// The edit target: −700 at revision 2 with its one history row.
	if s := readOpState(t, pool, f.b); s.status != string(model.OpConfirmed) || s.amount != -700 || s.revision != 2 || s.deletedBy != nil {
		t.Fatalf("b = %+v, want CONFIRMED -700 at revision 2", s)
	}
	if rb := readRevisions(t, pool, f.b); len(rb) != 1 || rb[0].amount != -800 || rb[0].supersededBy != legs[2] {
		t.Fatalf("history of b = %+v, want one row {-800, superseded by %d}", rb, legs[2])
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operation_revisions`); n != revsBefore+1 {
		t.Fatalf("operation_revisions = %d, want %d (the edit's row only; deletes append nothing)", n, revsBefore+1)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations`); n != opsBefore+4 {
		t.Fatalf("operations = %d, want %d (nothing physically deleted)", n, opsBefore+4)
	}

	// Nets applied once per account: cash −2300 + 1900 = −400; wallet 200 − 200 = 0.
	if bal, ver := readAccount(t, pool, f.cash); bal != -400 || ver != cashVer+1 {
		t.Fatalf("cash = %d/%d, want -400/%d (one CAS)", bal, ver, cashVer+1)
	}
	if bal, ver := readAccount(t, pool, f.wallet); bal != 0 || ver != walletVer+1 {
		t.Fatalf("wallet = %d/%d, want 0/%d (one CAS)", bal, ver, walletVer+1)
	}
	// Snapshots: one apply observed per delete leg, one per coalesced edit pair,
	// one per regular leg — four for this group — and every day recomputes.
	if c1, _ := snapshotRowsObserved(t, m); c1 != c0+4 {
		t.Fatalf("snapshot observations grew by %d, want 4 (delete, delete, edit pair, new)", c1-c0)
	}
	assertSnapshotsRecomputed(t, pool, f.cash)
	assertSnapshotsRecomputed(t, pool, f.wallet)

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
	if got := decisionCount(t, m, obs.KindDelete, obs.OutcomeApplied); got != 0 {
		t.Fatalf("decisions{delete,applied} = %v, want 0 (a group is one group decision)", got)
	}
}

// IT-021: the same group where the net violates one account (wallet min 100:
// 200 − 200 = 0) is REJECTED as a whole — every leg INVALID with the one shared
// LIMIT_VIOLATED{wallet, min, 100}, edit-class legs stamped with a decision
// instant, the regular leg not; a, d and b unchanged column by column and by
// tuple identity (no deletion stamps, no revision, no history row); no balance,
// version or snapshot write.
func TestMixedGroupWithDeletesRejectsWhole(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	m := obs.NewMetrics(nil)
	p.SetMetrics(m)
	ctx := context.Background()
	f := seedMixed(t, p, pool, ptr(100))
	before := map[int64]string{f.a: rowJSON(t, pool, f.a), f.b: rowJSON(t, pool, f.b), f.d: rowJSON(t, pool, f.d)}
	cashBal, cashVer := readAccount(t, pool, f.cash)
	walletBal, walletVer := readAccount(t, pool, f.wallet)
	xmins := snapshotXmins(t, pool, f.cash)
	revsBefore := countRows(t, pool, `SELECT count(*) FROM operation_revisions`)

	txID, legs := registerGroup(t, pool, 1, f.mixedItems()...)
	must(t, p.processGroup(ctx, txID))

	r := readTx(t, pool, txID)
	if r.status != string(model.TxRejected) || r.reason == nil {
		t.Fatalf("tx = %+v, want REJECTED with a reason", r)
	}
	rej, err := model.ParseRejection(*r.reason)
	if err != nil || rej.Code != model.ReasonLimitViolated || rej.Account != "wallet" || rej.LimitSide != model.LimitMin || rej.Shortfall != 100 {
		t.Fatalf("tx reason = %+v (%v), want LIMIT_VIOLATED{wallet, min, 100}", rej, err)
	}
	for i, id := range legs {
		s := readOpState(t, pool, id)
		if s.status != string(model.OpInvalid) || s.reason == nil || *s.reason != *r.reason {
			t.Fatalf("leg %d = %+v, want INVALID with the group's reason %s", id, s, *r.reason)
		}
		if i < 3 && s.confirmed == nil {
			t.Fatalf("rejected edit-class leg %d has no decision instant", id)
		}
		if i == 3 && s.confirmed != nil {
			t.Fatalf("rejected regular leg %d carries a decision instant", id)
		}
	}
	for id, row := range before {
		if got := rowJSON(t, pool, id); got != row {
			t.Fatalf("target %d changed:\n before %s\n after  %s", id, row, got)
		}
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operation_revisions`); n != revsBefore {
		t.Fatalf("operation_revisions = %d, want %d unchanged", n, revsBefore)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations WHERE status = 'DELETED'`); n != 0 {
		t.Fatalf("%d DELETED rows after a rejected group, want 0", n)
	}
	if bal, ver := readAccount(t, pool, f.cash); bal != cashBal || ver != cashVer {
		t.Fatalf("cash moved %d/%d → %d/%d", cashBal, cashVer, bal, ver)
	}
	if bal, ver := readAccount(t, pool, f.wallet); bal != walletBal || ver != walletVer {
		t.Fatalf("wallet moved %d/%d → %d/%d", walletBal, walletVer, bal, ver)
	}
	got := snapshotXmins(t, pool, f.cash)
	if len(got) != len(xmins) {
		t.Fatalf("snapshot rows changed")
	}
	for day, x := range xmins {
		if got[day] != x {
			t.Fatalf("snapshot %s rewritten", day)
		}
	}
	if got := decisionCount(t, m, obs.KindGroup, obs.OutcomeRejected); got != 1 {
		t.Fatalf("decisions{group,rejected} = %v, want 1", got)
	}
}

// IT-022: a group of two deletes of both legs of COMMITTED group 7 → both legs
// DELETED (each by its own delete leg, keeping transaction_id 7), group 7 still
// COMMITTED with its op_count and decision instant, the delete group COMMITTED
// with both legs APPLIED; balances back where they were before group 7.
func TestGroupOfDeletesOverCommittedGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	wallet := createAccount(t, pool, editOwner, "wallet", ptr(-10_000), ptr(10_000))

	seven, legs7 := registerGroup(t, pool, 1,
		api.InsertOp{ExternalID: "cash", Amount: -700, EffectiveAt: t1},
		api.InsertOp{ExternalID: "wallet", Amount: 700, EffectiveAt: t1},
	)
	must(t, p.processGroup(ctx, seven))
	txBefore := readTx(t, pool, seven)
	if txBefore.status != string(model.TxCommitted) {
		t.Fatalf("setup: group 7 = %+v, want COMMITTED", txBefore)
	}

	nine, legs9 := registerGroup(t, pool, 2,
		api.InsertOp{DeleteOf: &legs7[0]},
		api.InsertOp{DeleteOf: &legs7[1]},
	)
	must(t, p.processGroup(ctx, nine))

	if r := readTx(t, pool, nine); r.status != string(model.TxCommitted) {
		t.Fatalf("delete group = %+v, want COMMITTED", r)
	}
	for i, legID := range legs7 {
		s := readOpState(t, pool, legID)
		if s.status != string(model.OpDeleted) || s.deletedBy == nil || *s.deletedBy != legs9[i] || s.deletedAt == nil || s.revision != 1 {
			t.Fatalf("leg %d of group 7 = %+v, want DELETED by %d at revision 1", legID, s, legs9[i])
		}
		var legTx *int64
		if err := pool.QueryRow(ctx, `SELECT transaction_id FROM operations WHERE id = $1`, legID).Scan(&legTx); err != nil || legTx == nil || *legTx != seven {
			t.Fatalf("deleted leg %d transaction_id = %v (%v), want %d", legID, legTx, err, seven)
		}
		if d := readOpState(t, pool, legs9[i]); d.status != string(model.OpApplied) || !d.isDelete {
			t.Fatalf("delete leg %d = %+v, want APPLIED delete", legs9[i], d)
		}
	}
	txAfter := readTx(t, pool, seven)
	var opCount int
	if err := pool.QueryRow(ctx, `SELECT op_count FROM transactions WHERE id = $1`, seven).Scan(&opCount); err != nil {
		t.Fatalf("op_count: %v", err)
	}
	if txAfter.status != string(model.TxCommitted) || opCount != 2 || !txAfter.decidedAt.Equal(*txBefore.decidedAt) {
		t.Fatalf("group 7 after the deletes = %+v op_count %d, want unchanged COMMITTED/2", txAfter, opCount)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("cash = %d, want 0", bal)
	}
	if bal, _ := readAccount(t, pool, wallet); bal != 0 {
		t.Fatalf("wallet = %d, want 0", bal)
	}
	assertSnapshotsRecomputed(t, pool, cash)
	assertSnapshotsRecomputed(t, pool, wallet)
}

// IT-023: deleting one leg of COMMITTED group 7 alone changes only that leg —
// it reads DELETED, its sibling stays CONFIRMED at revision 1 untouched by tuple
// identity, and the group stays COMMITTED with its op_count and decision instant
// (group membership is not a deletion unit).
func TestDeleteOneLegOfCommittedGroup(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	createAccount(t, pool, editOwner, "wallet", ptr(-10_000), ptr(10_000))

	txID, legs := registerGroup(t, pool, 1,
		api.InsertOp{ExternalID: "cash", Amount: -700, EffectiveAt: t1},
		api.InsertOp{ExternalID: "wallet", Amount: 700, EffectiveAt: t1},
	)
	must(t, p.processGroup(ctx, txID))
	txBefore := readTx(t, pool, txID)
	sibling := rowJSON(t, pool, legs[1])

	del := registerDelete(t, pool, 2, legs[0], nil)
	must(t, p.processSingle(ctx, del))

	if s := readOpState(t, pool, del.ID); s.status != string(model.OpApplied) {
		t.Fatalf("delete = %s, want APPLIED", s.status)
	}
	leg := readOpState(t, pool, legs[0])
	if leg.status != string(model.OpDeleted) || leg.deletedBy == nil || *leg.deletedBy != del.ID || leg.revision != 1 || leg.amount != -700 {
		t.Fatalf("deleted leg = %+v, want DELETED by %d, last values kept", leg, del.ID)
	}
	if s := readOpState(t, pool, legs[1]); s.status != string(model.OpConfirmed) || s.revision != 1 {
		t.Fatalf("sibling = %+v, want CONFIRMED at revision 1", s)
	}
	if got := rowJSON(t, pool, legs[1]); got != sibling {
		t.Fatalf("sibling leg changed:\n before %s\n after  %s", sibling, got)
	}
	txAfter := readTx(t, pool, txID)
	var opCount int
	if err := pool.QueryRow(ctx, `SELECT op_count FROM transactions WHERE id = $1`, txID).Scan(&opCount); err != nil {
		t.Fatalf("op_count: %v", err)
	}
	if txAfter.status != string(model.TxCommitted) || opCount != 2 || !txAfter.decidedAt.Equal(*txBefore.decidedAt) {
		t.Fatalf("group after the delete = %+v op_count %d, want unchanged COMMITTED/2", txAfter, opCount)
	}
	if bal, _ := readAccount(t, pool, cash); bal != 0 {
		t.Fatalf("cash = %d, want 0 (−700 removed)", bal)
	}
}

// IT-024: a delete of a leg of a REJECTED group is TARGET_NOT_EDITABLE{leg} —
// as a single and as a leg of a group (which rejects the whole group with that
// one reason); the leg and its group are untouched.
func TestDeleteLegOfRejectedGroup(t *testing.T) {
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

	del := registerDelete(t, pool, 2, legs[1], nil)
	must(t, p.processSingle(ctx, del))
	rej := requireRejection(t, readOpState(t, pool, del.ID), model.ReasonTargetNotEditable, legs[1])
	if rej.ExpectedRevision != nil || rej.ActualRevision != nil {
		t.Fatalf("TARGET_NOT_EDITABLE must not carry revisions: %+v", rej)
	}

	group, groupLegs := registerGroup(t, pool, 3,
		api.InsertOp{ExternalID: "wallet", Amount: 5, EffectiveAt: t2},
		api.InsertOp{DeleteOf: &legs[1]},
	)
	must(t, p.processGroup(ctx, group))
	r := readTx(t, pool, group)
	if r.status != string(model.TxRejected) || r.reason == nil {
		t.Fatalf("group = %+v, want REJECTED", r)
	}
	if rej, err := model.ParseRejection(*r.reason); err != nil || rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != legs[1] {
		t.Fatalf("group reason = %+v (%v), want TARGET_NOT_EDITABLE{%d}", rej, err, legs[1])
	}
	for _, id := range groupLegs {
		if s := readOpState(t, pool, id); s.status != string(model.OpInvalid) || s.reason == nil || *s.reason != *r.reason {
			t.Fatalf("leg %d = %+v, want INVALID with the group's reason", id, s)
		}
	}
	if got := rowJSON(t, pool, legs[1]); got != legBefore {
		t.Fatalf("rejected leg changed:\n before %s\n after  %s", legBefore, got)
	}
	if r := readTx(t, pool, txID); r.status != string(model.TxRejected) {
		t.Fatalf("original group = %+v, want still REJECTED", r)
	}
}

// IT-042 (grouped delete-leg half): a group carrying a delete of a DELETED
// target — with or without a matching guard — is REJECTED as a whole with
// TARGET_NOT_EDITABLE{target}; a delete leg with a stale guard rejects the
// whole group with STALE_REVISION. The other legs' targets, the DELETED row and
// the balances are untouched.
func TestGroupWithDeleteOfDeletedOrStaleTargetRejectsWhole(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	cash := createAccount(t, pool, editOwner, "cash", ptr(-10_000), ptr(10_000))
	gone := confirmSingle(t, p, pool, cash, -1500, t1)
	live := confirmSingle(t, p, pool, cash, 800, t2)
	must(t, p.processSingle(ctx, registerDelete(t, pool, 1, gone, nil)))
	goneRow := rowJSON(t, pool, gone)
	liveRow := rowJSON(t, pool, live)
	bal0, ver0 := readAccount(t, pool, cash)
	key := 10

	check := func(name string, wantCode model.ReasonCode, items ...api.InsertOp) model.Rejection {
		t.Helper()
		key++
		txID, legs := registerGroup(t, pool, key, items...)
		must(t, p.processGroup(ctx, txID))
		r := readTx(t, pool, txID)
		if r.status != string(model.TxRejected) || r.reason == nil {
			t.Fatalf("%s: tx = %+v, want REJECTED", name, r)
		}
		rej, err := model.ParseRejection(*r.reason)
		if err != nil || rej.Code != wantCode {
			t.Fatalf("%s: reason = %s (%v), want %s", name, *r.reason, err, wantCode)
		}
		for _, id := range legs {
			if s := readOpState(t, pool, id); s.status != string(model.OpInvalid) || s.reason == nil || *s.reason != *r.reason {
				t.Fatalf("%s: leg %d = %+v, want INVALID with the group's reason", name, id, s)
			}
		}
		if got := rowJSON(t, pool, gone); got != goneRow {
			t.Fatalf("%s: DELETED row changed:\n before %s\n after  %s", name, goneRow, got)
		}
		if got := rowJSON(t, pool, live); got != liveRow {
			t.Fatalf("%s: live target changed:\n before %s\n after  %s", name, liveRow, got)
		}
		if bal, ver := readAccount(t, pool, cash); bal != bal0 || ver != ver0 {
			t.Fatalf("%s: cash moved %d/%d → %d/%d", name, bal0, ver0, bal, ver)
		}
		return rej
	}

	rej := check("deleted", model.ReasonTargetNotEditable,
		api.InsertOp{DeleteOf: &live},
		api.InsertOp{DeleteOf: &gone},
		api.InsertOp{ExternalID: "cash", Amount: 10, EffectiveAt: t3},
	)
	if rej.OperationID == nil || *rej.OperationID != gone || rej.ExpectedRevision != nil {
		t.Fatalf("deleted: rejection = %+v, want TARGET_NOT_EDITABLE{%d}", rej, gone)
	}
	rej = check("deleted-guarded", model.ReasonTargetNotEditable,
		api.InsertOp{DeleteOf: &gone, ExpectedRevision: ptr32(1)}, // status rules first, even with a matching guard
		api.InsertOp{EditOf: &live, Amount: 700},
	)
	if rej.OperationID == nil || *rej.OperationID != gone {
		t.Fatalf("deleted-guarded: rejection = %+v, want TARGET_NOT_EDITABLE{%d}", rej, gone)
	}
	rej = check("stale", model.ReasonStaleRevision,
		api.InsertOp{ExternalID: "cash", Amount: 10, EffectiveAt: t3},
		api.InsertOp{DeleteOf: &live, ExpectedRevision: ptr32(2)},
	)
	if rej.OperationID == nil || *rej.OperationID != live || rej.ExpectedRevision == nil || *rej.ExpectedRevision != 2 || rej.ActualRevision == nil || *rej.ActualRevision != 1 {
		t.Fatalf("stale: rejection = %+v, want STALE_REVISION{%d, 2, 1}", rej, live)
	}
}

// IT-046: the Guard 2 split with delete legs, and a Guard 3 miss. (a) After a
// clean commit the CONFIRMED flip covered exactly the regular legs and the
// APPLIED flip exactly the edit + delete legs. (b) A leg decided from under the
// leader between its reads and Guard 2 — a delete leg flipped INVALID, then a
// regular leg — makes the corresponding flip's rowcount fall short ⇒
// errGuardMiss, whole group rolled back, no target touched. (c) The
// afterAccountRead seam bumps wallet's version ⇒ Guard 3 miss rolls the whole
// group back — the delete CAS and the edit's history row included: a and d
// still CONFIRMED with no stamps, b at revision 1, every leg PENDING — and the
// clean retry commits.
func TestMixedGroupGuardRowcountsAndGuard3Miss(t *testing.T) {
	pool := dbtest.NewSchema(t)
	p := leaderProcessor(t, pool)
	ctx := context.Background()
	f := seedMixed(t, p, pool, nil)
	before := map[int64]string{f.a: rowJSON(t, pool, f.a), f.b: rowJSON(t, pool, f.b), f.d: rowJSON(t, pool, f.d)}
	cashBal, cashVer := readAccount(t, pool, f.cash)
	walletBal, walletVer := readAccount(t, pool, f.wallet)
	revsBefore := countRows(t, pool, `SELECT count(*) FROM operation_revisions`)

	txID, legs := registerGroup(t, pool, 1, f.mixedItems()...)

	assertUntouched := func(name string) {
		t.Helper()
		if r := readTx(t, pool, txID); r.status != string(model.TxPending) {
			t.Fatalf("%s: tx = %+v, want still PENDING", name, r)
		}
		for id, row := range before {
			if got := rowJSON(t, pool, id); got != row {
				t.Fatalf("%s: target %d changed:\n before %s\n after  %s", name, id, row, got)
			}
		}
		if n := countRows(t, pool, `SELECT count(*) FROM operation_revisions`); n != revsBefore {
			t.Fatalf("%s: operation_revisions = %d, want %d", name, n, revsBefore)
		}
		if bal, ver := readAccount(t, pool, f.cash); bal != cashBal || ver != cashVer {
			t.Fatalf("%s: cash moved %d/%d → %d/%d", name, cashBal, cashVer, bal, ver)
		}
	}

	// (b) A delete leg decided from under us: the APPLIED flip counts one short.
	p.afterAccountRead = func() {
		if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'INVALID', invalidation_reason = '{}' WHERE id = $1`, legs[1]); err != nil {
			t.Errorf("hook flip delete leg: %v", err)
		}
	}
	if err := p.processGroup(ctx, txID); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a delete leg decided under us, got %v", err)
	}
	assertUntouched("delete leg flipped")
	for _, id := range []int64{legs[0], legs[2], legs[3]} {
		if s := readOpState(t, pool, id); s.status != string(model.OpPending) {
			t.Fatalf("leg %d = %s after the rollback, want PENDING", id, s.status)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'PENDING', invalidation_reason = NULL WHERE id = $1`, legs[1]); err != nil {
		t.Fatalf("restore delete leg: %v", err)
	}
	// A regular leg decided from under us: the CONFIRMED flip counts one short.
	p.afterAccountRead = func() {
		if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'INVALID', invalidation_reason = '{}' WHERE id = $1`, legs[3]); err != nil {
			t.Errorf("hook flip regular leg: %v", err)
		}
	}
	if err := p.processGroup(ctx, txID); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a regular leg decided under us, got %v", err)
	}
	assertUntouched("regular leg flipped")
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'PENDING', invalidation_reason = NULL WHERE id = $1`, legs[3]); err != nil {
		t.Fatalf("restore regular leg: %v", err)
	}

	// (c) Guard 3 miss on wallet rolls everything back, delete CAS included.
	p.afterAccountRead = func() {
		if _, err := pool.Exec(ctx, `UPDATE accounts SET version = version + 1 WHERE id = $1`, f.wallet); err != nil {
			t.Errorf("hook version bump: %v", err)
		}
	}
	if err := p.processGroup(ctx, txID); !errors.Is(err, errGuardMiss) {
		t.Fatalf("expected errGuardMiss on a stale wallet version, got %v", err)
	}
	assertUntouched("guard 3")
	for _, id := range legs {
		if s := readOpState(t, pool, id); s.status != string(model.OpPending) {
			t.Fatalf("leg %d = %s after the Guard 3 miss, want PENDING", id, s.status)
		}
	}
	for _, id := range []int64{f.a, f.d} {
		if s := readOpState(t, pool, id); s.status != string(model.OpConfirmed) || s.deletedBy != nil || s.deletedAt != nil {
			t.Fatalf("target %d = %+v after the Guard 3 miss, want CONFIRMED with no stamps", id, s)
		}
	}
	if bal, ver := readAccount(t, pool, f.wallet); bal != walletBal || ver != walletVer+1 {
		t.Fatalf("wallet = %d/%d, want %d/%d (the hook's bump only)", bal, ver, walletBal, walletVer+1)
	}

	// (a) Clean retry: commits against the new version; the flips covered
	// exactly their classes.
	p.afterAccountRead = nil
	must(t, p.processGroup(ctx, txID))
	if r := readTx(t, pool, txID); r.status != string(model.TxCommitted) {
		t.Fatalf("tx = %+v on clean retry, want COMMITTED", r)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations WHERE transaction_id = $1 AND status = 'CONFIRMED' AND edit_of IS NULL AND confirmed_at IS NOT NULL`, txID); n != 1 {
		t.Fatalf("CONFIRMED regular legs = %d, want 1", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations WHERE transaction_id = $1 AND status = 'APPLIED' AND edit_of IS NOT NULL AND confirmed_at IS NOT NULL`, txID); n != 3 {
		t.Fatalf("APPLIED edit-class legs = %d, want 3 (two deletes + one edit)", n)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM operations WHERE transaction_id = $1 AND status = 'APPLIED' AND is_delete`, txID); n != 2 {
		t.Fatalf("APPLIED delete legs = %d, want 2", n)
	}
	for i, id := range []int64{f.a, f.d} {
		if s := readOpState(t, pool, id); s.status != string(model.OpDeleted) || s.deletedBy == nil || *s.deletedBy != legs[i] {
			t.Fatalf("target %d = %+v, want DELETED by %d", id, s, legs[i])
		}
	}
	if s := readOpState(t, pool, f.b); s.revision != 2 || s.amount != -700 {
		t.Fatalf("b = %+v, want -700 at revision 2", s)
	}
	if bal, ver := readAccount(t, pool, f.cash); bal != -400 || ver != cashVer+1 {
		t.Fatalf("cash = %d/%d, want -400/%d", bal, ver, cashVer+1)
	}
	if bal, ver := readAccount(t, pool, f.wallet); bal != 0 || ver != walletVer+2 {
		t.Fatalf("wallet = %d/%d, want 0/%d", bal, ver, walletVer+2)
	}
}
