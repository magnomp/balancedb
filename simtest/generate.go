package simtest

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/model"
)

// This file is the M10 seeded generator for the full spec §15 scenario space. It
// emits rounds of Actions — inserts (singles and groups), idempotent replays,
// payload-conflict retries, reversals (including double-reversals and
// reversal-after-reject retries), edits (ADR-0010: single, grouped and mixed
// with new operations; amount, account and day-crossing effective_at changes,
// expected_revision hits and misses, targets that are CONFIRMED, INVALID,
// already edited, DELETED, or still PENDING from the same round), deletes
// (ADR-0011: single, grouped and mixed with edits and new operations; the same
// target classes, expected_revision hits and misses, replays and conflicts
// through the shared replay/conflict draws), and limit changes —
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
	actInsert      actionKind = iota // a fresh single or group
	actReplay                        // an exact resend of a prior request (must replay)
	actConflict                      // same key, mutated payload (must conflict)
	actReversal                      // a fresh single reversing a confirmed operation
	actSetLimits                     // a state-aware limit change
	actEdit                          // a fresh single edit of a regular operation (ADR-0010)
	actEditGroup                     // a fresh group of edits, optionally mixed with new operations
	actDelete                        // a fresh single delete of a regular operation (ADR-0011)
	actDeleteGroup                   // a fresh group of deletes, optionally mixed with edits and new operations
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
	case actDelete:
		return "delete"
	case actDeleteGroup:
		return "delete_group"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// Mixed reports whether a group action carries more than its own kind: an edit
// group with new operations (a delete item is edit-class, never "new"), or a
// delete group with edits or new operations.
func (a Action) Mixed() bool {
	for _, op := range a.Req.Operations {
		switch a.Kind {
		case actEditGroup:
			if op.EditOf == nil && op.DeleteOf == nil {
				return true
			}
		case actDeleteGroup:
			if op.DeleteOf == nil {
				return true
			}
		}
	}
	return false
}

// actionMix tallies generated actions by kind (edit and delete groups split
// into pure and mixed) and every fine-grained delete class an action carries
// (Action.Classes), so a sweep can print — and assert — that every class of
// move, edits and deletes included, is present in the schedule it just ran.
type actionMix map[string]int

// mixKinds is the fixed print order of the action kinds.
var mixKinds = []string{"insert", "replay", "conflict", "reversal", "set_limits", "edit", "edit_group", "edit_group_mixed", "delete", "delete_group", "delete_group_mixed"}

// deleteClasses is every fine-grained delete class the generator can emit (UT-043):
// the guard draw, the target's state when the delete was drawn, and a replay or
// a conflict of a request carrying a delete item.
var deleteClasses = []string{
	"del_guard_none", "del_guard_hit", "del_guard_miss",
	"del_target_confirmed", "del_target_edited", "del_target_invalid", "del_target_deleted", "del_target_fresh",
	"del_replay", "del_conflict",
}

func (x actionMix) add(a Action) {
	name := a.Kind.String()
	if a.Mixed() {
		name += "_mixed"
	}
	x[name]++
	for _, c := range a.Classes {
		x[c]++
	}
}

// String renders the tally: the kinds in a fixed order, then every other key
// (the delete classes) sorted.
func (x actionMix) String() string {
	out := ""
	for _, k := range mixKinds {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, x[k])
	}
	known := make(map[string]bool, len(mixKinds))
	for _, k := range mixKinds {
		known[k] = true
	}
	var rest []string
	for k := range x {
		if !known[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out += fmt.Sprintf(" %s=%d", k, x[k])
	}
	return out
}

// missingClasses names the edit-class kinds absent from the tally, or nil when
// single edits, pure and mixed edit groups, single deletes and pure and mixed
// delete groups all occurred.
func (x actionMix) missingClasses() []string {
	var missing []string
	for _, k := range []string{"edit", "edit_group", "edit_group_mixed", "delete", "delete_group", "delete_group_mixed"} {
		if x[k] == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

// missingDeleteClasses names the fine-grained delete classes absent from the
// tally, or nil when every one occurred (UT-043; asserted by the breadth sweep,
// whose 1,000+ seeds make every class certain).
func (x actionMix) missingDeleteClasses() []string {
	var missing []string
	for _, k := range deleteClasses {
		if x[k] == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

// Action is one generated move. For the insert-family kinds Req carries the
// request; for actSetLimits Limits carries the change. ExpectReplay/ExpectConflict
// record the outcome the reference model and the database must agree on.
// Classes lists the fine-grained delete classes the move carries (one guard and
// one target class per delete item, del_replay / del_conflict on a resend of a
// request with a delete item), for the tally.
type Action struct {
	Kind           actionKind
	Req            api.InsertRequest
	Limits         *limitChange
	ExpectReplay   bool
	ExpectConflict bool
	Classes        []string
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

// editCandidate is one regular operation an edit or a delete may target: a
// settled one from the model (any status — CONFIRMED, INVALID or DELETED — any
// revision) or a fresh regular leg emitted earlier in the same round — its
// reference id is predictable because the reference allocates ids densely per
// recorded leg and records nothing on a replay or a conflict — which the drain
// decides first by id order, so the edit or delete meets it CONFIRMED or INVALID
// (never PENDING, whose deferral is unreachable through the insertion path: an
// edit-class row cannot reference an uncommitted target). status is the
// settled status, or "" for a fresh (same-round PENDING) leg.
type editCandidate struct {
	id       int64
	ext      string
	revision int32
	status   model.OpStatus
}

// targetClass names the candidate's state for the delete-class tally.
func (c editCandidate) targetClass() string {
	switch {
	case c.status == "":
		return "del_target_fresh"
	case c.status == model.OpInvalid:
		return "del_target_invalid"
	case c.status == model.OpDeleted:
		return "del_target_deleted"
	case c.revision > 1:
		return "del_target_edited"
	default:
		return "del_target_confirmed"
	}
}

// hasDelete reports whether a request carries a delete item.
func hasDelete(req api.InsertRequest) bool {
	for _, op := range req.Operations {
		if op.DeleteOf != nil {
			return true
		}
	}
	return false
}

// Round produces the next batch of moves, shaped by the current model state m
// (confirmed balances for limit changes, confirmed operations for reversals,
// regular operations for edits and deletes). It is called once per drain cycle, so reversals
// and limit changes always act on settled (drained) state. n is the target number
// of moves.
func (g *Generator) Round(m *Model, n int) []Action {
	actions := make([]Action, 0, n)
	targets := m.confirmedReversalTargets()
	views := m.accountViews()

	var editable []editCandidate
	for _, op := range m.editTargets() {
		editable = append(editable, editCandidate{id: op.ID, ext: m.accountsByID[op.AccountID].ExternalID, revision: op.Revision, status: op.Status})
	}
	// nextID predicts the reference id of the next fresh leg emitted this round.
	nextID := m.nextOpID + 1
	fresh := func(req api.InsertRequest) {
		g.history = append(g.history, req)
		for _, op := range req.Operations {
			if op.EditOf == nil && op.DeleteOf == nil {
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
			prior := g.history[g.rng.Intn(len(g.history))]
			a := Action{Kind: actReplay, ExpectReplay: true, Req: prior}
			if hasDelete(prior) {
				a.Classes = []string{"del_replay"}
			}
			actions = append(actions, a)

		case roll < 14 && len(g.history) > 0:
			// Same key, mutated payload → conflict, changes nothing (G6). A delete
			// item carries only its target and guard, so its payload is perturbed
			// through the guard (a delete with an account would be malformed, not a
			// conflict); every other item shape takes an account perturbation.
			prior := g.history[g.rng.Intn(len(g.history))]
			legs := append([]api.InsertOp(nil), prior.Operations...)
			if legs[0].DeleteOf != nil {
				rev := int32(1)
				if legs[0].ExpectedRevision != nil {
					rev = *legs[0].ExpectedRevision + 1
				}
				legs[0].ExpectedRevision = &rev
			} else {
				legs[0].ExternalID += "-x" // perturb payload without risking amount 0
			}
			a := Action{
				Kind: actConflict, ExpectConflict: true,
				Req: api.InsertRequest{IdempotencyKey: prior.IdempotencyKey, Operations: legs},
			}
			if hasDelete(prior) {
				a.Classes = []string{"del_conflict"}
			}
			actions = append(actions, a)

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

		case roll < 62 && len(editable) > 0:
			// A single delete of a regular operation (ADR-0011): the same target
			// classes as an edit — CONFIRMED, edited, INVALID, DELETED, same-round
			// fresh — with expected_revision hits and misses.
			req, classes := g.deleteRequest(editable[g.rng.Intn(len(editable))])
			fresh(req)
			actions = append(actions, Action{Kind: actDelete, Req: req, Classes: classes})

		case roll < 70 && len(editable) > 0:
			// A group of edits — distinct targets, since two items may not edit one
			// operation — optionally mixed with new operations (ADR-0010: grouped
			// edits are literally groups, netted per account, all-or-nothing).
			req := g.editGroupRequest(editable)
			fresh(req)
			actions = append(actions, Action{Kind: actEditGroup, Req: req})

		case roll < 78 && len(editable) > 0:
			// A group of deletes — distinct targets across every kind, since two
			// items may not target one operation — optionally mixed with edits and
			// new operations (ADR-0011: grouped deletes are literally groups, one
			// virtual leg each, netted per account, all-or-nothing).
			req, classes := g.deleteGroupRequest(editable)
			fresh(req)
			actions = append(actions, Action{Kind: actDeleteGroup, Req: req, Classes: classes})

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
	op.ExpectedRevision = g.expectedRevision(c)
	return op
}

// expectedRevision draws the optional optimistic guard of an edit or delete of
// c: a third of the time the revision known now (a hit unless an earlier edit of
// the same target in this round applies first), a sixth of the time one or two
// past it (a STALE_REVISION miss), otherwise none.
func (g *Generator) expectedRevision(c editCandidate) *int32 {
	switch g.rng.Intn(6) {
	case 0, 1:
		rev := c.revision
		return &rev
	case 2:
		rev := c.revision + 1 + int32(g.rng.Intn(2))
		return &rev
	}
	return nil
}

// deleteRequest builds one single delete registration of c (ADR-0011): the
// target and, with the same odds as an edit, an expected_revision hit or miss.
// Nothing else — a delete item carries no account, amount or instant. It also
// returns the item's delete classes for the tally.
func (g *Generator) deleteRequest(c editCandidate) (api.InsertRequest, []string) {
	item, classes := g.deleteItem(c)
	return api.InsertRequest{IdempotencyKey: g.nextKey(), Operations: []api.InsertOp{item}}, classes
}

// deleteItem builds one delete item of c and names its guard and target classes.
func (g *Generator) deleteItem(c editCandidate) (api.InsertOp, []string) {
	target := c.id
	op := api.InsertOp{OwnerID: g.owner, DeleteOf: &target, ExpectedRevision: g.expectedRevision(c)}
	guard := "del_guard_none"
	switch {
	case op.ExpectedRevision != nil && *op.ExpectedRevision == c.revision:
		guard = "del_guard_hit"
	case op.ExpectedRevision != nil:
		guard = "del_guard_miss"
	}
	return op, []string{guard, c.targetClass()}
}

// deleteGroupRequest builds one group of 1..3 deletes of distinct candidates —
// each shaped exactly like a single delete (deleteItem) — and, half the time
// (always when there is one delete, so the group is never the single form),
// 0..2 edits of further distinct candidates (editItem) and/or 1..2 new
// operations (newItem) interleaved anywhere, so mixed groups net a delete's
// virtual leg against edits and fresh legs on the same accounts. A group of one
// delete plus one edit or new operation is the smallest mixed unit; a pure
// group has at least two deletes. It also returns the delete classes of every
// delete item for the tally.
func (g *Generator) deleteGroupRequest(editable []editCandidate) (api.InsertRequest, []string) {
	nDel := 1 + g.rng.Intn(3)
	if nDel > len(editable) {
		nDel = len(editable)
	}
	nEdits, nNew := 0, 0
	if nDel == 1 || g.rng.Intn(2) == 0 {
		nEdits = g.rng.Intn(3)
		if nEdits > len(editable)-nDel {
			nEdits = len(editable) - nDel
		}
		if nEdits == 0 || g.rng.Intn(2) == 0 {
			nNew = 1 + g.rng.Intn(2)
		}
	}
	if nDel == 1 && nEdits == 0 && nNew == 0 {
		nNew = 1 // never the single form
	}
	// Distinct targets across deletes and edits: draw without replacement.
	perm := g.rng.Perm(len(editable))
	ops := make([]api.InsertOp, 0, nDel+nEdits+nNew)
	var classes []string
	for _, i := range perm[:nDel] {
		item, cs := g.deleteItem(editable[i])
		ops = append(ops, item)
		classes = append(classes, cs...)
	}
	for _, i := range perm[nDel : nDel+nEdits] {
		at := g.rng.Intn(len(ops) + 1)
		ops = append(ops[:at], append([]api.InsertOp{g.editItem(editable[i])}, ops[at:]...)...)
	}
	for i := 0; i < nNew; i++ {
		at := g.rng.Intn(len(ops) + 1) // anywhere in the group, kinds interleaved
		ops = append(ops[:at], append([]api.InsertOp{g.newItem()}, ops[at:]...)...)
	}
	return api.InsertRequest{IdempotencyKey: g.nextKey(), Operations: ops}, classes
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
	default: // actInsert, actReversal, actEdit, actEditGroup, actDelete, actDeleteGroup
		if _, err := m.Insert(a.Req); err != nil {
			return fmt.Errorf("insert errored: %w", err)
		}
		return nil
	}
}
