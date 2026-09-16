//go:build itest

package ledger_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// Delete registration itests (ADR-0011; IT-006 … IT-008 driven through
// ledger.Insert): the delete row shape, every sentinel with its kind, owner
// scoping, replay, payload conflict, the allow_deletes policy and the query
// core's DeleteOf / DeletedAt / DeletedBy. Nothing is decided here — rows land
// PENDING for the processor (task 03 owns the DELETED transition).

// deleteOf builds a single-delete request for owner 7 under key(n).
func deleteOf(n int, target int64, guard *int32) ledger.InsertRequest {
	return ledger.InsertRequest{
		IdempotencyKey: key(n),
		Operations:     []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, ExpectedRevision: guard}},
	}
}

// targetKind asserts err wraps sentinel through a TargetError of the given kind
// naming target.
func targetKind(t *testing.T, err, sentinel error, kind ledger.TargetKind, target int64) {
	t.Helper()
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	var te *ledger.TargetError
	if !errors.As(err, &te) || te.Kind != kind || te.Target != target {
		t.Fatalf("err = %v, want TargetError{%s, %d}", err, kind, target)
	}
}

// Happy path: a single delete registers a PENDING edit-class row with
// is_delete set, the target's current state copied as the informational
// snapshot, the guard bound, the key on the row and the doorbell rung; the
// outcome reports DeleteOf (never EditOf) and the target is untouched.
func TestInsertDeleteSingleRegistersPending(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	target := seedOp(t, pool, 1, 7, "cash", -1500)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN work_available"); err != nil {
		t.Fatalf("listen: %v", err)
	}

	res, err := insert(t, pool, deleteOf(2, target, i32(1)))
	if err != nil {
		t.Fatalf("insert delete: %v", err)
	}
	if res.Replayed || res.TransactionID != nil || len(res.Operations) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	out := res.Operations[0]
	if out.Status != string(model.OpPending) || out.DeleteOf == nil || *out.DeleteOf != target || out.EditOf != nil {
		t.Fatalf("unexpected outcome: %+v", out)
	}

	row := readOp(t, pool, out.ID)
	targetRow := readOp(t, pool, target)
	if row.Status != "PENDING" || !row.IsDelete || row.EditOf == nil || *row.EditOf != target || row.ExpectedRevision == nil || *row.ExpectedRevision != 1 {
		t.Fatalf("delete row not registered as expected: %+v", row)
	}
	if row.AccountID != targetRow.AccountID || row.Amount != -1500 || !row.EffectiveAt.Equal(t1) {
		t.Fatalf("delete row informational copy wrong: %+v (target %+v)", row, targetRow)
	}
	if !row.HasKey || row.TxID != nil || row.ReversalOf != nil || row.Revision != 1 {
		t.Fatalf("delete row metadata wrong: %+v", row)
	}
	if targetRow.Amount != -1500 || targetRow.Status != "PENDING" || targetRow.Revision != 1 || targetRow.IsDelete {
		t.Fatalf("target mutated by registration: %+v", targetRow)
	}
	if n := count(t, pool, "accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1 (a delete never upserts)", n)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if n, err := conn.Conn().WaitForNotification(waitCtx); err != nil || n.Channel != "work_available" {
		t.Fatalf("expected doorbell, got %v / %v", n, err)
	}
}

// The delete's informational copy follows the target's *current* row: after an
// applied edit (stood in for here) the copy carries the edited values, and the
// target's status is never checked at submission — a PENDING, INVALID or (stood
// in for) DELETED target registers the same way (user decision: decision-time
// only).
func TestInsertDeleteCopiesCurrentStateAndIgnoresStatus(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	target := seedOp(t, pool, 1, 7, "cash", -1500)
	moved := t1.Add(2 * time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE operations SET amount = -1200, effective_at = $2, revision = 2 WHERE id = $1`, target, moved); err != nil {
		t.Fatalf("stand in for an applied edit: %v", err)
	}
	res, err := insert(t, pool, deleteOf(2, target, nil))
	if err != nil {
		t.Fatalf("delete of edited target: %v", err)
	}
	row := readOp(t, pool, res.Operations[0].ID)
	if row.Amount != -1200 || !row.EffectiveAt.Equal(moved) || row.Revision != 1 || row.ExpectedRevision != nil {
		t.Fatalf("copy must follow the current row: %+v", row)
	}

	for _, status := range []string{"INVALID", "DELETED"} {
		t.Run(status, func(t *testing.T) {
			n := 10
			if status == "DELETED" {
				n = 11
			}
			other := seedOp(t, pool, n, 7, "cash", -5)
			stamp := `UPDATE operations SET status = $2 WHERE id = $1`
			if status == "DELETED" {
				stamp = `UPDATE operations SET status = $2, deleted_by = $1, deleted_at = now() WHERE id = $1`
			}
			if _, err := pool.Exec(ctx, stamp, other, status); err != nil {
				t.Fatalf("stamp %s: %v", status, err)
			}
			res, err := insert(t, pool, deleteOf(n+10, other, nil))
			if err != nil {
				t.Fatalf("delete of %s target must register: %v", status, err)
			}
			if r := readOp(t, pool, res.Operations[0].ID); r.Status != "PENDING" || !r.IsDelete {
				t.Fatalf("registration of %s target: %+v", status, r)
			}
		})
	}
}

// Structural sentinels fire before any write, with the same semantics as for
// edits: extra fields, expected_revision < 1, duplicate targets across kinds.
func TestInsertDeleteStructuralSentinels(t *testing.T) {
	pool := dbtest.NewSchema(t)
	target := seedOp(t, pool, 1, 7, "cash", -1500)

	cases := []struct {
		name string
		ops  []ledger.InsertOp
		want error
	}{
		{"amount", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, Amount: -1}}, ledger.ErrDeleteWithFields},
		{"account", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, ExternalID: "cash"}}, ledger.ErrDeleteWithFields},
		{"effective_at", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, EffectiveAt: t1}}, ledger.ErrDeleteWithFields},
		{"reversal_of", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, ReversalOf: &target}}, ledger.ErrDeleteWithFields},
		{"edit_of", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, EditOf: &target}}, ledger.ErrDeleteWithFields},
		{"expected_revision 0", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target, ExpectedRevision: i32(0)}}, ledger.ErrInvalidExpectedRevision},
		{"delete twice", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target}, {OwnerID: 7, DeleteOf: &target}}, ledger.ErrDuplicateEditTarget},
		{"delete then edit", []ledger.InsertOp{{OwnerID: 7, DeleteOf: &target}, {OwnerID: 7, EditOf: &target, Amount: -1}}, ledger.ErrDuplicateEditTarget},
		{"edit then delete", []ledger.InsertOp{{OwnerID: 7, EditOf: &target, Amount: -1}, {OwnerID: 7, DeleteOf: &target}}, ledger.ErrDuplicateEditTarget},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := insert(t, pool, ledger.InsertRequest{IdempotencyKey: key(100 + i), Operations: tc.ops})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n := count(t, pool, "operations"); n != 1 {
		t.Fatalf("operations = %d, want 1 (seed only)", n)
	}
	if n := count(t, pool, "transactions"); n != 0 {
		t.Fatalf("transactions = %d, want 0", n)
	}
}

// Target sentinels are shared with edits and carry the delete kind: unknown and
// foreign targets are the same "not found"; an edit or delete registration is
// not a deletable target. No rows are written.
func TestInsertDeleteTargetSentinelsCarryKind(t *testing.T) {
	pool := dbtest.NewSchema(t)
	mine := seedOp(t, pool, 1, 7, "cash", -1500)
	theirs := seedOp(t, pool, 2, 8, "cash", -1500)

	_, err := insert(t, pool, deleteOf(3, theirs, nil))
	targetKind(t, err, ledger.ErrEditTargetNotFound, ledger.TargetDelete, theirs)
	unknown := theirs + 1000
	_, err = insert(t, pool, deleteOf(4, unknown, nil))
	targetKind(t, err, ledger.ErrEditTargetNotFound, ledger.TargetDelete, unknown)

	// Edit-kind wrapping is unchanged for edit items.
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(5),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &theirs, Amount: -1}},
	})
	targetKind(t, err, ledger.ErrEditTargetNotFound, ledger.TargetEdit, theirs)

	edit, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(6),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &mine, Amount: -1200}},
	})
	if err != nil {
		t.Fatalf("register edit: %v", err)
	}
	del, err := insert(t, pool, deleteOf(7, mine, nil))
	if err != nil {
		t.Fatalf("register delete: %v", err)
	}
	editID, delID := edit.Operations[0].ID, del.Operations[0].ID

	_, err = insert(t, pool, deleteOf(8, editID, nil))
	targetKind(t, err, ledger.ErrEditTargetNotOperation, ledger.TargetDelete, editID)
	_, err = insert(t, pool, deleteOf(9, delID, nil))
	targetKind(t, err, ledger.ErrEditTargetNotOperation, ledger.TargetDelete, delID)
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(10),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &delID, Amount: -1}},
	})
	targetKind(t, err, ledger.ErrEditTargetNotOperation, ledger.TargetEdit, delID)

	if n := count(t, pool, "operations"); n != 4 {
		t.Fatalf("operations = %d, want 4 (2 seeds, 1 edit, 1 delete)", n)
	}
}

// IT-006: group-level sentinels — duplicate target across kinds, unknown delete
// target — leave no transactions row (nor account) behind.
func TestInsertDeleteGroupSentinelsWriteNothing(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)

	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(2),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, DeleteOf: &a},
			{OwnerID: 7, EditOf: &a, Amount: -1},
		},
	})
	targetKind(t, err, ledger.ErrDuplicateEditTarget, ledger.TargetEdit, a)

	unknown := a + 998
	_, err = insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &a, Amount: -1200},
			{OwnerID: 7, ExternalID: "wallet", Amount: 300, EffectiveAt: t1},
			{OwnerID: 7, DeleteOf: &unknown},
		},
	})
	targetKind(t, err, ledger.ErrEditTargetNotFound, ledger.TargetDelete, unknown)

	if n := count(t, pool, "transactions"); n != 0 {
		t.Fatalf("transactions = %d, want 0", n)
	}
	if n := count(t, pool, "operations"); n != 1 {
		t.Fatalf("operations = %d, want 1 (seed only)", n)
	}
	if n := count(t, pool, "accounts"); n != 1 {
		t.Fatalf("accounts = %d, want 1 (wallet not upserted before the failed lookup)", n)
	}
}

// A group mixing deletes, an edit and a new operation registers atomically
// with one transactions row, each leg's kind reported exclusively.
func TestInsertDeleteGroupMixedRegistersAtomically(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)
	c := seedOp(t, pool, 3, 7, "cash", -700)

	res, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, DeleteOf: &a},
			{OwnerID: 7, DeleteOf: &b, ExpectedRevision: i32(2)},
			{OwnerID: 7, EditOf: &c, Amount: -600},
			{OwnerID: 7, ExternalID: "cash", Amount: 300, EffectiveAt: t1},
		},
	})
	if err != nil {
		t.Fatalf("mixed group: %v", err)
	}
	if res.TransactionID == nil || res.TransactionStatus != string(model.TxPending) || len(res.Operations) != 4 || res.Replayed {
		t.Fatalf("unexpected result: %+v", res)
	}
	ops := res.Operations
	if ops[0].DeleteOf == nil || *ops[0].DeleteOf != a || ops[0].EditOf != nil ||
		ops[1].DeleteOf == nil || *ops[1].DeleteOf != b || ops[1].EditOf != nil ||
		ops[2].EditOf == nil || *ops[2].EditOf != c || ops[2].DeleteOf != nil ||
		ops[3].EditOf != nil || ops[3].DeleteOf != nil {
		t.Fatalf("leg kinds: %+v", ops)
	}
	for i, want := range []struct {
		isDelete bool
		amount   int64
		guard    *int32
	}{{true, -1500, nil}, {true, -300, i32(2)}, {false, -600, nil}, {false, 300, nil}} {
		row := readOp(t, pool, ops[i].ID)
		if row.IsDelete != want.isDelete || row.Amount != want.amount || row.TxID == nil || *row.TxID != *res.TransactionID || row.Status != "PENDING" {
			t.Fatalf("leg %d: %+v", i, row)
		}
		if (row.ExpectedRevision == nil) != (want.guard == nil) || (want.guard != nil && *row.ExpectedRevision != *want.guard) {
			t.Fatalf("leg %d guard: %+v", i, row)
		}
	}
	if n := count(t, pool, "transactions"); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
}

// IT-007: same key + same delete item twice → replay of the original id with
// DeleteOf set (and EditOf nil), no new row, for singles and groups.
func TestInsertDeleteIdempotentReplay(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)

	single := deleteOf(3, a, i32(1))
	first, err := insert(t, pool, single)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := insert(t, pool, single)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.Operations[0].ID != first.Operations[0].ID {
		t.Fatalf("single replay mismatch: %+v vs %+v", second, first)
	}
	if o := second.Operations[0]; o.DeleteOf == nil || *o.DeleteOf != a || o.EditOf != nil || o.Status != string(model.OpPending) {
		t.Fatalf("single replay outcome: %+v", o)
	}

	group := ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, DeleteOf: &b},
			{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1},
		},
	}
	gFirst, err := insert(t, pool, group)
	if err != nil {
		t.Fatalf("group first: %v", err)
	}
	gSecond, err := insert(t, pool, group)
	if err != nil {
		t.Fatalf("group replay: %v", err)
	}
	if !gSecond.Replayed || *gSecond.TransactionID != *gFirst.TransactionID || len(gSecond.Operations) != 2 {
		t.Fatalf("group replay mismatch: %+v vs %+v", gSecond, gFirst)
	}
	if o := gSecond.Operations; o[0].DeleteOf == nil || *o[0].DeleteOf != b || o[0].EditOf != nil || o[1].DeleteOf != nil || o[1].EditOf != nil {
		t.Fatalf("group replay kinds: %+v", o)
	}

	if n := count(t, pool, "operations"); n != 5 {
		t.Fatalf("operations = %d, want 5 (2 seeds + 1 single delete + 2 legs)", n)
	}
	if n := count(t, pool, "transactions"); n != 1 {
		t.Fatalf("transactions = %d, want 1", n)
	}
}

// IT-008: same key + a different item → ErrPayloadConflict: the guard changed
// or removed, another target, an edit of the same target, a regular item, and a
// key reused by another owner (owner is part of the hash).
func TestInsertDeleteSameKeyDifferentItemConflicts(t *testing.T) {
	pool := dbtest.NewSchema(t)
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)
	theirs := seedOp(t, pool, 3, 8, "cash", -1500)

	if _, err := insert(t, pool, deleteOf(4, a, i32(1))); err != nil {
		t.Fatalf("first: %v", err)
	}
	variants := map[string]ledger.InsertOp{
		"guard changed":          {OwnerID: 7, DeleteOf: &a, ExpectedRevision: i32(2)},
		"guard removed":          {OwnerID: 7, DeleteOf: &a},
		"other target":           {OwnerID: 7, DeleteOf: &b, ExpectedRevision: i32(1)},
		"edit of same target":    {OwnerID: 7, EditOf: &a, Amount: -1500, ExpectedRevision: i32(1)},
		"regular item, same key": {OwnerID: 7, ExternalID: "cash", Amount: -1500, EffectiveAt: t1},
		"other owner, own op":    {OwnerID: 8, DeleteOf: &theirs, ExpectedRevision: i32(1)},
	}
	for name, op := range variants {
		t.Run(name, func(t *testing.T) {
			req := ledger.InsertRequest{IdempotencyKey: key(4), Operations: []ledger.InsertOp{op}}
			if _, err := insert(t, pool, req); !errors.Is(err, ledger.ErrPayloadConflict) {
				t.Fatalf("err = %v, want ErrPayloadConflict", err)
			}
		})
	}
	// The reverse direction too: a key first used by a bare delete conflicts
	// with the guarded form.
	if _, err := insert(t, pool, deleteOf(5, b, nil)); err != nil {
		t.Fatalf("bare: %v", err)
	}
	if _, err := insert(t, pool, deleteOf(5, b, i32(1))); !errors.Is(err, ledger.ErrPayloadConflict) {
		t.Fatalf("bare then guarded: err = %v, want ErrPayloadConflict", err)
	}
	if n := count(t, pool, "operations"); n != 5 {
		t.Fatalf("operations = %d, want 5 (3 seeds + 2 deletes)", n)
	}
}

// allow_deletes = false refuses new delete registrations (singles and groups
// with a delete leg) but leaves edits and plain inserts untouched; allow_edits
// = false does not refuse deletes. Both flags are read per request.
func TestInsertDeletesDisabledByConfigIndependently(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)
	if _, err := pool.Exec(ctx, `UPDATE config SET allow_deletes = false`); err != nil {
		t.Fatalf("disable deletes: %v", err)
	}

	if _, err := insert(t, pool, deleteOf(3, a, nil)); !errors.Is(err, ledger.ErrDeletesDisabled) {
		t.Fatalf("single: err = %v, want ErrDeletesDisabled", err)
	}
	_, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(4),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, EditOf: &b, Amount: -200},
			{OwnerID: 7, DeleteOf: &a},
		},
	})
	if !errors.Is(err, ledger.ErrDeletesDisabled) {
		t.Fatalf("group: err = %v, want ErrDeletesDisabled", err)
	}
	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(5),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &b, Amount: -200}},
	}); err != nil {
		t.Fatalf("edit with deletes disabled: %v", err)
	}
	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(6),
		Operations:     []ledger.InsertOp{{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1}},
	}); err != nil {
		t.Fatalf("plain insert with deletes disabled: %v", err)
	}
	if n := count(t, pool, "operations"); n != 4 {
		t.Fatalf("operations = %d, want 4", n)
	}

	// Independent: edits off, deletes back on → deletes register, edits do not.
	if _, err := pool.Exec(ctx, `UPDATE config SET allow_deletes = true, allow_edits = false`); err != nil {
		t.Fatalf("flip flags: %v", err)
	}
	if _, err := insert(t, pool, deleteOf(7, a, nil)); err != nil {
		t.Fatalf("delete with edits disabled: %v", err)
	}
	if _, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(8),
		Operations:     []ledger.InsertOp{{OwnerID: 7, EditOf: &b, Amount: -100}},
	}); !errors.Is(err, ledger.ErrEditsDisabled) {
		t.Fatalf("edit with edits disabled: err = %v, want ErrEditsDisabled", err)
	}
}

// Query core: GetOperation and GetTransaction legs report a delete registration
// under DeleteOf (EditOf nil) with the informational copy, a plain edit under
// EditOf (DeleteOf nil), and a DELETED regular row (stood in for here — task 03
// makes the leader write it) with its last values, DeletedAt and DeletedBy.
func TestGetOperationReadsDeleteFields(t *testing.T) {
	pool := dbtest.NewSchema(t)
	ctx := context.Background()
	a := seedOp(t, pool, 1, 7, "cash", -1500)
	b := seedOp(t, pool, 2, 7, "cash", -300)

	group, err := insert(t, pool, ledger.InsertRequest{
		IdempotencyKey: key(3),
		Operations: []ledger.InsertOp{
			{OwnerID: 7, DeleteOf: &a, ExpectedRevision: i32(1)},
			{OwnerID: 7, EditOf: &b, Amount: -200},
		},
	})
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	delID, editID := group.Operations[0].ID, group.Operations[1].ID

	del, err := ledger.GetOperation(ctx, pool, 7, delID)
	if err != nil {
		t.Fatalf("get delete: %v", err)
	}
	if del.Status != model.OpPending || del.DeleteOf == nil || *del.DeleteOf != a || del.EditOf != nil ||
		del.ExpectedRevision == nil || *del.ExpectedRevision != 1 || del.Account != "cash" || del.Amount != -1500 ||
		!del.EffectiveAt.Equal(t1) || del.DeletedAt != nil || del.DeletedBy != nil {
		t.Fatalf("delete registration outcome: %+v", del)
	}
	edit, err := ledger.GetOperation(ctx, pool, 7, editID)
	if err != nil {
		t.Fatalf("get edit: %v", err)
	}
	if edit.EditOf == nil || *edit.EditOf != b || edit.DeleteOf != nil || edit.DeletedAt != nil || edit.DeletedBy != nil {
		t.Fatalf("edit registration outcome: %+v", edit)
	}
	before, err := ledger.GetOperation(ctx, pool, 7, a)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if before.Status != model.OpPending || before.DeleteOf != nil || before.EditOf != nil || before.DeletedAt != nil || before.DeletedBy != nil {
		t.Fatalf("undecided target outcome: %+v", before)
	}

	// Stand in for the leader's decision (task 03): delete APPLIED, target DELETED.
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'APPLIED', confirmed_at = now() WHERE id = $1`, delID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET status = 'DELETED', deleted_by = $2, deleted_at = now() WHERE id = $1`, a, delID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	after, err := ledger.GetOperation(ctx, pool, 7, a)
	if err != nil {
		t.Fatalf("get deleted target: %v", err)
	}
	if after.Status != model.OpDeleted || after.DeletedAt == nil || after.DeletedBy == nil || *after.DeletedBy != delID ||
		after.Amount != -1500 || after.Account != "cash" || after.Revision != 1 || after.DeleteOf != nil || after.EditOf != nil {
		t.Fatalf("deleted target outcome: %+v", after)
	}

	legs, err := ledger.GetTransaction(ctx, pool, 7, *group.TransactionID)
	if err != nil {
		t.Fatalf("get transaction: %v", err)
	}
	if len(legs.Operations) != 2 {
		t.Fatalf("legs = %d, want 2", len(legs.Operations))
	}
	l0, l1 := legs.Operations[0], legs.Operations[1]
	if l0.ID != delID || l0.Status != model.OpApplied || l0.DeleteOf == nil || *l0.DeleteOf != a || l0.EditOf != nil {
		t.Fatalf("delete leg: %+v", l0)
	}
	if l1.ID != editID || l1.EditOf == nil || *l1.EditOf != b || l1.DeleteOf != nil {
		t.Fatalf("edit leg: %+v", l1)
	}
	// Owner scoping is unchanged: another owner sees nothing.
	if _, err := ledger.GetOperation(ctx, pool, 8, delID); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("foreign read: err = %v, want ErrNotFound", err)
	}
}
