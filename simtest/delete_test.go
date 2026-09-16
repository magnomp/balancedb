package simtest

import (
	"errors"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// Unit tests for the reference model's delete semantics (ADR-0011): UT-040
// (apply), UT-041 (limit rejection), the target-state rules the processor's
// TARGET_NOT_EDITABLE / STALE_REVISION branches mirror, and UT-042 (group
// decisions with delete legs). The breadth sweep (generate_test.go) drives the
// same code across 1,000+ seeds.

// seedDeleteModel builds a model with one owner-1 account "cash" (min 0,
// unbounded max), a CONFIRMED +1500 credit (id 1) and a CONFIRMED −500 spend
// (id 2), leaving the balance at 1000.
func seedDeleteModel(t *testing.T) (*Model, int64) {
	t.Helper()
	m := NewModel()
	if err := m.SetLimits(1, "cash", ptr(0), nil); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	for i, amount := range []int64{1500, -500} {
		if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(100 + i), Operations: []api.InsertOp{
			{OwnerID: 1, ExternalID: "cash", Amount: amount, EffectiveAt: when.Add(time.Duration(i) * time.Hour)},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if ds := m.ProcessAll(); len(ds) != 2 || ds[0].Status != model.OpConfirmed || ds[1].Status != model.OpConfirmed {
		t.Fatalf("seed decisions = %+v, want two CONFIRMED", ds)
	}
	if bal := m.AccountBalance(1, "cash"); bal != 1000 {
		t.Fatalf("seed balance = %d, want 1000", bal)
	}
	return m, 1
}

// registerDelete records one single delete of target and returns its id.
func registerDelete(t *testing.T, m *Model, key int, target int64, expected *int32) int64 {
	t.Helper()
	res, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(key), Operations: []api.InsertOp{
		{OwnerID: 1, DeleteOf: &target, ExpectedRevision: expected},
	}})
	if err != nil {
		t.Fatalf("register delete of %d: %v", target, err)
	}
	out := res.Operations[0]
	if out.Status != string(model.OpPending) || out.DeleteOf == nil || *out.DeleteOf != target || out.EditOf != nil {
		t.Fatalf("delete outcome = %+v, want PENDING with DeleteOf %d and no EditOf", out, target)
	}
	return out.ID
}

// UT-040: apply. Deleting the −500 spend (id 2) on a balance of 1000 passes min 0
// → the delete is APPLIED, the target is DELETED with DeletedBy = the delete,
// its last values and Revision 1 intact, no revision appended, and the balance
// moved by +500. The registration row copied the target's values and is
// otherwise an ordinary edit-class row (never CONFIRMED).
func TestReferenceDeleteApply(t *testing.T) {
	m, _ := seedDeleteModel(t)
	del := registerDelete(t, m, 1, 2, nil)
	row := m.ops[del]
	if !row.isDelete() || *row.EditOf != 2 || row.Amount != -500 || row.AccountID != m.ops[2].AccountID {
		t.Fatalf("delete row = %+v, want is_delete of 2 with the target's informational copy", *row)
	}

	ds := m.ProcessAll()
	if len(ds) != 1 || ds[0].OpID != del || ds[0].Status != model.OpApplied || ds[0].Reason != nil {
		t.Fatalf("decisions = %+v, want one APPLIED delete", ds)
	}
	target := m.ops[2]
	if target.Status != model.OpDeleted || target.DeletedBy == nil || *target.DeletedBy != del {
		t.Fatalf("target = %+v, want DELETED by %d", *target, del)
	}
	if target.Amount != -500 || target.Revision != 1 || len(m.revisions[2]) != 0 {
		t.Fatalf("target = %+v with %d revisions, want last values kept, revision 1, no history", *target, len(m.revisions[2]))
	}
	if bal := m.AccountBalance(1, "cash"); bal != 1500 {
		t.Fatalf("balance = %d, want 1500 (moved by +500)", bal)
	}
	if err := m.CheckEdits(); err != nil {
		t.Fatalf("CheckEdits: %v", err)
	}
	if err := m.CheckDeletes(); err != nil {
		t.Fatalf("CheckDeletes: %v", err)
	}
	// A replay returns the row's current status under DeleteOf.
	res, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(1), Operations: []api.InsertOp{{OwnerID: 1, DeleteOf: ptr(2)}}})
	if err != nil || !res.Replayed || res.Operations[0].ID != del || res.Operations[0].Status != string(model.OpApplied) || res.Operations[0].DeleteOf == nil {
		t.Fatalf("replay = %+v (%v), want the APPLIED delete", res, err)
	}
}

// UT-041: limit rejection. Deleting the +1500 credit (id 1) on a balance of
// 1000 with min 0 lands at −500 → the delete is INVALID with the processor's
// exact LIMIT_VIOLATED{cash, min, 500}; the target and balance are untouched.
func TestReferenceDeleteLimitViolationRejects(t *testing.T) {
	m, target := seedDeleteModel(t)
	del := registerDelete(t, m, 1, target, nil)

	ds := m.ProcessAll()
	if len(ds) != 1 || ds[0].OpID != del || ds[0].Status != model.OpInvalid || ds[0].Reason == nil {
		t.Fatalf("decisions = %+v, want one INVALID delete", ds)
	}
	want := model.Rejection{Code: model.ReasonLimitViolated, Account: "cash", LimitSide: model.LimitMin, Shortfall: 500}
	if got := *ds[0].Reason; got != want {
		t.Fatalf("rejection = %+v, want %+v", got, want)
	}
	if m.ops[del].Status != model.OpInvalid {
		t.Fatalf("delete row = %s, want INVALID", m.ops[del].Status)
	}
	if tgt := m.ops[target]; tgt.Status != model.OpConfirmed || tgt.DeletedBy != nil || tgt.Revision != 1 {
		t.Fatalf("target touched by a rejected delete: %+v", *tgt)
	}
	if bal := m.AccountBalance(1, "cash"); bal != 1000 {
		t.Fatalf("balance = %d, want 1000 (untouched)", bal)
	}
	if err := m.CheckDeletes(); err != nil {
		t.Fatalf("CheckDeletes: %v", err)
	}
}

// Target-state rules: a delete of a DELETED target, an edit of a DELETED target,
// and a delete of an INVALID target are all TARGET_NOT_EDITABLE{target}; a
// stale expected_revision is STALE_REVISION{target, expected, actual}; a
// matching one applies. Deletes and edits of a DELETED target are registered
// (never refused at submission) and the DELETED row is never touched.
func TestReferenceDeleteTargetStateRules(t *testing.T) {
	m, _ := seedDeleteModel(t)

	// Stale then matching guard on the −500 spend (id 2, revision 1).
	stale := registerDelete(t, m, 1, 2, ptr32(2))
	ds := m.ProcessAll()
	if len(ds) != 1 || ds[0].Status != model.OpInvalid || ds[0].Reason == nil || ds[0].Reason.Code != model.ReasonStaleRevision ||
		*ds[0].Reason.OperationID != 2 || *ds[0].Reason.ExpectedRevision != 2 || *ds[0].Reason.ActualRevision != 1 {
		t.Fatalf("stale delete %d decisions = %+v, want STALE_REVISION{2, 2, 1}", stale, ds)
	}
	first := registerDelete(t, m, 2, 2, ptr32(1))
	if ds := m.ProcessAll(); len(ds) != 1 || ds[0].Status != model.OpApplied {
		t.Fatalf("guarded delete %d decisions = %+v, want APPLIED", first, ds)
	}
	deletedRow := *m.ops[2]

	// Second delete, an edit and a guarded delete of the DELETED target.
	second := registerDelete(t, m, 3, 2, nil)
	editRes, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(4), Operations: []api.InsertOp{{OwnerID: 1, EditOf: ptr(2), Amount: -400}}})
	if err != nil {
		t.Fatalf("edit of a DELETED target must register: %v", err)
	}
	guarded := registerDelete(t, m, 5, 2, ptr32(1))
	ds = m.ProcessAll()
	if len(ds) != 3 {
		t.Fatalf("decisions = %+v, want three rejections", ds)
	}
	for i, id := range []int64{second, editRes.Operations[0].ID, guarded} {
		d := ds[i]
		if d.OpID != id || d.Status != model.OpInvalid || d.Reason == nil || d.Reason.Code != model.ReasonTargetNotEditable || *d.Reason.OperationID != 2 {
			t.Fatalf("decision %d = %+v, want TARGET_NOT_EDITABLE{2} for row %d", i, d, id)
		}
		if d.Reason.ExpectedRevision != nil || d.Reason.ActualRevision != nil {
			t.Fatalf("TARGET_NOT_EDITABLE must not carry revisions: %+v", *d.Reason)
		}
	}
	if got := *m.ops[2]; got != deletedRow {
		t.Fatalf("DELETED row changed: before %+v after %+v", deletedRow, got)
	}
	if bal := m.AccountBalance(1, "cash"); bal != 1500 {
		t.Fatalf("balance = %d, want 1500", bal)
	}

	// An INVALID target: a −5000 debit rejected on min 0, then deleted.
	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(6), Operations: []api.InsertOp{
		{OwnerID: 1, ExternalID: "cash", Amount: -5000, EffectiveAt: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)},
	}}); err != nil {
		t.Fatal(err)
	}
	ds = m.ProcessAll()
	invalid := ds[0].OpID
	if ds[0].Status != model.OpInvalid {
		t.Fatalf("debit = %+v, want INVALID", ds[0])
	}
	registerDelete(t, m, 7, invalid, nil)
	if ds := m.ProcessAll(); len(ds) != 1 || ds[0].Status != model.OpInvalid || ds[0].Reason.Code != model.ReasonTargetNotEditable || *ds[0].Reason.OperationID != invalid {
		t.Fatalf("delete of an INVALID target = %+v, want TARGET_NOT_EDITABLE{%d}", ds, invalid)
	}
	if err := m.CheckEdits(); err != nil {
		t.Fatalf("CheckEdits: %v", err)
	}
	if err := m.CheckDeletes(); err != nil {
		t.Fatalf("CheckDeletes: %v", err)
	}
}

// Structural guards mirror the ledger: a delete item with any other field is
// ErrDeleteWithFields; a delete and an edit of one target in one request is
// ErrDuplicateEditTarget; AllowDeletes=false is ErrDeletesDisabled independently
// of AllowEdits; a delete of a registration row is ErrEditTargetNotOperation;
// the hash covers the item as sent (a changed guard conflicts).
func TestReferenceDeleteInsertGuards(t *testing.T) {
	m, target := seedDeleteModel(t)
	del := func(key int, ops ...api.InsertOp) error {
		_, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(key), Operations: ops})
		return err
	}
	if err := del(1, api.InsertOp{OwnerID: 1, DeleteOf: &target, Amount: -1}); !errors.Is(err, ledger.ErrDeleteWithFields) {
		t.Fatalf("delete with amount: %v, want ErrDeleteWithFields", err)
	}
	if err := del(2, api.InsertOp{OwnerID: 1, DeleteOf: &target}, api.InsertOp{OwnerID: 1, EditOf: &target, Amount: -1}); !errors.Is(err, ledger.ErrDuplicateEditTarget) {
		t.Fatalf("delete + edit of one target: %v, want ErrDuplicateEditTarget", err)
	}
	if err := del(3, api.InsertOp{OwnerID: 1, DeleteOf: &target, ExpectedRevision: ptr32(0)}); !errors.Is(err, ledger.ErrInvalidExpectedRevision) {
		t.Fatalf("expected_revision 0: %v, want ErrInvalidExpectedRevision", err)
	}
	m.AllowDeletes = false
	if err := del(4, api.InsertOp{OwnerID: 1, DeleteOf: &target}); !errors.Is(err, ledger.ErrDeletesDisabled) {
		t.Fatalf("allow_deletes=false: %v, want ErrDeletesDisabled", err)
	}
	if err := del(5, api.InsertOp{OwnerID: 1, EditOf: &target, Amount: -1}); err != nil {
		t.Fatalf("allow_deletes=false must not refuse edits: %v", err)
	}
	m.AllowDeletes, m.AllowEdits = true, false
	if err := del(6, api.InsertOp{OwnerID: 1, DeleteOf: &target}); err != nil {
		t.Fatalf("allow_edits=false must not refuse deletes: %v", err)
	}
	m.AllowEdits = true
	if err := del(7, api.InsertOp{OwnerID: 1, DeleteOf: ptr(m.nextOpID)}); !errors.Is(err, ledger.ErrEditTargetNotOperation) {
		t.Fatalf("delete of a delete registration: %v, want ErrEditTargetNotOperation", err)
	}
	if err := del(8, api.InsertOp{OwnerID: 2, DeleteOf: &target}); !errors.Is(err, ledger.ErrEditTargetNotFound) {
		t.Fatalf("foreign owner: %v, want ErrEditTargetNotFound", err)
	}
	if err := del(6, api.InsertOp{OwnerID: 1, DeleteOf: &target, ExpectedRevision: ptr32(1)}); !errors.Is(err, api.ErrPayloadConflict) {
		t.Fatalf("same key, changed guard: %v, want ErrPayloadConflict", err)
	}
	if n := m.OpCount(); n != 4 { // two seeds, the edit (key 5), the delete (key 6)
		t.Fatalf("op count = %d, want 4 (refused items record nothing)", n)
	}
}

// UT-042: group decisions with delete legs. A mixed group [delete 2, edit 1
// −1500 → +1200, new −300] on cash (min 0, balance 1000) nets +500 −300 −300 =
// −100 → 900: COMMITTED with the delete and edit legs APPLIED, the new leg
// CONFIRMED, target 2 DELETED by its leg (revision untouched, nothing appended)
// and target 1 at revision 2 with one history row. Then a group carrying a
// delete of the DELETED target 2 — even with a matching guard — next to a valid
// new leg is REJECTED as a whole with TARGET_NOT_EDITABLE{2}: every leg INVALID,
// the DELETED row and the balance untouched. G2 and the deletion invariants
// hold throughout.
func TestReferenceGroupWithDeleteLegs(t *testing.T) {
	m, _ := seedDeleteModel(t)
	when := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	res, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(1), Operations: []api.InsertOp{
		{OwnerID: 1, DeleteOf: ptr(2)},
		{OwnerID: 1, EditOf: ptr(1), Amount: 1200},
		{OwnerID: 1, ExternalID: "cash", Amount: -300, EffectiveAt: when},
	}})
	if err != nil || res.TransactionID == nil || len(res.Operations) != 3 {
		t.Fatalf("mixed group insert = %+v (%v), want a 3-leg group", res, err)
	}
	if o := res.Operations[0]; o.DeleteOf == nil || *o.DeleteOf != 2 || o.EditOf != nil {
		t.Fatalf("delete leg outcome = %+v, want DeleteOf 2", o)
	}
	del, edit, fresh := res.Operations[0].ID, res.Operations[1].ID, res.Operations[2].ID

	ds := m.ProcessAll()
	if len(ds) != 1 || ds[0].TxID == nil || *ds[0].TxID != *res.TransactionID || ds[0].TxStatus != model.TxCommitted || ds[0].Reason != nil {
		t.Fatalf("decisions = %+v, want one COMMITTED group", ds)
	}
	if m.ops[del].Status != model.OpApplied || m.ops[edit].Status != model.OpApplied || m.ops[fresh].Status != model.OpConfirmed {
		t.Fatalf("legs = %s/%s/%s, want APPLIED/APPLIED/CONFIRMED", m.ops[del].Status, m.ops[edit].Status, m.ops[fresh].Status)
	}
	if tgt := m.ops[2]; tgt.Status != model.OpDeleted || tgt.DeletedBy == nil || *tgt.DeletedBy != del || tgt.Revision != 1 || len(m.revisions[2]) != 0 {
		t.Fatalf("target 2 = %+v (%d revisions), want DELETED by %d at revision 1 with no history", *tgt, len(m.revisions[2]), del)
	}
	if tgt := m.ops[1]; tgt.Status != model.OpConfirmed || tgt.Amount != 1200 || tgt.Revision != 2 || len(m.revisions[1]) != 1 || m.revisions[1][0].SupersededBy != edit {
		t.Fatalf("target 1 = %+v (%d revisions), want +1200 at revision 2 superseded by %d", *tgt, len(m.revisions[1]), edit)
	}
	if bal := m.AccountBalance(1, "cash"); bal != 900 {
		t.Fatalf("balance = %d, want 900", bal)
	}
	for _, check := range []func() error{m.CheckG1, m.CheckG2, m.CheckEdits, m.CheckDeletes} {
		if err := check(); err != nil {
			t.Fatal(err)
		}
	}
	deletedRow := *m.ops[2]

	// A group with a delete of the DELETED target is rejected as a whole.
	res, err = m.Insert(api.InsertRequest{IdempotencyKey: uuid(2), Operations: []api.InsertOp{
		{OwnerID: 1, ExternalID: "cash", Amount: 100, EffectiveAt: when},
		{OwnerID: 1, DeleteOf: ptr(2), ExpectedRevision: ptr32(1)},
	}})
	if err != nil {
		t.Fatalf("a delete of a DELETED target must register: %v", err)
	}
	ds = m.ProcessAll()
	if len(ds) != 1 || ds[0].TxStatus != model.TxRejected || ds[0].Reason == nil ||
		ds[0].Reason.Code != model.ReasonTargetNotEditable || ds[0].Reason.OperationID == nil || *ds[0].Reason.OperationID != 2 {
		t.Fatalf("decisions = %+v, want one group REJECTED with TARGET_NOT_EDITABLE{2}", ds)
	}
	if ds[0].Reason.ExpectedRevision != nil || ds[0].Reason.ActualRevision != nil {
		t.Fatalf("TARGET_NOT_EDITABLE must not carry revisions: %+v", *ds[0].Reason)
	}
	for _, o := range res.Operations {
		if m.ops[o.ID].Status != model.OpInvalid {
			t.Fatalf("leg %d = %s, want INVALID", o.ID, m.ops[o.ID].Status)
		}
	}
	if got := *m.ops[2]; got != deletedRow {
		t.Fatalf("DELETED row changed: before %+v after %+v", deletedRow, got)
	}
	if bal := m.AccountBalance(1, "cash"); bal != 900 {
		t.Fatalf("balance = %d, want 900 (untouched)", bal)
	}
	for _, check := range []func() error{m.CheckG2, m.CheckEdits, m.CheckDeletes} {
		if err := check(); err != nil {
			t.Fatal(err)
		}
	}
}

func ptr32(v int32) *int32 { return &v }
