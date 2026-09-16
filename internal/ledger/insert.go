package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/magnomp/balancedb/internal/model"
)

// Insertion errors. They are sentinels so the HTTP layer (M7) can map each to a
// status code without string matching. A guard that trips one returns before (or
// rolls back) any write; the caller's transaction wrapper handles the rollback.
var (
	// ErrNoOperations — a request with zero operations. → 400.
	ErrNoOperations = errors.New("insert: request has no operations")
	// ErrInvalidIdempotencyKey — the key is not a canonical UUID. → 400.
	ErrInvalidIdempotencyKey = errors.New("insert: idempotency key must be a UUID")
	// ErrMixedOwners — a group whose legs do not all belong to one owner; the
	// sharding invariant (spec §2). → 422.
	ErrMixedOwners = errors.New("insert: all operations of a group must belong to one owner")
	// ErrGroupTooLarge — more operations than max_group_size (config table). → 422.
	ErrGroupTooLarge = errors.New("insert: group exceeds max_group_size")
	// ErrPayloadConflict — the idempotency key was reused with a different
	// payload (spec §10.1). → 422.
	ErrPayloadConflict = errors.New("insert: idempotency key reused with a different payload")
	// ErrZeroAmount — an operation amount of 0, rejected by the operations.amount
	// CHECK (spec §5.2). → 422.
	ErrZeroAmount = errors.New("insert: operation amount must be non-zero")
)

// Edit-item errors (ADR-0010). The structural ones fire before any query; the
// target ones fire after the owner-scoped lookup but before any write. HTTP
// mappings are per endpoint (a PATCH target miss is 404, a group item's is 422).
var (
	// ErrEditTargetNotFound — no operation with that id belongs to the requesting
	// owner. Unknown and foreign targets are deliberately indistinguishable.
	ErrEditTargetNotFound = errors.New("insert: edit target not found")
	// ErrEditTargetNotOperation — the target is itself an edit registration.
	ErrEditTargetNotOperation = errors.New("insert: edit target is an edit, not an operation")
	// ErrDuplicateEditTarget — two items of one request edit the same operation.
	ErrDuplicateEditTarget = errors.New("insert: duplicate edit target")
	// ErrEditChangesNothing — an edit item that sets none of amount, effective_at,
	// account.
	ErrEditChangesNothing = errors.New("insert: edit changes nothing")
	// ErrInvalidExpectedRevision — expected_revision set below 1.
	ErrInvalidExpectedRevision = errors.New("insert: expected_revision must be >= 1")
	// ErrEditsDisabled — config.allow_edits is false for this cell; already
	// registered edits are still decided.
	ErrEditsDisabled = errors.New("insert: editing is disabled for this cell")
	// ErrEditWithReversal — reversal_of on an edit item; reversal_of is immutable
	// metadata of a regular operation and never part of an edit.
	ErrEditWithReversal = errors.New("insert: reversal_of is not allowed on an edit item")
)

// Delete-item errors (ADR-0011). A delete is an edit-class registration, so the
// target sentinels above (ErrEditTargetNotFound, ErrEditTargetNotOperation,
// ErrDuplicateEditTarget, ErrInvalidExpectedRevision) are shared; the errors
// below are the ones only a delete item can trip.
var (
	// ErrDeleteWithFields — a delete item that also carries account, amount,
	// effective_at, reversal_of or edit_of; a delete names its target and an
	// optional revision guard, nothing else.
	ErrDeleteWithFields = errors.New("insert: a delete item carries only delete_of and expected_revision")
	// ErrDeletesDisabled — config.allow_deletes is false for this cell; already
	// registered deletes are still decided. Independent of allow_edits.
	ErrDeletesDisabled = errors.New("insert: deleting is disabled for this cell")
)

// TargetKind names which kind of item a target error concerns: the sentinels a
// delete shares with an edit stay one `errors.Is` each, and the kind travels in
// the wrapper so a transport can say "delete target 41" vs "edit target 41"
// with errors.As instead of string matching.
type TargetKind string

const (
	TargetEdit   TargetKind = "edit"
	TargetDelete TargetKind = "delete"
)

// TargetError wraps one of the shared target sentinels with the offending item's
// kind and the operation it names. Unwrap yields the sentinel, so errors.Is
// keeps working; errors.As(&TargetError{}) yields Kind and Target.
type TargetError struct {
	Kind   TargetKind
	Target int64
	Err    error
}

func (e *TargetError) Error() string { return fmt.Sprintf("%v: %s of %d", e.Err, e.Kind, e.Target) }
func (e *TargetError) Unwrap() error { return e.Err }

// targetErr builds the TargetError for an item's shared target sentinel.
func targetErr(sentinel error, op InsertOp) error {
	return &TargetError{Kind: op.targetKind(), Target: op.target(), Err: sentinel}
}

// uuidRE matches a canonical 8-4-4-4-12 hex UUID. Validated in Go so a malformed
// key returns a clean error instead of a Postgres 22P02 cast failure.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// InsertOp is one item in an insertion request. Amount is int64 minor units
// (already parsed via model.ParseAmount at the transport boundary); accounts are
// referenced by (OwnerID, ExternalID) and upserted on demand. ReversalOf is
// immutable metadata, unused by the engine (spec §5.4).
//
// With EditOf set the item is an edit registration of that operation (ADR-0010)
// and the zero value of ExternalID, Amount and EffectiveAt means "unchanged":
// insertion fills them from the target's current row so the edit row carries the
// full proposed state, while the idempotency hash covers the item as sent.
// ReversalOf must be nil on an edit item. ExpectedRevision, when set, must be
// >= 1 and is checked by the leader against the target's revision at decision
// time (STALE_REVISION on mismatch).
//
// With DeleteOf set the item is a delete registration of that operation
// (ADR-0011), exclusive with EditOf: every other field must be zero. Insertion
// copies the target's current account, amount and effective_at onto the row as
// an informational snapshot the leader never reads; the hash covers only the
// target and the guard. ExpectedRevision has the same meaning as on an edit.
type InsertOp struct {
	OwnerID     int64
	ExternalID  string
	Amount      int64
	EffectiveAt time.Time
	ReversalOf  *int64

	EditOf           *int64
	DeleteOf         *int64
	ExpectedRevision *int32
}

// isEdit reports whether the item is an edit registration.
func (op InsertOp) isEdit() bool { return op.EditOf != nil }

// isDelete reports whether the item is a delete registration.
func (op InsertOp) isDelete() bool { return op.DeleteOf != nil }

// targetsOp reports whether the item is edit-class: it names an existing
// operation (an edit or a delete) rather than registering a new one.
func (op InsertOp) targetsOp() bool { return op.isEdit() || op.isDelete() }

// target is the operation an edit-class item names. A delete wins when both
// EditOf and DeleteOf are set, matching validateTargetItems, which refuses such
// an item as a malformed delete. Only meaningful when targetsOp.
func (op InsertOp) target() int64 {
	if op.isDelete() {
		return *op.DeleteOf
	}
	return *op.EditOf
}

// targetKind is the kind reported in a TargetError for this item.
func (op InsertOp) targetKind() TargetKind {
	if op.isDelete() {
		return TargetDelete
	}
	return TargetEdit
}

// InsertRequest is one atomic unit: one operation is a single, two or more form a
// group. The idempotency key covers the whole request (spec §10.1).
type InsertRequest struct {
	IdempotencyKey string
	Operations     []InsertOp
}

// OpOutcome is a per-item result: its registration id and current status.
// On a fresh insert the status is PENDING; on a replay it is the row's current
// status. Exactly one of EditOf and DeleteOf is set on an edit-class item — the
// edited or deleted operation's id; both are nil on a regular operation.
type OpOutcome struct {
	ID       int64
	Status   string
	EditOf   *int64
	DeleteOf *int64
}

// setTarget fills EditOf / DeleteOf exclusively from a row's edit_of and
// is_delete: the column is shared, the marker says which kind the row is.
func (o *OpOutcome) setTarget(editOf *int64, isDelete bool) {
	o.EditOf, o.DeleteOf = nil, nil
	if isDelete {
		o.DeleteOf = editOf
		return
	}
	o.EditOf = editOf
}

// InsertResult reports what an insert produced. For a single, TransactionID is
// nil and TransactionStatus is empty. For a group, both are set. Operations is in
// registration (id) order — for a group this matches request order. Replayed is
// true when an idempotency key matched an existing record and no new rows were
// written.
type InsertResult struct {
	TransactionID     *int64
	TransactionStatus string
	Operations        []OpOutcome
	Replayed          bool
}

// SQL kept as consts beside their single call site (plan §0), written unqualified
// (search_path owns the schema).
const (
	selectConfig = `SELECT max_group_size, allow_edits, allow_deletes FROM config`

	// Edit-class target lookup (edits and deletes alike), owner-scoped through
	// accounts so an unknown id and another owner's operation are the same "not
	// found". Returns the current state used to fill omitted edit fields or the
	// delete's informational copy, plus edit_of to refuse targeting an edit-class
	// row. Status is deliberately not read: a DELETED, INVALID or PENDING target
	// is a decision-time outcome, never a submission refusal.
	selectEditTarget = `SELECT o.account_id, o.amount, o.effective_at, o.edit_of
FROM operations o JOIN accounts a ON a.id = o.account_id
WHERE o.id = $1 AND a.owner_id = $2`

	// Upsert-on-demand: create the account only if absent. DO NOTHING (not DO
	// UPDATE) so an existing account's row is never rewritten — that would create
	// needless MVCC churn and row-lock contention with the processor's version CAS
	// (spec §7.2). The empty-return path falls back to a plain SELECT.
	upsertAccount   = `INSERT INTO accounts (owner_id, external_id) VALUES ($1, $2) ON CONFLICT (owner_id, external_id) DO NOTHING RETURNING id`
	selectAccountID = `SELECT id FROM accounts WHERE owner_id = $1 AND external_id = $2`

	// Single: key + hash live on the operation. Probe-and-insert via ON CONFLICT
	// DO NOTHING so a concurrent-retry conflict returns no row instead of raising
	// 23505 (which would poison the surrounding transaction); ADR-0005.
	insertSingle = `INSERT INTO operations (account_id, amount, effective_at, reversal_of, edit_of, is_delete, expected_revision, idempotency_key, payload_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::uuid, $9)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING id`
	selectSingleByKey = `SELECT id, status, edit_of, is_delete, payload_hash FROM operations WHERE idempotency_key = $1::uuid`

	// Group: transaction row carries key + hash; legs carry transaction_id.
	insertTransaction = `INSERT INTO transactions (idempotency_key, payload_hash, op_count)
VALUES ($1::uuid, $2, $3) ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`
	selectTxByKey = `SELECT id, status, payload_hash, op_count FROM transactions WHERE idempotency_key = $1::uuid`
	insertLeg     = `INSERT INTO operations (account_id, amount, effective_at, reversal_of, edit_of, is_delete, expected_revision, transaction_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`
	selectLegs     = `SELECT id, status, edit_of, is_delete FROM operations WHERE transaction_id = $1 ORDER BY id`
	countLegs      = `SELECT count(*) FROM operations WHERE transaction_id = $1`
	notifyWorkStmt = `NOTIFY work_available`
)

// Insert performs one insertion within the caller's transaction (spec §10.1). It
// branches single vs group, upserts accounts on demand, enforces max_group_size
// and the one-owner-per-group sharding invariant, and applies Stripe-model
// idempotency: a repeated key with the same payload replays the original result,
// a repeated key with a different payload is ErrPayloadConflict. On a fresh insert
// it rings the processor doorbell before returning so the enclosing commit
// delivers it (ADR-0002); a replay writes nothing and does not ring.
//
// Edit items (ADR-0010) and delete items (ADR-0011) go through the same path:
// structural checks before any query, the allow_edits / allow_deletes policies
// with the config read, hashing as sent, then an owner-scoped target lookup that
// fills the omitted edit fields (or a delete's informational copy), and finally
// the same row writes with edit_of / is_delete / expected_revision bound. Every
// sentinel fires before any write.
//
// The caller owns the transaction (db.WithTx): a returned error must roll it back.
// Insert near commit: concurrent transactions are not serialized, so a higher ID
// can become visible and be decided before a still-uncommitted lower ID (ADR-0008).
func Insert(ctx context.Context, tx pgx.Tx, req InsertRequest) (*InsertResult, error) {
	if len(req.Operations) == 0 {
		return nil, ErrNoOperations
	}
	if !uuidRE.MatchString(req.IdempotencyKey) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidIdempotencyKey, req.IdempotencyKey)
	}

	// One-owner-per-group (spec §2), validated here and only here. Checked before
	// any DB access so a mixed-owner group never touches the database.
	owner := req.Operations[0].OwnerID
	for _, op := range req.Operations[1:] {
		if op.OwnerID != owner {
			return nil, ErrMixedOwners
		}
	}

	// Edit- and delete-item shape is also decided from the request alone, before
	// the first query: a malformed item never touches the database.
	hasEdits, hasDeletes, err := validateTargetItems(req.Operations)
	if err != nil {
		return nil, err
	}

	var (
		maxGroup     int
		allowEdits   bool
		allowDeletes bool
	)
	if err := tx.QueryRow(ctx, selectConfig).Scan(&maxGroup, &allowEdits, &allowDeletes); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if len(req.Operations) > maxGroup {
		return nil, fmt.Errorf("%w: %d operations, max %d", ErrGroupTooLarge, len(req.Operations), maxGroup)
	}
	if hasEdits && !allowEdits {
		return nil, ErrEditsDisabled
	}
	if hasDeletes && !allowDeletes {
		return nil, ErrDeletesDisabled
	}

	hash, err := model.HashPayload(canonicalOps(req.Operations))
	if err != nil {
		return nil, err
	}

	ops, err := prepareOps(ctx, tx, req.Operations)
	if err != nil {
		return nil, err
	}

	if len(ops) == 1 {
		return insertSingleOp(ctx, tx, req.IdempotencyKey, ops[0], hash)
	}
	return insertGroup(ctx, tx, req.IdempotencyKey, ops, hash)
}

// validateTargetItems runs the structural edit-class checks that need no
// database: reversal_of on an edit, any extra field on a delete,
// expected_revision below 1, an edit that changes nothing, and two items
// naming one target (in any mix of edits and deletes — the seen set is shared).
// It reports whether the request carries edits and deletes so the caller can
// apply the allow_edits and allow_deletes policies independently.
func validateTargetItems(ops []InsertOp) (hasEdits, hasDeletes bool, err error) {
	seen := make(map[int64]struct{})
	for _, op := range ops {
		if !op.targetsOp() {
			continue
		}
		target := op.target()
		if op.isDelete() {
			hasDeletes = true
			if op.EditOf != nil || op.ReversalOf != nil || op.ExternalID != "" || op.Amount != 0 || !op.EffectiveAt.IsZero() {
				return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ErrDeleteWithFields, target)
			}
		} else {
			hasEdits = true
			if op.ReversalOf != nil {
				return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ErrEditWithReversal, target)
			}
		}
		if op.ExpectedRevision != nil && *op.ExpectedRevision < 1 {
			return hasEdits, hasDeletes, fmt.Errorf("%w: got %d", ErrInvalidExpectedRevision, *op.ExpectedRevision)
		}
		if op.isEdit() && op.ExternalID == "" && op.Amount == 0 && op.EffectiveAt.IsZero() {
			return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ErrEditChangesNothing, target)
		}
		if _, dup := seen[target]; dup {
			return hasEdits, hasDeletes, targetErr(ErrDuplicateEditTarget, op)
		}
		seen[target] = struct{}{}
	}
	return hasEdits, hasDeletes, nil
}

// canonicalOps builds the idempotency payload as sent: a regular item is a
// CanonicalOp (frozen encoding); an edit item is a CanonicalEditOp whose omitted
// fields stay nil even though insertion later fills them from the target; a
// delete item is a CanonicalDeleteOp — target and guard only, never the
// informational copy insertion writes to the row.
func canonicalOps(ops []InsertOp) []any {
	out := make([]any, len(ops))
	for i, op := range ops {
		if op.isDelete() {
			out[i] = model.CanonicalDeleteOp{
				OwnerID:          op.OwnerID,
				DeleteOf:         *op.DeleteOf,
				ExpectedRevision: op.ExpectedRevision,
			}
			continue
		}
		if !op.isEdit() {
			out[i] = model.CanonicalOp{
				OwnerID:     op.OwnerID,
				Account:     op.ExternalID,
				Amount:      op.Amount,
				EffectiveAt: op.EffectiveAt,
				ReversalOf:  op.ReversalOf,
			}
			continue
		}
		edit := model.CanonicalEditOp{
			OwnerID:          op.OwnerID,
			EditOf:           *op.EditOf,
			ExpectedRevision: op.ExpectedRevision,
		}
		if op.ExternalID != "" {
			account := op.ExternalID
			edit.Account = &account
		}
		if op.Amount != 0 {
			amount := op.Amount
			edit.Amount = &amount
		}
		if !op.EffectiveAt.IsZero() {
			at := op.EffectiveAt
			edit.EffectiveAt = &at
		}
		out[i] = edit
	}
	return out
}

// editTarget is an edit-class target's current state as read at submission:
// the source of every field an edit item left unchanged, and of the
// informational copy a delete item carries.
type editTarget struct {
	AccountID   int64
	Amount      int64
	EffectiveAt time.Time
}

// writeOp is one item ready to be written as an operations row. AccountID is
// set when the account is already known (a resolved edit whose account is
// unchanged, or a delete); otherwise it is 0 and (OwnerID, ExternalID) is
// upserted on demand. A delete is an edit-class row (EditOf = the target) with
// IsDelete set.
type writeOp struct {
	OwnerID          int64
	ExternalID       string
	AccountID        int64
	Amount           int64
	EffectiveAt      time.Time
	ReversalOf       *int64
	EditOf           *int64
	IsDelete         bool
	ExpectedRevision *int32
}

// outcome is the fresh-insert result of a written row: PENDING, with the
// target reported under EditOf or DeleteOf according to the row's kind.
func (op writeOp) outcome(id int64) OpOutcome {
	o := OpOutcome{ID: id, Status: string(model.OpPending)}
	o.setTarget(op.EditOf, op.IsDelete)
	return o
}

// resolveEditItem fills the omitted fields of an edit item from its target so the
// row carries the full proposed state (spec Key Decisions): zero Amount and
// EffectiveAt take the target's current values; an empty ExternalID keeps the
// target's account id, a set one names the account to upsert for the owner.
// Pure — the target row is read by the caller.
func resolveEditItem(op InsertOp, target editTarget) writeOp {
	out := writeOp{
		OwnerID:          op.OwnerID,
		AccountID:        target.AccountID,
		Amount:           op.Amount,
		EffectiveAt:      op.EffectiveAt,
		EditOf:           op.EditOf,
		ExpectedRevision: op.ExpectedRevision,
	}
	if op.ExternalID != "" {
		out.AccountID = 0
		out.ExternalID = op.ExternalID
	}
	if op.Amount == 0 {
		out.Amount = target.Amount
	}
	if op.EffectiveAt.IsZero() {
		out.EffectiveAt = target.EffectiveAt
	}
	return out
}

// resolveDeleteItem builds the edit-class row of a delete item: the target's
// current account, amount and effective_at copied verbatim as the informational
// snapshot (spec Data Models — the processor never reads them), EditOf set to
// the target, IsDelete marking the kind, ExpectedRevision passed through. Pure —
// the target row is read by the caller.
func resolveDeleteItem(op InsertOp, target editTarget) writeOp {
	return writeOp{
		OwnerID:          op.OwnerID,
		AccountID:        target.AccountID,
		Amount:           target.Amount,
		EffectiveAt:      target.EffectiveAt,
		EditOf:           op.DeleteOf,
		IsDelete:         true,
		ExpectedRevision: op.ExpectedRevision,
	}
}

// prepareOps turns the request items into rows to write. Every edit-class
// target (edit or delete) is looked up (owner-scoped) and its row resolved
// before the first write, so a bad target in the last item of a group leaves no
// transactions row behind. Regular items pass through unchanged. Nothing is
// written.
func prepareOps(ctx context.Context, tx pgx.Tx, ops []InsertOp) ([]writeOp, error) {
	out := make([]writeOp, len(ops))
	for i, op := range ops {
		if !op.targetsOp() {
			out[i] = writeOp{
				OwnerID:     op.OwnerID,
				ExternalID:  op.ExternalID,
				Amount:      op.Amount,
				EffectiveAt: op.EffectiveAt,
				ReversalOf:  op.ReversalOf,
			}
			continue
		}
		var (
			target   editTarget
			targetOf *int64
		)
		err := tx.QueryRow(ctx, selectEditTarget, op.target(), op.OwnerID).
			Scan(&target.AccountID, &target.Amount, &target.EffectiveAt, &targetOf)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil, targetErr(ErrEditTargetNotFound, op)
		case err != nil:
			return nil, fmt.Errorf("resolve %s target %d: %w", op.targetKind(), op.target(), err)
		case targetOf != nil:
			return nil, targetErr(ErrEditTargetNotOperation, op)
		}
		if op.isDelete() {
			out[i] = resolveDeleteItem(op, target)
			continue
		}
		out[i] = resolveEditItem(op, target)
	}
	return out, nil
}

// accountIDFor resolves the account a row is written against: the id already
// carried by the item, otherwise the on-demand upsert of (owner, external_id).
func accountIDFor(ctx context.Context, tx pgx.Tx, op writeOp) (int64, error) {
	if op.AccountID != 0 {
		return op.AccountID, nil
	}
	return upsertAccountID(ctx, tx, op.OwnerID, op.ExternalID)
}

func insertSingleOp(ctx context.Context, tx pgx.Tx, key string, op writeOp, hash []byte) (*InsertResult, error) {
	accountID, err := accountIDFor(ctx, tx, op)
	if err != nil {
		return nil, err
	}

	var id int64
	err = tx.QueryRow(ctx, insertSingle, accountID, op.Amount, op.EffectiveAt, op.ReversalOf, op.EditOf, op.IsDelete, op.ExpectedRevision, key, hash).Scan(&id)
	switch {
	case err == nil:
		// Fresh insert: new PENDING work exists, so ring the doorbell.
		if err := ringDoorbell(ctx, tx); err != nil {
			return nil, err
		}
		return &InsertResult{Operations: []OpOutcome{op.outcome(id)}}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Key already present: replay the original (spec §10.1).
		return replaySingle(ctx, tx, key, hash)
	default:
		return nil, mapInsertError(err)
	}
}

func replaySingle(ctx context.Context, tx pgx.Tx, key string, hash []byte) (*InsertResult, error) {
	var (
		o            OpOutcome
		editOf       *int64
		isDelete     bool
		existingHash []byte
	)
	if err := tx.QueryRow(ctx, selectSingleByKey, key).Scan(&o.ID, &o.Status, &editOf, &isDelete, &existingHash); err != nil {
		return nil, fmt.Errorf("fetch existing operation by key: %w", err)
	}
	if !bytes.Equal(existingHash, hash) {
		return nil, ErrPayloadConflict
	}
	o.setTarget(editOf, isDelete)
	return &InsertResult{Operations: []OpOutcome{o}, Replayed: true}, nil
}

func insertGroup(ctx context.Context, tx pgx.Tx, key string, ops []writeOp, hash []byte) (*InsertResult, error) {
	var txID int64
	err := tx.QueryRow(ctx, insertTransaction, key, hash, len(ops)).Scan(&txID)
	switch {
	case err == nil:
		// Fresh group.
	case errors.Is(err, pgx.ErrNoRows):
		return replayGroup(ctx, tx, key, hash)
	default:
		return nil, fmt.Errorf("insert transaction: %w", err)
	}

	outcomes := make([]OpOutcome, 0, len(ops))
	for _, op := range ops {
		accountID, err := accountIDFor(ctx, tx, op)
		if err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRow(ctx, insertLeg, accountID, op.Amount, op.EffectiveAt, op.ReversalOf, op.EditOf, op.IsDelete, op.ExpectedRevision, txID).Scan(&id); err != nil {
			return nil, mapInsertError(err)
		}
		outcomes = append(outcomes, op.outcome(id))
	}

	// op_count cross-check: the transaction row's op_count must equal the number
	// of legs actually written (spec §8.3 cross-check, enforced on the insert side
	// so a partial group can never be committed).
	var legCount int
	if err := tx.QueryRow(ctx, countLegs, txID).Scan(&legCount); err != nil {
		return nil, fmt.Errorf("op_count cross-check: %w", err)
	}
	if legCount != len(ops) {
		return nil, fmt.Errorf("op_count cross-check: wrote %d legs, expected %d", legCount, len(ops))
	}

	if err := ringDoorbell(ctx, tx); err != nil {
		return nil, err
	}
	return &InsertResult{
		TransactionID:     &txID,
		TransactionStatus: string(model.TxPending),
		Operations:        outcomes,
	}, nil
}

func replayGroup(ctx context.Context, tx pgx.Tx, key string, hash []byte) (*InsertResult, error) {
	var (
		txID         int64
		status       string
		existingHash []byte
		opCount      int
	)
	if err := tx.QueryRow(ctx, selectTxByKey, key).Scan(&txID, &status, &existingHash, &opCount); err != nil {
		return nil, fmt.Errorf("fetch existing transaction by key: %w", err)
	}
	if !bytes.Equal(existingHash, hash) {
		return nil, ErrPayloadConflict
	}

	rows, err := tx.Query(ctx, selectLegs, txID)
	if err != nil {
		return nil, fmt.Errorf("load group legs: %w", err)
	}
	defer rows.Close()

	var outcomes []OpOutcome
	for rows.Next() {
		var (
			o        OpOutcome
			editOf   *int64
			isDelete bool
		)
		if err := rows.Scan(&o.ID, &o.Status, &editOf, &isDelete); err != nil {
			return nil, fmt.Errorf("scan group leg: %w", err)
		}
		o.setTarget(editOf, isDelete)
		outcomes = append(outcomes, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group legs: %w", err)
	}
	if len(outcomes) != opCount {
		return nil, fmt.Errorf("op_count cross-check on replay: loaded %d legs, op_count %d", len(outcomes), opCount)
	}

	return &InsertResult{
		TransactionID:     &txID,
		TransactionStatus: status,
		Operations:        outcomes,
		Replayed:          true,
	}, nil
}

// upsertAccountID resolves (owner, external_id) to an account id, creating the
// account with unbounded (NULL) limits if absent. It writes only when creating.
func upsertAccountID(ctx context.Context, tx pgx.Tx, ownerID int64, externalID string) (int64, error) {
	var id int64
	err := tx.QueryRow(ctx, upsertAccount, ownerID, externalID).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, pgx.ErrNoRows):
		if err := tx.QueryRow(ctx, selectAccountID, ownerID, externalID).Scan(&id); err != nil {
			return 0, fmt.Errorf("resolve account (%d,%q) after conflict: %w", ownerID, externalID, err)
		}
		return id, nil
	default:
		return 0, fmt.Errorf("upsert account (%d,%q): %w", ownerID, externalID, err)
	}
}

// ringDoorbell emits the payload-free work doorbell (ADR-0002). It is a pure
// wakeup hint carrying no correctness weight; because it runs inside the caller's
// transaction, Postgres delivers it only if that transaction commits.
func ringDoorbell(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, notifyWorkStmt); err != nil {
		return fmt.Errorf("notify work_available: %w", err)
	}
	return nil
}

// mapInsertError translates the one CHECK a well-formed insert can trip — the
// amount <> 0 constraint (spec §5.2) — into a sentinel; other errors pass
// through. The edit CHECKs of migration 0002 (ops_edit_not_reversal,
// ops_expected_rev_pos, ops_edit_not_self) are database backstops for rules
// already enforced in validateTargetItems (as is 0003's ops_delete_is_edit), so
// they are never expected here and are not mapped.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" && pgErr.ConstraintName == amountCheckName {
		return fmt.Errorf("%w: %s", ErrZeroAmount, pgErr.ConstraintName)
	}
	return fmt.Errorf("insert operation: %w", err)
}

// amountCheckName is the Postgres-generated name of the inline
// `amount BIGINT NOT NULL CHECK (amount <> 0)` constraint in 0001_init.sql.
const amountCheckName = "operations_amount_check"
