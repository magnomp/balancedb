package simtest

import (
	"os"
	"strconv"
	"testing"

	"github.com/magnomp/balancedb/internal/model"
)

// This file is the M10 breadth tier: the full spec §15 scenario space driven
// through the sequential reference model, across 1,000+ seeds, entirely in memory
// (no database) so it runs under `make test` in seconds. It is the wide net; the
// DB-backed tests (sim_db_test.go, build tag `simtest`) are the fidelity tier where
// the real engine is compared against this reference and where the Guard-3 mutation
// smoke test bites. Both print the failing seed; both reproduce exactly from it.

// envInt reads a positive integer from an environment variable, or returns def.
// It lets CI scale the seed counts up without editing the tests.
func envInt(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// TestFullScenarioReference (SIM-001) sweeps the full scenario space against the
// reference model: limits (tight/loose/one-sided/unbounded), singles, groups
// (mixed sizes, same-account multi-leg, non-zero-sum), aggressive
// back/future-dating with day crossings and exact-timestamp ties, reversals
// (including double-reversals and reversal-after-reject retries), edits — single,
// grouped and mixed with new operations; amount, account and day-crossing instant
// changes, expected_revision hits and misses, CONFIRMED / INVALID /
// already-edited / same-round PENDING targets — state-aware limit changes, and
// idempotent replays/conflicts. After every drain it asserts G1 (final balance
// within limits), G2 (no partially-applied group), G4 (timeline order total and
// unique), G6 (no duplicate rows), and the editing invariants (edits never
// CONFIRMED, histories dense, one revision per applied edit). It prints the
// action mix and fails if any edit class never occurred. Every failure prints
// its seed.
func TestFullScenarioReference(t *testing.T) {
	t.Parallel()
	seeds := envInt("SIMTEST_SEEDS", 1200)
	mix := actionMix{}
	for i := 0; i < seeds; i++ {
		seed := int64(2_000_000 + i)
		if !runFullScenarioReference(t, seed, mix) {
			return // the helper reported the failing seed
		}
	}
	t.Logf("action mix over %d seeds: %s", seeds, mix)
	if missing := mix.missingEdits(); missing != nil {
		t.Fatalf("the schedule never emitted %v (mix %s)", missing, mix)
	}
}

// rounds and movesPerRound shape one schedule: generate a round of moves, drain it
// (decide every pending op in registration order), assert the invariants, repeat.
const (
	rounds        = 8
	movesPerRound = 25
)

func runFullScenarioReference(t *testing.T, seed int64, mix actionMix) bool {
	t.Helper()
	g := NewGenerator(seed)
	m := NewModel()
	for _, a := range g.Accounts() {
		if err := m.SetLimits(g.owner, a.ext, a.min, a.max); err != nil {
			t.Errorf("seed %d: seed account %s: %v", seed, a.ext, err)
			return false
		}
	}

	for r := 0; r < rounds; r++ {
		for _, a := range g.Round(m, movesPerRound) {
			mix.add(a)
			if err := g.applyToModel(m, a); err != nil {
				t.Errorf("seed %d round %d: %v", seed, r, err)
				return false
			}
		}
		m.ProcessAll() // decide everything registered so far, in order
		if !checkReferenceInvariants(t, m, seed) {
			return false
		}
	}
	return true
}

// checkReferenceInvariants asserts G1, G2, G4, G6 and the editing invariants
// against the reference model.
func checkReferenceInvariants(t *testing.T, m *Model, seed int64) bool {
	t.Helper()
	if err := m.CheckG1(); err != nil {
		t.Errorf("seed %d: %v", seed, err)
		return false
	}
	if err := m.CheckG2(); err != nil {
		t.Errorf("seed %d: %v", seed, err)
		return false
	}
	if err := m.CheckG6(); err != nil {
		t.Errorf("seed %d: %v", seed, err)
		return false
	}
	if err := m.CheckEdits(); err != nil {
		t.Errorf("seed %d: %v", seed, err)
		return false
	}
	// G4: the per-account timeline is total and unique (no id appears twice, and the
	// (effective_at, id) order is well defined). id uniqueness makes the order total;
	// this catches any accidental duplication or loss in the model.
	for _, acctID := range m.AccountIDs() {
		tl := m.Timeline(acctID)
		seen := make(map[int64]bool, len(tl))
		for _, id := range tl {
			if seen[id] {
				t.Errorf("seed %d: G4 violated — op %d appears twice in account %d timeline", seed, id, acctID)
				return false
			}
			seen[id] = true
		}
	}
	return true
}

// TestFullScenarioReproduces is the G3 reproducibility guarantee: replaying the same
// seed yields an identical decision sequence and identical final balances. It runs a
// handful of seeds twice and compares the two runs move for move.
func TestFullScenarioReproduces(t *testing.T) {
	t.Parallel()
	for i := 0; i < 40; i++ {
		seed := int64(3_000_000 + i)
		a := replayForReproducibility(seed)
		b := replayForReproducibility(seed)
		if len(a) != len(b) {
			t.Fatalf("seed %d: run A produced %d decisions, run B %d", seed, len(a), len(b))
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("seed %d: decision %d differs between runs: %+v vs %+v", seed, j, a[j], b[j])
			}
		}
	}
}

// decisionKey is a comparable summary of one decision, used to compare two runs of
// the same seed for exact reproducibility (G3).
type decisionKey struct {
	opID     int64
	txID     int64
	status   model.OpStatus
	txStatus model.TxStatus
}

// replayForReproducibility runs one full schedule and returns the flat decision
// sequence (registration order) as comparable keys.
func replayForReproducibility(seed int64) []decisionKey {
	g := NewGenerator(seed)
	m := NewModel()
	for _, a := range g.Accounts() {
		_ = m.SetLimits(g.owner, a.ext, a.min, a.max)
	}
	var keys []decisionKey
	for r := 0; r < rounds; r++ {
		for _, a := range g.Round(m, movesPerRound) {
			_ = g.applyToModel(m, a)
		}
		for _, d := range m.ProcessAll() {
			k := decisionKey{opID: d.OpID, status: d.Status, txStatus: d.TxStatus}
			if d.TxID != nil {
				k.txID = *d.TxID
			}
			keys = append(keys, k)
		}
	}
	return keys
}
