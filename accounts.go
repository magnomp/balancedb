package balancedb

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/magnomp/balancedb/internal/ledger"
)

// Account contains owner identity, bounds, final confirmed balance and CAS version.
type Account = ledger.Account

// Limits replaces both bounds. Nil means unbounded; money is int64 minor units.
type Limits = ledger.Limits

// Domain errors support errors.Is and are shared with the HTTP handlers.
var (
	ErrNotFound         = ledger.ErrNotFound
	ErrBalanceOverflow  = ledger.ErrBalanceOverflow
	ErrAccountExists    = ledger.ErrAccountExists
	ErrInvalidArgument  = ledger.ErrInvalidArgument
	ErrInvalidLimits    = ledger.ErrInvalidLimits
	ErrConcurrentUpdate = ledger.ErrConcurrentUpdate
)

// CreateAccount creates an account within the host's READ COMMITTED transaction.
// Limits must contain its initial zero balance. Existing accounts return
// ErrAccountExists. Only the host commits; errors roll back this call's savepoint.
func (d *DB) CreateAccount(ctx context.Context, tx pgx.Tx, ownerID int64, externalID string, limits Limits) (*Account, error) {
	return withSchema(ctx, d, tx, func(sp pgx.Tx) (*Account, error) {
		return ledger.CreateAccount(ctx, sp, ownerID, externalID, limits)
	})
}

// UpdateLimits replaces both bounds in the host's READ COMMITTED transaction.
// The current final balance must fit the new bounds. Account-version CAS is
// retried once; a second miss returns ErrConcurrentUpdate. The host owns commit.
func (d *DB) UpdateLimits(ctx context.Context, tx pgx.Tx, ownerID int64, externalID string, limits Limits) (*Account, error) {
	return withSchema(ctx, d, tx, func(sp pgx.Tx) (*Account, error) {
		return ledger.UpdateLimits(ctx, sp, ownerID, externalID, limits, nil)
	})
}
