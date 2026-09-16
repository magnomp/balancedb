package api

import (
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"
	"github.com/magnomp/balancedb/internal/ledger"
)

// mapInsertErr translates an insertion sentinel (insert.go) into the HTTP status
// its contract requires (spec §10.1; edit items ADR-0010, delete items ADR-0011).
// Mapping by errors.Is/As keeps the transport free of string matching. Anything
// unrecognised is a real server fault (500) — the DB layer's own errors, a bug —
// and must not be reported as a client error.
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

	// Edit and delete items (ADR-0010, ADR-0011). Messages are the contract's
	// wording (the feature _dx.md error tables); the sentinel text carries the
	// ledger's "insert:" prefix and, where the ledger wraps it, the offending id.
	case errors.Is(err, ErrEditChangesNothing):
		return huma.Error400BadRequest("edit changes nothing")
	case errors.Is(err, ErrInvalidExpectedRevision):
		return huma.Error400BadRequest("expected_revision must be >= 1")
	case errors.Is(err, ErrEditWithReversal):
		return huma.Error400BadRequest("reversal_of is not allowed on an edit item")
	case errors.Is(err, ErrDeleteWithFields):
		return huma.Error400BadRequest("a delete item carries only delete_of and expected_revision")
	case errors.Is(err, ErrEditTargetNotFound):
		// Group context: the offending item is one of several, so its kind and id
		// are named (PATCH and DELETE map this to 404 instead — mapEditErr,
		// mapDeleteErr).
		kind, id := targetOf(err)
		return huma.Error422UnprocessableEntity(fmt.Sprintf("%s target %d not found", kind, id))
	case errors.Is(err, ErrEditTargetNotOperation):
		kind, id := targetOf(err)
		return huma.Error422UnprocessableEntity(fmt.Sprintf("%s target %d is an edit, not an operation", kind, id))
	case errors.Is(err, ErrDuplicateEditTarget):
		// One wording for any mix of kinds: the fault is the shared target.
		_, id := targetOf(err)
		return huma.Error422UnprocessableEntity(fmt.Sprintf("duplicate edit target %d", id))
	case errors.Is(err, ErrEditsDisabled):
		// Policy, not a malformed request: the cell refuses edits (config.allow_edits).
		return huma.Error403Forbidden("editing is disabled for this cell")
	case errors.Is(err, ErrDeletesDisabled):
		// Same for deletes (config.allow_deletes), independently of allow_edits.
		return huma.Error403Forbidden("deleting is disabled for this cell")
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

// mapDeleteErr is mapEditErr for DELETE /operations/{id} (ADR-0011): the same
// 404 rule for the path id; every other sentinel maps as an insert (the
// TargetError kind makes the messages say "delete target …").
func mapDeleteErr(err error) error {
	return mapEditErr(err)
}

// targetOf reads the item kind and target id the ledger wraps around a shared
// target sentinel (TargetError, ADR-0011) so the message can name the offending
// item. An unwrapped sentinel reads as an edit of id 0 — the status never depends
// on it, and every producer (the ledger and this package's pre-validation) wraps.
func targetOf(err error) (TargetKind, int64) {
	var te *TargetError
	if errors.As(err, &te) {
		return te.Kind, te.Target
	}
	return TargetEdit, 0
}
