package simtest

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/magnomp/balancedb/internal/api"
)

// This file is the M10 seeded generator for the full spec §15 scenario space. It
// emits rounds of Actions — inserts (singles and groups), idempotent replays,
// payload-conflict retries, reversals (including double-reversals and
// reversal-after-reject retries), and limit changes — drawn from a seeded PRNG and
// shaped by the current reference-model state so every one is a valid, state-aware
// move. The same Action stream drives the pure reference model (breadth: 1,000+
// seeds under `make test`/`make simtest`) and, in lockstep, the real processor over
// a throwaway schema (fidelity: the DB-backed tests behind the `simtest` build
// tag). A generator is fully determined by its seed, so every failure reproduces
// exactly from the seed the test prints.

// simOwnerID is the single owner every schedule targets (one owner per cell keeps
// the sharding invariant, spec §2, trivially satisfied).
const simOwnerID int64 = 1

// acctSpec is one account the schedule creates up front, with its limit shape.
// The mix spans tight, loose, one-sided, and unbounded limits so decisions
// routinely accept and reject.
type acctSpec struct {
	ext string
	min *int64
	max *int64
}

// scenarioAccounts is the fixed account set every full-scenario schedule seeds.
// Kept small so many operations land on the same account — that concentrates
// version contention (the object of Guard 3) under the competing/zombie-leader
// tests.
var scenarioAccounts = []acctSpec{
	{"tight", ptr(-60), ptr(60)},
	{"loose", ptr(-100000), ptr(100000)},
	{"unbounded", nil, nil},
	{"floor", ptr(-300), nil},
	{"ceiling", nil, ptr(300)},
	{"narrow", ptr(-20), ptr(20)},
}

// actionKind classifies one generated move.
type actionKind int

const (
	actInsert    actionKind = iota // a fresh single or group
	actReplay                      // an exact resend of a prior request (must replay)
	actConflict                    // same key, mutated payload (must conflict)
	actReversal                    // a fresh single reversing a confirmed operation
	actSetLimits                   // a state-aware limit change
)

// Action is one generated move. For the insert-family kinds Req carries the
// request; for actSetLimits Limits carries the change. ExpectReplay/ExpectConflict
// record the outcome the reference model and the database must agree on.
type Action struct {
	Kind           actionKind
	Req            api.InsertRequest
	Limits         *limitChange
	ExpectReplay   bool
	ExpectConflict bool
}

// limitChange is a state-aware §6 limit update: it always brackets the account's
// confirmed balance (so it is satisfiable) and keeps min <= max.
type limitChange struct {
	ExternalID string
	Min        *int64
	Max        *int64
}

// Generator produces the deterministic Action stream for one seed.
type Generator struct {
	rng     *rand.Rand
	owner   int64
	keySeq  int
	history []api.InsertRequest // fresh requests, for replay/conflict draws
	usedAt  []time.Time         // previously emitted effective_at values, for ties
	base    time.Time
}

// NewGenerator returns a generator seeded by seed. The base time sits on a UTC day
// boundary so day-offset arithmetic lands operations on clean snapshot buckets and
// on the midnight edge itself.
func NewGenerator(seed int64) *Generator {
	return &Generator{
		rng:   rand.New(rand.NewSource(seed)),
		owner: simOwnerID,
		base:  time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

// Accounts returns the fixed account specs a schedule seeds before any operation.
func (g *Generator) Accounts() []acctSpec { return scenarioAccounts }

// nextKey returns a fresh canonical-UUID idempotency key.
func (g *Generator) nextKey() string {
	g.keySeq++
	return uuid(g.keySeq)
}

// effTime returns an effective_at for one operation. It aggressively backdates and
// future-dates across a multi-day span, deliberately lands some operations exactly
// on a UTC midnight (a snapshot-day boundary), and — with meaningful probability —
// reuses a previously emitted timestamp to force an exact tie so the (effective_at,
// id) tiebreaker of G4 is exercised.
func (g *Generator) effTime() time.Time {
	if len(g.usedAt) > 0 && g.rng.Intn(4) == 0 {
		return g.usedAt[g.rng.Intn(len(g.usedAt))] // exact tie
	}
	day := g.rng.Intn(7) // spans 7 UTC days: crosses snapshot boundaries both ways
	var within time.Duration
	switch g.rng.Intn(3) {
	case 0:
		within = 0 // exactly on the midnight day boundary
	case 1:
		within = time.Duration(g.rng.Intn(86400)) * time.Second
	default:
		within = 24*time.Hour - time.Second // just before the next day boundary
	}
	t := g.base.Add(time.Duration(day)*24*time.Hour + within)
	g.usedAt = append(g.usedAt, t)
	return t
}

// smallAmount returns a non-zero amount that routinely crosses the tight bounds.
func (g *Generator) smallAmount() int64 {
	a := int64(g.rng.Intn(121) - 60) // -60..60
	if a == 0 {
		a = 1
	}
	return a
}

// Round produces the next batch of moves, shaped by the current model state m
// (confirmed balances for limit changes, confirmed operations for reversals). It is
// called once per drain cycle, so reversals and limit changes always act on settled
// (drained) state. n is the target number of moves.
func (g *Generator) Round(m *Model, n int) []Action {
	actions := make([]Action, 0, n)
	targets := m.confirmedReversalTargets()
	views := m.accountViews()

	for i := 0; i < n; i++ {
		roll := g.rng.Intn(100)
		switch {
		case roll < 8 && len(g.history) > 0:
			// Exact idempotent replay (G6).
			actions = append(actions, Action{
				Kind: actReplay, ExpectReplay: true,
				Req: g.history[g.rng.Intn(len(g.history))],
			})

		case roll < 14 && len(g.history) > 0:
			// Same key, mutated payload → conflict, changes nothing (G6).
			prior := g.history[g.rng.Intn(len(g.history))]
			legs := append([]api.InsertOp(nil), prior.Operations...)
			legs[0].ExternalID += "-x" // perturb payload without risking amount 0
			actions = append(actions, Action{
				Kind: actConflict, ExpectConflict: true,
				Req: api.InsertRequest{IdempotencyKey: prior.IdempotencyKey, Operations: legs},
			})

		case roll < 30 && len(targets) > 0:
			// Reverse a confirmed operation. Popping the target from the local slice
			// prevents two live reversals of the same op within one round (the
			// idx_ops_reversal unique index forbids it); double-reversals arise
			// naturally because a confirmed reversal is itself an eligible target next
			// round, and reversal-after-reject retries arise because a rejected
			// reversal leaves its original eligible again.
			idx := g.rng.Intn(len(targets))
			t := targets[idx]
			targets = append(targets[:idx], targets[idx+1:]...)
			revOf := t.ID
			eff := t.EffectiveAt
			if g.rng.Intn(2) == 0 {
				eff = g.effTime() // reversal need not share the original's timestamp (N4)
			}
			req := api.InsertRequest{
				IdempotencyKey: g.nextKey(),
				Operations: []api.InsertOp{{
					OwnerID:     g.owner,
					ExternalID:  m.accountsByID[t.AccountID].ExternalID,
					Amount:      -t.Amount,
					EffectiveAt: eff,
					ReversalOf:  &revOf,
				}},
			}
			g.history = append(g.history, req)
			actions = append(actions, Action{Kind: actReversal, Req: req})

		case roll < 40 && len(views) > 0:
			// State-aware limit change (spec §6): brackets the confirmed balance.
			v := views[g.rng.Intn(len(views))]
			actions = append(actions, Action{Kind: actSetLimits, Limits: g.limitChangeFor(v)})

		default:
			// A fresh single or group.
			actions = append(actions, Action{Kind: actInsert, Req: g.freshRequest()})
			g.history = append(g.history, actions[len(actions)-1].Req)
		}
	}
	return actions
}

// freshRequest builds a fresh single (1 leg) or group (2..4 legs, mixed accounts,
// same-account multi-leg and non-zero-sum both reachable).
func (g *Generator) freshRequest() api.InsertRequest {
	nLegs := 1
	if g.rng.Intn(2) == 0 {
		nLegs = 2 + g.rng.Intn(3) // 2..4
	}
	ops := make([]api.InsertOp, nLegs)
	for i := range ops {
		ops[i] = api.InsertOp{
			OwnerID:     g.owner,
			ExternalID:  scenarioAccounts[g.rng.Intn(len(scenarioAccounts))].ext,
			Amount:      g.smallAmount(),
			EffectiveAt: g.effTime(),
		}
	}
	return api.InsertRequest{IdempotencyKey: g.nextKey(), Operations: ops}
}

// limitChangeFor proposes a satisfiable limit change for one account: it always
// brackets the current confirmed balance and keeps min <= max, occasionally
// relaxing a side to unbounded.
func (g *Generator) limitChangeFor(v accountView) *limitChange {
	bal := v.Balance
	var min, max *int64
	if g.rng.Intn(4) != 0 {
		m := bal - int64(g.rng.Intn(120)) // <= balance
		min = &m
	}
	if g.rng.Intn(4) != 0 {
		x := bal + int64(g.rng.Intn(120)) // >= balance
		max = &x
	}
	// Guarantee min <= max even in the unlikely both-set-equal-around-balance case.
	if min != nil && max != nil && *min > *max {
		min, max = max, min
	}
	return &limitChange{ExternalID: v.ExternalID, Min: min, Max: max}
}

// applyToModel replays one action against the reference model and verifies the
// model's own outcome matches what the action expects. It returns an error on any
// disagreement (so the pure tier fails with the seed).
func (g *Generator) applyToModel(m *Model, a Action) error {
	switch a.Kind {
	case actSetLimits:
		// A satisfiable, bracketing change must be accepted by the §6 rule.
		if err := m.SetLimits(g.owner, a.Limits.ExternalID, a.Limits.Min, a.Limits.Max); err != nil {
			return fmt.Errorf("limit change on %s rejected by reference: %w", a.Limits.ExternalID, err)
		}
		return nil
	case actReplay:
		res, err := m.Insert(a.Req)
		if err != nil {
			return fmt.Errorf("replay errored: %w", err)
		}
		if !res.Replayed {
			return fmt.Errorf("replay not flagged Replayed")
		}
		return nil
	case actConflict:
		if _, err := m.Insert(a.Req); err == nil {
			return fmt.Errorf("conflict expected, got success")
		}
		return nil
	default: // actInsert, actReversal
		if _, err := m.Insert(a.Req); err != nil {
			return fmt.Errorf("insert errored: %w", err)
		}
		return nil
	}
}
