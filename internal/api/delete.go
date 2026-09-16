package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/model"
)

// --- DELETE /operations/{id} ------------------------------------------------

// DeleteOperationInput is the DELETE /operations/{id} request (ADR-0011): one
// delete registration against the operation named in the path. There is no
// body — a request body is ignored; both inputs beyond the headers are query
// parameters. The idempotency key covers the whole request exactly as for
// POST /transactions (the target and expected_revision are the payload; wait_ms
// is not).
type DeleteOperationInput struct {
	OwnerID          string `header:"X-Owner-Id" required:"true" doc:"Owner scope for the request (§2). The target must belong to this owner."`
	IdempotencyKey   string `header:"Idempotency-Key" required:"true" doc:"Client-generated UUID. Retrying with the same key never registers a second delete (G6); reusing it with a different payload is rejected with 422."`
	ID               int64  `path:"id" doc:"Registration id of the operation to delete."`
	ExpectedRevision int32  `query:"expected_revision" minimum:"1" doc:"Optional optimistic guard (>= 1): the delete is rejected with STALE_REVISION unless the operation is at exactly this revision when the delete is decided."`
	WaitMs           int    `query:"wait_ms" minimum:"0" doc:"Optional synchronous wait budget in milliseconds, same semantics as POST /transactions: 0 or omitted returns 202 immediately; > 0 waits up to this many ms (capped by api_max_wait_ms) for the decision — 200 with the decided status if it lands in time, otherwise 202 with the current (still PENDING) state."`
}

// DeletionOutcome is the delete registration as the response reports it: its
// own registration id (same sequence as operation ids), its status, the
// operation it targets and, once rejected, the machine-readable rejection.
type DeletionOutcome struct {
	ID          int64            `json:"id" doc:"Registration id of the delete; GET /operations/{id} reads it back."`
	Status      string           `json:"status" doc:"Delete status: PENDING | APPLIED | INVALID."`
	OperationID int64            `json:"operation_id" doc:"The deleted operation (the id in the request path)."`
	Rejection   *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the delete is INVALID."`
}

// DeleteOutput is the DELETE /operations/{id} response. Status is the HTTP
// status code (Huma reads this special field): 202 for the fire-and-forget
// path, an idempotent replay and a synchronous-wait expiry; 200 when a
// synchronous wait resolved to a decided outcome. The OpenAPI contract
// advertises 202.
type DeleteOutput struct {
	Status int
	Body   struct {
		Deletion DeletionOutcome `json:"deletion"`
		Replayed bool            `json:"replayed" doc:"True when this request replayed an existing idempotency key and wrote no new rows (G6)."`
	}
}

func (s *Server) deleteOperation(ctx context.Context, in *DeleteOperationInput) (*DeleteOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	// Query parameters cannot be pointers in Huma, so an omitted guard reads as
	// 0 — unreachable otherwise, since the schema refuses expected_revision < 1
	// (the ledger enforces the same rule as its own invariant).
	var expected *int32
	if in.ExpectedRevision > 0 {
		rev := in.ExpectedRevision
		expected = &rev
	}

	// One delete item: the ledger copies the target's current values onto the
	// registration row (informational only) and never upserts an account.
	targetID := in.ID
	op := InsertOp{OwnerID: ownerID, DeleteOf: &targetID, ExpectedRevision: expected}
	req := InsertRequest{IdempotencyKey: in.IdempotencyKey, Operations: []InsertOp{op}}

	var res *InsertResult
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, e := Insert(ctx, tx, req)
		if e != nil {
			return e
		}
		res = r
		return nil
	})
	if err != nil {
		return nil, mapDeleteErr(err)
	}
	deleteID := res.Operations[0].ID

	// wait_ms <= 0 (or omitted): fire-and-forget, 202 with the registered state (on
	// a replay, the delete's current state).
	if in.WaitMs <= 0 {
		out := deleteOutput(deleteID, res.Operations[0].Status, targetID, nil, res.Replayed)
		out.Status = http.StatusAccepted
		return out, nil
	}

	// wait_ms > 0: the processor notifies 'op:<delete id>' inside the deciding
	// commit, exactly as for a single insert or an edit — the same wait loop applies.
	out, status, err := awaitDecision(ctx, s, fmt.Sprintf("op:%d", deleteID), in.WaitMs,
		func(c context.Context) (*DeleteOutput, bool, error) {
			return s.loadDeleteOutcome(c, deleteID, targetID, ownerID, res.Replayed)
		})
	if err != nil {
		return nil, err
	}
	out.Status = status
	return out, nil
}

// deleteOutput shapes the DELETE response body from a delete's id, status and
// target.
func deleteOutput(deleteID int64, status string, targetID int64, rej *model.Rejection, replayed bool) *DeleteOutput {
	out := &DeleteOutput{}
	out.Body.Deletion = DeletionOutcome{ID: deleteID, Status: status, OperationID: targetID, Rejection: rej}
	out.Body.Replayed = replayed
	return out
}

// loadDeleteOutcome reads a delete registration's current status (owner-scoped:
// the delete row's account belongs to the owner) into a response body and
// reports whether it is terminal (APPLIED or INVALID).
func (s *Server) loadDeleteOutcome(ctx context.Context, deleteID, targetID, ownerID int64, replayed bool) (*DeleteOutput, bool, error) {
	var (
		status string
		reason *string
	)
	if err := s.pool.QueryRow(ctx, selectOpStatusForOwner, deleteID, ownerID).Scan(&status, &reason); err != nil {
		return nil, false, err
	}
	out := deleteOutput(deleteID, status, targetID, parseRejection(reason), replayed)
	return out, status != string(model.OpPending), nil
}
