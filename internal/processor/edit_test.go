package processor

import (
	"errors"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/model"
)

// Unit tests for the pure pieces of the single-edit path (ADR-0010): virtual-leg
// netting against the §6 check, the target-state rules, and same-bucket snapshot
// coalescing. The DB-backed path is covered by edit_itest_test.go.

// UT-010: virtual-leg netting. Target {cash, +100} CONFIRMED with the account at
// balance 10 (a −90 spend confirmed after it), min 0; an edit to +20 expands to
// legs −100 and +20 → net −80, so violates(10 − 80, min 0) reports the min side
// short by 70. At balance 90 the same net lands on 10 and passes.
func TestNetLegsAgainstLimits(t *testing.T) {
	const cash = int64(7)
	minZero := int64(0)
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	target := targetState{AccountID: cash, Amount: 100, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}
	edit := pendingOp{ID: 99, AccountID: cash, Amount: 20, EffectiveAt: t1, EditOf: ptr64(41)}

	ids, net := netLegs(expandEdit(target, edit))
	if len(ids) != 1 || ids[0] != cash {
		t.Fatalf("involved accounts = %v, want [%d]", ids, cash)
	}
	if net[cash] != -80 {
		t.Fatalf("net on cash = %d, want -80", net[cash])
	}

	side, shortfall, bad := violates(10+net[cash], &minZero, nil)
	if !bad || side != model.LimitMin || shortfall != 70 {
		t.Fatalf("violates(10-80, min 0) = (%q, %d, %v), want (min, 70, true)", side, shortfall, bad)
	}
	if _, _, bad := violates(90+net[cash], &minZero, nil); bad {
		t.Fatalf("violates(90-80, min 0) reported a violation")
	}
}

// Netting across an account move: −old lands on the old account and +new on the
// new one, each its own net, ids ascending.
func TestNetLegsAccountMove(t *testing.T) {
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	target := targetState{AccountID: 9, Amount: -300, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}
	edit := pendingOp{ID: 50, AccountID: 3, Amount: -300, EffectiveAt: t1, EditOf: ptr64(42)}

	ids, net := netLegs(expandEdit(target, edit))
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 9 {
		t.Fatalf("involved accounts = %v, want [3 9]", ids)
	}
	if net[9] != 300 || net[3] != -300 {
		t.Fatalf("nets = old %d / new %d, want +300 / -300", net[9], net[3])
	}
}

// UT-011: mixed-group netting through expandGroup. Legs {edit 41: cash −1500 →
// −1200} and {new: cash +300} net to +600 on cash (+1500 −1200 +300); a second
// group {edit 42: cash → wallet, −300 unchanged} nets +300 on cash and −300 on
// wallet — the account move is two legs on two accounts.
func TestExpandGroupMixedNetting(t *testing.T) {
	const cash, wallet = int64(1), int64(2)
	t1 := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

	legs := []groupLeg{
		{ID: 58, AccountID: cash, Amount: -1200, EffectiveAt: t1, EditOf: ptr64(41)},
		{ID: 60, AccountID: cash, Amount: 300, EffectiveAt: t2},
	}
	edits := []groupEdit{{leg: legs[0], target: targetState{AccountID: cash, Amount: -1500, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}}}
	virtual := expandGroup(legs, edits)
	if len(virtual) != 3 || virtual[0].Amount != 1500 || virtual[1].Amount != -1200 || virtual[2].Amount != 300 {
		t.Fatalf("virtual legs = %+v, want [+1500 −1200 +300]", virtual)
	}
	ids, net := netLegs(virtual)
	if len(ids) != 1 || ids[0] != cash || net[cash] != 600 {
		t.Fatalf("mixed group: ids=%v net=%v, want cash +600", ids, net)
	}

	move := []groupLeg{{ID: 59, AccountID: wallet, Amount: -300, EffectiveAt: t1, EditOf: ptr64(42)}}
	moveEdits := []groupEdit{{leg: move[0], target: targetState{AccountID: cash, Amount: -300, EffectiveAt: t1, Status: "CONFIRMED", Revision: 1}}}
	ids, net = netLegs(expandGroup(move, moveEdits))
	if len(ids) != 2 || ids[0] != cash || ids[1] != wallet || net[cash] != 300 || net[wallet] != -300 {
		t.Fatalf("account move: ids=%v net=%v, want cash +300 / wallet −300", ids, net)
	}

	// A group without edits expands one leg per leg, unchanged.
	plain := expandGroup([]groupLeg{{ID: 1, AccountID: cash, Amount: 5, EffectiveAt: t1}}, nil)
	if len(plain) != 1 || plain[0] != (virtualLeg{AccountID: cash, Amount: 5, EffectiveAt: t1}) {
		t.Fatalf("plain group: %+v", plain)
	}
}

// UT-012: checkTarget. Revision 3 with expected_revision 2 → STALE_REVISION{41, 2,
// 3}; expected 2 against revision 2 passes; no guard passes; PENDING defers;
// INVALID → TARGET_NOT_EDITABLE{41}.
func TestCheckTarget(t *testing.T) {
	confirmed := func(rev int32) targetState {
		return targetState{Status: string(model.OpConfirmed), Revision: rev}
	}

	rej, err := checkTarget(41, confirmed(3), ptr32(2))
	if err != nil || rej == nil {
		t.Fatalf("stale: rej=%v err=%v, want a rejection", rej, err)
	}
	if rej.Code != model.ReasonStaleRevision || rej.OperationID == nil || *rej.OperationID != 41 ||
		rej.ExpectedRevision == nil || *rej.ExpectedRevision != 2 ||
		rej.ActualRevision == nil || *rej.ActualRevision != 3 {
		t.Fatalf("stale rejection = %+v, want STALE_REVISION{41, 2, 3}", *rej)
	}

	if rej, err := checkTarget(41, confirmed(2), ptr32(2)); err != nil || rej != nil {
		t.Fatalf("matching expected_revision: rej=%v err=%v, want pass", rej, err)
	}
	if rej, err := checkTarget(41, confirmed(7), nil); err != nil || rej != nil {
		t.Fatalf("no guard: rej=%v err=%v, want pass", rej, err)
	}

	if _, err := checkTarget(41, targetState{Status: string(model.OpPending), Revision: 1}, nil); !errors.Is(err, errDeferred) {
		t.Fatalf("PENDING target: err=%v, want errDeferred", err)
	}

	rej, err = checkTarget(41, targetState{Status: string(model.OpInvalid), Revision: 1}, ptr32(1))
	if err != nil || rej == nil || rej.Code != model.ReasonTargetNotEditable || rej.OperationID == nil || *rej.OperationID != 41 {
		t.Fatalf("INVALID target: rej=%+v err=%v, want TARGET_NOT_EDITABLE{41}", rej, err)
	}
	if rej.ExpectedRevision != nil || rej.ActualRevision != nil {
		t.Fatalf("TARGET_NOT_EDITABLE must not carry revisions: %+v", *rej)
	}
}

// UT-013: coalesceSnapshotLegs. Same account and UTC day, −1500 → −1200 → one leg
// of +300 at the new instant; equal amounts with a time move within the day →
// zero legs; 23:30Z → 00:10Z next day → two legs; account change → two legs. UTC
// bucketing is asserted with a non-UTC input offset.
func TestCoalesceSnapshotLegs(t *testing.T) {
	const cash = int64(1)
	tA := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 8, 22, 15, 30, 0, 0, time.UTC)

	got := coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: tA},
		virtualLeg{AccountID: cash, Amount: -1200, EffectiveAt: tB},
	)
	if len(got) != 1 || got[0].AccountID != cash || got[0].Amount != 300 || !got[0].EffectiveAt.Equal(tB) {
		t.Fatalf("same bucket: %+v, want one leg of +300 at %v", got, tB)
	}

	got = coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: tA},
		virtualLeg{AccountID: cash, Amount: -1500, EffectiveAt: tB},
	)
	if len(got) != 0 {
		t.Fatalf("zero delta same bucket: %+v, want no legs", got)
	}

	late := time.Date(2026, 8, 22, 23, 30, 0, 0, time.UTC)
	next := time.Date(2026, 8, 23, 0, 10, 0, 0, time.UTC)
	got = coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: late},
		virtualLeg{AccountID: cash, Amount: -1500, EffectiveAt: next},
	)
	if len(got) != 2 || got[0].Amount != 1500 || got[1].Amount != -1500 {
		t.Fatalf("day crossing: %+v, want both legs", got)
	}

	got = coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: tA},
		virtualLeg{AccountID: 2, Amount: -1500, EffectiveAt: tA},
	)
	if len(got) != 2 || got[0].AccountID != cash || got[1].AccountID != 2 {
		t.Fatalf("account change: %+v, want both legs", got)
	}

	// UTC bucketing: 2026-08-23T01:00+02:00 is 2026-08-22T23:00Z — the same UTC day
	// as tA even though its local calendar day differs.
	plus2 := time.FixedZone("plus2", 2*3600)
	local := time.Date(2026, 8, 23, 1, 0, 0, 0, plus2)
	got = coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: tA},
		virtualLeg{AccountID: cash, Amount: -1200, EffectiveAt: local},
	)
	if len(got) != 1 || got[0].Amount != 300 {
		t.Fatalf("non-UTC offset on the same UTC day: %+v, want one coalesced leg", got)
	}
	// ...and 2026-08-22T01:00−02:00 is 03:00Z of the 22nd, while 2026-08-23T00:30Z
	// in −02:00 reads as the 22nd locally but is the 23rd in UTC — two buckets.
	minus2 := time.FixedZone("minus2", -2*3600)
	got = coalesceSnapshotLegs(
		virtualLeg{AccountID: cash, Amount: 1500, EffectiveAt: time.Date(2026, 8, 22, 1, 0, 0, 0, minus2)},
		virtualLeg{AccountID: cash, Amount: -1200, EffectiveAt: time.Date(2026, 8, 22, 22, 30, 0, 0, minus2)},
	)
	if len(got) != 2 {
		t.Fatalf("non-UTC offset crossing the UTC day: %+v, want both legs", got)
	}
}

func ptr64(v int64) *int64 { return &v }
func ptr32(v int32) *int32 { return &v }
