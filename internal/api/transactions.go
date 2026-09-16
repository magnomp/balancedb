package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// --- POST /transactions -----------------------------------------------------

// OperationInput is one item of an insertion request. Without edit_of it is a new
// operation and account, amount and effective_at are all required (the handler
// refuses an item missing one with 400 — the schema leaves them optional only so
// edit items can omit them). With edit_of it is an edit registration of that
// operation (ADR-0010): every omitted field means "unchanged", at least one of
// amount, effective_at and account must be present, and reversal_of is not
// allowed. Amount is int64 minor units (see the Amount type); the account is
// referenced by its owner-scoped external id and upserted on demand.
type OperationInput struct {
	Account          string     `json:"account,omitempty" minLength:"1" doc:"Client-defined account external id (unique per owner). Upserted on demand with unbounded limits if it does not exist. Required on a new operation; on an edit item, omitted means unchanged."`
	Amount           *Amount    `json:"amount,omitempty"`
	EffectiveAt      *time.Time `json:"effective_at,omitempty" doc:"Timeline timestamp (RFC 3339). May be in the past (backdated) or future; it never affects validation, only where the operation lands on the timeline (§5.4). Required on a new operation; on an edit item, omitted means unchanged."`
	ReversalOf       *int64     `json:"reversal_of,omitempty" doc:"Registration id of the operation this reverses. Immutable metadata, unused by the engine (§5.4). Not allowed on an edit item."`
	EditOf           *int64     `json:"edit_of,omitempty" doc:"Makes this item an edit of the named operation (ADR-0010): the group applies it atomically with its other items, netting all changes per account. The target must be a regular operation of this owner; two items may not edit the same operation."`
	ExpectedRevision *int32     `json:"expected_revision,omitempty" doc:"Edit items only. Optional optimistic guard (>= 1): the group is rejected with STALE_REVISION unless the target is at exactly this revision when decided."`
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
// status (PENDING on a fresh insert; the current status on a replay). An edit
// item's outcome names the operation it edits.
type OperationOutcome struct {
	ID     int64  `json:"id" doc:"Registration id — the global insertion order and the timeline tiebreaker."`
	Status string `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID; an edit item is PENDING | APPLIED | INVALID."`
	EditOf *int64 `json:"edit_of,omitempty" doc:"Present only on an edit item: the operation it edits."`
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

	ops, err := insertOps(ownerID, in.Body.Operations)
	if err != nil {
		return nil, err
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

// insertOps shapes the request items into ledger items after the contract's
// structural checks, all before any database access (the _dx.md error table):
// a new operation must carry account, amount and effective_at (400); a zero
// amount is the insertion path's zero-amount error (422); an edit item's
// expected_revision must be >= 1, it may not carry reversal_of, it must change
// something (400 each), and two items may not edit one target (422). The ledger
// enforces the same rules again as its own invariant; checking them here keeps
// the messages exact and the DB untouched for a malformed request.
func insertOps(ownerID int64, items []OperationInput) ([]InsertOp, error) {
	ops := make([]InsertOp, len(items))
	seen := make(map[int64]struct{}, len(items))
	for i, o := range items {
		if o.EditOf == nil && (o.Account == "" || o.Amount == nil || o.EffectiveAt == nil) {
			return nil, huma.Error400BadRequest(fmt.Sprintf("operations[%d]: account, amount and effective_at are required on a new operation", i))
		}
		if o.Amount != nil && *o.Amount == 0 {
			return nil, mapInsertErr(ErrZeroAmount)
		}
		op := InsertOp{OwnerID: ownerID, ExternalID: o.Account, ReversalOf: o.ReversalOf, EditOf: o.EditOf, ExpectedRevision: o.ExpectedRevision}
		if o.Amount != nil {
			op.Amount = int64(*o.Amount)
		}
		if o.EffectiveAt != nil {
			op.EffectiveAt = *o.EffectiveAt
		}
		if o.EditOf != nil {
			target := *o.EditOf
			switch {
			case o.ReversalOf != nil:
				return nil, mapInsertErr(fmt.Errorf("%w: target %d", ErrEditWithReversal, target))
			case o.ExpectedRevision != nil && *o.ExpectedRevision < 1:
				return nil, mapInsertErr(fmt.Errorf("%w: got %d", ErrInvalidExpectedRevision, *o.ExpectedRevision))
			case o.Account == "" && o.Amount == nil && o.EffectiveAt == nil:
				return nil, mapInsertErr(fmt.Errorf("%w: target %d", ErrEditChangesNothing, target))
			}
			if _, dup := seen[target]; dup {
				return nil, mapInsertErr(fmt.Errorf("%w: %d", ErrDuplicateEditTarget, target))
			}
			seen[target] = struct{}{}
		}
		ops[i] = op
	}
	return ops, nil
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
		out.Body.Operations[i] = OperationOutcome{ID: o.ID, Status: o.Status, EditOf: o.EditOf}
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
// An edit leg (ADR-0010) names the operation it edits and omits revision; a
// regular leg carries its current revision and omits edit_of.
type LegStatus struct {
	ID        int64            `json:"id"`
	Status    string           `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID; an edit leg is PENDING | APPLIED | INVALID."`
	EditOf    *int64           `json:"edit_of,omitempty" doc:"Present only on an edit leg: the operation it edits."`
	Revision  int32            `json:"revision,omitempty" doc:"Current revision of a regular leg (1 until its first applied edit); absent on an edit leg."`
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

// --- GET /operations/{id} ---------------------------------------------------

// GetOperationInput identifies an operation by its registration id, scoped to the
// requesting owner.
type GetOperationInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	ID      int64  `path:"id" doc:"Operation (registration) id."`
}

// GetOperationOutput is an operation's current state: status, timeline values and
// revision for a regular operation; for an edit registration (edit_of present,
// ADR-0010) the proposed state as resolved at submission and its optional guard.
// Regular rows omit edit_of/expected_revision; edit rows omit revision.
type GetOperationOutput struct {
	Body struct {
		ID               int64            `json:"id"`
		Status           string           `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID; an edit registration is PENDING | APPLIED | INVALID."`
		TransactionID    *int64           `json:"transaction_id,omitempty" doc:"Group transaction id if this operation is a group leg; absent for a single."`
		EditOf           *int64           `json:"edit_of,omitempty" doc:"Present only on an edit registration: the operation it edits."`
		Account          string           `json:"account" doc:"Account external id (current value; for an edit, the proposed one)."`
		Amount           Amount           `json:"amount"`
		EffectiveAt      time.Time        `json:"effective_at" doc:"Timeline instant (current value; for an edit, the proposed one)."`
		Revision         int32            `json:"revision,omitempty" doc:"Current revision of a regular operation (1 until its first applied edit); absent on an edit registration."`
		ExpectedRevision *int32           `json:"expected_revision,omitempty" doc:"Present only on an edit registration that carries an optimistic guard."`
		Rejection        *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the row is INVALID."`
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
		leg := LegStatus{ID: op.ID, Status: string(op.Status), Rejection: op.Rejection}
		// Edit legs name their target and hide the meaningless default revision;
		// regular legs carry their revision (the same split as GET /operations/{id}).
		if op.EditOf != nil {
			leg.EditOf = op.EditOf
		} else {
			leg.Revision = op.Revision
		}
		out.Body.Operations[i] = leg
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
	out.Body.Account = value.Account
	out.Body.Amount = Amount(value.Amount)
	out.Body.EffectiveAt = value.EffectiveAt.UTC()
	if value.EditOf != nil {
		// Edit registration: the guard travels with it; revision is meaningless
		// (the column is the default 1) and stays hidden.
		out.Body.EditOf = value.EditOf
		out.Body.ExpectedRevision = value.ExpectedRevision
	} else {
		out.Body.Revision = value.Revision
	}
	return out, nil
}
