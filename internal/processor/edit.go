package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
	"github.com/magnomp/balancedb/internal/snapshot"
)

// This file is the single-edit decision path (ADR-0010, spec §8.2 for edits) and
// the edit helpers the group path (group.go) shares — target read, target-state
// rules, virtual-leg expansion, history insert, revision CAS and snapshot
// coalescing. An edit registration is a PENDING operations row with edit_of set
// and the full proposed state; the leader decides it as two virtual legs —
// −current on the current account at the current instant, +proposed on the
// proposed account at the proposed instant — nets them per account, runs the
// unchanged §6 check, and on accept overwrites the target's current columns under
// a revision CAS while appending the superseded state to the append-only
// operation_revisions table. The three guards of §7.2 stay verbatim; the revision
// CAS is a fourth rowcount-checked write, never a replacement for one.

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	// selectEditTarget reads the target's current state (the −old virtual leg),
	// its status and revision (deferral / TARGET_NOT_EDITABLE / STALE_REVISION and
	// the revision CAS), and the two instants that decide the history row's
	// recorded_at. Plain SELECT — the revision CAS is the concurrency control.
	selectEditTarget = `SELECT account_id, amount, effective_at, status, revision, registered_at, revised_at
  FROM operations WHERE id = $1`

	// Guard 2 for an edit registration (spec §7.2): PENDING → APPLIED only while
	// still PENDING; rowcount other than 1 ⇒ already decided ⇒ rollback. An edit
	// row is never CONFIRMED, so every existing status = 'CONFIRMED' read keeps
	// excluding it without a query change.
	guardFlipApplied = `UPDATE operations SET status = 'APPLIED', confirmed_at = now()
 WHERE id = $1 AND status = 'PENDING'`

	// Guard 2 for a rejected edit registration: PENDING → INVALID with the reason.
	// Unlike a rejected regular operation, an edit row stamps its decision instant
	// (confirmed_at is the decision instant of every decided edit, applied or not)
	// so the history endpoint can report decided_at for rejected edits.
	guardFlipEditInvalid = `UPDATE operations SET status = 'INVALID', invalidation_reason = $2, confirmed_at = now()
 WHERE id = $1 AND status = 'PENDING'`

	// insertRevision appends the superseded state (Safety Invariant 3): only this
	// path writes operation_revisions and nothing updates or deletes it. recorded_at
	// is when the superseded revision became current — COALESCE(revised_at,
	// registered_at) of the target as read; superseded_at defaults to now().
	insertRevision = `INSERT INTO operation_revisions (operation_id, revision, account_id, amount, effective_at, recorded_at, superseded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// Revision CAS (ADR-0010): overwrite the target's current columns only if it
	// still holds the revision that was read and is still CONFIRMED. 0 rows ⇒ the
	// target moved under us (a concurrent apply) ⇒ rollback, re-decide next cycle.
	// Only account_id, amount, effective_at, revision and revised_at change, and
	// only together (Safety Invariant 5).
	guardRevisionCAS = `UPDATE operations
   SET account_id = $2, amount = $3, effective_at = $4, revision = revision + 1, revised_at = now()
 WHERE id = $1 AND revision = $5 AND status = 'CONFIRMED'`
)

// virtualLeg is one balance movement a unit implies: a regular operation is one
// leg, an edit is two (−current, +proposed). Netting virtual legs per account is
// what the §6 check and the Guard 3 CAS operate on.
type virtualLeg struct {
	AccountID   int64
	Amount      int64
	EffectiveAt time.Time
}

// targetState is the edit target's current row as read inside the deciding
// transaction — the source of the −current leg, the revision to CAS on, and the
// recorded_at of the history row.
type targetState struct {
	AccountID    int64
	Amount       int64
	EffectiveAt  time.Time
	Status       string
	Revision     int32
	RegisteredAt time.Time
	RevisedAt    *time.Time
}

// recordedAt is when the target's current revision became current: revised_at
// once it has been edited, its registration instant before that.
func (t targetState) recordedAt() time.Time {
	if t.RevisedAt != nil {
		return *t.RevisedAt
	}
	return t.RegisteredAt
}

// processSingleEditTx decides one single edit registration inside the caller's
// transaction, after Guard 1 has passed in processSingleTx. It reads the target,
// defers (errDeferred) while the target is still PENDING, rejects a terminal or
// stale target, otherwise nets the two virtual legs, validates every involved
// account (spec §6) and applies or rejects. Any guard miss — including the
// revision CAS — returns errGuardMiss, rolling the (possibly batched)
// transaction back.
func (p *Processor) processSingleEditTx(ctx context.Context, tx pgx.Tx, op pendingOp) error {
	targetID := *op.EditOf
	target, err := p.readTarget(ctx, tx, targetID)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent target mutation to exercise the revision CAS.
	if p.afterTargetRead != nil {
		p.afterTargetRead()
	}

	rej, err := checkTarget(targetID, target, op.ExpectedRevision)
	if err != nil {
		return err
	}
	if rej != nil {
		return p.rejectEdit(ctx, tx, op, *rej)
	}

	legs := expandEdit(target, op)
	accts, err := p.netAndReadAccounts(ctx, tx, legs)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent account mutation to exercise Guard 3.
	if p.afterAccountRead != nil {
		p.afterAccountRead()
	}

	// Binary validation (spec §6) on each involved account's net; the first
	// violation in ascending account id rejects, exactly as for a group.
	for _, a := range accts {
		if side, shortfall, bad := violates(a.Balance+a.Net, a.MinBalance, a.MaxBalance); bad {
			return p.rejectEdit(ctx, tx, op, model.Rejection{
				Code:      model.ReasonLimitViolated,
				Account:   a.ExternalID,
				LimitSide: side,
				Shortfall: shortfall,
			})
		}
	}
	return p.applyEdit(ctx, tx, op, target, legs, accts)
}

// readTarget reads the edit target's current row inside the deciding transaction.
func (p *Processor) readTarget(ctx context.Context, tx pgx.Tx, targetID int64) (targetState, error) {
	var t targetState
	if err := tx.QueryRow(ctx, selectEditTarget, targetID).
		Scan(&t.AccountID, &t.Amount, &t.EffectiveAt, &t.Status, &t.Revision, &t.RegisteredAt, &t.RevisedAt); err != nil {
		return targetState{}, fmt.Errorf("read edit target %d: %w", targetID, err)
	}
	return t, nil
}

// checkTarget applies the target-state rules of ADR-0010/0011 to an edit or a
// delete of targetID: a PENDING target defers the unit (errDeferred); a target
// that is not CONFIRMED (INVALID or DELETED — both terminal) rejects with
// TARGET_NOT_EDITABLE; an expected_revision that no longer matches rejects with
// STALE_REVISION. A nil rejection and nil error mean the unit may be decided on
// its limits.
func checkTarget(targetID int64, t targetState, expected *int32) (*model.Rejection, error) {
	switch model.OpStatus(t.Status) {
	case model.OpPending:
		return nil, errDeferred
	case model.OpConfirmed:
		// decidable
	default:
		id := targetID
		return &model.Rejection{Code: model.ReasonTargetNotEditable, OperationID: &id}, nil
	}
	if expected != nil && *expected != t.Revision {
		id, want, got := targetID, *expected, t.Revision
		return &model.Rejection{
			Code:             model.ReasonStaleRevision,
			OperationID:      &id,
			ExpectedRevision: &want,
			ActualRevision:   &got,
		}, nil
	}
	return nil, nil
}

// expandEdit returns the two virtual legs of an edit: −current on the target's
// current account and instant, +proposed on the edit row's account and instant.
func expandEdit(t targetState, op pendingOp) []virtualLeg {
	return []virtualLeg{
		{AccountID: t.AccountID, Amount: -t.Amount, EffectiveAt: t.EffectiveAt},
		{AccountID: op.AccountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt},
	}
}

// applyEdit accepts an edit: Guard 2 flips the edit row to APPLIED, the
// superseded state is appended to operation_revisions, the target's current
// columns are overwritten under the revision CAS, Guard 3 applies each involved
// account's net under its version CAS, the snapshots move (coalesced when both
// legs share a bucket), and the outcome is notified — all in the caller's
// transaction (Safety Invariants 1, 2, 5).
func (p *Processor) applyEdit(ctx context.Context, tx pgx.Tx, op pendingOp, t targetState, legs []virtualLeg, accts []groupAccount) error {
	targetID := *op.EditOf

	// Guard 2: conditional flip of the edit row to APPLIED.
	tag, err := tx.Exec(ctx, guardFlipApplied, op.ID)
	if err != nil {
		return fmt.Errorf("guard 2 (apply flip of edit %d): %w", op.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// History first, then the overwrite: the superseded revision row and the
	// target's revision + 1 commit together or not at all.
	if _, err := tx.Exec(ctx, insertRevision,
		targetID, t.Revision, t.AccountID, t.Amount, t.EffectiveAt, t.recordedAt(), op.ID); err != nil {
		return fmt.Errorf("append revision %d of operation %d: %w", t.Revision, targetID, err)
	}

	// Revision CAS: overwrite the target only at the revision that was read.
	tag, err = tx.Exec(ctx, guardRevisionCAS, targetID, op.AccountID, op.Amount, op.EffectiveAt, t.Revision)
	if err != nil {
		return fmt.Errorf("revision CAS on operation %d: %w", targetID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Guard 3: per-account version CAS with the account's net of the virtual legs.
	for _, a := range accts {
		tag, err := tx.Exec(ctx, guardAccountCAS, a.ID, a.Net, a.Version)
		if err != nil {
			return fmt.Errorf("guard 3 (account %d CAS for edit %d): %w", a.ID, op.ID, err)
		}
		if tag.RowsAffected() != 1 {
			return errGuardMiss
		}
	}

	// Snapshots (spec §8.4), one apply per virtual leg after same-bucket
	// coalescing; one observation per edit so the saving is visible in the metric.
	var rows int64
	for _, l := range coalesceSnapshotLegs(legs[0], legs[1]) {
		n, err := snapshot.Apply(ctx, tx, l.AccountID, l.EffectiveAt, l.Amount)
		if err != nil {
			return err
		}
		rows += n
	}
	p.recordSnapshotRows(rows)
	p.recordDecision(obs.KindEdit, obs.OutcomeApplied)

	return notifyOp(ctx, tx, op.ID)
}

// rejectEdit rejects an edit-class registration — a plain edit or a delete
// (ADR-0011): Guard 2 flips it to INVALID (terminal) with the machine-readable
// reason and its decision instant. Nothing else is written — no balance, no
// snapshot, no revision, and the target keeps its current row untouched. The
// decision is counted under the row's own kind.
func (p *Processor) rejectEdit(ctx context.Context, tx pgx.Tx, op pendingOp, reason model.Rejection) error {
	detail, err := reason.Marshal()
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, guardFlipEditInvalid, op.ID, detail)
	if err != nil {
		return fmt.Errorf("guard 2 (invalidate flip of edit %d): %w", op.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}
	p.recordDecision(op.decisionKind(), obs.OutcomeInvalid)

	return notifyOp(ctx, tx, op.ID)
}

// decisionKind is the §13 decision-counter kind of an edit-class row: delete for
// a delete registration, edit for a plain edit.
func (op pendingOp) decisionKind() string {
	if op.isDelete() {
		return obs.KindDelete
	}
	return obs.KindEdit
}

// snapshotDay is the UTC calendar-day bucket of a snapshot row (spec §5.2): the
// same format snapshot.Apply derives, computed from the instant alone.
const snapshotDay = "2006-01-02"

// coalesceSnapshotLegs collapses an edit's two virtual legs before snapshot.Apply
// when they land in the same snapshot bucket — same account and same UTC day of
// effective_at (a time change within the day keeps the bucket). Same bucket with
// a non-zero delta → one leg carrying new − old at the new instant, so the
// cascade touches each later day once; same bucket with a zero delta → no leg at
// all (no daily cumulative balance changes); different account or day → both
// legs, exactly the two-apply form. The collapse is an arithmetic identity on the
// cumulative daily balances, so the reference model needs no special case.
func coalesceSnapshotLegs(old, cur virtualLeg) []virtualLeg {
	if old.AccountID != cur.AccountID ||
		old.EffectiveAt.UTC().Format(snapshotDay) != cur.EffectiveAt.UTC().Format(snapshotDay) {
		return []virtualLeg{old, cur}
	}
	delta := old.Amount + cur.Amount
	if delta == 0 {
		return nil
	}
	return []virtualLeg{{AccountID: cur.AccountID, Amount: delta, EffectiveAt: cur.EffectiveAt}}
}
