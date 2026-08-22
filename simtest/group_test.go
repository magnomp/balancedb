package simtest

import (
	"math/rand"
	"sort"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/model"
)

// TestReferenceGroupCommit checks a multi-account group commits atomically: every
// account's net is applied, every leg is CONFIRMED, the transaction is COMMITTED.
func TestReferenceGroupCommit(t *testing.T) {
	t.Parallel()
	m := NewModel()
	const owner int64 = 1
	when := time.Unix(0, 0).UTC()
	for _, n := range []string{"a", "b", "c"} {
		mustSetLimits(t, m, owner, n, ptr(-1000), ptr(1000))
	}

	res := mustInsert(t, m, api.InsertRequest{
		IdempotencyKey: uuid(1),
		Operations: []api.InsertOp{
			{OwnerID: owner, ExternalID: "a", Amount: 100, EffectiveAt: when},
			{OwnerID: owner, ExternalID: "b", Amount: -40, EffectiveAt: when},
			{OwnerID: owner, ExternalID: "c", Amount: 25, EffectiveAt: when},
		},
	})
	if res.TransactionID == nil {
		t.Fatalf("group insert returned no transaction id")
	}

	d, ok := m.ProcessNext()
	if !ok || d.TxID == nil {
		t.Fatalf("expected a group decision, got %+v ok=%v", d, ok)
	}
	if d.TxStatus != model.TxCommitted || d.Status != model.OpConfirmed {
		t.Fatalf("group should COMMIT, got tx=%s legs=%s", d.TxStatus, d.Status)
	}
	if m.AccountBalance(owner, "a") != 100 || m.AccountBalance(owner, "b") != -40 || m.AccountBalance(owner, "c") != 25 {
		t.Fatalf("balances = a:%d b:%d c:%d, want 100/-40/25",
			m.AccountBalance(owner, "a"), m.AccountBalance(owner, "b"), m.AccountBalance(owner, "c"))
	}
	if err := m.CheckG1(); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckG2(); err != nil {
		t.Fatal(err)
	}
}

// TestReferenceGroupOneLegFailsRejectsWhole checks that a single offending leg
// rejects the entire group with the offending account and shortfall, and no
// balance moves (G2: all-or-nothing).
func TestReferenceGroupOneLegFailsRejectsWhole(t *testing.T) {
	t.Parallel()
	m := NewModel()
	const owner int64 = 1
	when := time.Unix(0, 0).UTC()
	mustSetLimits(t, m, owner, "a", ptr(-1000), ptr(1000))
	mustSetLimits(t, m, owner, "b", ptr(0), ptr(50)) // b cannot exceed 50

	mustInsert(t, m, api.InsertRequest{
		IdempotencyKey: uuid(1),
		Operations: []api.InsertOp{
			{OwnerID: owner, ExternalID: "a", Amount: 100, EffectiveAt: when}, // fine
			{OwnerID: owner, ExternalID: "b", Amount: 90, EffectiveAt: when},  // 90 > 50 → whole group rejects
		},
	})

	d, ok := m.ProcessNext()
	if !ok || d.TxID == nil {
		t.Fatalf("expected a group decision, got %+v ok=%v", d, ok)
	}
	if d.TxStatus != model.TxRejected || d.Status != model.OpInvalid {
		t.Fatalf("group should REJECT, got tx=%s legs=%s", d.TxStatus, d.Status)
	}
	if d.Reason == nil || d.Reason.Account != "b" || d.Reason.LimitSide != model.LimitMax || d.Reason.Shortfall != 40 {
		t.Fatalf("rejection = %+v, want offending account b/max/shortfall 40", d.Reason)
	}
	if m.AccountBalance(owner, "a") != 0 || m.AccountBalance(owner, "b") != 0 {
		t.Fatalf("rejected group changed balances a:%d b:%d", m.AccountBalance(owner, "a"), m.AccountBalance(owner, "b"))
	}
	if err := m.CheckG1(); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckG2(); err != nil {
		t.Fatal(err)
	}
}

// TestReferenceGroupNetPerAccount checks that two legs on the same account net
// before validation and application: +100 and -30 on one account is a single +70
// net, validated once and applied once.
func TestReferenceGroupNetPerAccount(t *testing.T) {
	t.Parallel()
	m := NewModel()
	const owner int64 = 1
	when := time.Unix(0, 0).UTC()
	// Max 80: the +100 leg alone would violate, but the net +70 is within limits.
	mustSetLimits(t, m, owner, "a", ptr(-1000), ptr(80))

	mustInsert(t, m, api.InsertRequest{
		IdempotencyKey: uuid(1),
		Operations: []api.InsertOp{
			{OwnerID: owner, ExternalID: "a", Amount: 100, EffectiveAt: when},
			{OwnerID: owner, ExternalID: "a", Amount: -30, EffectiveAt: when},
		},
	})

	d, ok := m.ProcessNext()
	if !ok || d.TxStatus != model.TxCommitted {
		t.Fatalf("net +70 within max 80 should COMMIT, got %+v ok=%v", d, ok)
	}
	if got := m.AccountBalance(owner, "a"); got != 70 {
		t.Fatalf("balance = %d, want 70 (net applied once)", got)
	}
}

// TestReferenceGroupNonZeroSum checks a non-zero-sum group (legs that do not net to
// zero across accounts) commits and moves the aggregate balance.
func TestReferenceGroupNonZeroSum(t *testing.T) {
	t.Parallel()
	m := NewModel()
	const owner int64 = 1
	when := time.Unix(0, 0).UTC()
	mustSetLimits(t, m, owner, "a", ptr(-1000), ptr(1000))
	mustSetLimits(t, m, owner, "b", ptr(-1000), ptr(1000))

	mustInsert(t, m, api.InsertRequest{
		IdempotencyKey: uuid(1),
		Operations: []api.InsertOp{
			{OwnerID: owner, ExternalID: "a", Amount: 100, EffectiveAt: when},
			{OwnerID: owner, ExternalID: "b", Amount: 100, EffectiveAt: when},
		},
	})
	if d, ok := m.ProcessNext(); !ok || d.TxStatus != model.TxCommitted {
		t.Fatalf("non-zero-sum group should COMMIT, got %+v ok=%v", d, ok)
	}
	if m.AccountBalance(owner, "a")+m.AccountBalance(owner, "b") != 200 {
		t.Fatalf("aggregate = %d, want 200", m.AccountBalance(owner, "a")+m.AccountBalance(owner, "b"))
	}
}

// TestReferenceGroupDecisionProperty is the M6 §15 property test for groups: across
// randomized schedules interleaving singles and groups (mixed sizes, same-account
// multi-leg, non-zero-sum) against tight/loose/unbounded accounts, each decision in
// registration order must match an independent net-per-account recomputation, and
// G1 and G2 must hold after every decision. Every failure prints its seed.
func TestReferenceGroupDecisionProperty(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 300; iter++ {
		seed := time.Now().UnixNano() + int64(iter)
		if !runGroupDecisionProperty(t, seed) {
			return
		}
	}
}

func runGroupDecisionProperty(t *testing.T, seed int64) bool {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	m := NewModel()
	const owner int64 = 1
	accts := []string{"tight", "loose", "unbounded", "floor", "ceiling"}
	for _, name := range accts {
		var lo, hi *int64
		switch name {
		case "tight":
			lo, hi = ptr(-60), ptr(60)
		case "loose":
			lo, hi = ptr(-100000), ptr(100000)
		case "unbounded":
		case "floor":
			lo = ptr(-300)
		case "ceiling":
			hi = ptr(300)
		}
		if err := m.SetLimits(owner, name, lo, hi); err != nil {
			t.Errorf("seed %d: SetLimits(%s): %v", seed, name, err)
			return false
		}
	}

	nextKey := 0
	when := time.Unix(0, 0).UTC()
	for step := 0; step < 200; step++ {
		nLegs := 1 + rng.Intn(4) // 1..4 legs: singles and groups both
		ops := make([]api.InsertOp, nLegs)
		for i := range ops {
			amt := int64(rng.Intn(121) - 60) // -60..60: routinely crosses tight bounds
			if amt == 0 {
				amt = 1
			}
			ops[i] = api.InsertOp{OwnerID: owner, ExternalID: accts[rng.Intn(len(accts))], Amount: amt, EffectiveAt: when}
		}
		if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(nextKey), Operations: ops}); err != nil {
			t.Errorf("seed %d: insert: %v", seed, err)
			return false
		}
		nextKey++

		if !decideNextAndVerify(t, m, seed) {
			return false
		}
	}
	// Drain the rest.
	for {
		if _, ok := m.peekPending(); !ok {
			return true
		}
		if !decideNextAndVerify(t, m, seed) {
			return false
		}
	}
}

// decideNextAndVerify decides the oldest pending unit (single or group),
// independently recomputes the expected outcome against the balances that held just
// before the decision, and checks G1 + G2 afterwards.
func decideNextAndVerify(t *testing.T, m *Model, seed int64) bool {
	t.Helper()
	target, ok := m.peekPending()
	if !ok {
		return true
	}

	// Snapshot the involved accounts and compute the expected net per account.
	if target.TransactionID == nil {
		return decideOneAndCheck(t, m, seed) // reuse the single-op verifier
	}

	tx := m.txs[*target.TransactionID]
	net := map[int64]int64{}
	before := map[int64]int64{}
	var ids []int64
	for _, legID := range tx.LegIDs {
		op := m.ops[legID]
		if _, seen := net[op.AccountID]; !seen {
			ids = append(ids, op.AccountID)
			before[op.AccountID] = m.accountsByID[op.AccountID].Balance
		}
		net[op.AccountID] += op.Amount
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Independently determine accept/reject and the offending account.
	var wantOffender int64 = -1
	var wantSide model.LimitSide
	var wantShort int64
	for _, id := range ids {
		a := m.accountsByID[id]
		nb := before[id] + net[id]
		if a.Max != nil && nb > *a.Max {
			wantOffender, wantSide, wantShort = id, model.LimitMax, nb-*a.Max
			break
		}
		if a.Min != nil && nb < *a.Min {
			wantOffender, wantSide, wantShort = id, model.LimitMin, *a.Min-nb
			break
		}
	}

	d, got := m.ProcessNext()
	if !got || d.TxID == nil || *d.TxID != tx.ID {
		t.Errorf("seed %d: expected group %d decision, got %+v ok=%v", seed, tx.ID, d, got)
		return false
	}

	if wantOffender == -1 {
		if d.TxStatus != model.TxCommitted {
			t.Errorf("seed %d: group %d should COMMIT, got %s", seed, tx.ID, d.TxStatus)
			return false
		}
		for _, id := range ids {
			if m.accountsByID[id].Balance != before[id]+net[id] {
				t.Errorf("seed %d: group %d committed but account %d balance %d != %d", seed, tx.ID, id, m.accountsByID[id].Balance, before[id]+net[id])
				return false
			}
		}
	} else {
		if d.TxStatus != model.TxRejected {
			t.Errorf("seed %d: group %d should REJECT (account %d), got %s", seed, tx.ID, wantOffender, d.TxStatus)
			return false
		}
		if d.Reason == nil || d.Reason.Account != m.accountsByID[wantOffender].ExternalID || d.Reason.LimitSide != wantSide || d.Reason.Shortfall != wantShort {
			t.Errorf("seed %d: group %d rejection %+v, want account %s/%s/%d", seed, tx.ID, d.Reason, m.accountsByID[wantOffender].ExternalID, wantSide, wantShort)
			return false
		}
		for _, id := range ids {
			if m.accountsByID[id].Balance != before[id] {
				t.Errorf("seed %d: group %d rejected but account %d balance changed %d -> %d", seed, tx.ID, id, before[id], m.accountsByID[id].Balance)
				return false
			}
		}
	}

	if err := m.CheckG1(); err != nil {
		t.Errorf("seed %d: after group %d: %v", seed, tx.ID, err)
		return false
	}
	if err := m.CheckG2(); err != nil {
		t.Errorf("seed %d: after group %d: %v", seed, tx.ID, err)
		return false
	}
	return true
}

// peekPending returns the lowest-id pending operation (single or group leg) without
// deciding it.
func (m *Model) peekPending() (*refOp, bool) {
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || op.Status != model.OpPending {
			continue
		}
		return op, true
	}
	return nil, false
}

func mustSetLimits(t *testing.T, m *Model, owner int64, ext string, lo, hi *int64) {
	t.Helper()
	if err := m.SetLimits(owner, ext, lo, hi); err != nil {
		t.Fatalf("SetLimits(%s): %v", ext, err)
	}
}

func mustInsert(t *testing.T, m *Model, req api.InsertRequest) *api.InsertResult {
	t.Helper()
	res, err := m.Insert(req)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return res
}
