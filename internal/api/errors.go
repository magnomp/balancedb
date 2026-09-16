package api

import (
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/magnomp/balancedb/internal/ledger"
)

// mapInsertErr translates an insertion sentinel (insert.go) into the HTTP status
// its contract requires (spec §10.1; edit items ADR-0009). Mapping by errors.Is
// keeps the transport free of string matching. Anything unrecognised is a real
// server fault (500) — the DB layer's own errors, a bug — and must not be reported
// as a client error.
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

	// Edit items (ADR-0009). Messages are the contract's wording (the feature
	// _dx.md error table); the sentinel text carries the ledger's "insert:" prefix
	// and, where the ledger wraps it, the offending id.
	case errors.Is(err, ErrEditChangesNothing):
		return huma.Error400BadRequest("edit changes nothing")
	case errors.Is(err, ErrInvalidExpectedRevision):
		return huma.Error400BadRequest("expected_revision must be >= 1")
	case errors.Is(err, ErrEditWithReversal):
		return huma.Error400BadRequest("reversal_of is not allowed on an edit item")
	case errors.Is(err, ErrEditTargetNotFound):
		// Group context: the offending item is one of several, so the id is named
		// (a PATCH maps this to 404 instead — mapEditErr).
		return huma.Error422UnprocessableEntity(fmt.Sprintf("edit target %s not found", trailingID(err)))
	case errors.Is(err, ErrEditTargetNotOperation):
		return huma.Error422UnprocessableEntity(fmt.Sprintf("edit target %s is an edit, not an operation", trailingID(err)))
	case errors.Is(err, ErrDuplicateEditTarget):
		return huma.Error422UnprocessableEntity(fmt.Sprintf("duplicate edit target %s", trailingID(err)))
	case errors.Is(err, ErrEditsDisabled):
		// Policy, not a malformed request: the cell refuses edits (config.allow_edits).
		return huma.Error403Forbidden("editing is disabled for this cell")
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

// mapEditErr is mapInsertErr for PATCH /operations/{id}: the single target is the
// path id, so an unknown or foreign target is the resource itself not being found
// (404, the same body GET /operations/{id} returns — no owner leak), not an
// unprocessable item.
func mapEditErr(err error) error {
	if errors.Is(err, ErrEditTargetNotFound) {
		return huma.Error404NotFound("operation not found")
	}
	return mapInsertErr(err)
}

// trailingID extracts the id the ledger appends after the sentinel text
// ("<sentinel>: <id>") so the message can name the offending item; empty when
// the error was not wrapped that way. The status itself never depends on it.
func trailingID(err error) string {
	msg := err.Error()
	i := len(msg)
	for i > 0 && msg[i-1] >= '0' && msg[i-1] <= '9' {
		i--
	}
	return msg[i:]
}
