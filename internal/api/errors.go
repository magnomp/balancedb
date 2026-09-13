package api

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"github.com/magnomp/balancedb/internal/ledger"
)

// mapInsertErr translates an insertion sentinel (insert.go) into the HTTP status
// its contract requires (spec §10.1). Mapping by errors.Is keeps the transport
// free of string matching. Anything unrecognised is a real server fault (500) —
// the DB layer's own errors, a bug — and must not be reported as a client error.
func mapInsertErr(err error) error {
	switch {
	case errors.Is(err, ErrNoOperations):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, ErrInvalidIdempotencyKey):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, ErrMixedOwners):
		// Sharding invariant (spec §2): a well-formed request that is not
		// serviceable, not a syntax error.
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, ErrGroupTooLarge):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, ErrPayloadConflict):
		// Same idempotency key, different payload (spec §10.1 → 422).
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, ErrZeroAmount):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return huma.Error500InternalServerError("insert failed", err)
	}
}

// mapLedgerErr keeps domain errors independent from HTTP status handling.
func mapLedgerErr(err error) error {
	switch {
	case errors.Is(err, ledger.ErrNotFound):
		return huma.Error404NotFound("record not found")
	case errors.Is(err, ledger.ErrAccountExists), errors.Is(err, ledger.ErrConcurrentUpdate):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, ledger.ErrInvalidLimits):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, ledger.ErrInvalidArgument):
		return huma.Error400BadRequest(err.Error())
	default:
		return huma.Error500InternalServerError("ledger query failed", err)
	}
}
