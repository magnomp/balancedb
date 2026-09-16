# Embedding BalanceDB in Go

Import `github.com/magnomp/balancedb`. The host needs Go 1.25+ and PostgreSQL.
Registration accepts an existing `pgx/v5.Tx`; it does not use HTTP. An adapter for
`database/sql.Tx` is not provided.

## Startup and replicas

```go
cfg := balancedb.Config{
    DatabaseURL: databaseURL,
    Schema:      "ledger", // default: "balancedb"
    MaxConns:    4,        // default: 10; minimum: 2
    Logger:      logger,   // optional *slog.Logger
}

// At application startup, or in a separate deployment migration command:
if _, err := balancedb.Migrate(ctx, cfg); err != nil {
    return err
}

ledger, err := balancedb.Open(ctx, cfg)
if err != nil {
    return err
}
defer ledger.Close()

// appCtx is cancelled by the host during shutdown.
processorDone := make(chan error, 1)
go func() { processorDone <- ledger.Run(appCtx) }()

// ... serve application requests ...

ledger.Close() // cancels and joins Run, releases the lease, closes its own pool
if err := <-processorDone; err != nil {
    return err
}
```

Run this in **every application replica**. Each handle has a unique processor
identity; the schema's PostgreSQL `leader_lease` elects one active processor.
Other replicas wait and automatically take over on graceful release or lease
expiry after a crash. All three transaction guards still fence stale processors.
There is no additional coordination service. `ledger.IsLeader()` is a diagnostic
view, suitable for the host's health surface; standbys are healthy too.

`Open` starts no processor, HTTP/metrics listener, signal handler, or migration.
The host chooses when to call `Run`, owns cancellation and logging, and may open
insertion-only handles without calling `Run`. A standalone `balancedb processor`
can also process the same schema. One handle permits only one concurrent `Run`;
it can be run again after cancellation, until `Close`.

The background pool is owned by BalanceDB and bound to its schema. The host keeps
its own pool and its existing connection/search-path configuration. Budget at
least two background connections per instance: one for LISTEN and one for work.
Behavioral settings such as `lease_ttl_ms`, `loop_interval_ms`, and `batch_size`
remain in the ledger schema's `config` table and are hot-reloaded by the processor.

## Register work in a business transaction

```go
tx, err := hostPool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
if err != nil {
    return err
}
defer tx.Rollback(context.Background())

// Existing host SQL: its schema/search_path is unchanged.
if _, err := tx.Exec(ctx,
    `INSERT INTO payments (id, amount) VALUES ($1, $2)`, paymentID, int64(1250)); err != nil {
    return err
}

result, err := ledger.Insert(ctx, tx, balancedb.InsertRequest{
    IdempotencyKey: requestUUID, // stable canonical UUID across retries
    Operations: []balancedb.InsertOp{
        {OwnerID: ownerID, ExternalID: "cash", Amount: 1250, EffectiveAt: effectiveAt},
    },
})
if err != nil {
    return err
}
// result.Operations contains the registered IDs. These are provisional until:
if err := tx.Commit(ctx); err != nil {
    return err
}
```

The payment and PENDING operation become durable together, or neither does.
BalanceDB wraps its writes in a savepoint and temporarily selects its schema with
a transaction-local `search_path`. It restores the previous path on success and
rolls back the savepoint on error, including partial group writes and account
creation. The outer transaction remains owned by the host. Connection loss or a
failed savepoint cleanup still requires the host to abandon/retry its transaction.

The transaction **must connect to the same PostgreSQL database** as the configured
cell. Host tables can be in `public` or another schema; BalanceDB can be in
`ledger`, with independent installations in other schemas. Atomic registration
across separate databases is not supported. Pass a READ COMMITTED transaction;
other isolation levels return `ErrUnsupportedIsolation`. Do not use the same
transaction concurrently from multiple goroutines.

One operation is a single; two or more operations form one atomic ledger group.
All group operations must have the same `OwnerID`, and `max_group_size` applies.
Amounts are `int64` minor units and must be nonzero. Accounts are created on demand
with unbounded limits. Preconfigured accounts retain their limits. Account management and queries are
available directly in Go as described below; edits are registered through `Insert` too (next section).
HTTP endpoints use the same core.

Repeated keys with identical payloads return the original IDs and current status
with `Replayed=true`. Different payloads return `ErrPayloadConflict`. Use
`errors.Is` with the exported insertion sentinels. Host business writes need their
own idempotency rules: replaying a ledger request does not deduplicate host SQL.

Registration success means **accepted for asynchronous processing**, not that a
balance change was confirmed. A later INVALID operation or REJECTED group does
not undo the host's committed payment row. Applications should represent that
pending decision in their workflow. Never wait for a decision inside the inserting
transaction: the processor cannot see its work until commit.

## Edit an operation in a business transaction

An item with `EditOf` set is an **edit registration** of that operation
(ADR-0010): the leader applies it in place — same id, new `account`/`amount`/
`effective_at`, `revision + 1` — and appends the superseded state to the
append-only `operation_revisions` table. Zero-valued fields mean "unchanged";
at least one of `Amount`, `EffectiveAt` and `ExternalID` must be set, and
`ReversalOf` is not allowed on an edit item. The target must be a regular
operation of the same owner; it is resolved inside the host transaction, so an
uncommitted target from another transaction is simply not found.

```go
// Edit one leg inside the host transaction; zero-valued fields mean "unchanged".
target := int64(41)
res, err := ledger.Insert(ctx, tx, balancedb.InsertRequest{
    IdempotencyKey: requestUUID,
    Operations: []balancedb.InsertOp{
        {OwnerID: 7, EditOf: &target, Amount: -1200},
    },
})
// res.Operations[0].ID == 57, Status == "PENDING", EditOf == &target
if err := tx.Commit(ctx); err != nil { return err }

// Grouped: two edits plus a new operation, one atomic unit.
rev := int32(1)
res, err = ledger.Insert(ctx, tx, balancedb.InsertRequest{
    IdempotencyKey: otherUUID,
    Operations: []balancedb.InsertOp{
        {OwnerID: 7, EditOf: &target, Amount: -1200},
        {OwnerID: 7, EditOf: &other, EffectiveAt: newInstant, ExpectedRevision: &rev},
        {OwnerID: 7, ExternalID: "cash", Amount: 300, EffectiveAt: effectiveAt},
    },
})
```

A single edit item is a single registration, identical to `PATCH /operations/{id}`.
Two or more items — edits, new operations, or both — form one atomic group with
exactly the implications a grouped insert has: one owner, `max_group_size`,
all-or-nothing, and validation on the **net per account** of every change (an
edit counts as −current +proposed). When decided, the group is `COMMITTED` (edit
items `APPLIED`, new items `CONFIRMED`) or `REJECTED` (every item `INVALID` with
one shared rejection: `LIMIT_VIOLATED`, `TARGET_NOT_EDITABLE` when a target
ended INVALID, or `STALE_REVISION` when `ExpectedRevision` no longer matches).
Two items may not edit the same operation. An edit registration is never
CONFIRMED; it never appears in statements or balance sums — only its target does,
at its current values. `OpOutcome.EditOf` names the target of an edit item.

New exported sentinels, all `errors.Is`-able through the savepoint rollback (the
host transaction stays usable after any of them): `ErrEditTargetNotFound`
(unknown or another owner's), `ErrEditTargetNotOperation` (the target is itself
an edit), `ErrDuplicateEditTarget`, `ErrEditChangesNothing`,
`ErrInvalidExpectedRevision` (must be ≥ 1), `ErrEditsDisabled` (the cell's
`config.allow_edits` is false — already registered edits are still decided) and
`ErrEditWithReversal`. Replays follow the usual idempotency rules: the same key
with the same edit returns the original id and its current status with
`Replayed=true`.

Registration success means the edit is **accepted for asynchronous decision**;
the target keeps its current values until the leader applies it. Read the
outcome later (`GET /operations/{edit id}`, the group, or the target's
`/history`). Deciding an edit reads the target's current state, so an edit
registered while its target is still PENDING is decided after the target, in id
order.

## Accounts, balances, statements and outcomes

Account writes use the existing host transaction, just like insertion:

```go
min, max := int64(0), int64(100_000)
account, err := ledger.CreateAccount(ctx, tx, ownerID, "cash", balancedb.Limits{
    MinBalance: &min,
    MaxBalance: &max,
})
// Handle err, then commit tx when the host's business work is complete.

// Both bounds are replaced; nil means unbounded.
account, err = ledger.UpdateLimits(ctx, tx, ownerID, "cash", balancedb.Limits{
    MinBalance: &min,
})
```

New accounts start at zero, so their limits must include zero. Duplicates return
`ErrAccountExists`; invalid bounds return `ErrInvalidLimits`. Updates validate the
current final balance and use a version CAS with one retry; repeated contention
returns `ErrConcurrentUpdate`. On error only this call's savepoint is rolled back.
The host still commits or rolls back its own transaction normally.

Reads use BalanceDB's schema-bound pool and see **committed state**. Commit the
host transaction before expecting its changes to appear through these methods:

```go
account, err := ledger.GetAccount(ctx, ownerID, "cash")
final, err := ledger.GetBalance(ctx, ownerID, "cash", nil)
historical, err := ledger.GetBalance(ctx, ownerID, "cash", &at)
page, err := ledger.GetStatement(ctx, ownerID, "cash", balancedb.StatementOptions{
    Limit: 50, // zero defaults to 50; range 1..500
    Cursor: previousNextCursor, // empty for the first page
})
operation, err := ledger.GetOperation(ctx, ownerID, operationID)
group, err := ledger.GetTransaction(ctx, ownerID, transactionID)
```

All methods scope access to the explicit owner ID. Unknown and foreign-owner
records both return `ErrNotFound`. Amounts, bounds, balances and shortfalls remain
`int64` minor units. `OperationOutcome.Status` uses `OpPending`, `OpConfirmed`, or
`OpInvalid`; groups use `TxPending`, `TxCommitted`, or `TxRejected`. Terminal
rejections include typed `Rejection` details. A PENDING result is normal; hosts
can query again later, after committing insertion, using their own request context.

Each read uses one consistent database snapshot. A statement page contains only
CONFIRMED operations ordered by `(effective_at, id)`, with running balances and an
opaque `NextCursor`. Pages are independent snapshots: confirmations or backdated
work between pages can alter later projections. Point-in-time and running balances
may violate account limits (N2); `ErrBalanceOverflow` or a database scan error is
returned if a projected result cannot fit in int64. Invalid limits/cursors/IDs are
reported via errors.Is sentinels, and calls after Close return `ErrClosed`.

## Registration order and long transactions

**Insert ledger operations at the very end of a short transaction, immediately
before commit. Avoid holding the transaction open after `Insert` returns.** The
caller uses its existing transaction and commits it normally. BalanceDB does not
wrap it, attach a before-commit hook, queue operations, or serialize insertion
transactions. Its savepoint only isolates ledger writes and schema selection.

PostgreSQL identity IDs are allocated before commit. The processor queries
committed PENDING operations in ID order, but cannot see uncommitted operations.
A lower ID committed after a work query can be decided after higher IDs already
fetched. This is not a promise of global ID order or commit-time order across
concurrent insertion transactions (G3 refined by ADR-0008).

For example, start with a balance and minimum of 0. A +100 credit at ID 41 remains
uncommitted while a -80 debit at ID 42 commits. If the processor sees only 42, it
rejects the debit. The later credit confirms, leaving 100; if both were visible
before the work query, ID order would accept both and leave 20. A terminal
rejection is never reconsidered, including when the debit belongs to a group.
This timing risk also exists with concurrent HTTP insertion requests.

Committing promptly reduces the window but **does not guarantee ordering**; there
is no safe time threshold. Final-balance limits, group atomicity and immutable
`(effective_at, id)` timeline order remain enforced. Decision order is distinct
from timeline order. Clients requiring stronger ordering must coordinate their
own insertion transactions; transaction duration guidance alone is insufficient.

Ordinary PostgreSQL constraint and row locks can still cause waits or deadlocks,
for example for account creation or duplicate idempotency keys. Handle those
through the host's normal transaction retry policy. Processor leadership remains
coordinated independently by the per-schema lease.

## Migrations and upgrades

`Migrate` creates the configured schema and applies the SQL bundled in the Go
module. The per-schema `schema_migrations` table records applied versions. Calls
are advisory-lock serialized, including schema creation, so all replicas can call
it safely during startup. A repeat call is a no-op. Separate schemas are migrated
independently. Migrations use a dedicated connection and are not part of a host
business transaction.

For a deployment gate, run `Migrate` once using a role with DDL privileges before
starting the application. The existing CLI `balancedb migrate` is equally usable
with `BALANCEDB_DATABASE_URL` and `BALANCEDB_SCHEMA` pointing at the same cell.
`Open` does not implicitly migrate. Migrations are forward-only; rollback requires
a new migration.

Direct SQL writes to ledger tables bypass insertion validation and idempotency.
Use the public insertion API for host transaction integration.

See [ADR-0007](decisions/0007-embedded-go-api.md) for embedding and
[ADR-0008](decisions/0008-caller-controlled-insertion-transactions.md) for the
caller-controlled transaction and ordering contract.
