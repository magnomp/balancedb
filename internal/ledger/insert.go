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

// uuidRE matches a canonical 8-4-4-4-12 hex UUID. Validated in Go so a malformed
// key returns a clean error instead of a Postgres 22P02 cast failure.
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// InsertOp is one operation in an insertion request. Amount is int64 minor units
// (already parsed via model.ParseAmount at the transport boundary); accounts are
// referenced by (OwnerID, ExternalID) and upserted on demand. ReversalOf is
// immutable metadata, unused by the engine (spec §5.4).
type InsertOp struct {
	OwnerID     int64
	ExternalID  string
	Amount      int64
	EffectiveAt time.Time
	ReversalOf  *int64
}

// InsertRequest is one atomic unit: one operation is a single, two or more form a
// group. The idempotency key covers the whole request (spec §10.1).
type InsertRequest struct {
	IdempotencyKey string
	Operations     []InsertOp
}

// OpOutcome is a per-operation result: its registration id and current status.
// On a fresh insert the status is PENDING; on a replay it is the operation's
// current status.
type OpOutcome struct {
	ID     int64
	Status string
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
	selectMaxGroupSize = `SELECT max_group_size FROM config`

	// Upsert-on-demand: create the account only if absent. DO NOTHING (not DO
	// UPDATE) so an existing account's row is never rewritten — that would create
	// needless MVCC churn and row-lock contention with the processor's version CAS
	// (spec §7.2). The empty-return path falls back to a plain SELECT.
	upsertAccount   = `INSERT INTO accounts (owner_id, external_id) VALUES ($1, $2) ON CONFLICT (owner_id, external_id) DO NOTHING RETURNING id`
	selectAccountID = `SELECT id FROM accounts WHERE owner_id = $1 AND external_id = $2`

	// Single: key + hash live on the operation. Probe-and-insert via ON CONFLICT
	// DO NOTHING so a concurrent-retry conflict returns no row instead of raising
	// 23505 (which would poison the surrounding transaction); ADR-0005.
	insertSingle = `INSERT INTO operations (account_id, amount, effective_at, reversal_of, idempotency_key, payload_hash)
VALUES ($1, $2, $3, $4, $5::uuid, $6)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING id`
	selectSingleByKey = `SELECT id, status, payload_hash FROM operations WHERE idempotency_key = $1::uuid`

	// Group: transaction row carries key + hash; legs carry transaction_id.
	insertTransaction = `INSERT INTO transactions (idempotency_key, payload_hash, op_count)
VALUES ($1::uuid, $2, $3) ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`
	selectTxByKey = `SELECT id, status, payload_hash, op_count FROM transactions WHERE idempotency_key = $1::uuid`
	insertLeg     = `INSERT INTO operations (account_id, amount, effective_at, reversal_of, transaction_id)
VALUES ($1, $2, $3, $4, $5) RETURNING id`
	selectLegs     = `SELECT id, status FROM operations WHERE transaction_id = $1 ORDER BY id`
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

	var maxGroup int
	if err := tx.QueryRow(ctx, selectMaxGroupSize).Scan(&maxGroup); err != nil {
		return nil, fmt.Errorf("read max_group_size: %w", err)
	}
	if len(req.Operations) > maxGroup {
		return nil, fmt.Errorf("%w: %d operations, max %d", ErrGroupTooLarge, len(req.Operations), maxGroup)
	}

	hash, err := model.HashPayload(canonicalOps(req.Operations))
	if err != nil {
		return nil, err
	}

	if len(req.Operations) == 1 {
		return insertSingleOp(ctx, tx, req, hash)
	}
	return insertGroup(ctx, tx, req, hash)
}

func canonicalOps(ops []InsertOp) []model.CanonicalOp {
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

func insertSingleOp(ctx context.Context, tx pgx.Tx, req InsertRequest, hash []byte) (*InsertResult, error) {
	op := req.Operations[0]
	accountID, err := upsertAccountID(ctx, tx, op.OwnerID, op.ExternalID)
	if err != nil {
		return nil, err
	}

	var id int64
	err = tx.QueryRow(ctx, insertSingle, accountID, op.Amount, op.EffectiveAt, op.ReversalOf, req.IdempotencyKey, hash).Scan(&id)
	switch {
	case err == nil:
		// Fresh insert: new PENDING work exists, so ring the doorbell.
		if err := ringDoorbell(ctx, tx); err != nil {
			return nil, err
		}
		return &InsertResult{Operations: []OpOutcome{{ID: id, Status: string(model.OpPending)}}}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Key already present: replay the original (spec §10.1).
		return replaySingle(ctx, tx, req, hash)
	default:
		return nil, mapInsertError(err)
	}
}

func replaySingle(ctx context.Context, tx pgx.Tx, req InsertRequest, hash []byte) (*InsertResult, error) {
	var (
		id           int64
		status       string
		existingHash []byte
	)
	if err := tx.QueryRow(ctx, selectSingleByKey, req.IdempotencyKey).Scan(&id, &status, &existingHash); err != nil {
		return nil, fmt.Errorf("fetch existing operation by key: %w", err)
	}
	if !bytes.Equal(existingHash, hash) {
		return nil, ErrPayloadConflict
	}
	return &InsertResult{Operations: []OpOutcome{{ID: id, Status: status}}, Replayed: true}, nil
}

func insertGroup(ctx context.Context, tx pgx.Tx, req InsertRequest, hash []byte) (*InsertResult, error) {
	var txID int64
	err := tx.QueryRow(ctx, insertTransaction, req.IdempotencyKey, hash, len(req.Operations)).Scan(&txID)
	switch {
	case err == nil:
		// Fresh group.
	case errors.Is(err, pgx.ErrNoRows):
		return replayGroup(ctx, tx, req, hash)
	default:
		return nil, fmt.Errorf("insert transaction: %w", err)
	}

	outcomes := make([]OpOutcome, 0, len(req.Operations))
	for _, op := range req.Operations {
		accountID, err := upsertAccountID(ctx, tx, op.OwnerID, op.ExternalID)
		if err != nil {
			return nil, err
		}
		var id int64
		if err := tx.QueryRow(ctx, insertLeg, accountID, op.Amount, op.EffectiveAt, op.ReversalOf, txID).Scan(&id); err != nil {
			return nil, mapInsertError(err)
		}
		outcomes = append(outcomes, OpOutcome{ID: id, Status: string(model.OpPending)})
	}

	// op_count cross-check: the transaction row's op_count must equal the number
	// of legs actually written (spec §8.3 cross-check, enforced on the insert side
	// so a partial group can never be committed).
	var legCount int
	if err := tx.QueryRow(ctx, countLegs, txID).Scan(&legCount); err != nil {
		return nil, fmt.Errorf("op_count cross-check: %w", err)
	}
	if legCount != len(req.Operations) {
		return nil, fmt.Errorf("op_count cross-check: wrote %d legs, expected %d", legCount, len(req.Operations))
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

func replayGroup(ctx context.Context, tx pgx.Tx, req InsertRequest, hash []byte) (*InsertResult, error) {
	var (
		txID         int64
		status       string
		existingHash []byte
		opCount      int
	)
	if err := tx.QueryRow(ctx, selectTxByKey, req.IdempotencyKey).Scan(&txID, &status, &existingHash, &opCount); err != nil {
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
		var o OpOutcome
		if err := rows.Scan(&o.ID, &o.Status); err != nil {
			return nil, fmt.Errorf("scan group leg: %w", err)
		}
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
// amount <> 0 constraint (spec §5.2) — into a sentinel; other errors pass through.
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return fmt.Errorf("%w: %s", ErrZeroAmount, pgErr.ConstraintName)
	}
	return fmt.Errorf("insert operation: %w", err)
}
