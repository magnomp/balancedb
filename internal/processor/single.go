package processor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/snapshot"
)

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	// Guard 1: lease fence (spec §7.2). A separate in-transaction check against the
	// DB clock — NOT lease.Acquire. Empty result ⇒ this instance is no longer the
	// leader ⇒ rollback.
	guardLeaseFence = `SELECT 1 FROM leader_lease WHERE owner = $1::uuid AND lease_until > now()`

	// Account read: balance, version (for the Guard 3 CAS), limits, and external id
	// (for the rejection detail). Plain SELECT — the version CAS is the concurrency
	// control, not a row lock.
	selectAccount = `SELECT confirmed_balance, version, min_balance, max_balance, external_id
  FROM accounts WHERE id = $1`

	// Guard 2: conditional status flip (spec §7.2). PENDING→CONFIRMED|INVALID only
	// while still PENDING — idempotent under retry and crash recovery. A rowcount
	// other than 1 ⇒ already decided ⇒ rollback.
	guardFlipConfirmed = `UPDATE operations SET status = 'CONFIRMED', confirmed_at = now()
 WHERE id = $1 AND status = 'PENDING'`
	guardFlipInvalid = `UPDATE operations SET status = 'INVALID', invalidation_reason = $2
 WHERE id = $1 AND status = 'PENDING'`

	// Guard 3: account-version CAS (spec §7.2). Closes the fence-to-commit window
	// and arbitrates API races. 0 rows ⇒ the version moved under us ⇒ rollback.
	guardAccountCAS = `UPDATE accounts SET confirmed_balance = confirmed_balance + $2, version = version + 1
 WHERE id = $1 AND version = $3`

	// Outcome fan-out (ADR-0002), emitted inside the deciding commit so it is
	// delivered only if the decision commits; API waiters (M8) demultiplex it.
	notifyOutcome = `SELECT pg_notify('outcomes', $1)`
)

// processSingle decides one single operation in its own transaction. It is the
// single-decision entry point used outside batching (and by the internal tests);
// the batching path (spec §8.5) calls processSingleTx directly inside a shared
// batch transaction. Both wrap the same guarded body.
func (p *Processor) processSingle(ctx context.Context, op pendingOp) error {
	return db.WithTx(ctx, p.pool, func(tx pgx.Tx) error {
		return p.processSingleTx(ctx, tx, op)
	})
}

// processSingleTx decides one single operation inside the caller's transaction,
// implementing spec §8.2 with all three guards of §7.2 verbatim, each
// rowcount-checked. On accept it flips the operation to CONFIRMED (Guard 2),
// applies the amount to the account under the version CAS (Guard 3), updates the
// daily snapshots, and notifies the outcome. On reject it flips the operation to
// INVALID with the machine-readable reason (Guard 2) and notifies — no balance
// change. Any guard miss returns errGuardMiss, which rolls the (possibly batched)
// transaction back.
func (p *Processor) processSingleTx(ctx context.Context, tx pgx.Tx, op pendingOp) error {
	// Guard 1: lease fence.
	var fenced int
	err := tx.QueryRow(ctx, guardLeaseFence, p.lease.Owner()).Scan(&fenced)
	if errors.Is(err, pgx.ErrNoRows) {
		return errGuardMiss
	}
	if err != nil {
		return fmt.Errorf("guard 1 (lease fence): %w", err)
	}

	// Read the account for validation and the version to CAS on.
	var (
		balance    int64
		version    int64
		minBalance *int64
		maxBalance *int64
		externalID string
	)
	if err := tx.QueryRow(ctx, selectAccount, op.AccountID).
		Scan(&balance, &version, &minBalance, &maxBalance, &externalID); err != nil {
		return fmt.Errorf("read account %d: %w", op.AccountID, err)
	}

	// Test seam: inject a concurrent account mutation to exercise Guard 3.
	if p.afterAccountRead != nil {
		p.afterAccountRead()
	}

	// Binary validation (spec §6): the operation's effect on the FINAL balance
	// is its amount, wherever it lands on the timeline.
	if side, shortfall, bad := violates(balance+op.Amount, minBalance, maxBalance); bad {
		return p.reject(ctx, tx, op, externalID, side, shortfall)
	}
	return p.accept(ctx, tx, op, version)
}

func (p *Processor) accept(ctx context.Context, tx pgx.Tx, op pendingOp, version int64) error {
	// Guard 2: conditional flip to CONFIRMED.
	tag, err := tx.Exec(ctx, guardFlipConfirmed, op.ID)
	if err != nil {
		return fmt.Errorf("guard 2 (confirm flip): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Guard 3: versioned account balance update.
	tag, err = tx.Exec(ctx, guardAccountCAS, op.AccountID, op.Amount, version)
	if err != nil {
		return fmt.Errorf("guard 3 (account version CAS): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Snapshots (spec §8.4), in the same commit as the balance change.
	if err := snapshot.Apply(ctx, tx, op.AccountID, op.EffectiveAt, op.Amount); err != nil {
		return err
	}

	return notifyOp(ctx, tx, op.ID)
}

func (p *Processor) reject(ctx context.Context, tx pgx.Tx, op pendingOp, externalID string, side model.LimitSide, shortfall int64) error {
	reason := model.Rejection{
		Code:      model.ReasonLimitViolated,
		Account:   externalID,
		LimitSide: side,
		Shortfall: shortfall,
	}
	detail, err := reason.Marshal()
	if err != nil {
		return err
	}

	// Guard 2: conditional flip to INVALID (terminal). No account write, so no
	// Guard 3 — the reject path leaves the balance untouched.
	tag, err := tx.Exec(ctx, guardFlipInvalid, op.ID, detail)
	if err != nil {
		return fmt.Errorf("guard 2 (invalidate flip): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	return notifyOp(ctx, tx, op.ID)
}

// violates applies the binary limit check (spec §6): min <= newBalance <= max,
// unbounded (NULL) sides skipped. It returns the crossed side and a positive
// shortfall magnitude, or bad=false when the candidate is within limits. A single
// operation can cross at most one side.
func violates(newBalance int64, minBalance, maxBalance *int64) (side model.LimitSide, shortfall int64, bad bool) {
	if maxBalance != nil && newBalance > *maxBalance {
		return model.LimitMax, newBalance - *maxBalance, true
	}
	if minBalance != nil && newBalance < *minBalance {
		return model.LimitMin, *minBalance - newBalance, true
	}
	return "", 0, false
}

// notifyOp emits the outcome doorbell for one operation (ADR-0002). It runs inside
// the deciding transaction, so Postgres delivers it only on commit.
func notifyOp(ctx context.Context, tx pgx.Tx, opID int64) error {
	if _, err := tx.Exec(ctx, notifyOutcome, fmt.Sprintf("op:%d", opID)); err != nil {
		return fmt.Errorf("notify outcome op:%d: %w", opID, err)
	}
	return nil
}
