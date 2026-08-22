package simtest

import (
	"math/rand"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/model"
)

// TestReferenceSingleDecisionEvolvesBalanceWithinLimits is the M5 §15 property
// test: across randomized schedules of single inserts against accounts with
// tight, loose, and unbounded limits, deciding each single in registration order
// must evolve confirmed balances exactly by the accepted amounts and never push a
// balance outside its limits. G1 is asserted after every single decision. Every
// failure prints its seed.
func TestReferenceSingleDecisionEvolvesBalanceWithinLimits(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 300; iter++ {
		seed := time.Now().UnixNano() + int64(iter)
		if !runSingleDecisionProperty(t, seed) {
			return // the helper already reported the failing seed
		}
	}
}

func runSingleDecisionProperty(t *testing.T, seed int64) bool {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	m := NewModel()

	const owner int64 = 1
	accts := []string{"tight", "loose", "unbounded", "floor", "ceiling"}
	// Give each account limits that bracket 0 (so the empty account satisfies G1),
	// spanning tight/loose/one-sided/unbounded shapes.
	for _, name := range accts {
		var lo, hi *int64
		switch name {
		case "tight":
			lo, hi = ptr(-50), ptr(50)
		case "loose":
			lo, hi = ptr(-100000), ptr(100000)
		case "unbounded":
			// both nil
		case "floor":
			lo = ptr(-200) // max unbounded
		case "ceiling":
			hi = ptr(200) // min unbounded
		}
		if err := m.SetLimits(owner, name, lo, hi); err != nil {
			t.Errorf("seed %d: SetLimits(%s): %v", seed, name, err)
			return false
		}
	}

	nextKey := 0
	when := time.Unix(0, 0).UTC()

	for step := 0; step < 200; step++ {
		// Insert a single onto a random account, then decide the oldest pending
		// single and check G1. Interleaving insert and decide exercises the
		// balance evolving under a moving sequence of decisions.
		name := accts[rng.Intn(len(accts))]
		amt := int64(rng.Intn(161) - 80) // -80..80: routinely crosses the tight bounds
		if amt == 0 {
			amt = 1
		}
		if _, err := m.Insert(api.InsertRequest{
			IdempotencyKey: uuid(nextKey),
			Operations:     []api.InsertOp{{OwnerID: owner, ExternalID: name, Amount: amt, EffectiveAt: when}},
		}); err != nil {
			t.Errorf("seed %d: insert: %v", seed, err)
			return false
		}
		nextKey++

		if !decideOneAndCheck(t, m, seed) {
			return false
		}
	}

	// Drain any remaining pending singles, checking G1 after each.
	for pendingSingleCount(m) > 0 {
		if !decideOneAndCheck(t, m, seed) {
			return false
		}
	}
	return true
}

// decideOneAndCheck decides the oldest pending single, verifies the decision
// matches the validation rule against the balance that held just before it, and
// asserts G1 afterwards.
func decideOneAndCheck(t *testing.T, m *Model, seed int64) bool {
	t.Helper()
	// Snapshot the target op and its account balance before deciding.
	target, ok := m.peekPendingSingle()
	if !ok {
		return true // nothing to decide this step
	}
	acct := m.accountsByID[target.AccountID]
	before := acct.Balance
	wantBal := before + target.Amount
	wantAccept := withinLimits(wantBal, acct.Min, acct.Max)

	d, got := m.ProcessNext()
	if !got {
		t.Errorf("seed %d: peek found a pending single but ProcessNext returned none", seed)
		return false
	}
	if d.OpID != target.ID {
		t.Errorf("seed %d: decided op %d, expected oldest pending %d (registration order)", seed, d.OpID, target.ID)
		return false
	}

	switch {
	case wantAccept:
		if d.Status != model.OpConfirmed {
			t.Errorf("seed %d: op %d should CONFIRM (%d in [%v,%v]) but got %s", seed, d.OpID, wantBal, deref(acct.Min), deref(acct.Max), d.Status)
			return false
		}
		if acct.Balance != wantBal {
			t.Errorf("seed %d: op %d confirmed but balance %d != expected %d", seed, d.OpID, acct.Balance, wantBal)
			return false
		}
	default:
		if d.Status != model.OpInvalid {
			t.Errorf("seed %d: op %d should INVALIDATE (%d outside [%v,%v]) but got %s", seed, d.OpID, wantBal, deref(acct.Min), deref(acct.Max), d.Status)
			return false
		}
		if acct.Balance != before {
			t.Errorf("seed %d: op %d rejected but balance changed %d -> %d", seed, d.OpID, before, acct.Balance)
			return false
		}
		if d.Reason == nil || d.Reason.Code != model.ReasonLimitViolated || d.Reason.Shortfall <= 0 {
			t.Errorf("seed %d: op %d rejection detail missing/invalid: %+v", seed, d.OpID, d.Reason)
			return false
		}
	}

	if err := m.CheckG1(); err != nil {
		t.Errorf("seed %d: after deciding op %d: %v", seed, d.OpID, err)
		return false
	}
	return true
}

// peekPendingSingle returns the lowest-id pending single without deciding it.
func (m *Model) peekPendingSingle() (*refOp, bool) {
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || op.Status != model.OpPending || op.TransactionID != nil {
			continue
		}
		return op, true
	}
	return nil, false
}

func pendingSingleCount(m *Model) int {
	n := 0
	for _, op := range m.ops {
		if op.Status == model.OpPending && op.TransactionID == nil {
			n++
		}
	}
	return n
}

// TestReferenceSetLimitsRejectsUnsatisfiable checks the §6 limit-change rule the
// reference model encodes: limits the confirmed balance already violates are
// refused.
func TestReferenceSetLimitsRejectsUnsatisfiable(t *testing.T) {
	t.Parallel()
	m := NewModel()
	const owner int64 = 1

	// Confirm a +100 single so the account balance is 100.
	if _, err := m.Insert(api.InsertRequest{
		IdempotencyKey: uuid(1),
		Operations:     []api.InsertOp{{OwnerID: owner, ExternalID: "a", Amount: 100, EffectiveAt: time.Unix(0, 0).UTC()}},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if d := m.ProcessAll(); len(d) != 1 || d[0].Status != model.OpConfirmed {
		t.Fatalf("expected one CONFIRMED decision, got %+v", d)
	}
	if got := m.AccountBalance(owner, "a"); got != 100 {
		t.Fatalf("balance = %d, want 100", got)
	}

	if err := m.SetLimits(owner, "a", ptr(200), nil); err == nil {
		t.Fatalf("expected SetLimits(min=200) to fail against balance 100")
	}
	if err := m.SetLimits(owner, "a", nil, ptr(50)); err == nil {
		t.Fatalf("expected SetLimits(max=50) to fail against balance 100")
	}
	if err := m.SetLimits(owner, "a", ptr(0), ptr(1000)); err != nil {
		t.Fatalf("expected SetLimits(0,1000) to succeed against balance 100: %v", err)
	}
}

func withinLimits(bal int64, minBalance, maxBalance *int64) bool {
	if maxBalance != nil && bal > *maxBalance {
		return false
	}
	if minBalance != nil && bal < *minBalance {
		return false
	}
	return true
}

func ptr(v int64) *int64 { return &v }

func deref(p *int64) any {
	if p == nil {
		return "∞"
	}
	return *p
}
