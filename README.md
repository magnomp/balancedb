# BalanceDB

BalanceDB keeps running balances correct over time. A balance can be anything
that goes up and down as events arrive: money in an account, units of a product in
stock, points, credits, hours. You give it a stream of operations, including
backdated and future-dated ones, and it decides which ones are accepted, keeps
every account inside its configured limits, and answers balance and statement
queries. It runs on plain PostgreSQL and ships as one Go binary, or as a Go library
you embed in your own application.

Typical uses: ledgers, banking cores, budgeting and envelope systems, product
inventory, loyalty points. Anything where "the balance must never go below X" (or
above Y) is a hard rule.

## The problem it solves

Keeping a balance correct sounds trivial until several things are true at once:

- Operations arrive concurrently from many clients and must not race each other
  past a limit: an overdraft, or selling stock you no longer have.
- Some operations are **atomic groups** (a transfer is two legs, a stock move is a
  pick and a put-away) that must apply together or not at all.
- Operations can be **backdated or future-dated**, so "the balance at time T" and
  "the final balance" are different questions with different answers.
- Confirmed operations sometimes need to be **corrected or removed**, but the audit
  trail must stay complete.
- Clients retry, so the same request must never be applied twice.
- The system must survive a crashed or stalled node without ever losing or
  double-applying a write.

BalanceDB handles all of that inside one database, with no distributed protocol.

## Guarantees

These are the contract clients can rely on (spec §4):

- **G1 Final-balance invariant.** An account's final balance never leaves its
  configured minimum and maximum.
- **G2 Group atomicity.** All legs of a group confirm together or reject together.
  Partial application is impossible, even transiently.
- **G3 Ordered work.** The processor decides committed pending work in registration
  order. Sequential commits are decided in ID order; overlapping insertion
  transactions can reorder decisions (ADR-0008).
- **G4 Deterministic timeline.** Order on the timeline is `(effective_at, id)`,
  total and unique, fixed at insert.
- **G5 Eventual decision.** Every operation ends CONFIRMED or INVALID. Nothing is
  aborted for taking too long.
- **G6 Idempotent insertion.** Re-sending a request with the same idempotency key
  never duplicates work.

Just as important is what it does **not** promise: there is no decision deadline,
only the final balance is protected (a backdated debit may make history dip below
the minimum at some past instant), and reversals, edits and deletes do not track
each other. The full list is in `docs/architecture-spec.md` §4.2.

## How it works

The design has one idea at its core: **the database is the serialization point,
and one elected process does all the writes.**

1. **Insert.** Clients insert operations as `PENDING` rows in a short database
   transaction. A single request is either one operation or one atomic group, and
   every group belongs to exactly one owner. An idempotency key makes retries safe.
2. **Elect.** Every processor instance tries to hold a lease row in the database.
   Whoever holds it is the leader; the others idle as hot standbys and take over
   within the lease TTL if the leader dies. All lease timing uses the database
   clock, never node clocks.
3. **Decide.** The leader selects pending work in ID order and, for each unit,
   checks `min <= balance + net <= max` on every involved account. Groups are netted
   per account. Pass means confirm and apply, fail means reject with a
   machine-readable reason. Batches of decisions share one commit.
4. **Guard.** Every write transaction carries three guards so that a stalled
   ex-leader writing late is harmless: a lease fence, a conditional status flip,
   and a version check on each account. If any guard misses, the transaction rolls
   back and the work is simply redone.
5. **Project.** Point-in-time balances and statements are derived on read from
   sparse daily snapshots, so a backdated operation costs only the days it crosses.
6. **Notify.** Outcomes are announced with `LISTEN/NOTIFY`, so an API client that
   asked to wait gets its answer within milliseconds of the deciding commit.

Edits and deletes of confirmed operations follow the same path. They are
registrations decided by the leader, validated like any other operation, and
recorded append-only: an edit keeps the superseded state in a history table, a
delete flips the row to a terminal `DELETED` status and keeps it for audit.

Scaling is horizontal by **cells**. A cell is one Postgres plus its processors and
serves a disjoint set of owners. Cells share nothing, so capacity grows linearly by
adding cells.

## Install

Requirements: PostgreSQL (18 is the tested default; the SQL is plain, so 16 and 17
work too), and either Go 1.25+ or Docker.

```sh
# From source
go install github.com/magnomp/balancedb/cmd/balancedb@latest

# Or as a container image (static, distroless, non-root)
git clone https://github.com/magnomp/balancedb && cd balancedb
make image                      # builds balancedb:local
```

The binary embeds its migrations. It needs nothing but a database URL.

## Use

### As a service

One binary, three roles:

```sh
export BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable
balancedb migrate      # apply migrations and exit (optional deploy gate)
balancedb processor &  # the decision loop; run two or more for failover
balancedb api          # HTTP API on :8080, interactive docs at /docs
```

`docker compose up --build` starts a reference deployment: Postgres, a migrate
gate, one API node and two processors. Then talk to it over HTTP:

```sh
# Move 100 units from alice to bob and wait up to 10 s for the decision.
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(uuidgen)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"account":"alice","amount":-100,"effective_at":"2026-01-01T00:00:00Z"},
      {"account":"bob",  "amount": 100,"effective_at":"2026-01-01T00:00:00Z"}
    ], "wait_ms": 10000 }'

curl -s localhost:8080/accounts/alice/balance -H 'X-Owner-Id: 1'
curl -s 'localhost:8080/accounts/alice/balance?at=2025-12-31T00:00:00Z' -H 'X-Owner-Id: 1'
```

Amounts are 64-bit integers in whatever unit you choose (cents, pieces, points).
Accounts are created on first use, or explicitly with limits via `POST /accounts`.
Corrections use `PATCH` and `DELETE /operations/{id}`. The full API is served at
`/docs` and checked into `api/openapi.yaml`.

### As a Go library

Register operations inside your own `pgx` transaction and run the processor in
every application replica. Postgres elects one leader among them.

```go
cfg := balancedb.Config{DatabaseURL: databaseURL}
_, err := balancedb.Migrate(ctx, cfg) // idempotent; or run `balancedb migrate` once
ledger, err := balancedb.Open(ctx, cfg)
go ledger.Run(appCtx) // processor; one replica becomes leader

tx, _ := pool.Begin(ctx)
_, err = ledger.Insert(ctx, tx, balancedb.InsertRequest{
    IdempotencyKey: requestUUID,
    Operations: []balancedb.InsertOp{
        {OwnerID: ownerID, ExternalID: "cash", Amount: 1250, EffectiveAt: now},
    },
})
tx.Commit(ctx)
```

See `docs/embedding.md` for the complete guide.

## Going deeper

- `docs/operations.md` – deployment, configuration, roles, upgrades, metrics.
- `docs/operations-editing.md` – editing and deleting confirmed operations.
- `docs/embedding.md` – using BalanceDB as a library.
- `docs/testing.md` – Makefile targets and the simulation harness.
- `docs/benchmarking.md` – measuring how many operations per second a cell holds.
- `docs/architecture-spec.md` – what the system does, in full.
- `docs/plan.md` and `docs/decisions/` – how it is built and every deviation.
- `CLAUDE.md` – orientation and inviolable rules for contributors.
