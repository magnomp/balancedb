package simtest

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/ledger"
	"github.com/magnomp/balancedb/internal/model"
)

// DefaultMaxGroupSize mirrors the config-table default (spec §5.2). The reference
// model holds it as a field so a schedule can vary it just like the real config.
const DefaultMaxGroupSize = 10

// refOp is one recorded operation in the reference model. EffectiveAt and
// ReversalOf are carried so the reference can reproduce the DB's timeline order
// (G4: order by (effective_at, id)) and its reversal metadata; neither influences
// the decision (a reversal is an ordinary operation validated against limits,
// spec §5.4/N3).
//
// EditOf set marks an edit-class registration (ADR-0010): on a plain edit
// AccountID, Amount and EffectiveAt hold the full proposed state resolved at
// registration; on a delete (IsDelete, ADR-0011 — the storage marker edit_of +
// is_delete, mirrored here) they are the informational copy of the target at
// submission, which no decision reads. ExpectedRevision is the optional
// optimistic guard of either kind. On a regular operation Revision is the current
// revision (1 until the first applied edit); it is unused (1) on edit-class rows.
// An applied edit overwrites its target's current columns in place — the timeline
// then orders the target by its new effective_at with its original id — and
// appends the superseded state to Model.revisions. An applied delete flips its
// target CONFIRMED → DELETED and stamps DeletedBy on it; the target's last values
// and Revision stay, and nothing is appended.
type refOp struct {
	ID               int64
	AccountID        int64
	Amount           int64
	EffectiveAt      time.Time
	ReversalOf       *int64
	TransactionID    *int64
	Status           model.OpStatus
	EditOf           *int64
	IsDelete         bool
	ExpectedRevision *int32
	Revision         int32
	DeletedBy        *int64
}

// isDelete reports whether the row is a delete registration.
func (op *refOp) isDelete() bool { return op.EditOf != nil && op.IsDelete }

// outcome renders the row as the ledger's OpOutcome: the target under DeleteOf
// for a delete registration, under EditOf for a plain edit, neither otherwise.
func (op *refOp) outcome() api.OpOutcome {
	o := api.OpOutcome{ID: op.ID, Status: string(op.Status)}
	if op.isDelete() {
		o.DeleteOf = op.EditOf
	} else {
		o.EditOf = op.EditOf
	}
	return o
}

// refRevision is one superseded state of a regular operation — a row of
// operation_revisions — appended when an edit applies and never changed again.
type refRevision struct {
	Revision     int32
	AccountID    int64
	Amount       int64
	EffectiveAt  time.Time
	SupersededBy int64
}

// refTx is one recorded group transaction.
type refTx struct {
	ID          int64
	OpCount     int
	PayloadHash []byte
	LegIDs      []int64
	Status      model.TxStatus
}

// refAccount tracks an account's limits and its FINAL (confirmed) balance — the
// object of G1. NULL limits are unbounded. ExternalID is carried so a group
// rejection can name the offending account exactly as the processor does.
type refAccount struct {
	ID         int64
	ExternalID string
	Min        *int64
	Max        *int64
	Balance    int64
}

// Model is the sequential reference model. It replays insertion in registration
// order, assigning ids from monotonic counters exactly as Postgres identity
// columns do, and enforces the same insert-time rules as internal/api.Insert. It
// is the source of truth later milestones assert the database against.
type Model struct {
	MaxGroupSize int
	// AllowEdits mirrors config.allow_edits (ADR-0010): false refuses new edit
	// registrations with ledger.ErrEditsDisabled; registered edits are still decided.
	AllowEdits bool
	// AllowDeletes mirrors config.allow_deletes (ADR-0011), independent of
	// AllowEdits: false refuses new delete registrations with
	// ledger.ErrDeletesDisabled; registered deletes are still decided.
	AllowDeletes bool

	// IDs are allocated at insertion, but only committed rows can be decided.
	// Independent host transactions may expose higher IDs first (ADR-0008).
	uncommitted map[int64]bool

	nextAccountID int64
	nextOpID      int64
	nextTxID      int64

	accounts     map[accountKey]int64
	accountsByID map[int64]*refAccount
	ops          map[int64]*refOp
	txs          map[int64]*refTx
	// revisions holds each regular operation's superseded states in revision
	// order (operation_revisions), appended only by an applied edit.
	revisions map[int64][]refRevision

	// Idempotency indexes, keyed by idempotency key. singleByKey stores the
	// operation id + payload hash of a single; groupByKey the transaction id +
	// payload hash of a group.
	singleByKey map[string]keyedRecord
	groupByKey  map[string]keyedRecord
}

type accountKey struct {
	ownerID    int64
	externalID string
}

type keyedRecord struct {
	id   int64
	hash []byte
}

// NewModel returns an empty reference model with the default max group size.
func NewModel() *Model {
	return &Model{
		MaxGroupSize: DefaultMaxGroupSize,
		AllowEdits:   true,
		AllowDeletes: true,
		uncommitted:  make(map[int64]bool),
		accounts:     make(map[accountKey]int64),
		accountsByID: make(map[int64]*refAccount),
		ops:          make(map[int64]*refOp),
		txs:          make(map[int64]*refTx),
		revisions:    make(map[int64][]refRevision),
		singleByKey:  make(map[string]keyedRecord),
		groupByKey:   make(map[string]keyedRecord),
	}
}

// CreateAccount models explicit creation, including the initial G1 check (ADR-0009).
func (m *Model) CreateAccount(owner int64, externalID string, min, max *int64) error {
	if owner <= 0 || externalID == "" {
		return ledger.ErrInvalidArgument
	}
	if (min != nil && *min > 0) || (max != nil && *max < 0) || (min != nil && max != nil && *min > *max) {
		return ledger.ErrInvalidLimits
	}
	if _, exists := m.accounts[accountKey{owner, externalID}]; exists {
		return ledger.ErrAccountExists
	}
	id := m.upsertAccount(owner, externalID)
	a := m.accountsByID[id]
	if min != nil {
		value := *min
		a.Min = &value
	}
	if max != nil {
		value := *max
		a.Max = &value
	}
	return nil
}

// OpCount returns the number of operation rows the model holds — the quantity G6
// forbids a replay from growing.
func (m *Model) OpCount() int { return len(m.ops) }

// BeginRegistration models a successful fresh insert in an open host transaction.
// This visibility seam uses preexisting accounts and distinct idempotency keys;
// it does not model PostgreSQL constraint-lock waits or transaction-local replays.
// Existing schedules use Insert for immediately committed registration.
func (m *Model) BeginRegistration(req api.InsertRequest) (*api.InsertResult, error) {
	for _, op := range req.Operations {
		if (op.EditOf != nil || op.DeleteOf != nil) && op.ExternalID == "" {
			continue // the account comes from the target
		}
		if _, ok := m.accounts[accountKey{ownerID: op.OwnerID, externalID: op.ExternalID}]; !ok {
			return nil, fmt.Errorf("reference: visibility schedule requires preexisting accounts")
		}
	}
	result, err := m.Insert(req)
	if err != nil {
		return nil, err
	}
	if result.Replayed {
		return nil, fmt.Errorf("reference: visibility schedule requires a fresh key")
	}
	for _, op := range result.Operations {
		m.uncommitted[op.ID] = true
	}
	return result, nil
}

// CommitRegistration exposes all legs together, retaining their allocated IDs.
func (m *Model) CommitRegistration(result *api.InsertResult) {
	for _, op := range result.Operations {
		delete(m.uncommitted, op.ID)
	}
}

// RollbackRegistration discards an open registration and its idempotency key.
// Allocated IDs remain consumed, like PostgreSQL sequences after rollback.
func (m *Model) RollbackRegistration(result *api.InsertResult) {
	for _, op := range result.Operations {
		delete(m.uncommitted, op.ID)
		delete(m.ops, op.ID)
		for key, record := range m.singleByKey {
			if record.id == op.ID {
				delete(m.singleByKey, key)
			}
		}
	}
	if result.TransactionID != nil {
		delete(m.txs, *result.TransactionID)
		for key, record := range m.groupByKey {
			if record.id == *result.TransactionID {
				delete(m.groupByKey, key)
			}
		}
	}
}

// Insert replays one insertion and returns the same shape of result the real
// inserter returns, including the same sentinel errors (api.ErrNoOperations,
// api.ErrInvalidIdempotencyKey, api.ErrMixedOwners, api.ErrGroupTooLarge,
// api.ErrPayloadConflict, api.ErrZeroAmount, and the ledger edit sentinels).
//
// Edit items (ADR-0010) and delete items (ADR-0011) follow internal/ledger.Insert:
// structural checks before anything else, the allow_edits / allow_deletes
// policies, hashing as sent, then an owner-scoped target lookup that fills the
// omitted edit fields (or a delete's informational copy) — all before the first
// row is recorded, so a bad target in the last item of a group records nothing.
func (m *Model) Insert(req api.InsertRequest) (*api.InsertResult, error) {
	if len(req.Operations) == 0 {
		return nil, api.ErrNoOperations
	}
	if !isUUID(req.IdempotencyKey) {
		return nil, fmt.Errorf("%w: %q", api.ErrInvalidIdempotencyKey, req.IdempotencyKey)
	}
	owner := req.Operations[0].OwnerID
	for _, op := range req.Operations[1:] {
		if op.OwnerID != owner {
			return nil, api.ErrMixedOwners
		}
	}
	hasEdits, hasDeletes, err := validateTargetItems(req.Operations)
	if err != nil {
		return nil, err
	}
	if len(req.Operations) > m.MaxGroupSize {
		return nil, fmt.Errorf("%w: %d operations, max %d", api.ErrGroupTooLarge, len(req.Operations), m.MaxGroupSize)
	}
	if hasEdits && !m.AllowEdits {
		return nil, ledger.ErrEditsDisabled
	}
	if hasDeletes && !m.AllowDeletes {
		return nil, ledger.ErrDeletesDisabled
	}
	for _, op := range req.Operations {
		if op.EditOf == nil && op.DeleteOf == nil && op.Amount == 0 {
			// Mirrors the operations.amount CHECK, which the DB enforces atomically
			// for the whole insert — so nothing is recorded. (On an edit item a zero
			// amount means "unchanged"; a delete item carries no amount at all.)
			return nil, fmt.Errorf("%w", api.ErrZeroAmount)
		}
	}

	hash, err := model.HashPayload(canonicalOps(req.Operations))
	if err != nil {
		return nil, err
	}
	rows, err := m.prepareOps(req.Operations)
	if err != nil {
		return nil, err
	}
	if len(rows) == 1 {
		return m.insertSingle(req.IdempotencyKey, rows[0], hash)
	}
	return m.insertGroup(req.IdempotencyKey, rows, hash)
}

// validateTargetItems mirrors internal/ledger.validateTargetItems: the
// structural edit-class checks that need no state — reversal_of on an edit, any
// extra field on a delete, expected_revision below 1, an edit that changes
// nothing, two items naming one target (edits and deletes share the seen set).
func validateTargetItems(ops []api.InsertOp) (hasEdits, hasDeletes bool, err error) {
	seen := make(map[int64]struct{})
	for _, op := range ops {
		if op.EditOf == nil && op.DeleteOf == nil {
			continue
		}
		var target int64
		if op.DeleteOf != nil {
			hasDeletes = true
			target = *op.DeleteOf
			if op.EditOf != nil || op.ReversalOf != nil || op.ExternalID != "" || op.Amount != 0 || !op.EffectiveAt.IsZero() {
				return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ledger.ErrDeleteWithFields, target)
			}
		} else {
			hasEdits = true
			target = *op.EditOf
			if op.ReversalOf != nil {
				return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ledger.ErrEditWithReversal, target)
			}
		}
		if op.ExpectedRevision != nil && *op.ExpectedRevision < 1 {
			return hasEdits, hasDeletes, fmt.Errorf("%w: got %d", ledger.ErrInvalidExpectedRevision, *op.ExpectedRevision)
		}
		if op.DeleteOf == nil && op.ExternalID == "" && op.Amount == 0 && op.EffectiveAt.IsZero() {
			return hasEdits, hasDeletes, fmt.Errorf("%w: target %d", ledger.ErrEditChangesNothing, target)
		}
		if _, dup := seen[target]; dup {
			return hasEdits, hasDeletes, fmt.Errorf("%w: %d", ledger.ErrDuplicateEditTarget, target)
		}
		seen[target] = struct{}{}
	}
	return hasEdits, hasDeletes, nil
}

// writeOp is one item ready to be recorded, mirroring the ledger's writeOp:
// AccountID is set when the account is already known (a resolved edit whose
// account is unchanged, or a delete), otherwise (OwnerID, ExternalID) is
// upserted at write. A delete carries EditOf = the target and IsDelete.
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

// prepareOps resolves every edit-class item against its target — owner-scoped
// and visibility-aware (an uncommitted target is not found, exactly as the
// ledger's SELECT cannot see it) — filling an edit's omitted fields or a delete's
// informational copy, before anything is recorded. The target's status is
// deliberately not checked (a DELETED, INVALID or PENDING target is a
// decision-time outcome). Regular items pass through unchanged.
func (m *Model) prepareOps(ops []api.InsertOp) ([]writeOp, error) {
	out := make([]writeOp, len(ops))
	for i, op := range ops {
		if op.EditOf == nil && op.DeleteOf == nil {
			out[i] = writeOp{OwnerID: op.OwnerID, ExternalID: op.ExternalID, Amount: op.Amount, EffectiveAt: op.EffectiveAt, ReversalOf: op.ReversalOf}
			continue
		}
		targetID := op.EditOf
		if op.DeleteOf != nil {
			targetID = op.DeleteOf
		}
		target, ok := m.ops[*targetID]
		if !ok || m.uncommitted[*targetID] || m.ownerOf(target.AccountID) != op.OwnerID {
			return nil, fmt.Errorf("%w: %d", ledger.ErrEditTargetNotFound, *targetID)
		}
		if target.EditOf != nil {
			return nil, fmt.Errorf("%w: %d", ledger.ErrEditTargetNotOperation, *targetID)
		}
		if op.DeleteOf != nil {
			out[i] = writeOp{
				OwnerID: op.OwnerID, AccountID: target.AccountID, Amount: target.Amount, EffectiveAt: target.EffectiveAt,
				EditOf: op.DeleteOf, IsDelete: true, ExpectedRevision: op.ExpectedRevision,
			}
			continue
		}
		w := writeOp{
			OwnerID: op.OwnerID, AccountID: target.AccountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt,
			EditOf: op.EditOf, ExpectedRevision: op.ExpectedRevision,
		}
		if op.ExternalID != "" {
			w.AccountID = 0
			w.ExternalID = op.ExternalID
		}
		if op.Amount == 0 {
			w.Amount = target.Amount
		}
		if op.EffectiveAt.IsZero() {
			w.EffectiveAt = target.EffectiveAt
		}
		out[i] = w
	}
	return out, nil
}

// accountIDFor resolves the account a row is recorded against: the id already
// carried by the item, otherwise the on-demand upsert of (owner, external_id).
func (m *Model) accountIDFor(op writeOp) int64 {
	if op.AccountID != 0 {
		return op.AccountID
	}
	return m.upsertAccount(op.OwnerID, op.ExternalID)
}

func (m *Model) insertSingle(key string, op writeOp, hash []byte) (*api.InsertResult, error) {
	if rec, ok := m.singleByKey[key]; ok {
		if !bytes.Equal(rec.hash, hash) {
			return nil, api.ErrPayloadConflict
		}
		existing := m.ops[rec.id]
		return &api.InsertResult{
			Operations: []api.OpOutcome{existing.outcome()},
			Replayed:   true,
		}, nil
	}

	accountID := m.accountIDFor(op)
	m.nextOpID++
	rec := &refOp{
		ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt, ReversalOf: op.ReversalOf,
		Status: model.OpPending, EditOf: op.EditOf, IsDelete: op.IsDelete, ExpectedRevision: op.ExpectedRevision, Revision: 1,
	}
	m.ops[rec.ID] = rec
	m.singleByKey[key] = keyedRecord{id: rec.ID, hash: hash}
	return &api.InsertResult{Operations: []api.OpOutcome{rec.outcome()}}, nil
}

func (m *Model) insertGroup(key string, ops []writeOp, hash []byte) (*api.InsertResult, error) {
	if rec, ok := m.groupByKey[key]; ok {
		if !bytes.Equal(rec.hash, hash) {
			return nil, api.ErrPayloadConflict
		}
		tx := m.txs[rec.id]
		outcomes := make([]api.OpOutcome, len(tx.LegIDs))
		for i, id := range tx.LegIDs {
			outcomes[i] = m.ops[id].outcome()
		}
		txID := tx.ID
		return &api.InsertResult{
			TransactionID:     &txID,
			TransactionStatus: string(tx.Status),
			Operations:        outcomes,
			Replayed:          true,
		}, nil
	}

	m.nextTxID++
	tx := &refTx{ID: m.nextTxID, OpCount: len(ops), PayloadHash: hash, Status: model.TxPending}
	outcomes := make([]api.OpOutcome, 0, len(ops))
	for _, op := range ops {
		accountID := m.accountIDFor(op)
		m.nextOpID++
		txID := tx.ID
		rec := &refOp{
			ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt, ReversalOf: op.ReversalOf,
			TransactionID: &txID, Status: model.OpPending, EditOf: op.EditOf, IsDelete: op.IsDelete, ExpectedRevision: op.ExpectedRevision, Revision: 1,
		}
		m.ops[rec.ID] = rec
		tx.LegIDs = append(tx.LegIDs, rec.ID)
		outcomes = append(outcomes, rec.outcome())
	}
	m.txs[tx.ID] = tx
	m.groupByKey[key] = keyedRecord{id: tx.ID, hash: hash}

	txID := tx.ID
	return &api.InsertResult{
		TransactionID:     &txID,
		TransactionStatus: string(model.TxPending),
		Operations:        outcomes,
	}, nil
}

func (m *Model) upsertAccount(ownerID int64, externalID string) int64 {
	k := accountKey{ownerID: ownerID, externalID: externalID}
	if id, ok := m.accounts[k]; ok {
		return id
	}
	m.nextAccountID++
	id := m.nextAccountID
	m.accounts[k] = id
	m.accountsByID[id] = &refAccount{ID: id, ExternalID: externalID}
	return id
}

// Timeline returns the ids of an account's operations in timeline order —
// (effective_at, id) — the total, unique, immutable order of G4 (spec §4.1/§5.4).
// Because id is a unique tiebreaker the order is total and unique by construction;
// this is the sequence the database's `ORDER BY effective_at, id` must reproduce.
func (m *Model) Timeline(accountID int64) []int64 {
	type te struct {
		id  int64
		eff time.Time
	}
	var es []te
	for _, op := range m.ops {
		if op.AccountID == accountID {
			es = append(es, te{id: op.ID, eff: op.EffectiveAt})
		}
	}
	sort.Slice(es, func(i, j int) bool {
		if !es[i].eff.Equal(es[j].eff) {
			return es[i].eff.Before(es[j].eff)
		}
		return es[i].id < es[j].id
	})
	out := make([]int64, len(es))
	for i, e := range es {
		out[i] = e.id
	}
	return out
}

// AccountIDs returns every account id the model knows, ascending — used to iterate
// the per-account timeline for G4 and to pick limit-change / reversal targets.
func (m *Model) AccountIDs() []int64 {
	ids := make([]int64, 0, len(m.accountsByID))
	for id := range m.accountsByID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// CheckG6 verifies the idempotency guarantee's structural half (spec §4.1): the
// number of recorded operations equals the number of distinct idempotency keys'
// legs — i.e. no key produced duplicate rows. It returns an error, or nil.
func (m *Model) CheckG6() error {
	want := 0
	for _, rec := range m.singleByKey {
		if _, ok := m.ops[rec.id]; ok {
			want++
		}
	}
	for _, rec := range m.groupByKey {
		if tx, ok := m.txs[rec.id]; ok {
			want += len(tx.LegIDs)
		}
	}
	if want != len(m.ops) {
		return fmt.Errorf("G6 violated: %d operation rows but %d unique keyed legs", len(m.ops), want)
	}
	return nil
}

// confirmedReversalTargets returns the ids of confirmed — or deleted (N7: a
// reversal of a DELETED operation is an ordinary operation, ADR-0011) —
// operations that have no live (non-INVALID) reversal yet, so a fresh reversal of
// them inserts cleanly under the one-live-reversal-per-op unique index
// (idx_ops_reversal; a DELETED reversal still counts as live, exactly as the
// index's status <> 'INVALID' predicate does). This is the candidate set the
// generator draws reversals (and double-reversals) from; a reversal that was
// itself rejected leaves its original eligible again (reversal-after-reject
// retry).
func (m *Model) confirmedReversalTargets() []*refOp {
	liveReversed := make(map[int64]bool)
	for _, op := range m.ops {
		if op.ReversalOf != nil && op.Status != model.OpInvalid {
			liveReversed[*op.ReversalOf] = true
		}
	}
	var out []*refOp
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || (op.Status != model.OpConfirmed && op.Status != model.OpDeleted) || liveReversed[op.ID] {
			continue
		}
		out = append(out, op)
	}
	return out
}

// editTargets returns every regular operation (never an edit or delete
// registration) in id order — the candidate set the generator draws edits and
// deletes from. Any status is eligible: a CONFIRMED target decides on its limits,
// an INVALID or DELETED one rejects with TARGET_NOT_EDITABLE, and a PENDING one is
// decided first by id order.
func (m *Model) editTargets() []*refOp {
	var out []*refOp
	for id := int64(1); id <= m.nextOpID; id++ {
		op, ok := m.ops[id]
		if !ok || op.EditOf != nil {
			continue
		}
		out = append(out, op)
	}
	return out
}

// CheckEdits verifies the editing invariants (ADR-0010, spec Safety Invariants
// 3–5): an edit-class registration (edit or delete) is never CONFIRMED or
// DELETED; an APPLIED edit is referenced by exactly one revision of its target
// and an APPLIED delete by none (a delete appends nothing); every regular
// operation's history is dense — revisions 1..revision−1, each superseded by an
// APPLIED edit of it — DELETED rows included, whose revision never moves again.
func (m *Model) CheckEdits() error {
	supersededBy := make(map[int64]int)
	for id, op := range m.ops {
		if op.EditOf != nil {
			if op.Status == model.OpConfirmed || op.Status == model.OpDeleted {
				return fmt.Errorf("edit-class row %d is %s; edits and deletes are only PENDING|APPLIED|INVALID", id, op.Status)
			}
			if len(m.revisions[id]) != 0 {
				return fmt.Errorf("edit-class row %d has revisions of its own", id)
			}
			if op.DeletedBy != nil {
				return fmt.Errorf("edit-class row %d carries deleted_by", id)
			}
			continue
		}
		revs := m.revisions[id]
		if int32(len(revs)) != op.Revision-1 {
			return fmt.Errorf("operation %d at revision %d has %d history rows, want %d", id, op.Revision, len(revs), op.Revision-1)
		}
		for i, r := range revs {
			if r.Revision != int32(i+1) {
				return fmt.Errorf("operation %d history not dense: row %d has revision %d", id, i, r.Revision)
			}
			edit, ok := m.ops[r.SupersededBy]
			if !ok || edit.EditOf == nil || edit.isDelete() || *edit.EditOf != id || edit.Status != model.OpApplied {
				return fmt.Errorf("operation %d revision %d superseded by %d, which is not an APPLIED edit of it", id, r.Revision, r.SupersededBy)
			}
			supersededBy[r.SupersededBy]++
		}
	}
	for id, op := range m.ops {
		if op.EditOf == nil {
			continue
		}
		want := 0
		if op.Status == model.OpApplied && !op.isDelete() {
			want = 1
		}
		if supersededBy[id] != want {
			return fmt.Errorf("edit-class row %d (%s) supersedes %d revisions, want %d", id, op.Status, supersededBy[id], want)
		}
	}
	return nil
}

// CheckDeletes verifies the deletion invariants (ADR-0011, spec Safety
// Invariants 2 and 7): a DELETED row is a regular operation stamped with exactly
// the APPLIED delete registration that targets it, and every APPLIED delete
// points at a row that is DELETED by it — so a DELETED row has exactly one
// APPLIED delete and can never read CONFIRMED again (an APPLIED delete is
// terminal, and no other status carries a stamp).
func (m *Model) CheckDeletes() error {
	appliedDeletes := make(map[int64]int) // target id → APPLIED deletes of it
	for id, op := range m.ops {
		if !op.isDelete() || op.Status != model.OpApplied {
			continue
		}
		target := m.ops[*op.EditOf]
		if target.Status != model.OpDeleted {
			return fmt.Errorf("delete %d is APPLIED but its target %d is %s, want DELETED", id, target.ID, target.Status)
		}
		if target.DeletedBy == nil || *target.DeletedBy != id {
			return fmt.Errorf("delete %d is APPLIED but target %d is deleted_by %v", id, target.ID, target.DeletedBy)
		}
		appliedDeletes[target.ID]++
	}
	for id, op := range m.ops {
		if op.EditOf != nil {
			continue
		}
		switch {
		case op.Status == model.OpDeleted:
			if op.DeletedBy == nil {
				return fmt.Errorf("operation %d is DELETED without deleted_by", id)
			}
			del, ok := m.ops[*op.DeletedBy]
			if !ok || !del.isDelete() || *del.EditOf != id || del.Status != model.OpApplied {
				return fmt.Errorf("operation %d deleted_by %d, which is not an APPLIED delete of it", id, *op.DeletedBy)
			}
			if appliedDeletes[id] != 1 {
				return fmt.Errorf("DELETED operation %d has %d APPLIED deletes, want exactly 1", id, appliedDeletes[id])
			}
		default:
			if op.DeletedBy != nil {
				return fmt.Errorf("operation %d is %s but carries deleted_by %d", id, op.Status, *op.DeletedBy)
			}
			if appliedDeletes[id] != 0 {
				return fmt.Errorf("operation %d is %s but has %d APPLIED deletes — a DELETED row came back", id, op.Status, appliedDeletes[id])
			}
		}
	}
	return nil
}

// accountView is a read-only snapshot of an account the generator uses to build
// state-aware limit changes (a new min/max must bracket the confirmed balance,
// spec §6).
type accountView struct {
	ID         int64
	OwnerID    int64
	ExternalID string
	Balance    int64
	Min        *int64
	Max        *int64
}

// accountViews returns a snapshot of every account, ascending by id.
func (m *Model) accountViews() []accountView {
	var out []accountView
	for id := int64(1); id <= m.nextAccountID; id++ {
		a, ok := m.accountsByID[id]
		if !ok {
			continue
		}
		out = append(out, accountView{
			ID: a.ID, ExternalID: a.ExternalID, Balance: a.Balance, Min: a.Min, Max: a.Max,
			OwnerID: m.ownerOf(a.ID),
		})
	}
	return out
}

// ownerOf recovers an account's owner id from the account index.
func (m *Model) ownerOf(accountID int64) int64 {
	for k, id := range m.accounts {
		if id == accountID {
			return k.ownerID
		}
	}
	return 0
}

// canonicalOps mirrors internal/ledger.canonicalOps: a regular item is a
// CanonicalOp (frozen encoding); an edit item is a CanonicalEditOp whose omitted
// fields stay nil — the hash covers the request as sent; a delete item is a
// CanonicalDeleteOp (target and guard only).
func canonicalOps(ops []api.InsertOp) []any {
	out := make([]any, len(ops))
	for i, op := range ops {
		if op.DeleteOf != nil {
			out[i] = model.CanonicalDeleteOp{OwnerID: op.OwnerID, DeleteOf: *op.DeleteOf, ExpectedRevision: op.ExpectedRevision}
			continue
		}
		if op.EditOf == nil {
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

// isUUID mirrors the canonical-UUID check in internal/api without importing its
// unexported regexp.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
