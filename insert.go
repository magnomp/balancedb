package balancedb

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/ledger"
)

// Insertion types are shared with the HTTP API. Amounts are int64 minor units.
type (
	InsertOp      = ledger.InsertOp
	InsertRequest = ledger.InsertRequest
	OpOutcome     = ledger.OpOutcome
	InsertResult  = ledger.InsertResult
)

// Insertion errors support errors.Is, including through wrapped SQL errors.
var (
	ErrNoOperations          = ledger.ErrNoOperations
	ErrInvalidIdempotencyKey = ledger.ErrInvalidIdempotencyKey
	ErrMixedOwners           = ledger.ErrMixedOwners
	ErrGroupTooLarge         = ledger.ErrGroupTooLarge
	ErrPayloadConflict       = ledger.ErrPayloadConflict
	ErrZeroAmount            = ledger.ErrZeroAmount
	ErrUnsupportedIsolation  = errors.New("balancedb: writes require READ COMMITTED")

	// Edit registrations (InsertOp.EditOf, ADR-0010).
	ErrEditTargetNotFound      = ledger.ErrEditTargetNotFound
	ErrEditTargetNotOperation  = ledger.ErrEditTargetNotOperation
	ErrDuplicateEditTarget     = ledger.ErrDuplicateEditTarget
	ErrEditChangesNothing      = ledger.ErrEditChangesNothing
	ErrInvalidExpectedRevision = ledger.ErrInvalidExpectedRevision
	ErrEditsDisabled           = ledger.ErrEditsDisabled
	ErrEditWithReversal        = ledger.ErrEditWithReversal
)

// Insert registers one single operation or an atomic group in a caller-owned
// READ COMMITTED pgx transaction in this cell's database. Accounts are created on
// demand with unbounded limits. No REST call or pool checkout occurs. An item
// with EditOf set registers an edit of that operation (zero-valued fields mean
// "unchanged"); the leader decides it like any other registration.
//
// A savepoint contains all ledger writes and schema changes; an error rolls it
// back, allowing the host to handle the error without accidentally committing a
// partial group. Success releases the savepoint and restores the host search_path
// but DOES NOT commit the host transaction. Returned IDs/statuses are provisional
// until that commit. NOTIFY is delivered only by the enclosing commit.
//
// Insert as close as possible to the host commit and keep the transaction short.
// Insertion is not serialized: a later ID can commit and be decided while an
// earlier operation is still uncommitted, changing acceptance outcomes (ADR-0008).
// Inserting at the end reduces this risk but does not guarantee global ID order.
// Never wait for a decision before committing, access this pgx.Tx concurrently,
// or write ledger tables directly.
func (d *DB) Insert(ctx context.Context, tx pgx.Tx, req InsertRequest) (*InsertResult, error) {
	return withSchema(ctx, d, tx, func(sp pgx.Tx) (*InsertResult, error) {
		return ledger.Insert(ctx, sp, req)
	})
}
