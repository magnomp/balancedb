package simtest

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/magnomp/balancedb/internal/api"
)

// issued records a request the schedule has sent, so a later step can replay it.
type issued struct {
	req    api.InsertRequest
	result *api.InsertResult
}

func uuid(n int) string {
	return fmt.Sprintf("%08x-0000-4000-8000-000000000000", n)
}

func randOps(rng *rand.Rand, owner int64) []api.InsertOp {
	n := 1 + rng.Intn(4) // 1..4 legs; single vs group both exercised
	ops := make([]api.InsertOp, n)
	for i := range ops {
		amt := int64(rng.Intn(2001) - 1000) // -1000..1000
		if amt == 0 {
			amt = 1 // reference model + DB both reject 0; keep schedules valid
		}
		ops[i] = api.InsertOp{
			OwnerID:     owner,
			ExternalID:  fmt.Sprintf("acct-%d", rng.Intn(3)),
			Amount:      amt,
			EffectiveAt: time.Unix(int64(rng.Intn(1_000_000)), 0).UTC(),
		}
	}
	return ops
}

// TestReferenceIdempotentReplayNeverDuplicates is the first §15 property test
// (G6): across a randomized schedule of fresh inserts and same-key replays, a
// replay must return the original ids and must never create a new operation row.
// It also asserts same-key/different-payload retries surface as conflicts and
// change nothing. Every failure prints its seed.
func TestReferenceIdempotentReplayNeverDuplicates(t *testing.T) {
	t.Parallel()
	for iter := 0; iter < 200; iter++ {
		seed := time.Now().UnixNano() + int64(iter)
		if !runReplayProperty(t, seed) {
			return // runReplayProperty already reported the failing seed
		}
	}
}

func runReplayProperty(t *testing.T, seed int64) bool {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	m := NewModel()

	var history []issued
	nextKey := 0

	for step := 0; step < 100; step++ {
		switch {
		case len(history) > 0 && rng.Intn(3) == 0:
			// Exact replay of a prior request: must return identical ids and add
			// no rows (G6).
			prior := history[rng.Intn(len(history))]
			before := m.OpCount()
			got, err := m.Insert(prior.req)
			if err != nil {
				t.Errorf("seed %d: replay errored: %v", seed, err)
				return false
			}
			if !got.Replayed {
				t.Errorf("seed %d: replay not flagged Replayed", seed)
				return false
			}
			if m.OpCount() != before {
				t.Errorf("seed %d: replay created rows (%d → %d)", seed, before, m.OpCount())
				return false
			}
			if !sameIDs(prior.result, got) {
				t.Errorf("seed %d: replay returned different ids", seed)
				return false
			}

		case len(history) > 0 && rng.Intn(4) == 0:
			// Same key, mutated payload: must conflict and change nothing.
			prior := history[rng.Intn(len(history))]
			mutated := prior.req
			legs := append([]api.InsertOp(nil), prior.req.Operations...)
			legs[0].ExternalID += "-x" // perturb the payload without risking amount 0
			mutated.Operations = legs
			before := m.OpCount()
			if _, err := m.Insert(mutated); !errors.Is(err, api.ErrPayloadConflict) {
				t.Errorf("seed %d: expected ErrPayloadConflict, got %v", seed, err)
				return false
			}
			if m.OpCount() != before {
				t.Errorf("seed %d: conflict created rows", seed)
				return false
			}

		default:
			// A fresh request with a brand-new key.
			req := api.InsertRequest{IdempotencyKey: uuid(nextKey), Operations: randOps(rng, int64(rng.Intn(2)+1))}
			nextKey++
			res, err := m.Insert(req)
			if err != nil {
				t.Errorf("seed %d: fresh insert errored: %v", seed, err)
				return false
			}
			if res.Replayed {
				t.Errorf("seed %d: fresh insert flagged Replayed", seed)
				return false
			}
			history = append(history, issued{req: req, result: res})
		}
	}

	// Cross-check: exactly one operation row per issued-and-recorded leg.
	wantOps := 0
	for _, h := range history {
		wantOps += len(h.req.Operations)
	}
	if m.OpCount() != wantOps {
		t.Errorf("seed %d: op count %d, expected %d unique legs", seed, m.OpCount(), wantOps)
		return false
	}
	return true
}

func sameIDs(a, b *api.InsertResult) bool {
	if (a.TransactionID == nil) != (b.TransactionID == nil) {
		return false
	}
	if a.TransactionID != nil && *a.TransactionID != *b.TransactionID {
		return false
	}
	if len(a.Operations) != len(b.Operations) {
		return false
	}
	for i := range a.Operations {
		if a.Operations[i].ID != b.Operations[i].ID {
			return false
		}
	}
	return true
}

// TestReferenceInsertGuards checks the reference model reproduces the inserter's
// synchronous guards.
func TestReferenceInsertGuards(t *testing.T) {
	t.Parallel()
	m := NewModel()
	when := time.Unix(0, 0).UTC()

	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(1)}); !errors.Is(err, api.ErrNoOperations) {
		t.Fatalf("empty: got %v", err)
	}
	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: "not-a-uuid", Operations: []api.InsertOp{{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: when}}}); !errors.Is(err, api.ErrInvalidIdempotencyKey) {
		t.Fatalf("bad key: got %v", err)
	}
	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(2), Operations: []api.InsertOp{
		{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: when},
		{OwnerID: 2, ExternalID: "b", Amount: 1, EffectiveAt: when},
	}}); !errors.Is(err, api.ErrMixedOwners) {
		t.Fatalf("mixed owners: got %v", err)
	}
	big := make([]api.InsertOp, m.MaxGroupSize+1)
	for i := range big {
		big[i] = api.InsertOp{OwnerID: 1, ExternalID: "a", Amount: 1, EffectiveAt: when}
	}
	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(3), Operations: big}); !errors.Is(err, api.ErrGroupTooLarge) {
		t.Fatalf("group too large: got %v", err)
	}
	if _, err := m.Insert(api.InsertRequest{IdempotencyKey: uuid(4), Operations: []api.InsertOp{{OwnerID: 1, ExternalID: "a", Amount: 0, EffectiveAt: when}}}); !errors.Is(err, api.ErrZeroAmount) {
		t.Fatalf("zero amount: got %v", err)
	}
	if m.OpCount() != 0 {
		t.Fatalf("rejected inserts must write nothing, have %d ops", m.OpCount())
	}
}
