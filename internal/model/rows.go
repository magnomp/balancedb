package model

import "time"

// The row types mirror the per-cell schema (spec §5.2) one-to-one. Nullable
// columns are pointers; amounts are int64 minor units; payload hashes are raw
// bytes; statuses use the typed vocabularies above. Later milestones scan into
// these; M3 uses only a subset, but the model package owns the whole shape.

// Account is a row of the accounts table. confirmed_balance is the FINAL balance
// (sum of all CONFIRMED operations) and the object of G1; version is the
// optimistic lock guarding processor-vs-API races. NULL limits are unbounded.
type Account struct {
	ID               int64
	OwnerID          int64
	ExternalID       string
	MinBalance       *int64
	MaxBalance       *int64
	ConfirmedBalance int64
	Version          int64
	CreatedAt        time.Time
}

// Transaction is a row of the transactions table — a group record, present only
// for groups (>= 2 operations). Singles carry their idempotency key and payload
// hash on the operation instead.
type Transaction struct {
	ID             int64
	IdempotencyKey string
	PayloadHash    []byte
	OpCount        int
	Status         TxStatus
	RejectReason   *string
	RegisteredAt   time.Time
	DecidedAt      *time.Time
}

// Operation is a row of the operations table. The id is the registration id: the
// global insertion order and the tiebreaker in the timeline key (effective_at,
// id). transaction_id is NULL for singles; idempotency_key/payload_hash are set
// on singles only.
type Operation struct {
	ID                 int64
	AccountID          int64
	Amount             int64
	EffectiveAt        time.Time
	TransactionID      *int64
	ReversalOf         *int64
	Status             OpStatus
	InvalidationReason *string
	IdempotencyKey     *string
	PayloadHash        []byte
	RegisteredAt       time.Time
	ConfirmedAt        *time.Time
}

// BalanceSnapshot is a row of the balance_snapshots table: the cumulative balance
// at the end of a UTC day for an account (spec §8.4). Day is a DATE.
type BalanceSnapshot struct {
	AccountID int64
	Day       time.Time
	Balance   int64
}

// Config is the single-row config table (spec §5.2): the behavioral knobs,
// hot-reloaded by the processor every cycle.
type Config struct {
	LeaseTTLMs     int
	LoopIntervalMs int
	BatchSize      int
	MaxGroupSize   int
	APIMaxWaitMs   int
}
