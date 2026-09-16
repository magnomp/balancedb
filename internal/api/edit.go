package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/model"
)

// --- PATCH /operations/{id} -------------------------------------------------

// EditOperationInput is the PATCH /operations/{id} request (ADR-0010): one edit
// registration against the operation named in the path. Every body field is
// optional; an omitted field means "unchanged", and at least one of amount,
// effective_at and account must be present. The idempotency key covers the whole
// request exactly as for POST /transactions.
type EditOperationInput struct {
	OwnerID        string `header:"X-Owner-Id" required:"true" doc:"Owner scope for the request (§2). The target must belong to this owner."`
	IdempotencyKey string `header:"Idempotency-Key" required:"true" doc:"Client-generated UUID. Retrying with the same key never registers a second edit (G6); reusing it with a different payload is rejected with 422."`
	ID             int64  `path:"id" doc:"Registration id of the operation to edit."`
	Body           struct {
		Amount           *Amount    `json:"amount,omitempty" doc:"New amount (non-zero). Omitted: unchanged."`
		EffectiveAt      *time.Time `json:"effective_at,omitempty" doc:"New timeline instant (RFC 3339), past or future. Omitted: unchanged."`
		Account          string     `json:"account,omitempty" minLength:"1" doc:"External id of an account of the same owner; created on demand with unbounded limits. Omitted: unchanged."`
		ExpectedRevision *int32     `json:"expected_revision,omitempty" doc:"Optional optimistic guard (>= 1): the edit is rejected with STALE_REVISION unless the operation is at exactly this revision when the edit is decided."`
		WaitMs           int        `json:"wait_ms,omitempty" minimum:"0" doc:"Optional synchronous wait budget in milliseconds, same semantics as POST /transactions: 0 or omitted returns 202 immediately; > 0 waits up to this many ms (capped by api_max_wait_ms) for the decision — 200 with the decided status if it lands in time, otherwise 202 with the current (still PENDING) state."`
	}
}

// EditOutcome is the edit registration as the response reports it: its own
// registration id (same sequence as operation ids), its status, the operation it
// targets and, once rejected, the machine-readable rejection.
type EditOutcome struct {
	ID          int64            `json:"id" doc:"Registration id of the edit; GET /operations/{id} reads it back."`
	Status      string           `json:"status" doc:"Edit status: PENDING | APPLIED | INVALID."`
	OperationID int64            `json:"operation_id" doc:"The edited operation (the id in the request path)."`
	Rejection   *model.Rejection `json:"rejection,omitempty" doc:"Machine-readable rejection detail; present only when the edit is INVALID."`
}

// EditOutput is the PATCH /operations/{id} response. Status is the HTTP status
// code (Huma reads this special field): 202 for the fire-and-forget path, an
// idempotent replay and a synchronous-wait expiry; 200 when a synchronous wait
// resolved to a decided outcome. The OpenAPI contract advertises 202.
type EditOutput struct {
	Status int
	Body   struct {
		Edit     EditOutcome `json:"edit"`
		Replayed bool        `json:"replayed" doc:"True when this request replayed an existing idempotency key and wrote no new rows (G6)."`
	}
}

func (s *Server) editOperation(ctx context.Context, in *EditOperationInput) (*EditOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if err := validateEditBody(in); err != nil {
		return nil, err
	}

	// One edit item; omitted fields stay zero, which the ledger reads as
	// "unchanged" and fills from the target (spec Core Interfaces).
	targetID := in.ID
	op := InsertOp{OwnerID: ownerID, ExternalID: in.Body.Account, EditOf: &targetID, ExpectedRevision: in.Body.ExpectedRevision}
	if in.Body.Amount != nil {
		op.Amount = int64(*in.Body.Amount)
	}
	if in.Body.EffectiveAt != nil {
		op.EffectiveAt = *in.Body.EffectiveAt
	}
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
		return nil, mapEditErr(err)
	}
	editID := res.Operations[0].ID

	// wait_ms <= 0 (or omitted): fire-and-forget, 202 with the registered state (on
	// a replay, the edit's current state).
	if in.Body.WaitMs <= 0 {
		out := editOutput(editID, res.Operations[0].Status, targetID, nil, res.Replayed)
		out.Status = http.StatusAccepted
		return out, nil
	}

	// wait_ms > 0: the processor notifies 'op:<edit id>' inside the deciding commit,
	// exactly as for a single insert — the same wait loop applies.
	out, status, err := awaitDecision(ctx, s, fmt.Sprintf("op:%d", editID), in.Body.WaitMs,
		func(c context.Context) (*EditOutput, bool, error) {
			return s.loadEditOutcome(c, editID, targetID, ownerID, res.Replayed)
		})
	if err != nil {
		return nil, err
	}
	out.Status = status
	return out, nil
}

// validateEditBody applies the contract's structural rules to the request before
// any database access (_dx.md error table): a zero amount is the insertion-path
// zero-amount error, expected_revision must be >= 1, and at least one editable
// field must be present. The ledger enforces the same rules again as its own
// invariant; checking them here keeps the messages exact and the DB untouched.
func validateEditBody(in *EditOperationInput) error {
	if in.Body.Amount != nil && *in.Body.Amount == 0 {
		return mapEditErr(ErrZeroAmount)
	}
	if in.Body.ExpectedRevision != nil && *in.Body.ExpectedRevision < 1 {
		return mapEditErr(ErrInvalidExpectedRevision)
	}
	if in.Body.Amount == nil && in.Body.EffectiveAt == nil && in.Body.Account == "" {
		return mapEditErr(ErrEditChangesNothing)
	}
	return nil
}

// editOutput shapes the PATCH response body from an edit's id, status and target.
func editOutput(editID int64, status string, targetID int64, rej *model.Rejection, replayed bool) *EditOutput {
	out := &EditOutput{}
	out.Body.Edit = EditOutcome{ID: editID, Status: status, OperationID: targetID, Rejection: rej}
	out.Body.Replayed = replayed
	return out
}

// loadEditOutcome reads an edit registration's current status (owner-scoped: the
// edit row's account belongs to the owner) into a response body and reports
// whether it is terminal (APPLIED or INVALID).
func (s *Server) loadEditOutcome(ctx context.Context, editID, targetID, ownerID int64, replayed bool) (*EditOutput, bool, error) {
	var (
		status string
		reason *string
	)
	if err := s.pool.QueryRow(ctx, selectOpStatusForOwner, editID, ownerID).Scan(&status, &reason); err != nil {
		return nil, false, err
	}
	out := editOutput(editID, status, targetID, parseRejection(reason), replayed)
	return out, status != string(model.OpPending), nil
}

// --- GET /operations/{id}/history -------------------------------------------

// OperationHistoryInput identifies a regular operation by its registration id,
// scoped to the requesting owner.
type OperationHistoryInput struct {
	OwnerID string `header:"X-Owner-Id" required:"true" doc:"Owner scope (§2)."`
	ID      int64  `path:"id" doc:"Operation (registration) id. Edit and delete registration ids are not operations and yield 404."`
}

// RevisionEntry is one state an operation has held. The current revision has no
// superseded_* fields; every earlier one names the APPLIED edit that replaced it.
type RevisionEntry struct {
	Revision     int32      `json:"revision"`
	Account      string     `json:"account" doc:"Account external id at this revision."`
	Amount       Amount     `json:"amount"`
	EffectiveAt  time.Time  `json:"effective_at"`
	RecordedAt   time.Time  `json:"recorded_at" doc:"When this revision became current: registration for revision 1, the applying edit's decision instant afterwards."`
	SupersededAt *time.Time `json:"superseded_at,omitempty" doc:"When this revision stopped being current; absent on the current revision."`
	SupersededBy *int64     `json:"superseded_by,omitempty" doc:"Registration id of the APPLIED edit that superseded this revision; absent on the current revision."`
}

// Registration kinds the history lists carry (ADR-0011): an entry is either an
// edit or a delete of the operation.
const (
	kindEdit   = string(TargetEdit)
	kindDelete = string(TargetDelete)
)

// registrationKind names an edit-class row's kind from its is_delete marker.
func registrationKind(isDelete bool) string {
	if isDelete {
		return kindDelete
	}
	return kindEdit
}

// PendingEdit is an edit or delete registration against the operation that has
// not been decided yet; its fields are the proposed state as resolved at
// submission (for a delete, the target's values at submission, informational).
type PendingEdit struct {
	ID               int64     `json:"id" doc:"Registration id of the edit or delete."`
	Kind             string    `json:"kind" enum:"edit,delete" doc:"Registration kind: edit | delete."`
	Account          string    `json:"account"`
	Amount           Amount    `json:"amount"`
	EffectiveAt      time.Time `json:"effective_at"`
	ExpectedRevision *int32    `json:"expected_revision,omitempty"`
}

// RejectedEdit is an edit or delete registration against the operation that
// ended INVALID, with its decision instant and machine-readable rejection.
type RejectedEdit struct {
	ID               int64            `json:"id" doc:"Registration id of the edit or delete."`
	Kind             string           `json:"kind" enum:"edit,delete" doc:"Registration kind: edit | delete."`
	Account          string           `json:"account"`
	Amount           Amount           `json:"amount"`
	EffectiveAt      time.Time        `json:"effective_at"`
	ExpectedRevision *int32           `json:"expected_revision,omitempty"`
	DecidedAt        time.Time        `json:"decided_at"`
	Rejection        *model.Rejection `json:"rejection"`
}

// OperationHistoryOutput is an operation's append-only history: every revision in
// order (the last one is current), plus the edits and deletes still pending
// against it and the ones rejected. Applied edits are not listed separately —
// each superseded revision names the edit that applied it; the applied delete,
// if any, is named by deleted_by (ADR-0011).
type OperationHistoryOutput struct {
	Body struct {
		OperationID     int64           `json:"operation_id"`
		Status          string          `json:"status" doc:"Operation status: PENDING | CONFIRMED | INVALID | DELETED."`
		CurrentRevision int32           `json:"current_revision"`
		DeletedAt       *time.Time      `json:"deleted_at,omitempty" doc:"Present only when DELETED: the instant the delete was applied."`
		DeletedBy       *int64          `json:"deleted_by,omitempty" doc:"Present only when DELETED: the registration id of the APPLIED delete."`
		Revisions       []RevisionEntry `json:"revisions" doc:"Ordered by revision; the last entry is the current state (the last values, once DELETED)."`
		PendingEdits    []PendingEdit   `json:"pending_edits" doc:"Undecided edits and deletes targeting this operation, in registration-id order."`
		RejectedEdits   []RejectedEdit  `json:"rejected_edits" doc:"Rejected (INVALID) edits and deletes targeting this operation, in registration-id order."`
	}
}

// SQL kept as consts beside their single call site (plan §0), unqualified. All
// three are reads; nothing in this package writes operation_revisions (Safety
// Invariant 3 — asserted by a source-level test).
const (
	// The current row of a regular operation, owner-scoped. edit_of is selected so
	// an edit or delete registration id can be refused (404) without a second
	// query; the deletion markers are set together once DELETED (ADR-0011).
	selectHistoryHead = `SELECT o.status, o.revision, o.edit_of, a.external_id, o.amount, o.effective_at, o.registered_at, o.revised_at, o.deleted_at, o.deleted_by
FROM operations o JOIN accounts a ON a.id = o.account_id
WHERE o.id = $1 AND a.owner_id = $2`

	// Every superseded revision, oldest first.
	selectRevisions = `SELECT r.revision, a.external_id, r.amount, r.effective_at, r.recorded_at, r.superseded_at, r.superseded_by
FROM operation_revisions r JOIN accounts a ON a.id = r.account_id
WHERE r.operation_id = $1
ORDER BY r.revision`

	// Undecided and rejected edits and deletes against the operation
	// (idx_ops_edit_of; is_delete tells the kinds apart). Applied edits are
	// reachable through operation_revisions.superseded_by instead, the applied
	// delete through deleted_by.
	selectEditsOf = `SELECT o.id, o.is_delete, a.external_id, o.amount, o.effective_at, o.expected_revision, o.status, o.confirmed_at, o.invalidation_reason
FROM operations o JOIN accounts a ON a.id = o.account_id
WHERE o.edit_of = $1 AND o.status IN ('PENDING', 'INVALID')
ORDER BY o.id`
)

func (s *Server) getOperationHistory(ctx context.Context, in *OperationHistoryInput) (*OperationHistoryOutput, error) {
	ownerID, err := s.resolver.Resolve(ctx, in.OwnerID)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}

	var (
		status       string
		revision     int32
		editOf       *int64
		account      string
		amount       int64
		effectiveAt  time.Time
		registeredAt time.Time
		revisedAt    *time.Time
		deletedAt    *time.Time
		deletedBy    *int64
	)
	err = s.pool.QueryRow(ctx, selectHistoryHead, in.ID, ownerID).
		Scan(&status, &revision, &editOf, &account, &amount, &effectiveAt, &registeredAt, &revisedAt, &deletedAt, &deletedBy)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && editOf != nil) {
		// Unknown, another owner's, or an edit/delete registration (both carry
		// edit_of): all indistinguishable to the caller by design (§2 — never leak
		// another owner's ids).
		return nil, huma.Error404NotFound("operation not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read operation", err)
	}

	revisions, err := s.readRevisions(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read operation revisions", err)
	}
	// The current revision lives on the operation row; recorded_at is when it
	// became current — registration until the first applied edit, revised_at after.
	recordedAt := registeredAt
	if revisedAt != nil {
		recordedAt = *revisedAt
	}
	revisions = append(revisions, RevisionEntry{
		Revision:    revision,
		Account:     account,
		Amount:      Amount(amount),
		EffectiveAt: effectiveAt.UTC(),
		RecordedAt:  recordedAt.UTC(),
	})

	pending, rejected, err := s.readEditsOf(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read operation edits", err)
	}

	out := &OperationHistoryOutput{}
	out.Body.OperationID = in.ID
	out.Body.Status = status
	out.Body.CurrentRevision = revision
	if deletedAt != nil {
		at := deletedAt.UTC()
		out.Body.DeletedAt = &at
	}
	out.Body.DeletedBy = deletedBy
	out.Body.Revisions = revisions
	out.Body.PendingEdits = pending
	out.Body.RejectedEdits = rejected
	return out, nil
}

// readRevisions returns the superseded revisions of an operation, oldest first.
func (s *Server) readRevisions(ctx context.Context, opID int64) ([]RevisionEntry, error) {
	rows, err := s.pool.Query(ctx, selectRevisions, opID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RevisionEntry
	for rows.Next() {
		var (
			e            RevisionEntry
			amount       int64
			supersededAt time.Time
			supersededBy int64
		)
		if err := rows.Scan(&e.Revision, &e.Account, &amount, &e.EffectiveAt, &e.RecordedAt, &supersededAt, &supersededBy); err != nil {
			return nil, err
		}
		e.Amount = Amount(amount)
		e.EffectiveAt = e.EffectiveAt.UTC()
		e.RecordedAt = e.RecordedAt.UTC()
		supersededAt = supersededAt.UTC()
		e.SupersededAt = &supersededAt
		e.SupersededBy = &supersededBy
		out = append(out, e)
	}
	return out, rows.Err()
}

// readEditsOf splits the undecided and rejected edits and deletes targeting an
// operation into the two history lists, both in registration-id order, each
// entry tagged with its kind. Slices are never nil so the JSON always carries
// the arrays.
func (s *Server) readEditsOf(ctx context.Context, opID int64) ([]PendingEdit, []RejectedEdit, error) {
	rows, err := s.pool.Query(ctx, selectEditsOf, opID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	pending := []PendingEdit{}
	rejected := []RejectedEdit{}
	for rows.Next() {
		var (
			id          int64
			isDelete    bool
			account     string
			amount      int64
			effectiveAt time.Time
			expected    *int32
			status      string
			decidedAt   *time.Time
			reason      *string
		)
		if err := rows.Scan(&id, &isDelete, &account, &amount, &effectiveAt, &expected, &status, &decidedAt, &reason); err != nil {
			return nil, nil, err
		}
		kind := registrationKind(isDelete)
		if status == string(model.OpPending) {
			pending = append(pending, PendingEdit{ID: id, Kind: kind, Account: account, Amount: Amount(amount), EffectiveAt: effectiveAt.UTC(), ExpectedRevision: expected})
			continue
		}
		r := RejectedEdit{ID: id, Kind: kind, Account: account, Amount: Amount(amount), EffectiveAt: effectiveAt.UTC(), ExpectedRevision: expected, Rejection: parseRejection(reason)}
		if decidedAt != nil {
			r.DecidedAt = decidedAt.UTC()
		}
		rejected = append(rejected, r)
	}
	return pending, rejected, rows.Err()
}

// parseRejection turns a reason column's JSON text into a Rejection for the edit
// wait and history loaders, mirroring the ledger query core: a NULL or
// unparseable column yields nil — the API never fabricates a rejection detail.
func parseRejection(reason *string) *model.Rejection {
	if reason == nil || *reason == "" {
		return nil
	}
	r, err := model.ParseRejection(*reason)
	if err != nil {
		return nil
	}
	return &r
}
