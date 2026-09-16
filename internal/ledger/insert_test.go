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

// Structural edit checks fire from the request alone, in the documented order.
func TestValidateEditItems(t *testing.T) {
	cases := []struct {
		name string
		ops  []InsertOp
		want error
		has  bool
	}{
		{"no edits", []InsertOp{{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: t1}}, nil, false},
		{"valid edit", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1}}, nil, true},
		{"reversal on edit", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ReversalOf: i64(9)}}, ErrEditWithReversal, true},
		{"expected_revision 0", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ExpectedRevision: i32(0)}}, ErrInvalidExpectedRevision, true},
		{"expected_revision 1 ok", []InsertOp{{OwnerID: 1, EditOf: i64(41), Amount: 1, ExpectedRevision: i32(1)}}, nil, true},
		{"changes nothing", []InsertOp{{OwnerID: 1, EditOf: i64(41)}}, ErrEditChangesNothing, true},
		{"guard alone changes nothing", []InsertOp{{OwnerID: 1, EditOf: i64(41), ExpectedRevision: i32(2)}}, ErrEditChangesNothing, true},
		{"duplicate target", []InsertOp{
			{OwnerID: 1, EditOf: i64(41), Amount: 1},
			{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: t1},
			{OwnerID: 1, EditOf: i64(41), EffectiveAt: t2},
		}, ErrDuplicateEditTarget, true},
		{"distinct targets", []InsertOp{
			{OwnerID: 1, EditOf: i64(41), Amount: 1},
			{OwnerID: 1, EditOf: i64(42), Amount: 1},
		}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			has, err := validateEditItems(tc.ops)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if has != tc.has {
				t.Fatalf("hasEdits = %v, want %v", has, tc.has)
			}
		})
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
