# BalanceDB

A Postgres-backed balance-maintenance engine (a ledger). Clients insert money
operations — singles or atomic groups — and a single elected leader validates and
confirms visible committed work in ID order while enforcing final-balance limits.
Concurrent insertion commits can change decision order and acceptance; insert
ledger operations near the end of a short transaction ([ordering contract](docs/embedding.md#registration-order-and-long-transactions)).

Go applications can also [embed BalanceDB](docs/embedding.md): register operations
inside an existing `pgx.Tx`, migrate a separate ledger schema, and run the processor
in every application replica with PostgreSQL electing one active leader.

One Go binary runs as three roles:

```
balancedb api        # HTTP API (spec §10): insertion, synchronous waiting, queries
balancedb processor  # the single-leader decision loop (spec §7–§8)
balancedb migrate    # run migrations and exit (a CI/CD deploy gate)
```

The authorities for behavior and structure are `docs/architecture-spec.md` (what
the system does) and `docs/plan.md` (how it is built); deviations are recorded as
ADRs in `docs/decisions/`. Start with `CLAUDE.md` for orientation and the
inviolable rules.

## Running locally

```sh
export BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable
go run ./cmd/balancedb processor &   # elects itself leader, drains work
go run ./cmd/balancedb api           # serves the API on :8080, docs at /docs
```

Both roles run migrations on boot (idempotent, advisory-lock guarded) unless
`BALANCEDB_MIGRATE_ON_START=false`. Deployment configuration is `BALANCEDB_*`
environment (see plan §0); behavioral knobs (`lease_ttl_ms`, `batch_size`, …) live
in the DB `config` table and are hot-reloaded each cycle.

## Deployment (Docker + compose)

The repository builds a **single, self-contained image** (multi-stage → static
binary on `gcr.io/distroless/static-debian12:nonroot` — no shell, non-root, and the
schema migrations are embedded via `go:embed`, so the image needs nothing but a
Postgres to point at):

```sh
make image                 # docker build -t balancedb:local .
```

The root `docker-compose.yml` is a **prod-like reference deployment**: one
`postgres:18`, a one-shot `migrate` gate, one `api`, and two `processor`s (one
active leader, one hot standby). Every BalanceDB service runs the *same* image and
differs only by its command (the role) and `BALANCEDB_*` environment.

```sh
docker compose up --build          # or: make smoke  (scripted end-to-end test)
```

| Service | Role | Host ports | Notes |
|---|---|---|---|
| `postgres` | — | `55432→5432` | Bundled Postgres 18. Published on 55432 to avoid colliding with a 5432 already in use; services reach it as `postgres:5432`. |
| `migrate` | `migrate` | — | Deploy gate: applies the embedded migrations and exits 0. The long-running roles start only after it succeeds. |
| `api` | `api` | `8080` (HTTP+`/docs`), `9090` (health/metrics) | `BALANCEDB_MIGRATE_ON_START=false` — the gate already migrated. |
| `processor` | `processor` | `9091→9090` | Leader-or-standby; the health body reports `leader`. |
| `processor-standby` | `processor` | `9092→9090` | Identical replica; takes over within `lease_ttl_ms` if the leader dies. |

Each role's health surface (`/healthz`, `/readyz`, `/metrics`) binds `:9090`
*inside* its container (isolated network namespaces, so no clash); the compose file
publishes them on distinct host ports. The ledger endpoints (spec §10) are on the
`api` container's `:8080`.

Exercising it once up (owner scope is the `X-Owner-Id` header, §2):

```sh
# Insert a 2-leg atomic group and wait synchronously for the decision (200 = decided):
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"account":"alice","amount":-100,"effective_at":"2026-01-01T00:00:00Z"},
      {"account":"bob",  "amount": 100,"effective_at":"2026-01-01T00:00:00Z"}
    ], "wait_ms": 10000 }'

curl -s localhost:8080/accounts/alice/balance -H 'X-Owner-Id: 1'   # {"balance":-100,...}
```

`make smoke` scripts exactly this plus a failover check: it kills the active
processor container and asserts the standby acquires leadership within the lease
TTL and resumes draining work. It is self-contained (builds, runs, tears down); set
`KEEP_UP=1` to leave the stack up for inspection. **M13 (CI) reuses this target.**

### Editing operations (ADR-0010)

A confirmed operation can be corrected **in place** — same id, new `amount`,
`effective_at` and/or `account` (same owner) — with an append-only history. An edit
is a *registration* like an insert: it enters the same PENDING queue, is decided by
the leader (validated against the final-balance limits as `−old` + `+new`, netted per
account), and is idempotent under the same `Idempotency-Key` rules. `revision`
starts at 1 and increments with every applied edit; registration id, owner,
`transaction_id`, `reversal_of` and `registered_at` are never editable
(`docs/decisions/0010-operation-editing.md`, spec §10.3).

```sh
# One edit: fix -15.00 to -12.00 and wait for the decision (200 = decided).
curl -s -X PATCH localhost:8080/operations/41 \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' \
  -d '{"amount": -1200, "expected_revision": 1, "wait_ms": 10000}'
# {"edit":{"id":57,"status":"APPLIED","operation_id":41},"replayed":false}

# Grouped / mixed: edits are POST /transactions items with edit_of — one atomic
# unit, one owner, max_group_size items, all applied or all rejected.
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"edit_of": 41, "amount": -1200},
      {"edit_of": 42, "effective_at": "2026-02-10T09:30:00Z"},
      {"account":"cash","amount":300,"effective_at":"2026-03-01T10:00:00Z"}
    ], "wait_ms": 10000 }'

# History: every superseded revision (oldest first, last = current) plus the
# pending and rejected edits — nothing is ever updated or deleted.
curl -s localhost:8080/operations/41/history -H 'X-Owner-Id: 1'
```

- **Statuses.** An edit registration goes `PENDING → APPLIED | INVALID` (terminal);
  the target stays `CONFIRMED` and only its current columns, `revision` and
  `revised_at` change. Rejections are `LIMIT_VIOLATED` (as for inserts),
  `TARGET_NOT_EDITABLE` (the target ended INVALID or is DELETED) or `STALE_REVISION`
  (`expected_revision` did not match at decision time). A group with any failing
  item is `REJECTED` as a whole; `COMMITTED` means every new item CONFIRMED and every
  edit item APPLIED.
- **Reads.** `GET /operations/{id}` returns the current state and `revision` (an edit
  registration id returns `edit_of` and the proposed state); statement entries carry
  `revision`, `GET /transactions/{id}` legs carry `revision` (regular legs) or
  `edit_of` (edit legs); `GET /operations/{id}/history` is the audit trail (`404`
  for an edit registration id). Edit registrations never
  appear in statements, balances or point-in-time sums.
- **Turning it off.** `UPDATE config SET allow_edits = false;` — hot-reloaded, so the
  next edit registration is refused with `403` (embedded: `ErrEditsDisabled`);
  edits already registered are still decided.
- **Non-guarantee N7.** Edits and reversals do not track each other: editing an
  operation never adjusts a reversal that points at it, and editing a reversal never
  adjusts its original — the client owns the follow-up. Reverse to cancel, edit to
  correct, delete to remove (next section).
- **Embedded hosts** register edits through the same `InsertOp.EditOf` /
  `ExpectedRevision` fields inside their own transaction
  ([embedding guide](docs/embedding.md#edit-an-operation-in-a-business-transaction)).

### Deleting operations (ADR-0011)

A confirmed operation can be **deleted**: same id, terminal status `DELETED`, last
values kept on the row with `deleted_at` and `deleted_by` stamped — nothing is ever
physically removed, so the audit trail stays complete. A delete is a *registration*
exactly like an edit: it enters the PENDING queue, is decided by the leader as one
virtual leg (`−current` on the target's account, validated against the final-balance
limits — removing a spent credit is a debit without funds), and is idempotent under
the same `Idempotency-Key` rules. `revision` is not bumped and no history row is
appended; the row's last values *are* its final state
(`docs/decisions/0011-operation-deletion.md`, spec §10.4).

```sh
# One delete, guarded by the revision the client last read, waiting for the decision.
curl -s -X DELETE 'localhost:8080/operations/41?expected_revision=2&wait_ms=10000' \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)"
# {"deletion":{"id":63,"status":"APPLIED","operation_id":41},"replayed":false}

# Grouped / mixed: deletes are POST /transactions items with delete_of (nothing
# else on the item) — they mix freely with edits and new operations in one unit.
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"delete_of": 42},
      {"delete_of": 43, "expected_revision": 1},
      {"edit_of": 44, "amount": -500},
      {"account":"cash","amount":300,"effective_at":"2026-03-01T10:00:00Z"}
    ], "wait_ms": 10000 }'

# The operation now reads DELETED with its last values; its history lists the
# revisions plus every edit *and* delete registration, each with a kind.
curl -s localhost:8080/operations/41 -H 'X-Owner-Id: 1'
curl -s localhost:8080/operations/41/history -H 'X-Owner-Id: 1'
```

- **Statuses.** A delete registration goes `PENDING → APPLIED | INVALID` (terminal);
  on APPLIED the target flips `CONFIRMED → DELETED` (terminal — there is no restore,
  re-insert instead). Rejections are `LIMIT_VIOLATED`, `TARGET_NOT_EDITABLE` (the
  target ended INVALID or is already DELETED — decided by the leader, never refused
  at submission, so a client handles it in one place) or `STALE_REVISION`. A group is
  all-or-nothing: `COMMITTED` means delete and edit items APPLIED and new items
  CONFIRMED; `REJECTED` touches no target.
- **Groups are not deletion units.** A leg of a COMMITTED group can be deleted alone;
  the transaction stays `COMMITTED` and that leg reads `DELETED` among its siblings.
  Deleting a whole group is just a group of `delete_of` items.
- **Reads.** A DELETED operation leaves every balance, statement and point-in-time
  sum (its `−current` leg was applied) but stays readable by id: `GET /operations/{id}`
  returns `DELETED`, its last values, `deleted_at` and `deleted_by`; a delete
  registration id returns `delete_of`; `GET /transactions/{id}` legs carry
  `delete_of` (delete legs), `edit_of` (edit legs) or `revision` (regular legs).
- **`expected_revision`** is a query parameter (`>= 1`); `0` or a non-integer is the
  schema's `422`, unlike the body-path `400` of `PATCH` — deliberate, both are
  client errors before any DB access.
- **Turning it off.** `UPDATE config SET allow_deletes = false;` — hot-reloaded and
  independent of `allow_edits`; the next delete registration is refused with `403`
  (embedded: `ErrDeletesDisabled`); deletes already registered are still decided.
- **Non-guarantee N7** extends to deletes: deleting an operation never adjusts a
  reversal that points at it, deleting a reversal never restores its original, and a
  reversal of a DELETED operation is an ordinary operation.
- **Embedded hosts** register deletes with `InsertOp.DeleteOf` (plus optional
  `ExpectedRevision`) and read `OperationOutcome.DeleteOf` / `DeletedAt` / `DeletedBy`
  ([embedding guide](docs/embedding.md#delete-an-operation-in-a-business-transaction)).

### Configuration contract

Deployment config is `BALANCEDB_*` environment, parsed once at boot; the process
refuses to start on any invalid value. *Behavioral* knobs (`lease_ttl_ms`,
`batch_size`, `api_max_wait_ms`, …) are **not** env — they live in the DB `config`
table (spec §5.2) and are hot-reloaded every cycle (defaults are seeded by the
first migration).

| Var | Required | Default | Meaning |
|---|---|---|---|
| `BALANCEDB_DATABASE_URL` | yes | — | Postgres URL/DSN (`postgres://…`, supports `sslmode` etc.). |
| `BALANCEDB_SCHEMA` | no | `balancedb` | Target schema; created if absent. Selects the cell (see below). |
| `BALANCEDB_HTTP_ADDR` | no | `:8080` | API listen address (`api` role). |
| `BALANCEDB_METRICS_ADDR` | no | `:9090` | Health + Prometheus endpoints (both roles). |
| `BALANCEDB_LOG_LEVEL` | no | `info` | slog level (`debug`\|`info`\|`warn`\|`error`). |
| `BALANCEDB_LOG_FORMAT` | no | `json` | `json` \| `text`. |
| `BALANCEDB_MIGRATE_ON_START` | no | `true` | Set `false` when using the explicit `migrate` role as a deploy gate. |
| `BALANCEDB_POOL_MAX_CONNS` | no | `10` | pgxpool size. |

Behavioral knobs — the single `config` row, changed with plain SQL (`UPDATE config
SET …;`) and picked up on the next cycle/request without a restart:

| Column | Default | Meaning |
|---|---|---|
| `lease_ttl_ms` | `15000` | Leader lease TTL; failover bound (spec §7.1). |
| `loop_interval_ms` | `1000` | Processor polling interval (the doorbell wakes it earlier, ADR-0002). |
| `batch_size` | `200` | Decisions per DB commit (spec §8.5). |
| `max_group_size` | `10` | Maximum items per `POST /transactions` unit — new operations, edits and deletes together. |
| `api_max_wait_ms` | `30000` | Cap on `wait_ms` for synchronous waits (spec §10.1). |
| `allow_edits` | `true` | `false` refuses **new** edit registrations (`403` / `ErrEditsDisabled`) and keeps the cell on the reversal-only contract; already registered edits are still decided (ADR-0010). Read per request by the insertion core; the processor never checks it. |
| `allow_deletes` | `true` | `false` refuses **new** delete registrations (`403` / `ErrDeletesDisabled`), independently of `allow_edits`; already registered deletes are still decided (ADR-0011). Same read pattern as `allow_edits`. |

### Roles

- **`api`** — serves the HTTP API (spec §10). Stateless; scale horizontally behind a
  load balancer. Each instance keeps its own `LISTEN outcomes` connection for the
  synchronous-wait path (ADR-0002).
- **`processor`** — the single-leader decision loop (spec §7–§8). Run **two or more**;
  exactly one holds the leader lease at a time (spec §7.1) and the rest idle as hot
  standbys. Keep every standby in service — on leader death a standby acquires the
  lease within `lease_ttl_ms` and continues selecting visible committed work in ID order (ADR-0008). The three
  processing guards (spec §7.2) make a stale ex-leader's late writes no-ops, so
  failover is safe even if the old leader is only *slow*, not dead.
- **`migrate`** — applies the embedded migrations and exits. Use it as a CI/CD gate
  (see upgrades). The runner is advisory-lock guarded, so running it concurrently
  (or alongside `MIGRATE_ON_START=true` roles) is safe and idempotent.

### Multiple cells / environments in one Postgres

`BALANCEDB_SCHEMA` selects an **independent installation** inside the same database:
all objects are unqualified and resolved through `search_path` (plan §0), and each
schema carries its own migration history. Point one cell's api+processors at
`BALANCEDB_SCHEMA=cell_a` and another's at `cell_b` (or `staging` / `prod`) against
the same Postgres and they share nothing — separate accounts, ledgers, leases, and
config. (Note: the LISTEN/NOTIFY doorbell/outcome channels are per-*database*, so
cells sharing a database may receive each other's wakeups — harmless hints; ordering
and correctness come from each cell's own `ORDER BY id` work select, ADR-0002.)

### Upgrade procedure

The image is self-contained and forward-only (migrations never edit an applied file;
a rollback is a new migration). Two supported paths:

1. **Migrate-on-boot (default).** Deploy the new image; every `api`/`processor`
   runs the embedded migrations on boot under an advisory lock
   (`BALANCEDB_MIGRATE_ON_START=true`), so concurrent rollout is safe and the first
   one to win the lock applies, the rest no-op.
2. **Gated migrate (recommended for controlled rollouts).** Set
   `BALANCEDB_MIGRATE_ON_START=false` on the long-running roles and run the new
   image once as the **`migrate`** role first (this is what the compose `migrate`
   service does). It applies migrations and exits; only then do the api/processor
   containers start. This makes "schema is up to date" an explicit, observable gate
   in your pipeline rather than a boot side effect.

## Observability (spec §13)

Every process — `api` and `processor` alike — serves an operational HTTP surface on
`BALANCEDB_METRICS_ADDR` (default `:9090`):

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus exposition of the metrics below (plus Go runtime + process collectors). |
| `GET /healthz` | Liveness: `200` while the process is serving. Never touches the DB. |
| `GET /readyz` | Readiness: `200` when the DB is reachable, `503` otherwise. For the processor the body also reports `leader` — **information only; a standby is healthy** (the standby must stay in the load-balancer so it can take over). |

Requests to the API are wrapped in **panic-recovery** (a handler panic becomes a
logged `500`, never a crashed process) and **request logging** (method, path,
status, latency; `5xx` logged at warn). Slow SQL is logged by a pgx query
**tracer** at `obs.DefaultSlowQueryThreshold` (200 ms) and counted in
`balancedb_slow_queries_total`.

### Metrics

All metrics are prefixed `balancedb_`. The rightmost column ties each to its
authority.

| Metric | Type | What it measures | Rationale |
|---|---|---|---|
| `queue_depth` | gauge | PENDING operations awaiting a decision. | §13 — the system's only latency promise is "~one cycle when healthy"; backlog is its health signal. |
| `oldest_pending_age_seconds` | gauge | Age of the oldest PENDING op, on the **DB clock** (`now() - registered_at`). | §13 — the other half of queue health; a growing oldest-age means the leader is behind. |
| `loop_busy_seconds_total` / `loop_wall_seconds_total` | counters | Leader time spent draining vs. total cycle wall-clock. | §13 loop utilization ρ = `rate(loop_busy_seconds_total) / rate(loop_wall_seconds_total)` — the queue-latency predictor and the cell-splitting trigger. |
| `snapshot_rows_touched` | histogram | Balance-snapshot rows written per confirmed operation (1 = newest day; more = a backdating cascade). | §13 — measures the backdating workload in production (spec §8.4 makes backdating O(days spanned)). |
| `leadership_changes_total` | counter | Lease ownership transitions (gained or lost) this instance observed. | §13 — `rate()*3600` = changes/hour; a high rate is lease flapping (tuning or infrastructure). |
| `leader` | gauge | `1` if this instance currently holds the lease, else `0`. | Companion to leadership changes: which instance is the leader right now. |
| `decisions_total{kind,outcome}` | counter | Committed terminal decisions: `kind` ∈ single\|group\|edit\|delete, `outcome` ∈ confirmed\|invalid\|committed\|rejected\|applied (`edit` and `delete` × applied\|invalid). | §13 decision counters by outcome; the `edit` and `delete` kinds are the adoption/rejection mix of each (ADR-0010, ADR-0011). Counted **only on a committed batch**, so a guard-miss rollback never inflates them. |
| `edit_deferrals_total` | counter | Units (a single edit or delete, or a whole group) skipped because an edit or delete target was still PENDING; decided on a later cycle once the target is. | §13 / ADR-0010 / ADR-0011 — one shared counter; zero is the norm; a sustained rate means overlapping insertion transactions (ADR-0008). Never blocks the work behind it. |
| `doorbell_wakeup_lag_seconds` | histogram | Insert-commit → leader-pickup lag per operation, on the **DB clock** (`now() - registered_at` at fetch). | **ADR-0002** — the measured doorbell latency win: tens of ms on an idle system, degrading gracefully into queue latency under load. |
| `api_wait_seconds{source,result}` | histogram | Synchronous-insert wait latency (register → return). `source` ∈ immediate\|notify\|poll\|timeout, `result` ∈ decided\|pending. | **ADR-0002** / §13 NOTIFY→outcome lag — API wait health. The `notify` source is the low-latency fast path; `poll` is the durability fallback firing. |
| `slow_queries_total` | counter | SQL queries over the slow-query threshold (also logged). | §13 DB-write health — surfaces queries approaching the cell's DB ceiling. |
| `pool_*` | gauges + counters | pgxpool stats read live on each scrape: `acquired_conns`, `idle_conns`, `constructing_conns`, `total_conns`, `max_conns`, `acquire_count_total`, `acquire_duration_seconds_total`, `empty_acquire_count_total`, `canceled_acquire_count_total`, `new_conns_count_total`. | plan §M9 — connection-pool saturation is an early warning before the DB ceiling. |

**Clock discipline.** Latency measurements that compare two ledger events —
`oldest_pending_age_seconds` and `doorbell_wakeup_lag_seconds` — take *both*
endpoints from the DB clock inside one SQL statement, never mixing in the process
clock (the same rule the lease and ordering paths follow). Within-process
durations (loop utilization, request latency, the wait histogram, slow-query
timing) are measured with the process clock, which is correct for an elapsed span
that begins and ends in the same process.

**Not app metrics.** Spec §13 also lists raw `DB row-writes/s`, `WAL throughput`,
and `fsync latency`. Those are Postgres-server metrics; scrape them from a Postgres
exporter alongside this app's `/metrics`, not from the app itself (plan §M9 scopes
the app to the set above plus pgxpool stats).

### Watching it locally

Run the load generator against a live `api` node and scrape the `processor`'s
`/metrics` to watch ρ, queue depth, and batching behave under load:

```sh
make loadgen ARGS="-rate 200 -duration 30s -accounts 20"
# groups instead of singles:
make loadgen ARGS="-rate 80 -group-size 3"
curl -s localhost:9090/metrics | grep balancedb_
```

`cmd/loadgen` is a developer tool only (not part of any deployed binary); it inserts
small positive amounts against unbounded accounts, so operations are always
accepted — the point is throughput and timing, not validation.

## Development

The `Makefile` is the interface:

| Target | What it does |
|---|---|
| `make lint` | gofumpt check + `golangci-lint run` (go vet across all build tags) + CLAUDE.md/AGENTS.md sync. |
| `make test` | unit tests (no external dependencies). |
| `make itest` | integration tests against `TEST_DATABASE_URL` (each test in a throwaway schema). |
| `make simtest` | deterministic simulation harness against `TEST_DATABASE_URL` (spec §15). |
| `make openapi` | regenerate `api/openapi.yaml` from the code, fail on drift, then `oasdiff breaking` vs `HEAD` (informational — no `--fail-on`; skipped when `oasdiff` is absent). |
| `make loadgen` | run the local load generator (see above). |

This box has no toolchain on the default PATH; see `docs/handoff.md` for the
bootstrap (`export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/bin:$PATH"`).

## Simulation testing (spec §15)

The `simtest/` package is the deterministic simulation harness — the spec's declared
first investment and verification of the consistency contract G1–G6 as refined by
ADR-0008 for concurrent insertion visibility. It runs in
two tiers (ADR-0006):

- **Breadth (in-memory, `make test` and `make simtest`).** A seeded generator drives
  the full scenario space — tight/loose/one-sided/unbounded limits, singles, groups
  (mixed sizes, same-account multi-leg, non-zero-sum), aggressive back/future-dating
  with day crossings and exact-timestamp ties, reversals (double-reversals and
  reversal-after-reject retries), state-aware limit changes, idempotent
  replays/conflicts, and edits (ADR-0010: single `PATCH`-shaped edits, grouped edits
  and mixed groups, with `expected_revision` guards, account moves and
  `effective_at` moves across days) and deletes (ADR-0011: single deletes, delete
  groups and mixed groups, with `expected_revision` guards and targets that are
  fresh, edited, INVALID or already DELETED) — through the sequential reference model across
  **1,000+ seeds** (default 1,200), asserting G1 (final balance within limits), G2 (no
  partially applied group), G4 (timeline order total and unique), G6 (no duplicate
  rows), G3 reproducibility (the same seed replays to an identical decision
  sequence), dense append-only revision histories, and the deletion invariants (a
  DELETED row has exactly one APPLIED delete and never reads CONFIRMED again). Every
  tier prints its action mix (`insert/replay/conflict/reversal/set_limits/edit/
  edit_group/edit_group_mixed/delete/delete_group/delete_group_mixed`, plus the
  fine-grained `del_*` guard/target/replay classes) and fails if any edit or delete
  class is absent from the run.
- **Fidelity (DB-backed, `make simtest`, build tag `simtest`).** The real processor
  runs over throwaway schemas and the database is asserted equal to the reference
  model — interleaved with fault injection: batch-boundary crashes, competing-leader
  failover, zombie-leader stale-lease writes (the lease row is expired/stolen under
  running processors; the guards must catch every stale write), and a Guard-3
  version-race stressor. Identity ids do not line up between the two (idempotent
  replays consume Postgres IDENTITY values the reference does not), so the harness
  compares through an explicit reference→database id map — statuses (APPLIED and
  DELETED included), balances, latest snapshots, each operation's current columns and
  `revision`, `edit_of`/`is_delete`, `deleted_by`/`deleted_at`, and every
  `operation_revisions` row.

Additional transaction-visibility schedules hold an embedded credit open while a
later HTTP-core debit commits, then vary processing time and credit commit/rollback.
These cover both singles and groups and demonstrate the accepted ordering risk:
an early debit rejection remains terminal after the credit becomes visible.

Every failure prints its seed, and every seed reproduces exactly. Seed counts are
environment-overridable so CI can scale the fidelity tier: `SIMTEST_SEEDS` (breadth),
`SIMTEST_DB_SEEDS` (full-scenario), `SIMTEST_CRASH_SEEDS`, `SIMTEST_FAULT_SEEDS`
(competing/zombie), `SIMTEST_GUARD_SEEDS` (version race).

### Guard-3 mutation smoke test (one-time, manual)

The plan (§M10 "Done when") calls for a deliberately introduced bug to be caught by
the harness. The guard with the subtlest teeth is Guard 3 (the account-version CAS,
spec §7.2): its protection is the **rowcount check** that rolls the transaction back
when the CAS matches zero rows (the account version moved between the processor's read
and its write — an API race). Removing that check lets a missed CAS commit the
operation's status flip *without* applying the balance.

To reproduce (do **not** commit the change): in `internal/processor/single.go`,
replace the Guard-3 rowcount check in `accept`

```go
	if tag.RowsAffected() != 1 {
		return errGuardMiss
	}
```

with `_ = tag` and run `make simtest`. `TestSimVersionRaceGuard3` fails immediately,
e.g.:

```
--- FAIL: TestSimVersionRaceGuard3/seed=7000000
    sim_faults_test.go:225: seed 7000000: account unbounded balance db=-21 reference=-43 (G1)
```

Then revert the file (`git checkout -- internal/processor/single.go`) — the guards are
inviolable (CLAUDE.md §2) and never ship weakened. Verified once on 2026-08-22; the
committed tree keeps all three guard rowcount checks.
