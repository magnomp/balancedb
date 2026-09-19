package processor

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/magnomp/balancedb/internal/model"
	"github.com/magnomp/balancedb/internal/obs"
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
	return p.withBatchTx(ctx, func(tx pgx.Tx) error {
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
// transaction back. An edit registration (EditOf set, ADR-0010) is decided by
// processSingleEditTx and a delete registration (EditOf set with IsDelete,
// ADR-0011) by processSingleDeleteTx, both after the shared lease fence.
func (p *Processor) processSingleTx(ctx context.Context, tx pgx.Tx, op pendingOp) error {
	if op.isDelete() {
		if err := p.leaseFence(ctx, tx); err != nil {
			return err
		}
		return p.processSingleDeleteTx(ctx, tx, op)
	}
	if op.EditOf != nil {
		if err := p.leaseFence(ctx, tx); err != nil {
			return err
		}
		return p.processSingleEditTx(ctx, tx, op)
	}

	balance, version, minBalance, maxBalance, externalID, err := p.fenceAndReadAccount(ctx, tx, op.AccountID)
	if err != nil {
		return err
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

// leaseFence runs Guard 1 alone, for the edit/delete dispatch paths which go on
// to read their own target rows rather than the plain account read below.
func (p *Processor) leaseFence(ctx context.Context, tx pgx.Tx) error {
	var fenced int
	err := tx.QueryRow(ctx, guardLeaseFence, p.lease.Owner()).Scan(&fenced)
	if errors.Is(err, pgx.ErrNoRows) {
		return errGuardMiss
	}
	if err != nil {
		return fmt.Errorf("guard 1 (lease fence): %w", err)
	}
	return nil
}

// fenceAndReadAccount pipelines Guard 1 (lease fence) with the account read in
// one round trip via pgx.Batch: neither statement's parameters depend on the
// other's result — which op this is and which account it targets are already
// known in Go — so both go out together instead of as two sequential queries.
func (p *Processor) fenceAndReadAccount(ctx context.Context, tx pgx.Tx, accountID int64) (
	balance, version int64, minBalance, maxBalance *int64, externalID string, err error,
) {
	batch := &pgx.Batch{}
	batch.Queue(guardLeaseFence, p.lease.Owner())
	batch.Queue(selectAccount, accountID)
	br := tx.SendBatch(ctx, batch)
	defer func() {
		if cerr := br.Close(); err == nil {
			err = cerr
		}
	}()

	var fenced int
	if err = br.QueryRow().Scan(&fenced); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = errGuardMiss
		} else {
			err = fmt.Errorf("guard 1 (lease fence): %w", err)
		}
		return
	}

	if err = br.QueryRow().Scan(&balance, &version, &minBalance, &maxBalance, &externalID); err != nil {
		err = fmt.Errorf("read account %d: %w", accountID, err)
	}
	return
}

// accept pipelines Guard 2, Guard 3, the snapshot writes and the outcome notify
// into one round trip via pgx.Batch. None of these statements' parameters
// depend on another's return value in this batch — only their rowcounts are
// checked afterward — and a miss on any guard rolls back the whole (possibly
// batched) transaction regardless of which statement is read first, so running
// all of them before checking is safe.
func (p *Processor) accept(ctx context.Context, tx pgx.Tx, op pendingOp, version int64) (err error) {
	batch := &pgx.Batch{}
	batch.Queue(guardFlipConfirmed, op.ID)
	batch.Queue(guardAccountCAS, op.AccountID, op.Amount, version)
	snapshot.QueueApply(batch, op.AccountID, op.EffectiveAt, op.Amount)
	batch.Queue(notifyOutcome, fmt.Sprintf("op:%d", op.ID))
	br := tx.SendBatch(ctx, batch)
	defer func() {
		if cerr := br.Close(); err == nil {
			err = cerr
		}
	}()

	// Guard 2: conditional flip to CONFIRMED.
	var tag pgconn.CommandTag
	tag, err = br.Exec()
	if err != nil {
		return fmt.Errorf("guard 2 (confirm flip): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Guard 3: versioned account balance update.
	tag, err = br.Exec()
	if err != nil {
		return fmt.Errorf("guard 3 (account version CAS): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Snapshots (spec §8.4), in the same commit as the balance change.
	rows, err := snapshot.ScanApply(br)
	if err != nil {
		return err
	}
	p.recordSnapshotRows(rows)
	p.recordDecision(obs.KindSingle, obs.OutcomeConfirmed)

	if _, err = br.Exec(); err != nil {
		return fmt.Errorf("notify outcome op:%d: %w", op.ID, err)
	}
	return nil
}

// reject pipelines Guard 2's invalidate flip with the outcome notify into one
// round trip. No account write, so no Guard 3 — the reject path leaves the
// balance untouched.
func (p *Processor) reject(
	ctx context.Context, tx pgx.Tx, op pendingOp, externalID string, side model.LimitSide, shortfall int64,
) (err error) {
	reason := model.Rejection{
		Code:      model.ReasonLimitViolated,
		Account:   externalID,
		LimitSide: side,
		Shortfall: shortfall,
	}
	var detail string
	detail, err = reason.Marshal()
	if err != nil {
		return err
	}

	batch := &pgx.Batch{}
	batch.Queue(guardFlipInvalid, op.ID, detail)
	batch.Queue(notifyOutcome, fmt.Sprintf("op:%d", op.ID))
	br := tx.SendBatch(ctx, batch)
	defer func() {
		if cerr := br.Close(); err == nil {
			err = cerr
		}
	}()

	var tag pgconn.CommandTag
	tag, err = br.Exec()
	if err != nil {
		return fmt.Errorf("guard 2 (invalidate flip): %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}
	p.recordDecision(obs.KindSingle, obs.OutcomeInvalid)

	if _, err = br.Exec(); err != nil {
		return fmt.Errorf("notify outcome op:%d: %w", op.ID, err)
	}
	return nil
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
