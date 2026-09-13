package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/ledger"
)

// Shared insertion types and errors keep HTTP and embedded behavior identical.
type (
	InsertOp      = ledger.InsertOp
	InsertRequest = ledger.InsertRequest
	OpOutcome     = ledger.OpOutcome
	InsertResult  = ledger.InsertResult
)

var (
	ErrNoOperations          = ledger.ErrNoOperations
	ErrInvalidIdempotencyKey = ledger.ErrInvalidIdempotencyKey
	ErrMixedOwners           = ledger.ErrMixedOwners
	ErrGroupTooLarge         = ledger.ErrGroupTooLarge
	ErrPayloadConflict       = ledger.ErrPayloadConflict
	ErrZeroAmount            = ledger.ErrZeroAmount
)

// Insert registers work using the shared core. The caller must roll back tx on
// error and commit on success. Its search_path must name the ledger schema.
func Insert(ctx context.Context, tx pgx.Tx, req InsertRequest) (*InsertResult, error) {
	return ledger.Insert(ctx, tx, req)
}
