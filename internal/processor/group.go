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
	selectGroupLegs = `SELECT id, account_id, amount, effective_at, status
  FROM operations WHERE transaction_id = $1 ORDER BY id`

	// selectGroupAccounts reads every involved account (balance, version, limits,
	// external id) in ascending id order — one query, deterministic order.
	selectGroupAccounts = `SELECT id, confirmed_balance, version, min_balance, max_balance, external_id
  FROM accounts WHERE id = ANY($1) ORDER BY id`

	// Guard 2 for a group (spec §7.2): the conditional PENDING flips of every leg
	// and of the transaction row. Each carries a rowcount check — the legs must all
	// still be PENDING (rowcount == leg count) and the transaction must still be
	// PENDING (rowcount == 1), else the group was decided under us ⇒ rollback.
	guardFlipLegsConfirmed = `UPDATE operations SET status = 'CONFIRMED', confirmed_at = now()
 WHERE transaction_id = $1 AND status = 'PENDING'`
	guardFlipLegsInvalid = `UPDATE operations SET status = 'INVALID', invalidation_reason = $2
 WHERE transaction_id = $1 AND status = 'PENDING'`
	guardFlipTxCommitted = `UPDATE transactions SET status = 'COMMITTED', decided_at = now()
 WHERE id = $1 AND status = 'PENDING'`
	guardFlipTxRejected = `UPDATE transactions SET status = 'REJECTED', reject_reason = $2, decided_at = now()
 WHERE id = $1 AND status = 'PENDING'`
)

// groupLeg is the slice of an operations row the group path needs.
type groupLeg struct {
	ID          int64
	AccountID   int64
	Amount      int64
	EffectiveAt time.Time
	Status      string
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
// §8.3): load all legs by transaction_id, cross-check op_count, net per account,
// validate every involved account, then all-or-nothing — all pass: apply every
// account's net (each under the Guard 3 version CAS), snapshot every leg, flip all
// legs to CONFIRMED and the transaction to COMMITTED (Guard 2); any fail: flip all
// legs to INVALID and the transaction to REJECTED with the offending account and
// shortfall (Guard 2), no balance change. Group atomicity is simply the DB
// transaction. Any guard miss returns errGuardMiss, rolling the transaction back.
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

	// Net per account, then read and validate every involved account in ascending
	// id order — the first violation (deterministic) rejects the whole group.
	accts, err := p.netAndReadAccounts(ctx, tx, legs)
	if err != nil {
		return err
	}

	// Test seam: inject a concurrent account mutation to exercise Guard 3.
	if p.afterAccountRead != nil {
		p.afterAccountRead()
	}

	for _, a := range accts {
		if side, shortfall, bad := violates(a.Balance+a.Net, a.MinBalance, a.MaxBalance); bad {
			return p.rejectGroup(ctx, tx, txID, len(legs), a.ExternalID, side, shortfall)
		}
	}
	return p.commitGroup(ctx, tx, txID, legs, accts)
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
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Amount, &l.EffectiveAt, &l.Status); err != nil {
			return nil, fmt.Errorf("scan group %d leg: %w", txID, err)
		}
		legs = append(legs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group %d legs: %w", txID, err)
	}
	return legs, nil
}

// netAndReadAccounts computes the net amount per involved account (spec §6) and
// reads those accounts, returning them in ascending id order with their net
// attached. The distinct account set drives one indexed read.
func (p *Processor) netAndReadAccounts(ctx context.Context, tx pgx.Tx, legs []groupLeg) ([]groupAccount, error) {
	net := make(map[int64]int64, len(legs))
	ids := make([]int64, 0, len(legs))
	for _, l := range legs {
		if _, seen := net[l.AccountID]; !seen {
			ids = append(ids, l.AccountID)
		}
		net[l.AccountID] += l.Amount
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

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

// commitGroup applies an accepted group: Guard 2 flips every leg to CONFIRMED and
// the transaction to COMMITTED, Guard 3 applies each account's net under its
// version CAS, snapshots update per leg, then the outcome is notified — all in the
// caller's transaction, so it is atomic (spec §8.3/G2).
func (p *Processor) commitGroup(ctx context.Context, tx pgx.Tx, txID int64, legs []groupLeg, accts []groupAccount) error {
	// Guard 2: flip all legs to CONFIRMED (rowcount must equal the leg count).
	tag, err := tx.Exec(ctx, guardFlipLegsConfirmed, txID)
	if err != nil {
		return fmt.Errorf("guard 2 (confirm legs of group %d): %w", txID, err)
	}
	if int(tag.RowsAffected()) != len(legs) {
		return errGuardMiss
	}

	// Guard 2: flip the transaction to COMMITTED.
	tag, err = tx.Exec(ctx, guardFlipTxCommitted, txID)
	if err != nil {
		return fmt.Errorf("guard 2 (commit transaction %d): %w", txID, err)
	}
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}

	// Guard 3: per-account version CAS with the account's net.
	for _, a := range accts {
		tag, err := tx.Exec(ctx, guardAccountCAS, a.ID, a.Net, a.Version)
		if err != nil {
			return fmt.Errorf("guard 3 (account %d CAS in group %d): %w", a.ID, txID, err)
		}
		if tag.RowsAffected() != 1 {
			return errGuardMiss
		}
	}

	// Snapshots (spec §8.4): one per leg, with the leg's own amount and day, so the
	// snapshot deltas on an account sum to the net applied above.
	for _, l := range legs {
		rows, err := snapshot.Apply(ctx, tx, l.AccountID, l.EffectiveAt, l.Amount)
		if err != nil {
			return err
		}
		p.recordSnapshotRows(rows)
	}
	p.recordDecision(obs.KindGroup, obs.OutcomeCommitted)

	return notifyTx(ctx, tx, txID)
}

// rejectGroup rejects a group: Guard 2 flips every leg to INVALID and the
// transaction to REJECTED with the machine-readable reason (offending account +
// shortfall). No account is written, so there is no Guard 3 (spec §8.3).
func (p *Processor) rejectGroup(ctx context.Context, tx pgx.Tx, txID int64, nLegs int, externalID string, side model.LimitSide, shortfall int64) error {
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

	// Guard 2: flip all legs to INVALID (rowcount must equal the leg count).
	tag, err := tx.Exec(ctx, guardFlipLegsInvalid, txID, detail)
	if err != nil {
		return fmt.Errorf("guard 2 (invalidate legs of group %d): %w", txID, err)
	}
	if int(tag.RowsAffected()) != nLegs {
		return errGuardMiss
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
