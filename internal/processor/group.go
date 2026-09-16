package processor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
	"github.com/magnomp/balancedb/internal/snapshot"
)

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	// selectTransaction reads the group's declared op_count (the completeness
	// cross-check, spec §8.3) and its current status (skip-by-status, spec §8.1).
	selectTransaction = `SELECT op_count, status FROM transactions WHERE id = $1`

	// selectGroupLegs loads every leg of a transaction, any status, in id order.
	// The universe is complete because groups insert atomically (spec §10.1).
	// edit_of / expected_revision mark an edit-class leg: a plain edit (ADR-0010)
	// whose row holds the full proposed state for the target it names, or — with
	// is_delete — a delete (ADR-0011) whose row is an informational copy the
	// decision never reads.
	selectGroupLegs = `SELECT id, account_id, amount, effective_at, status, edit_of, is_delete, expected_revision
  FROM operations WHERE transaction_id = $1 ORDER BY id`

	// selectGroupAccounts reads every involved account (balance, version, limits,
	// external id) in ascending id order — one query, deterministic order.
	selectGroupAccounts = `SELECT id, confirmed_balance, version, min_balance, max_balance, external_id
  FROM accounts WHERE id = ANY($1) ORDER BY id`

	// Guard 2 for a group (spec §7.2): the conditional PENDING flips of every leg
	// and of the transaction row. Each carries a rowcount check — the legs must all
	// still be PENDING (the two leg flips' rowcounts sum to the leg count: regular
	// legs, edit_of IS NULL, and edit-class legs — edits and deletes alike,
	// edit_of IS NOT NULL — each matching its own count) and the transaction must
	// still be PENDING (rowcount == 1), else the group was decided under us ⇒
	// rollback. Regular legs flip to CONFIRMED, edit-class legs to APPLIED
	// (ADR-0010/0011: an edit or delete registration is never CONFIRMED).
	guardFlipLegsConfirmed = `UPDATE operations SET status = 'CONFIRMED', confirmed_at = now()
 WHERE transaction_id = $1 AND status = 'PENDING' AND edit_of IS NULL`
	guardFlipEditLegsApplied = `UPDATE operations SET status = 'APPLIED', confirmed_at = now()
 WHERE transaction_id = $1 AND status = 'PENDING' AND edit_of IS NOT NULL`
	// The reject flips carry the same split: a rejected edit leg stamps its
	// decision instant (confirmed_at is the decision instant of every decided edit,
	// applied or not — the history endpoint reports it as decided_at), a rejected
	// regular leg does not, exactly as for singles.
	guardFlipLegsInvalid = `UPDATE operations SET status = 'INVALID', invalidation_reason = $2
 WHERE transaction_id = $1 AND status = 'PENDING' AND edit_of IS NULL`
	guardFlipEditLegsInvalid = `UPDATE operations SET status = 'INVALID', invalidation_reason = $2, confirmed_at = now()
 WHERE transaction_id = $1 AND status = 'PENDING' AND edit_of IS NOT NULL`
	guardFlipTxCommitted = `UPDATE transactions SET status = 'COMMITTED', decided_at = now()
 WHERE id = $1 AND status = 'PENDING'`
	guardFlipTxRejected = `UPDATE transactions SET status = 'REJECTED', reject_reason = $2, decided_at = now()
 WHERE id = $1 AND status = 'PENDING'`
)

// groupLeg is the slice of an operations row the group path needs. EditOf set
// marks an edit-class leg: a plain edit (ADR-0010), whose AccountID, Amount and
// EffectiveAt are the full proposed state for the target, or — with IsDelete —
// a delete (ADR-0011), whose copy of those columns the decision never reads.
// ExpectedRevision is the optional guard of either kind.
type groupLeg struct {
	ID               int64
	AccountID        int64
	Amount           int64
	EffectiveAt      time.Time
	Status           string
	EditOf           *int64
	IsDelete         bool
	ExpectedRevision *int32
}

// isEdit reports whether the leg is an edit-class registration (a plain edit or
// a delete) — the class the Guard 2 split flips to APPLIED.
func (l groupLeg) isEdit() bool { return l.EditOf != nil }

// isDelete reports whether the leg is a delete registration.
func (l groupLeg) isDelete() bool { return l.EditOf != nil && l.IsDelete }

// groupEdit pairs an edit-class leg (edit or delete) with its target as read
// inside the deciding transaction — what the apply path needs for the history
// row and revision CAS of an edit, the delete CAS of a delete, and the snapshot
// legs of either.
type groupEdit struct {
	leg    groupLeg
	target targetState
}

// groupAccount is an involved account plus the group's net amount on it. Netting
// per account is sound because all legs apply in one DB transaction (spec §6/G2).
type groupAccount struct {
	ID         int64
	Balance    int64
	Version    int64
	MinBalance *int64
	MaxBalance *int64
	ExternalID string
	Net        int64
}

// processGroup decides one group in its own transaction. It is the single-decision
// entry point used outside batching (and by the internal tests); the batching path
// (spec §8.5) calls processGroupTx directly inside a shared batch transaction.
func (p *Processor) processGroup(ctx context.Context, txID int64) error {
	return p.withBatchTx(ctx, func(tx pgx.Tx) error {
		return p.processGroupTx(ctx, tx, txID)
	})
}

// processGroupTx decides one whole group inside the caller's transaction (spec
// §8.3): load all legs by transaction_id, cross-check op_count, expand every leg
// into virtual legs (one per regular leg, two per edit leg — −current on the
// target as read, +proposed — ADR-0010, one per delete leg — −current on the
// target as read — ADR-0011), net per account, validate every involved account,
// then all-or-nothing — all pass: apply every account's net (each under the
// Guard 3 version CAS), append each edit target's superseded state and overwrite
// it under the revision CAS, flip each delete target CONFIRMED → DELETED under
// the delete CAS, snapshot every virtual leg, flip regular legs to CONFIRMED,
// edit-class legs to APPLIED and the transaction to COMMITTED (Guard 2); any
// fail: flip all legs to INVALID and the transaction to REJECTED with one shared
// reason (Guard 2), no balance, snapshot, revision or target write. An edit-class
// leg whose target is still PENDING defers the whole group (errDeferred); a
// target that ended INVALID or DELETED, or is not at the expected revision,
// rejects the whole group with TARGET_NOT_EDITABLE / STALE_REVISION (Safety
// Invariant 6). Group atomicity is simply the DB transaction. Any guard miss
// returns errGuardMiss, rolling the transaction back.
func (p *Processor) processGroupTx(ctx context.Context, tx pgx.Tx, txID int64) error {
	// Guard 1: lease fence (spec §7.2) — evaluated per decision even within a batch.
	var fenced int
	err := tx.QueryRow(ctx, guardLeaseFence, p.lease.Owner()).Scan(&fenced)
	if errors.Is(err, pgx.ErrNoRows) {
		return errGuardMiss
	}
	if err != nil {
		return fmt.Errorf("guard 1 (lease fence): %w", err)
	}

	// Load the transaction row: declared op_count and current status.
	var (
		opCount  int
		txStatus string
	)
	if err := tx.QueryRow(ctx, selectTransaction, txID).Scan(&opCount, &txStatus); err != nil {
		return fmt.Errorf("read transaction %d: %w", txID, err)
	}
	// Skip-by-status (spec §8.1): a group already decided in an earlier batch or
	// recovered after a crash — its legs are non-PENDING, so this is a no-op.
	if txStatus != string(model.TxPending) {
		return nil
	}

	// Load all legs and cross-check completeness against op_count (spec §8.3).
	legs, err := p.loadGroupLegs(ctx, tx, txID)
	if err != nil {
		return err
	}
	if len(legs) != opCount {
		return fmt.Errorf("group %d: loaded %d legs but op_count is %d", txID, len(legs), opCount)
	}
	// Defensive skip-by-status at leg granularity. Legs flip atomically with the
	// transaction, so a PENDING transaction always has all-PENDING legs; a decided
	// leg here means the group was already decided.
	for _, l := range legs {
		if l.Status != string(model.OpPending) {
			return nil
		}
	}

	// Read every edit-class leg's target inside this transaction. Deferral is
	// decided over the whole group first — any PENDING target defers every leg —
	// then the target-state rejections in leg order, so the outcome never depends
	// on which leg happens to come first.
	edits, err := p.readGroupTargets(ctx, tx, legs)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent target mutation to exercise the revision CAS.
	if p.afterTargetRead != nil && len(edits) > 0 {
		p.afterTargetRead()
	}

	for _, e := range edits {
		if e.target.Status == string(model.OpPending) {
			return errDeferred
		}
	}
	for _, e := range edits {
		rej, err := checkTarget(*e.leg.EditOf, e.target, e.leg.ExpectedRevision)
		if err != nil {
			return err
		}
		if rej != nil {
			return p.rejectGroup(ctx, tx, txID, legs, *rej)
		}
	}

	// Net per account over the virtual legs, then read and validate every
	// involved account in ascending id order — the first violation
	// (deterministic) rejects the whole group.
	accts, err := p.netAndReadAccounts(ctx, tx, expandGroup(legs, edits))
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent account mutation to exercise Guard 3.
	if p.afterAccountRead != nil {
		p.afterAccountRead()
	}

	for _, a := range accts {
		if side, shortfall, bad := violates(a.Balance+a.Net, a.MinBalance, a.MaxBalance); bad {
			return p.rejectGroup(ctx, tx, txID, legs, model.Rejection{
				Code:      model.ReasonLimitViolated,
				Account:   a.ExternalID,
				LimitSide: side,
				Shortfall: shortfall,
			})
		}
	}
	return p.commitGroup(ctx, tx, txID, legs, edits, accts)
}

// readGroupTargets reads the target of every edit-class leg — edits and deletes
// alike — in leg order. Regular legs contribute nothing; a group without
// edit-class legs reads nothing.
func (p *Processor) readGroupTargets(ctx context.Context, tx pgx.Tx, legs []groupLeg) ([]groupEdit, error) {
	var edits []groupEdit
	for _, l := range legs {
		if !l.isEdit() {
			continue
		}
		target, err := p.readTarget(ctx, tx, *l.EditOf)
		if err != nil {
			return nil, err
		}
		edits = append(edits, groupEdit{leg: l, target: target})
	}
	return edits, nil
}

// loadGroupLegs reads every leg of the transaction in id order.
func (p *Processor) loadGroupLegs(ctx context.Context, tx pgx.Tx, txID int64) ([]groupLeg, error) {
	rows, err := tx.Query(ctx, selectGroupLegs, txID)
	if err != nil {
		return nil, fmt.Errorf("load group %d legs: %w", txID, err)
	}
	defer rows.Close()

	var legs []groupLeg
	for rows.Next() {
		var l groupLeg
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Amount, &l.EffectiveAt, &l.Status, &l.EditOf, &l.IsDelete, &l.ExpectedRevision); err != nil {
			return nil, fmt.Errorf("scan group %d leg: %w", txID, err)
		}
		legs = append(legs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group %d legs: %w", txID, err)
	}
	return legs, nil
}

// expandGroup turns a group's legs into virtual legs in leg order: one per
// regular leg, two per edit leg (−current on the target as read, +proposed —
// expandEdit, ADR-0010), one per delete leg (−current on the target as read —
// expandDelete, ADR-0011). edits holds the targets of the edit-class legs in
// the same leg order. Pure: shared by the decision and the unit tests (UT-011).
func expandGroup(legs []groupLeg, edits []groupEdit) []virtualLeg {
	out := make([]virtualLeg, 0, len(legs)+len(edits))
	next := 0
	for _, l := range legs {
		if !l.isEdit() {
			out = append(out, virtualLeg{AccountID: l.AccountID, Amount: l.Amount, EffectiveAt: l.EffectiveAt})
			continue
		}
		e := edits[next]
		next++
		if l.isDelete() {
			out = append(out, expandDelete(e.target)...)
			continue
		}
		out = append(out, expandEdit(e.target, pendingOp{
			ID: l.ID, AccountID: l.AccountID, Amount: l.Amount, EffectiveAt: l.EffectiveAt,
			EditOf: l.EditOf, ExpectedRevision: l.ExpectedRevision,
		})...)
	}
	return out
}

// netLegs computes the net amount per involved account over a unit's virtual
// legs (spec §6) and returns the distinct account ids in ascending order — the
// deterministic validation and CAS order. Pure: shared by the group path, the
// edit path and the unit tests.
func netLegs(legs []virtualLeg) (ids []int64, net map[int64]int64) {
	net = make(map[int64]int64, len(legs))
	ids = make([]int64, 0, len(legs))
	for _, l := range legs {
		if _, seen := net[l.AccountID]; !seen {
			ids = append(ids, l.AccountID)
		}
		net[l.AccountID] += l.Amount
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, net
}

// netAndReadAccounts nets the unit's virtual legs per involved account (netLegs)
// and reads those accounts, returning them in ascending id order with their net
// attached. The distinct account set drives one indexed read.
func (p *Processor) netAndReadAccounts(ctx context.Context, tx pgx.Tx, legs []virtualLeg) ([]groupAccount, error) {
	ids, net := netLegs(legs)

	rows, err := tx.Query(ctx, selectGroupAccounts, ids)
	if err != nil {
		return nil, fmt.Errorf("read group accounts: %w", err)
	}
	defer rows.Close()

	accts := make([]groupAccount, 0, len(ids))
	for rows.Next() {
		var a groupAccount
		if err := rows.Scan(&a.ID, &a.Balance, &a.Version, &a.MinBalance, &a.MaxBalance, &a.ExternalID); err != nil {
			return nil, fmt.Errorf("scan group account: %w", err)
		}
		a.Net = net[a.ID]
		accts = append(accts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group accounts: %w", err)
	}
	if len(accts) != len(ids) {
		return nil, fmt.Errorf("read %d of %d involved accounts", len(accts), len(ids))
	}
	return accts, nil
}

// commitGroup applies an accepted group: Guard 2 flips every regular leg to
// CONFIRMED, every edit-class leg to APPLIED and the transaction to COMMITTED;
// for each edit leg the superseded state is appended to operation_revisions and
// the target overwritten under the revision CAS (ADR-0010); for each delete leg
// the target flips CONFIRMED → DELETED under the delete CAS, stamped with the
// leg as deleted_by (ADR-0011); Guard 3 applies each account's net — over all
// virtual legs, once per account — under its version CAS; snapshots update per
// regular leg, per coalesced edit leg pair and per delete leg; then the outcome
// is notified — all in the caller's transaction, so it is atomic (spec §8.3/G2,
// Safety Invariants 1, 2, 5, 6).
func (p *Processor) commitGroup(ctx context.Context, tx pgx.Tx, txID int64, legs []groupLeg, edits []groupEdit, accts []groupAccount) error {
	// Guard 2: flip the regular legs to CONFIRMED and the edit-class legs (edits
	// and deletes) to APPLIED; the two rowcounts must each match their leg count
	// (summing to all legs).
	nEdits := len(edits)
	tag, err := tx.Exec(ctx, guardFlipLegsConfirmed, txID)
	if err != nil {
		return fmt.Errorf("guard 2 (confirm legs of group %d): %w", txID, err)
	}
	if int(tag.RowsAffected()) != len(legs)-nEdits {
		return errGuardMiss
	}
	if nEdits > 0 {
		tag, err = tx.Exec(ctx, guardFlipEditLegsApplied, txID)
		if err != nil {
			return fmt.Errorf("guard 2 (apply edit legs of group %d): %w", txID, err)
		}
		if int(tag.RowsAffected()) != nEdits {
			return errGuardMiss
		}
	}

	// Guard 2: flip the transaction to COMMITTED.
	tag, err = tx.Exec(ctx, guardFlipTxCommitted, txID)
	if err != nil {
		return fmt.Errorf("guard 2 (commit transaction %d): %w", txID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Per edit-class leg, in leg order. An edit leg: history first, then the
	// revision CAS on the target — the same two writes the single-edit path makes
	// (applyEdit). A delete leg: the delete CAS alone — the same write the
	// single-delete path makes (applyDelete); nothing is appended and revision is
	// untouched. Two legs never share a target (ErrDuplicateEditTarget at
	// registration), so each CAS runs against the revision this transaction read.
	for _, e := range edits {
		targetID := *e.leg.EditOf
		t := e.target
		if e.leg.isDelete() {
			tag, err := tx.Exec(ctx, guardDeleteCAS, targetID, e.leg.ID, t.Revision)
			if err != nil {
				return fmt.Errorf("delete CAS on operation %d (group %d): %w", targetID, txID, err)
			}
			if tag.RowsAffected() != 1 {
				return errGuardMiss
			}
			continue
		}
		if _, err := tx.Exec(ctx, insertRevision,
			targetID, t.Revision, t.AccountID, t.Amount, t.EffectiveAt, t.recordedAt(), e.leg.ID); err != nil {
			return fmt.Errorf("append revision %d of operation %d (group %d): %w", t.Revision, targetID, txID, err)
		}
		tag, err := tx.Exec(ctx, guardRevisionCAS, targetID, e.leg.AccountID, e.leg.Amount, e.leg.EffectiveAt, t.Revision)
		if err != nil {
			return fmt.Errorf("revision CAS on operation %d (group %d): %w", targetID, txID, err)
		}
		if tag.RowsAffected() != 1 {
			return errGuardMiss
		}
	}

	// Guard 3: per-account version CAS with the account's net of all virtual legs.
	for _, a := range accts {
		tag, err := tx.Exec(ctx, guardAccountCAS, a.ID, a.Net, a.Version)
		if err != nil {
			return fmt.Errorf("guard 3 (account %d CAS in group %d): %w", a.ID, txID, err)
		}
		if tag.RowsAffected() != 1 {
			return errGuardMiss
		}
	}

	// Snapshots (spec §8.4): one per regular leg with the leg's own amount and
	// day; an edit leg's two virtual legs after same-bucket coalescing, observed
	// once per edit as on the single path; a delete leg's one virtual leg as-is
	// (−current at the target's instant — there is no pair to coalesce), as on
	// the single-delete path. The snapshot deltas on an account sum to the net
	// applied above.
	next := 0
	for _, l := range legs {
		if !l.isEdit() {
			rows, err := snapshot.Apply(ctx, tx, l.AccountID, l.EffectiveAt, l.Amount)
			if err != nil {
				return err
			}
			p.recordSnapshotRows(rows)
			continue
		}
		e := edits[next]
		next++
		if l.isDelete() {
			v := expandDelete(e.target)[0]
			rows, err := snapshot.Apply(ctx, tx, v.AccountID, v.EffectiveAt, v.Amount)
			if err != nil {
				return err
			}
			p.recordSnapshotRows(rows)
			continue
		}
		old := virtualLeg{AccountID: e.target.AccountID, Amount: -e.target.Amount, EffectiveAt: e.target.EffectiveAt}
		cur := virtualLeg{AccountID: l.AccountID, Amount: l.Amount, EffectiveAt: l.EffectiveAt}
		var rows int64
		for _, v := range coalesceSnapshotLegs(old, cur) {
			n, err := snapshot.Apply(ctx, tx, v.AccountID, v.EffectiveAt, v.Amount)
			if err != nil {
				return err
			}
			rows += n
		}
		p.recordSnapshotRows(rows)
	}
	p.recordDecision(obs.KindGroup, obs.OutcomeCommitted)

	return notifyTx(ctx, tx, txID)
}

// rejectGroup rejects a group: Guard 2 flips every leg to INVALID and the
// transaction to REJECTED with the one machine-readable reason every leg shares
// (an offending account + shortfall, or an edit or delete target that is not
// editable / not at the expected revision). No account, snapshot or revision is
// written, so there is no Guard 3 and every target keeps its current row (spec
// §8.3).
func (p *Processor) rejectGroup(ctx context.Context, tx pgx.Tx, txID int64, legs []groupLeg, reason model.Rejection) error {
	detail, err := reason.Marshal()
	if err != nil {
		return err
	}
	nEdits := 0
	for _, l := range legs {
		if l.isEdit() {
			nEdits++
		}
	}

	// Guard 2: flip the regular legs and the edit-class legs to INVALID; the two
	// rowcounts must each match their leg count (summing to all legs).
	tag, err := tx.Exec(ctx, guardFlipLegsInvalid, txID, detail)
	if err != nil {
		return fmt.Errorf("guard 2 (invalidate legs of group %d): %w", txID, err)
	}
	if int(tag.RowsAffected()) != len(legs)-nEdits {
		return errGuardMiss
	}
	if nEdits > 0 {
		tag, err = tx.Exec(ctx, guardFlipEditLegsInvalid, txID, detail)
		if err != nil {
			return fmt.Errorf("guard 2 (invalidate edit legs of group %d): %w", txID, err)
		}
		if int(tag.RowsAffected()) != nEdits {
			return errGuardMiss
		}
	}

	// Guard 2: flip the transaction to REJECTED.
	tag, err = tx.Exec(ctx, guardFlipTxRejected, txID, detail)
	if err != nil {
		return fmt.Errorf("guard 2 (reject transaction %d): %w", txID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}
	p.recordDecision(obs.KindGroup, obs.OutcomeRejected)

	return notifyTx(ctx, tx, txID)
}

// notifyTx emits the outcome doorbell for one group transaction (ADR-0002). It runs
// inside the deciding transaction, so Postgres delivers it only on commit.
func notifyTx(ctx context.Context, tx pgx.Tx, txID int64) error {
	if _, err := tx.Exec(ctx, notifyOutcome, fmt.Sprintf("tx:%d", txID)); err != nil {
		return fmt.Errorf("notify outcome tx:%d: %w", txID, err)
	}
	return nil
}
