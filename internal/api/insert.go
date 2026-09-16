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

	// Edit items (ADR-0009).
	ErrEditTargetNotFound      = ledger.ErrEditTargetNotFound
	ErrEditTargetNotOperation  = ledger.ErrEditTargetNotOperation
	ErrDuplicateEditTarget     = ledger.ErrDuplicateEditTarget
	ErrEditChangesNothing      = ledger.ErrEditChangesNothing
	ErrInvalidExpectedRevision = ledger.ErrInvalidExpectedRevision
	ErrEditsDisabled           = ledger.ErrEditsDisabled
	ErrEditWithReversal        = ledger.ErrEditWithReversal

	// Delete items (ADR-0011). The target sentinels above are shared with edits;
	// the ledger wraps them in a TargetError that carries the offending item's
	// kind and id.
	ErrDeleteWithFields = ledger.ErrDeleteWithFields
	ErrDeletesDisabled  = ledger.ErrDeletesDisabled
)

// TargetError is the ledger's kind-carrying wrap of a shared target sentinel
// (ErrEditTargetNotFound, ErrEditTargetNotOperation, ErrDuplicateEditTarget):
// errors.As yields the item kind and target id the error message names.
type (
	TargetError = ledger.TargetError
	TargetKind  = ledger.TargetKind
)

const (
	TargetEdit   = ledger.TargetEdit
	TargetDelete = ledger.TargetDelete
)

// Insert registers work using the shared core. The caller must roll back tx on
// error and commit on success. Its search_path must name the ledger schema.
func Insert(ctx context.Context, tx pgx.Tx, req InsertRequest) (*InsertResult, error) {
	return ledger.Insert(ctx, tx, req)
}
