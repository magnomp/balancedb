package simtest

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/magnomp/balancedb/internal/api"
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
type refOp struct {
	ID            int64
	AccountID     int64
	Amount        int64
	EffectiveAt   time.Time
	ReversalOf    *int64
	TransactionID *int64
	Status        model.OpStatus
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
		uncommitted:  make(map[int64]bool),
		accounts:     make(map[accountKey]int64),
		accountsByID: make(map[int64]*refAccount),
		ops:          make(map[int64]*refOp),
		txs:          make(map[int64]*refTx),
		singleByKey:  make(map[string]keyedRecord),
		groupByKey:   make(map[string]keyedRecord),
	}
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
// api.ErrPayloadConflict, api.ErrZeroAmount).
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
	if len(req.Operations) > m.MaxGroupSize {
		return nil, fmt.Errorf("%w: %d operations, max %d", api.ErrGroupTooLarge, len(req.Operations), m.MaxGroupSize)
	}
	for _, op := range req.Operations {
		if op.Amount == 0 {
			// Mirrors the operations.amount CHECK, which the DB enforces atomically
			// for the whole insert — so nothing is recorded.
			return nil, fmt.Errorf("%w", api.ErrZeroAmount)
		}
	}

	hash, err := model.HashPayload(canonicalOps(req.Operations))
	if err != nil {
		return nil, err
	}
	if len(req.Operations) == 1 {
		return m.insertSingle(req, hash)
	}
	return m.insertGroup(req, hash)
}

func (m *Model) insertSingle(req api.InsertRequest, hash []byte) (*api.InsertResult, error) {
	if rec, ok := m.singleByKey[req.IdempotencyKey]; ok {
		if !bytes.Equal(rec.hash, hash) {
			return nil, api.ErrPayloadConflict
		}
		op := m.ops[rec.id]
		return &api.InsertResult{
			Operations: []api.OpOutcome{{ID: op.ID, Status: string(op.Status)}},
			Replayed:   true,
		}, nil
	}

	op := req.Operations[0]
	accountID := m.upsertAccount(op.OwnerID, op.ExternalID)
	m.nextOpID++
	rec := &refOp{ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt, ReversalOf: op.ReversalOf, Status: model.OpPending}
	m.ops[rec.ID] = rec
	m.singleByKey[req.IdempotencyKey] = keyedRecord{id: rec.ID, hash: hash}
	return &api.InsertResult{Operations: []api.OpOutcome{{ID: rec.ID, Status: string(model.OpPending)}}}, nil
}

func (m *Model) insertGroup(req api.InsertRequest, hash []byte) (*api.InsertResult, error) {
	if rec, ok := m.groupByKey[req.IdempotencyKey]; ok {
		if !bytes.Equal(rec.hash, hash) {
			return nil, api.ErrPayloadConflict
		}
		tx := m.txs[rec.id]
		outcomes := make([]api.OpOutcome, len(tx.LegIDs))
		for i, id := range tx.LegIDs {
			outcomes[i] = api.OpOutcome{ID: id, Status: string(m.ops[id].Status)}
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
	tx := &refTx{ID: m.nextTxID, OpCount: len(req.Operations), PayloadHash: hash, Status: model.TxPending}
	outcomes := make([]api.OpOutcome, 0, len(req.Operations))
	for _, op := range req.Operations {
		accountID := m.upsertAccount(op.OwnerID, op.ExternalID)
		m.nextOpID++
		txID := tx.ID
		rec := &refOp{ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, EffectiveAt: op.EffectiveAt, ReversalOf: op.ReversalOf, TransactionID: &txID, Status: model.OpPending}
		m.ops[rec.ID] = rec
		tx.LegIDs = append(tx.LegIDs, rec.ID)
		outcomes = append(outcomes, api.OpOutcome{ID: rec.ID, Status: string(model.OpPending)})
	}
	m.txs[tx.ID] = tx
	m.groupByKey[req.IdempotencyKey] = keyedRecord{id: tx.ID, hash: hash}

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

// confirmedReversalTargets returns the ids of confirmed operations that have no
// live (non-INVALID) reversal yet, so a fresh reversal of them inserts cleanly
// under the one-live-reversal-per-op unique index (idx_ops_reversal). This is the
// candidate set the generator draws reversals (and double-reversals) from; a
// reversal that was itself rejected leaves its original eligible again
// (reversal-after-reject retry).
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
		if !ok || op.Status != model.OpConfirmed || liveReversed[op.ID] {
			continue
		}
		out = append(out, op)
	}
	return out
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

func canonicalOps(ops []api.InsertOp) []model.CanonicalOp {
	out := make([]model.CanonicalOp, len(ops))
	for i, op := range ops {
		out[i] = model.CanonicalOp{
			OwnerID:     op.OwnerID,
			Account:     op.ExternalID,
			Amount:      op.Amount,
			EffectiveAt: op.EffectiveAt,
			ReversalOf:  op.ReversalOf,
		}
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
