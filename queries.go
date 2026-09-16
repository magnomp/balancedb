package balancedb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// Read results use int64 money, explicit owner scoping and typed terminal states.
type (
	Balance            = ledger.Balance
	StatementOptions   = ledger.StatementOptions
	StatementEntry     = ledger.StatementEntry
	Statement          = ledger.Statement
	OperationOutcome   = ledger.OperationOutcome
	TransactionOutcome = ledger.TransactionOutcome
	OpStatus           = model.OpStatus
	TxStatus           = model.TxStatus
	Rejection          = model.Rejection
	ReasonCode         = model.ReasonCode
	LimitSide          = model.LimitSide
)

const (
	OpPending           = model.OpPending
	OpConfirmed         = model.OpConfirmed
	OpInvalid           = model.OpInvalid
	OpApplied           = model.OpApplied
	OpDeleted           = model.OpDeleted
	TxPending           = model.TxPending
	TxCommitted         = model.TxCommitted
	TxRejected          = model.TxRejected
	ReasonLimitViolated = model.ReasonLimitViolated
	LimitMin            = model.LimitMin
	LimitMax            = model.LimitMax
)

// read uses the owned pool, so it sees committed state, never pending host writes.
func read[T any](ctx context.Context, d *DB, fn func(pgx.Tx) (T, error)) (T, error) {
	d.mu.Lock()
	closed := d.closed
	d.mu.Unlock()
	if closed {
		var zero T
		return zero, ErrClosed
	}
	return db.Read(ctx, d.pool, fn)
}

// GetAccount reads a committed account scoped to its owner and external ID.
// An unknown account or one belonging to another owner returns ErrNotFound.
func (d *DB) GetAccount(ctx context.Context, ownerID int64, externalID string) (*Account, error) {
	return read(ctx, d, func(tx pgx.Tx) (*Account, error) { return ledger.GetAccount(ctx, tx, ownerID, externalID) })
}

// GetBalance returns the final confirmed balance when at is nil, or the timeline
// projection at that instant. Point-in-time balances may violate limits (N2).
// Each call reads one consistent database snapshot through the owned pool.
func (d *DB) GetBalance(ctx context.Context, ownerID int64, externalID string, at *time.Time) (*Balance, error) {
	return read(ctx, d, func(tx pgx.Tx) (*Balance, error) { return ledger.GetBalance(ctx, tx, ownerID, externalID, at) })
}

// GetStatement reads CONFIRMED operations with running balances in timeline order.
// Limit defaults to 50 and must be 1..500. Pass NextCursor to read the next page.
// A page uses one snapshot; different pages can reflect intervening confirmations
// or backdated work. Cursors are positions, not a frozen multi-page snapshot.
func (d *DB) GetStatement(ctx context.Context, ownerID int64, externalID string, opts StatementOptions) (*Statement, error) {
	return read(ctx, d, func(tx pgx.Tx) (*Statement, error) { return ledger.GetStatement(ctx, tx, ownerID, externalID, opts) })
}

// GetOperation reads a committed operation's decision state and rejection detail.
// Unknown and foreign-owner IDs both return ErrNotFound. PENDING is a valid state.
// A DELETED operation keeps its last values and reports DeletedAt/DeletedBy; an
// edit or delete registration reports EditOf or DeleteOf (never both).
func (d *DB) GetOperation(ctx context.Context, ownerID, operationID int64) (*OperationOutcome, error) {
	return read(ctx, d, func(tx pgx.Tx) (*OperationOutcome, error) { return ledger.GetOperation(ctx, tx, ownerID, operationID) })
}

// GetTransaction reads a group's status and all leg statuses from one snapshot.
// Unknown and foreign-owner IDs both return ErrNotFound.
func (d *DB) GetTransaction(ctx context.Context, ownerID, transactionID int64) (*TransactionOutcome, error) {
	return read(ctx, d, func(tx pgx.Tx) (*TransactionOutcome, error) {
		return ledger.GetTransaction(ctx, tx, ownerID, transactionID)
	})
}
