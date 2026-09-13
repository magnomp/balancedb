# ADR-0007 — Embedded Go API and transaction registration order

Status: accepted (2026-09-13, requested Go embedding)

Refined by [ADR-0008](0008-caller-controlled-insertion-transactions.md): the
registration lock and unconditional G3 preservation described below are
superseded. Embedding, migrations, savepoint isolation and leader lifecycle remain.

## Context

Hosts need to register ledger work in their own PostgreSQL transaction and run
BalanceDB inside every application replica. The insertion core, migrations and
fenced leader already exist, but are internal packages tied to the standalone
deployment. An open host transaction also exposes a G3 gap: identity allocation
does not imply commit order, so a processor can otherwise decide a higher ID
while an earlier insertion is still invisible.

## Decision

- Add the public root `balancedb` package with typed deployment configuration,
  explicit `Migrate`, `Open`, caller-owned `pgx.Tx` insertion, and a blocking,
  context-controlled `Run`. Embedding starts no HTTP server, changes no global
  logger, and installs no signal handlers. Existing CLI roles remain supported.
- Move the insertion core into `internal/ledger`; HTTP and embedded calls share
  its request types, sentinels, validation, hashing and SQL. No new dependencies.
- Embedded insertion uses a savepoint, transaction-local `search_path`, and
  restores the host path before returning. Errors roll back the savepoint so no
  partial group or stray account can be committed by a host that handles the
  error. Only the host commits its transaction. Require READ COMMITTED, matching
  the core's conflict-followed-by-SELECT semantics. The host transaction must use
  the same PostgreSQL database as the configured cell; separate schemas work,
  separate databases cannot participate in this atomic commit.
- All core insertions take one transaction advisory lock before allocating IDs
  or writing accounts. Its two-integer key is namespace 1111773778 plus the
  `operations` relation OID, separating cells and the migration lock namespace.
  It lasts until the outer commit/rollback, including across multiple inserts.
  Later inserts cannot allocate or publish IDs ahead of an uncommitted insert.
  Rolled-back sequence gaps are harmless. The processor keeps all three guards
  and its existing ordered work selection unchanged.
- Each embedded instance owns a separate schema-bound background pool and a
  unique lease identity. Every host replica may call Run: PostgreSQL elects one
  active processor per schema; the others wait and take over on release/expiry.
  Close cancels and joins Run before closing the pool. Migrations remain explicit,
  forward-only, embedded, and serialized by the existing per-schema advisory lock.

## Consequences

G1–G6 and asynchronous decisions remain the contract. Host business writes and
PENDING ledger registration commit atomically; later INVALID/REJECTED decisions
cannot undo that host commit. Hosts should insert near the end of a short
transaction, avoid waiting for decisions before commit, and handle transaction
deadlocks/retries as usual. One owner's group remains the only atomic ledger unit.
Typed deployment options supplement the CLI's env configuration; behavioral
configuration stays in the schema's `config` table.

Insert throughput is serialized per cell until outer commit. This is necessary
for G3 with arbitrary host transaction durations; processing can continue over
already committed work. Deployments must upgrade all insertion nodes before
enabling embedding: older binaries do not participate in the registration lock.
Raw writes to ledger tables bypass the contract. No applied migration is edited.

Transaction advisory lock lifetime follows PostgreSQL's documented
[locking semantics](https://www.postgresql.org/docs/18/explicit-locking.html#ADVISORY-LOCKS).
