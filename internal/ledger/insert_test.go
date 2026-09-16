package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/model"
)

var (
	t1     = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	t2     = time.Date(2026, 8, 23, 9, 30, 0, 0, time.UTC)
	target = editTarget{AccountID: 11, Amount: -1500, EffectiveAt: t1}
)

func i64(v int64) *int64 { return &v }
func i32(v int32) *int32 { return &v }

// UT-003: an edit that sets only the amount keeps the target's account and
// effective_at.
func TestResolveEditItemFillsOmittedFields(t *testing.T) {
	got := resolveEditItem(InsertOp{OwnerID: 7, EditOf: i64(41), Amount: -1200}, target)
	want := writeOp{OwnerID: 7, AccountID: 11, Amount: -1200, EffectiveAt: t1, EditOf: got.EditOf}
	if got != want {
		t.Fatalf("resolveEditItem = %+v, want %+v", got, want)
	}
}

// UT-004: an edit that sets only the account keeps amount and effective_at and
// leaves the new account to the on-demand upsert (AccountID 0, ExternalID set).
func TestResolveEditItemChangesAccountOnly(t *testing.T) {
	got := resolveEditItem(InsertOp{OwnerID: 7, EditOf: i64(41), ExternalID: "wallet"}, target)
	want := writeOp{OwnerID: 7, ExternalID: "wallet", Amount: -1500, EffectiveAt: t1, EditOf: got.EditOf}
	if got != want {
		t.Fatalf("resolveEditItem = %+v, want %+v", got, want)
	}
}

func TestResolveEditItemKeepsExplicitFieldsAndGuard(t *testing.T) {
	got := resolveEditItem(InsertOp{OwnerID: 7, EditOf: i64(41), Amount: 5, EffectiveAt: t2, ExpectedRevision: i32(3)}, target)
	if got.Amount != 5 || !got.EffectiveAt.Equal(t2) || got.AccountID != 11 || *got.ExpectedRevision != 3 || *got.EditOf != 41 {
		t.Fatalf("explicit fields not preserved: %+v", got)
	}
}

// Structural edit-class checks fire from the request alone, in the documented
// order. Edit cases as before; delete cases are UT-020 (a delete item with any
// extra field → ErrDeleteWithFields; expected_revision 0 → ErrInvalidExpectedRevision)
// and UT-022 (duplicate targets across kinds → ErrDuplicateEditTarget naming 41).
func TestValidateTargetItems(t *testing.T) {
	cases := []struct {
		name       string
		ops        []InsertOp
		want       error
		hasEdits   bool
		hasDeletes bool
	}{
		{"no edits", []InsertOp{{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: t1}}, nil, false, false},
		{"valid edit", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1}}, nil, true, false},
		{"reversal on edit", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ReversalOf: i64(9)}}, ErrEditWithReversal, true, false},
		{"expected_revision 0", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ExpectedRevision: i32(0)}}, ErrInvalidExpectedRevision, true, false},
		{"expected_revision 1 ok", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ExpectedRevision: i32(1)}}, nil, true, false},
		{"changes nothing", []InsertOp{{OwnerID: 1, EditOf: i64(41)}}, ErrEditChangesNothing, true, false},
		{"guard alone changes nothing", []InsertOp{{OwnerID: 1, EditOf: i64(41), ExpectedRevision: i32(2)}}, ErrEditChangesNothing, true, false},
		{"duplicate target", []InsertOp{
			{OwnerID: 1, EditOf: i64(41), Amount: 1},
			{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: t1},
			{OwnerID: 1, EditOf: i64(41), EffectiveAt: t2},
		}, ErrDuplicateEditTarget, true, false},
		{"distinct targets", []InsertOp{
			{OwnerID: 1, EditOf: i64(41), Amount: 1},
			{OwnerID: 1, EditOf: i64(42), Amount: 1},
		}, nil, true, false},

		// Deletes (UT-020).
		{"valid delete", []InsertOp{{OwnerID: 1, DeleteOf: i64(41)}}, nil, false, true},
		{"delete with guard", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), ExpectedRevision: i32(1)}}, nil, false, true},
		{"delete with account", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), ExternalID: "a"}}, ErrDeleteWithFields, false, true},
		{"delete with amount", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), Amount: -1}}, ErrDeleteWithFields, false, true},
		{"delete with effective_at", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), EffectiveAt: t1}}, ErrDeleteWithFields, false, true},
		{"delete with reversal_of", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), ReversalOf: i64(9)}}, ErrDeleteWithFields, false, true},
		{"delete with edit_of", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), EditOf: i64(41)}}, ErrDeleteWithFields, false, true},
		{"delete expected_revision 0", []InsertOp{{OwnerID: 1, DeleteOf: i64(41), ExpectedRevision: i32(0)}}, ErrInvalidExpectedRevision, false, true},
		{"delete and edit of distinct targets", []InsertOp{
			{OwnerID: 1, DeleteOf: i64(41)},
			{OwnerID: 1, EditOf: i64(42), Amount: 1},
			{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: t1},
		}, nil, true, true},

		// Duplicate targets across kinds share one seen set (UT-022).
		{"delete twice", []InsertOp{{OwnerID: 1, DeleteOf: i64(41)}, {OwnerID: 1, DeleteOf: i64(41)}}, ErrDuplicateEditTarget, false, true},
		{"delete then edit", []InsertOp{{OwnerID: 1, DeleteOf: i64(41)}, {OwnerID: 1, EditOf: i64(41), Amount: -1}}, ErrDuplicateEditTarget, true, true},
		{"edit then delete", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: -1}, {OwnerID: 1, DeleteOf: i64(41)}}, ErrDuplicateEditTarget, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hasEdits, hasDeletes, err := validateTargetItems(tc.ops)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if hasEdits != tc.hasEdits || hasDeletes != tc.hasDeletes {
				t.Fatalf("hasEdits, hasDeletes = %v, %v, want %v, %v", hasEdits, hasDeletes, tc.hasEdits, tc.hasDeletes)
			}
			if errors.Is(err, ErrDuplicateEditTarget) {
				var te *TargetError
				if !errors.As(err, &te) || te.Target != 41 {
					t.Fatalf("duplicate must name target 41 through TargetError, got %v", err)
				}
			}
		})
	}
}

// UT-022 detail: the duplicate error names the *second* item's kind, so a
// transport can say "delete target 41" or "edit target 41" without string
// matching.
func TestDuplicateTargetCarriesKind(t *testing.T) {
	cases := []struct {
		name string
		ops  []InsertOp
		kind TargetKind
	}{
		{"delete after edit", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: -1}, {OwnerID: 1, DeleteOf: i64(41)}}, TargetDelete},
		{"edit after delete", []InsertOp{{OwnerID: 1, DeleteOf: i64(41)}, {OwnerID: 1, EditOf: i64(41), Amount: -1}}, TargetEdit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := validateTargetItems(tc.ops)
			var te *TargetError
			if !errors.As(err, &te) {
				t.Fatalf("err = %v, want *TargetError", err)
			}
			if te.Kind != tc.kind || te.Target != 41 || !errors.Is(te, ErrDuplicateEditTarget) {
				t.Fatalf("TargetError = %+v, want kind %s of 41 wrapping ErrDuplicateEditTarget", te, tc.kind)
			}
			if got, want := err.Error(), ErrDuplicateEditTarget.Error()+": "+string(tc.kind)+" of 41"; got != want {
				t.Fatalf("Error() = %q, want %q", got, want)
			}
		})
	}
}

// UT-021: a delete row carries the target's current state verbatim as its
// informational copy, EditOf = target, IsDelete set, the guard passed through.
func TestResolveDeleteItemCopiesTarget(t *testing.T) {
	got := resolveDeleteItem(InsertOp{OwnerID: 7, DeleteOf: i64(41), ExpectedRevision: i32(2)}, target)
	want := writeOp{OwnerID: 7, AccountID: 11, Amount: -1500, EffectiveAt: t1, EditOf: got.EditOf, IsDelete: true, ExpectedRevision: got.ExpectedRevision}
	if got != want || *got.EditOf != 41 || *got.ExpectedRevision != 2 {
		t.Fatalf("resolveDeleteItem = %+v, want %+v (EditOf 41, ExpectedRevision 2)", got, want)
	}
	if bare := resolveDeleteItem(InsertOp{OwnerID: 7, DeleteOf: i64(41)}, target); bare.ExpectedRevision != nil || !bare.IsDelete {
		t.Fatalf("bare delete = %+v, want nil guard and IsDelete", bare)
	}
	// The fresh-insert outcome reports the target as DeleteOf, never EditOf.
	if o := got.outcome(63); o.ID != 63 || o.Status != string(model.OpPending) || o.EditOf != nil || o.DeleteOf == nil || *o.DeleteOf != 41 {
		t.Fatalf("delete outcome = %+v", o)
	}
	edit := resolveEditItem(InsertOp{OwnerID: 7, EditOf: i64(41), Amount: -1200}, target)
	if o := edit.outcome(57); o.DeleteOf != nil || o.EditOf == nil || *o.EditOf != 41 {
		t.Fatalf("edit outcome = %+v", o)
	}
}

// The payload is hashed as sent: omitted edit fields stay nil, so "omit amount"
// and "send the target's current amount" are different payloads, and the
// canonical form of a regular item is unchanged.
func TestCanonicalOpsHashesEditsAsSent(t *testing.T) {
	omitted := canonicalOps([]InsertOp{{OwnerID: 7, EditOf: i64(41), EffectiveAt: t2}})
	explicit := canonicalOps([]InsertOp{{OwnerID: 7, EditOf: i64(41), Amount: -1500, EffectiveAt: t2}})

	e := omitted[0].(model.CanonicalEditOp)
	if e.OwnerID != 7 || e.EditOf != 41 || e.Account != nil || e.Amount != nil || e.EffectiveAt == nil || !e.EffectiveAt.Equal(t2) || e.ExpectedRevision != nil {
		t.Fatalf("unexpected canonical edit: %+v", e)
	}
	h1, err := model.HashPayload(omitted)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := model.HashPayload(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if string(h1) == string(h2) {
		t.Fatal("omitted and explicit amount must hash differently (as-sent semantics)")
	}

	regular := canonicalOps([]InsertOp{{OwnerID: 7, ExternalID: "cash", Amount: 5, EffectiveAt: t1, ReversalOf: i64(3)}})
	if _, ok := regular[0].(model.CanonicalOp); !ok {
		t.Fatalf("regular item must stay a CanonicalOp, got %T", regular[0])
	}
}

// UT-002: a delete item hashes as a CanonicalDeleteOp covering exactly owner,
// target and guard — so adding a guard or changing the owner changes the hash,
// and a delete never hashes like an edit of the same target. DELETE
// /operations/{id} and a single {"delete_of": id} item are the same InsertOp,
// hence the same canonical item.
func TestCanonicalOpsHashesDeletesAsSent(t *testing.T) {
	hashOf := func(ops ...InsertOp) string {
		t.Helper()
		h, err := model.HashPayload(canonicalOps(ops))
		if err != nil {
			t.Fatal(err)
		}
		return string(h)
	}
	bare := canonicalOps([]InsertOp{{OwnerID: 7, DeleteOf: i64(41)}})
	d, ok := bare[0].(model.CanonicalDeleteOp)
	if !ok || d.OwnerID != 7 || d.DeleteOf != 41 || d.ExpectedRevision != nil {
		t.Fatalf("unexpected canonical delete: %#v", bare[0])
	}
	guarded := canonicalOps([]InsertOp{{OwnerID: 7, DeleteOf: i64(41), ExpectedRevision: i32(2)}})
	if g := guarded[0].(model.CanonicalDeleteOp); g.ExpectedRevision == nil || *g.ExpectedRevision != 2 {
		t.Fatalf("guard not carried: %#v", guarded[0])
	}

	base := hashOf(InsertOp{OwnerID: 7, DeleteOf: i64(41)})
	if base != hashOf(InsertOp{OwnerID: 7, DeleteOf: i64(41)}) {
		t.Fatal("the same delete item must hash identically")
	}
	if base == hashOf(InsertOp{OwnerID: 7, DeleteOf: i64(41), ExpectedRevision: i32(2)}) {
		t.Fatal("adding expected_revision must change the hash")
	}
	if base == hashOf(InsertOp{OwnerID: 8, DeleteOf: i64(41)}) {
		t.Fatal("the owner is part of the hash")
	}
	if base == hashOf(InsertOp{OwnerID: 7, EditOf: i64(41), Amount: -1}) {
		t.Fatal("a delete must not hash like an edit of the same target")
	}
	// Frozen encodings: neither a regular nor an edit item's hash moves because
	// a delete kind exists (a group mixing all three is still deterministic).
	mixed := canonicalOps([]InsertOp{
		{OwnerID: 7, DeleteOf: i64(41)},
		{OwnerID: 7, EditOf: i64(42), Amount: -700},
		{OwnerID: 7, ExternalID: "cash", Amount: 300, EffectiveAt: t1},
	})
	if _, ok := mixed[1].(model.CanonicalEditOp); !ok {
		t.Fatalf("edit item must stay a CanonicalEditOp, got %T", mixed[1])
	}
	if _, ok := mixed[2].(model.CanonicalOp); !ok {
		t.Fatalf("regular item must stay a CanonicalOp, got %T", mixed[2])
	}
}
