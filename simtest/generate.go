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
// reversal-after-reject retries), edits (ADR-0010: single, grouped and mixed
// with new operations; amount, account and day-crossing effective_at changes,
// expected_revision hits and misses, targets that are CONFIRMED, INVALID,
// already edited, or still PENDING from the same round), and limit changes —
// drawn from a seeded PRNG and shaped by the current
// reference-model state so every one is a valid, state-aware move. The same Action
// stream drives the pure reference model (breadth: 1,000+ seeds under `make test`/
// `make simtest`) and, in lockstep, the real processor over a throwaway schema
// (fidelity: the DB-backed tests behind the `simtest` build tag). A generator is
// fully determined by its seed, so every failure reproduces exactly from the seed
// the test prints.

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
	actEdit                        // a fresh single edit of a regular operation (ADR-0010)
	actEditGroup                   // a fresh group of edits, optionally mixed with new operations
)

// String names an action kind for the action-mix tally the tests print.
func (k actionKind) String() string {
	switch k {
	case actInsert:
		return "insert"
	case actReplay:
		return "replay"
	case actConflict:
		return "conflict"
	case actReversal:
		return "reversal"
	case actSetLimits:
		return "set_limits"
	case actEdit:
		return "edit"
	case actEditGroup:
		return "edit_group"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// Mixed reports whether an edit-group action also carries new operations.
func (a Action) Mixed() bool {
	if a.Kind != actEditGroup {
		return false
	}
	for _, op := range a.Req.Operations {
		if op.EditOf == nil {
			return true
		}
	}
	return false
}

// actionMix tallies generated actions by kind (edit groups split into pure and
// mixed) so a sweep can print — and assert — that every class of move, edits
// included, is present in the schedule it just ran.
type actionMix map[string]int

func (x actionMix) add(a Action) {
	name := a.Kind.String()
	if a.Mixed() {
		name = "edit_group_mixed"
	}
	x[name]++
}

// String renders the tally in a fixed order.
func (x actionMix) String() string {
	out := ""
	for _, k := range []string{"insert", "replay", "conflict", "reversal", "set_limits", "edit", "edit_group", "edit_group_mixed"} {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, x[k])
	}
	return out
}

// missingEdits names the edit classes absent from the tally, or nil when single
// edits, pure edit groups and mixed groups all occurred.
func (x actionMix) missingEdits() []string {
	var missing []string
	for _, k := range []string{"edit", "edit_group", "edit_group_mixed"} {
		if x[k] == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

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

// editCandidate is one regular operation an edit may target: a settled one from
// the model (any status, any revision) or a fresh regular leg emitted earlier in
// the same round — its reference id is predictable because the reference
// allocates ids densely per recorded leg and records nothing on a replay or a
// conflict — which the drain decides first by id order, so the edit meets it
// CONFIRMED or INVALID (never PENDING, whose deferral is unreachable through the
// insertion path: an edit row cannot reference an uncommitted target).
type editCandidate struct {
	id       int64
	ext      string
	revision int32
}

// Round produces the next batch of moves, shaped by the current model state m
// (confirmed balances for limit changes, confirmed operations for reversals,
// regular operations for edits). It is called once per drain cycle, so reversals
// and limit changes always act on settled (drained) state. n is the target number
// of moves.
func (g *Generator) Round(m *Model, n int) []Action {
	actions := make([]Action, 0, n)
	targets := m.confirmedReversalTargets()
	views := m.accountViews()

	var editable []editCandidate
	for _, op := range m.editTargets() {
		editable = append(editable, editCandidate{id: op.ID, ext: m.accountsByID[op.AccountID].ExternalID, revision: op.Revision})
	}
	// nextID predicts the reference id of the next fresh leg emitted this round.
	nextID := m.nextOpID + 1
	fresh := func(req api.InsertRequest) {
		g.history = append(g.history, req)
		for _, op := range req.Operations {
			if op.EditOf == nil {
				editable = append(editable, editCandidate{id: nextID, ext: op.ExternalID, revision: 1})
			}
			nextID++
		}
	}

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
			fresh(req)
			actions = append(actions, Action{Kind: actReversal, Req: req})

		case roll < 40 && len(views) > 0:
			// State-aware limit change (spec §6): brackets the confirmed balance.
			v := views[g.rng.Intn(len(views))]
			actions = append(actions, Action{Kind: actSetLimits, Limits: g.limitChangeFor(v)})

		case roll < 54 && len(editable) > 0:
			// A single edit of a regular operation (ADR-0010).
			req := g.editRequest(editable[g.rng.Intn(len(editable))])
			fresh(req)
			actions = append(actions, Action{Kind: actEdit, Req: req})

		case roll < 64 && len(editable) > 0:
			// A group of edits — distinct targets, since two items may not edit one
			// operation — optionally mixed with new operations (ADR-0010: grouped
			// edits are literally groups, netted per account, all-or-nothing).
			req := g.editGroupRequest(editable)
			fresh(req)
			actions = append(actions, Action{Kind: actEditGroup, Req: req})

		default:
			// A fresh single or group.
			req := g.freshRequest()
			fresh(req)
			actions = append(actions, Action{Kind: actInsert, Req: req})
		}
	}
	return actions
}

// editRequest builds one single edit registration of c (editItem): at least one of amount,
// effective_at (aggressively day-crossing, via effTime) and account (a different
// scenario account) changes — the omitted ones stay omitted so the ledger fills
// them from the target and the hash covers the item as sent. With some
// probability it carries expected_revision: the revision known now (a hit unless
// an earlier edit of the same target in this round applies first) or one past it
// (a STALE_REVISION miss).
func (g *Generator) editRequest(c editCandidate) api.InsertRequest {
	return api.InsertRequest{IdempotencyKey: g.nextKey(), Operations: []api.InsertOp{g.editItem(c)}}
}

// editItem builds one edit item of c (see editRequest for the shape).
func (g *Generator) editItem(c editCandidate) api.InsertOp {
	target := c.id
	op := api.InsertOp{OwnerID: g.owner, EditOf: &target}
	if g.rng.Intn(2) == 0 {
		op.Amount = g.smallAmount()
	}
	if g.rng.Intn(3) == 0 {
		op.EffectiveAt = g.effTime()
	}
	if g.rng.Intn(4) == 0 {
		ext := scenarioAccounts[g.rng.Intn(len(scenarioAccounts))].ext
		if ext != c.ext {
			op.ExternalID = ext
		}
	}
	if op.Amount == 0 && op.EffectiveAt.IsZero() && op.ExternalID == "" {
		op.Amount = g.smallAmount() // an edit must change something
	}
	switch g.rng.Intn(6) {
	case 0, 1:
		rev := c.revision
		op.ExpectedRevision = &rev
	case 2:
		rev := c.revision + 1 + int32(g.rng.Intn(2))
		op.ExpectedRevision = &rev
	}
	return op
}

// editGroupRequest builds one group of 1..3 edits of distinct candidates — each
// shaped exactly like a single edit (editItem) — and, half the time, 1..2 new
// operations (newItem) in between, so mixed groups net an edit's virtual legs
// against fresh legs on the same accounts. A group of one edit plus one new
// operation is the smallest mixed unit; a lone edit is the single form, covered
// by actEdit, so a group here always has at least two items.
func (g *Generator) editGroupRequest(editable []editCandidate) api.InsertRequest {
	nEdits := 1 + g.rng.Intn(3)
	if nEdits > len(editable) {
		nEdits = len(editable)
	}
	nNew := 0
	if nEdits == 1 || g.rng.Intn(2) == 0 {
		nNew = 1 + g.rng.Intn(2)
	}
	// Distinct targets: draw without replacement from a shuffled index.
	perm := g.rng.Perm(len(editable))[:nEdits]
	ops := make([]api.InsertOp, 0, nEdits+nNew)
	for _, i := range perm {
		ops = append(ops, g.editItem(editable[i]))
	}
	for i := 0; i < nNew; i++ {
		at := g.rng.Intn(len(ops) + 1) // anywhere in the group, edits and new legs interleaved
		ops = append(ops[:at], append([]api.InsertOp{g.newItem()}, ops[at:]...)...)
	}
	return api.InsertRequest{IdempotencyKey: g.nextKey(), Operations: ops}
}

// newItem builds one fresh regular operation item.
func (g *Generator) newItem() api.InsertOp {
	return api.InsertOp{
		OwnerID:     g.owner,
		ExternalID:  scenarioAccounts[g.rng.Intn(len(scenarioAccounts))].ext,
		Amount:      g.smallAmount(),
		EffectiveAt: g.effTime(),
	}
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
		ops[i] = g.newItem()
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
	default: // actInsert, actReversal, actEdit, actEditGroup
		if _, err := m.Insert(a.Req); err != nil {
			return fmt.Errorf("insert errored: %w", err)
		}
		return nil
	}
}
