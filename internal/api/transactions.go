package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// --- POST /transactions -----------------------------------------------------

// OperationInput is one leg of an insertion request. Amount is int64 minor units
// (see the Amount type); the account is referenced by its owner-scoped external id
// and upserted on demand.
type OperationInput struct {
	Account     string    `json:"account" minLength:"1" doc:"Client-defined account external id (unique per owner). Upserted on demand with unbounded limits if it does not exist."`
	Amount      Amount    `json:"amount"`
	EffectiveAt time.Time `json:"effective_at" doc:"Timeline timestamp (RFC 3339). May be in the past (backdated) or future; it never affects validation, only where the operation lands on the timeline (§5.4)."`
	ReversalOf  *int64    `json:"reversal_of,omitempty" doc:"Registration id of the operation this reverses. Immutable metadata, unused by the engine (§5.4)."`
}

// CreateTransactionInput is the POST /transactions request. The idempotency key is
// required and covers the whole request (spec §10.1).
type CreateTransactionInput struct {
	OwnerID        string `header:"X-Owner-Id" required:"true" doc:"Owner scope for the request (§2). Single-cell deployments pass the numeric owner_id directly."`
	IdempotencyKey string `header:"Idempotency-Key" required:"true" doc:"Client-generated UUID. Retrying with the same key never duplicates operations (G6); reusing it with a different payload is rejected with 422."`
	Body           struct {
		Operations []OperationInput `json:"operations" minItems:"1" doc:"One operation is a single; 2..max_group_size operations form an atomic group (§10.1)."`
		WaitMs     int              `json:"wait_ms,omitempty" minimum:"0" doc:"Optional synchronous wait budget in milliseconds. 0 or omitted returns 202 immediately (fire-and-forget); query the outcome later. When > 0 the call waits up to this many ms (capped by api_max_wait_ms) for the decision — 200 with the decided status if it lands in time, otherwise 202 with the current (still PENDING) state. Waiting is best-effort, never a guaranteed outcome."`
	}
}

// OperationOutcome is a per-operation result: its registration id and current
// status (PENDING on a fresh insert; the current status on a replay).
type OperationOutcome struct {
	ID     int64  `json:"id" doc:"Registration id — the global insertion order and the timeline tiebreaker."`
	Status string `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID."`
}

// CreateTransactionOutput is the insertion response. TransactionID/TransactionStatus
// are set only for groups; a single leaves them empty. Replayed is true when an
// idempotency key matched an existing record and no new rows were written.
//
// Status is the HTTP status code (Huma reads this special field): 202 for the
// fire-and-forget path and on a synchronous-wait expiry (still PENDING); 200 when a
// synchronous wait resolved to a decided outcome. It is not part of the response
// body — the OpenAPI contract advertises 202 as the operation's declared response.
type CreateTransactionOutput struct {
	Status int
	Body   struct {
		TransactionID     *int64             `json:"transaction_id,omitempty" doc:"Group transaction id; absent for a single operation."`
		TransactionStatus string             `json:"transaction_status,omitempty" doc:"Group status: PENDING | COMMITTED | REJECTED; absent for a single operation."`
		Operations        []OperationOutcome `json:"operations"`
		Replayed          bool               `json:"replayed" doc:"True when this request replayed an existing idempotency key and wrote no new rows (G6)."`
	}
}

func (s *Server) createTransaction(ctx context.Context, in *CreateTransactionInput) (*CreateTransactionOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	ops := make([]InsertOp, len(in.Body.Operations))
	for i, o := range in.Body.Operations {
		ops[i] = InsertOp{
			OwnerID:     ownerID,
			ExternalID:  o.Account,
			Amount:      int64(o.Amount),
			EffectiveAt: o.EffectiveAt,
			ReversalOf:  o.ReversalOf,
		}
	}
	req := InsertRequest{IdempotencyKey: in.IdempotencyKey, Operations: ops}

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
		return nil, mapInsertErr(err)
	}

	// wait_ms <= 0 (or omitted): fire-and-forget, 202 immediately. This path never
	// touches the notify machinery (ADR-0002: waiting is strictly opt-in).
	if in.Body.WaitMs <= 0 {
		out := insertOutput(res)
		out.Status = http.StatusAccepted
		return out, nil
	}

	// wait_ms > 0: wait up to the (capped) budget for the decision — 200 on outcome,
	// 202 with the current state on expiry (spec §10.1).
	return s.waitForOutcome(ctx, ownerID, res, in.Body.WaitMs)
}

// insertOutput builds the response body directly from the insert result (the
// fire-and-forget path, before any decision is awaited).
func insertOutput(res *InsertResult) *CreateTransactionOutput {
	out := &CreateTransactionOutput{}
	out.Body.TransactionID = res.TransactionID
	out.Body.TransactionStatus = res.TransactionStatus
	out.Body.Replayed = res.Replayed
	out.Body.Operations = make([]OperationOutcome, len(res.Operations))
	for i, o := range res.Operations {
		out.Body.Operations[i] = OperationOutcome{ID: o.ID, Status: o.Status}
	}
	return out
}

// --- GET /transactions/{id} -------------------------------------------------

// GetTransactionInput identifies a group by its transaction id, scoped to the
// requesting owner.
type GetTransactionInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	ID      int64  `path:"id" doc:"Transaction (group) id."`
}

// LegStatus is one leg of a group with its status and, if INVALID, its rejection.
type LegStatus struct {
	ID        int64            `json:"id"`
	Status    string           `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID."`
	Rejection *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the group was REJECTED."`
}

// GetTransactionOutput is a group's status plus its per-leg statuses.
type GetTransactionOutput struct {
	Body struct {
		ID         int64            `json:"id"`
		Status     string           `json:"status" doc:"Group status: PENDING | COMMITTED | REJECTED."`
		OpCount    int              `json:"op_count"`
		Rejection  *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the group was REJECTED."`
		Operations []LegStatus      `json:"operations"`
	}
}

// GetOperationInput identifies an operation by its registration id, scoped to the
// requesting owner.
type GetOperationInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	ID      int64  `path:"id" doc:"Operation (registration) id."`
}

// GetOperationOutput is an operation's status and, if INVALID, its rejection.
type GetOperationOutput struct {
	Body struct {
		ID            int64            `json:"id"`
		Status        string           `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID."`
		TransactionID *int64           `json:"transaction_id,omitempty" doc:"Group transaction id if this operation is a group leg; absent for a single."`
		Rejection     *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the operation is INVALID."`
	}
}

func (s *Server) getTransaction(ctx context.Context, in *GetTransactionInput) (*GetTransactionOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	value, err := db.Read(ctx, s.pool, func(tx pgx.Tx) (*ledger.TransactionOutcome, error) {
		return ledger.GetTransaction(ctx, tx, owner, in.ID)
	})
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	out := &GetTransactionOutput{}
	out.Body.ID, out.Body.Status, out.Body.OpCount, out.Body.Rejection = value.ID, string(value.Status), value.OpCount, value.Rejection
	out.Body.Operations = make([]LegStatus, len(value.Operations))
	for i, op := range value.Operations {
		out.Body.Operations[i] = LegStatus{ID: op.ID, Status: string(op.Status), Rejection: op.Rejection}
	}
	return out, nil
}

func (s *Server) getOperation(ctx context.Context, in *GetOperationInput) (*GetOperationOutput, error) {
	owner, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	value, err := db.Read(ctx, s.pool, func(tx pgx.Tx) (*ledger.OperationOutcome, error) { return ledger.GetOperation(ctx, tx, owner, in.ID) })
	if err != nil {
		return nil, mapLedgerErr(err)
	}
	out := &GetOperationOutput{}
	out.Body.ID, out.Body.Status, out.Body.TransactionID, out.Body.Rejection = value.ID, string(value.Status), value.TransactionID, value.Rejection
	return out, nil
}
