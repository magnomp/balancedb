package simtest

import (
	"bytes"
	"fmt"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/model"
)

// DefaultMaxGroupSize mirrors the config-table default (spec §5.2). The reference
// model holds it as a field so a schedule can vary it just like the real config.
const DefaultMaxGroupSize = 10

// refOp is one recorded operation in the reference model.
type refOp struct {
	ID            int64
	AccountID     int64
	Amount        int64
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
// object of G1. NULL limits are unbounded.
type refAccount struct {
	ID      int64
	Min     *int64
	Max     *int64
	Balance int64
}

// Model is the sequential reference model. It replays insertion in registration
// order, assigning ids from monotonic counters exactly as Postgres identity
// columns do, and enforces the same insert-time rules as internal/api.Insert. It
// is the source of truth later milestones assert the database against.
type Model struct {
	MaxGroupSize int

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
	rec := &refOp{ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, Status: model.OpPending}
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
		rec := &refOp{ID: m.nextOpID, AccountID: accountID, Amount: op.Amount, TransactionID: &txID, Status: model.OpPending}
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
	m.accountsByID[id] = &refAccount{ID: id}
	return id
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
