package processor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
	"github.com/magnomp/balancedb/internal/snapshot"
)

// This file is the single-delete decision path (ADR-0011, spec §8.2 for deletes).
// A delete registration is an edit-class PENDING row (edit_of set, is_delete) whose
// proposed state is "none": the leader decides it as one virtual leg — −current
// on the target's current account at its current instant, read inside the
// deciding transaction — runs the unchanged §6 check, and on accept flips the
// target CONFIRMED → DELETED under the revision CAS, stamping deleted_by and
// deleted_at. No revision row is appended and revision is not bumped: the row's
// last values are its final state. The three guards of §7.2 stay verbatim; the
// delete CAS is the fourth rowcount-checked write, never a replacement for one.
// Target read, target-state rules and rejection are the edit helpers (edit.go).
// The group path (group.go) reuses expandDelete and guardDeleteCAS per delete
// leg.

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	// Delete CAS (ADR-0011): flip the target to DELETED only if it still holds the
	// revision that was read and is still CONFIRMED. 0 rows ⇒ the target moved
	// under us (a concurrent edit bumped revision, or a concurrent delete already
	// flipped it) ⇒ rollback, re-decide next cycle. Only status, deleted_by and
	// deleted_at change, and only together (Safety Invariant 2); account_id,
	// amount, effective_at and revision are never touched again. Shared with the
	// group path, which runs it once per delete leg (commitGroup).
	guardDeleteCAS = `UPDATE operations SET status = 'DELETED', deleted_by = $2, deleted_at = now()
 WHERE id = $1 AND revision = $3 AND status = 'CONFIRMED'`
)

// processSingleDeleteTx decides one single delete registration inside the
// caller's transaction, after Guard 1 has passed in processSingleTx. It reads the
// target, defers (errDeferred) while the target is still PENDING, rejects a
// terminal (INVALID or DELETED → TARGET_NOT_EDITABLE) or stale target, otherwise
// nets the one virtual leg, validates the involved account (spec §6) and applies
// or rejects. Any guard miss — including the delete CAS — returns errGuardMiss,
// rolling the (possibly batched) transaction back.
func (p *Processor) processSingleDeleteTx(ctx context.Context, tx pgx.Tx, op pendingOp) error {
	targetID := *op.EditOf
	target, err := p.readTarget(ctx, tx, targetID)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent target mutation to exercise the delete CAS.
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

	legs := expandDelete(target)
	accts, err := p.netAndReadAccounts(ctx, tx, legs)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent account mutation to exercise Guard 3.
	if p.afterAccountRead != nil {
		p.afterAccountRead()
	}

	// Binary validation (spec §6) on the involved account's net — removing a spent
	// credit is a debit without funds — with the same shape as every other unit.
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
	return p.applyDelete(ctx, tx, op, target, legs[0], accts)
}

// expandDelete returns the single virtual leg of a deletion: −current on the
// target's current account at its current instant, as read in the deciding
// transaction — never the delete row's informational copy.
func expandDelete(t targetState) []virtualLeg {
	return []virtualLeg{{AccountID: t.AccountID, Amount: -t.Amount, EffectiveAt: t.EffectiveAt}}
}

// applyDelete accepts a delete: Guard 2 flips the delete row to APPLIED, the
// target flips CONFIRMED → DELETED under the delete CAS (deleted_by = the delete
// row, deleted_at = now()), Guard 3 applies the involved account's net under its
// version CAS, exactly one snapshot apply removes the amount at the target's
// instant, and the outcome is notified — all in the caller's transaction (Safety
// Invariants 1, 2, 7). Nothing is appended to operation_revisions and the
// target's revision is untouched.
func (p *Processor) applyDelete(ctx context.Context, tx pgx.Tx, op pendingOp, t targetState, leg virtualLeg, accts []groupAccount) error {
	targetID := *op.EditOf

	// Guard 2: conditional flip of the delete row to APPLIED.
	tag, err := tx.Exec(ctx, guardFlipApplied, op.ID)
	if err != nil {
		return fmt.Errorf("guard 2 (apply flip of delete %d): %w", op.ID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Delete CAS: flip the target only at the revision that was read.
	tag, err = tx.Exec(ctx, guardDeleteCAS, targetID, op.ID, t.Revision)
	if err != nil {
		return fmt.Errorf("delete CAS on operation %d: %w", targetID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Guard 3: per-account version CAS with the account's net of the virtual leg.
	for _, a := range accts {
		tag, err := tx.Exec(ctx, guardAccountCAS, a.ID, a.Net, a.Version)
		if err != nil {
			return fmt.Errorf("guard 3 (account %d CAS for delete %d): %w", a.ID, op.ID, err)
		}
		if tag.RowsAffected() != 1 {
			return errGuardMiss
		}
	}

	// Snapshots (spec §8.4): one apply of −amount at the target's instant; there
	// is no second leg to coalesce with.
	rows, err := snapshot.Apply(ctx, tx, leg.AccountID, leg.EffectiveAt, leg.Amount)
	if err != nil {
		return err
	}
	p.recordSnapshotRows(rows)
	p.recordDecision(obs.KindDelete, obs.OutcomeApplied)

	return notifyOp(ctx, tx, op.ID)
}
