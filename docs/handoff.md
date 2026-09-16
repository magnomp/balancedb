# Handoff notes

Running log left by each milestone agent for the next. Newest section at the
bottom. See `tasks/_common.md` for the protocol.

## OPEN QUESTIONS

_(none)_

## Go embedding — 2026-09-13

Historical implementation note: its insertion lock was subsequently removed by
ADR-0008 at the user's request. See the caller-controlled transaction entry below
for the current behavior and replacement visibility simulations.

Added the public root `balancedb` package: typed `Config`, explicit `Migrate`,
`Open`, caller-owned `pgx.Tx` insertion, context-controlled `Run`, diagnostic
`IsLeader`, and cancelling/joining `Close`. Every host replica can run the existing
fenced processor; PostgreSQL elects one leader per schema. Embedding opens no HTTP
server, installs no signals and does not mutate global logging. Background pools
are owned separately; insertion never checks out a second connection.

ADR-0007 records the refinement to the standalone plan. The insertion core moved
from `internal/api` to `internal/ledger`; HTTP retains aliases and delegates to it.
Public insertion uses a savepoint and temporarily sets/restores transaction-local
search_path, including keeping host temporary tables from shadowing ledger tables.
Failures discard partial groups/accounts without committing the host transaction.
READ COMMITTED and the same PostgreSQL database are required; separate schemas
work. Explicit Migrate reuses the advisory-locked forward-only runner unchanged.

G3 ordering fix: the shared core acquires a schema-specific transaction advisory
lock before writing accounts/allocating IDs, retained through the outer commit or
rollback. This prevents an invisible earlier insertion being overtaken by a later
committed operation. Long host transactions therefore delay subsequent insertions
in that cell. Upgrade/drain all old insertion nodes before enabling embedding;
older binaries do not participate in this lock. No processor guards or decision
logic changed; no dependencies or SQL migrations added.

The simulation reference now models an open registration, commit/rollback, and
blocked successors. Eight DB-backed schedules compare a held embedded credit and
a concurrent HTTP-core debit against it, observing the actual blocked advisory
lock before releasing the first transaction. Tests cover both commit and rollback.
Public integration coverage includes host-write atomicity, schema restoration and
temporary-table shadowing, savepoint error recovery, group idempotency/conflicts,
isolation rejection, parallel migrations, independent schema registration and
leaders, and replica shutdown/takeover with subsequent processing.

Validation: `make lint test itest simtest openapi` passed (full simulation ~49s),
plus `go test -race -tags itest .` passed. OpenAPI remained identical; oasdiff is
absent, so its optional breaking-change check was skipped. Tests used an isolated
PostgreSQL 18 container on localhost:55439, removed after verification. Sandbox
socket restrictions required escalation for DB tests. Local make is in
`$HOME/bin`; Go/lint caches were placed under `/tmp/balancedb-*`.

Usage and rollout notes: `docs/embedding.md`, with a compiled example in
`example_test.go`. Existing user changes to `.devcontainer/devcontainer.json`,
`.claude/settings.json`, and `.compozy/` were left as found.

## M1 — 2026-08-22

Built:
- Repo scaffold: `go mod init github.com/magnomp/balancedb` (Go 1.25), `.gitignore`,
  `Makefile` (`lint`, `test`, `itest` [stub until M2], `fmt`, `tools`).
- `internal/config` — parses/validates all `BALANCEDB_*` vars (plan §0 table),
  schema-name regex `^[a-z_][a-z0-9_]{0,62}$`, postgres URL validation, and
  `RedactedDSN()` (password → `xxxxx`) for the startup log. `LoadFrom(getenv)`
  seam keeps tests off the process environment.
- `internal/db` — `Connect()` builds a pgxpool, sets `search_path` per connection
  in `AfterConnect` via `pgx.Identifier{schema}.Sanitize()` (quoted, never
  interpolated), pings on boot with actionable error text. `WithTx()` helper
  (commit on success, rollback on error/panic).
- `cmd/balancedb/main.go` — role dispatch `api|processor|migrate`, slog setup
  (json/text, level from config), `signal.NotifyContext` on SIGTERM/SIGINT,
  graceful shutdown. api/processor boot → hold on ctx → clean exit; migrate is a
  stub (runner is M2).

Decisions: no ADRs. Only dependency added is `jackc/pgx/v5` (already mandated by
plan §0, so no ADR needed). No deviations from spec/plan.

Deferred/known issues:
- `make itest` is a stub — no DB integration tests until the migration/throwaway-
  schema harness lands (M2 + the M10 test-infra note).
- `make lint` is gofumpt-check + `go vet` only. golangci-lint config is planned
  for M13; the CLAUDE.md/AGENTS.md sync check is M1.5.
- `search_path` is set to a schema that does not exist until M2 migrations run.
  Postgres accepts this (resolution is lazy); `Ping` does not touch schema
  objects, so boot succeeds against an empty database. Fine as-is.

Notes for next agents (environment):
- **This box had no `go`, `make`, or `gofumpt` preinstalled and no passwordless
  sudo.** I installed Go 1.25 to `$HOME/sdk/go`, `gofumpt` to `$HOME/go/bin`, and
  extracted `make` to `$HOME/bin` (from the `make` .deb). Put these on PATH before
  running the Makefile:
  `export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/bin:$PATH"`.
  `make tools` installs gofumpt; a similar bootstrap may be needed for other tools.
- A `postgres:18` container `balancedb-pg` is running on `127.0.0.1:5432`
  (db/user/pass all `balancedb`). Manual boot check used:
  `BALANCEDB_DATABASE_URL=postgres://balancedb:balancedb@127.0.0.1:5432/balancedb?sslmode=disable`.
- Gates run green: `make lint`, `make test`. Manual: `migrate`, `api`, `processor`
  all boot, log the redacted startup line, and shut down cleanly.

## M1.5 — 2026-08-22

Built:
- `CLAUDE.md` (96 lines, ≤150 cap) — five sections per plan §M1.5: orientation,
  inviolables, code conventions, workflow, package map. Points at spec/plan/ADR
  sections; paraphrases nothing. Package map lists the full `internal/` target set
  with spec sections, marking unbuilt packages `(Mn)`.
- `AGENTS.md` — symlink → `CLAUDE.md` (sync by construction).
- ADR skeleton: `docs/decisions/README.md` (numbering rule + index),
  `template.md`, and `0001-custom-migration-runner.md` (records the plan §M2
  decision). Existing 0002/0003 integrated into the index, not renumbered.
- `doc.go` for `internal/config` and `internal/db` — package comment moved out of
  `config.go`/`db.go` into `doc.go` (single package comment per package), each
  stating responsibility + spec/plan section.
- `make check-docs` (wired into `make lint`) — verifies `AGENTS.md` is a symlink to
  `CLAUDE.md`, else byte-identical, else fails with an actionable message.

Decisions: ADR-0001 written (custom migration runner; retroactive record of the
plan §M2 call, no behavior change). No new dependencies. No deviations from
spec/plan.

Deferred/known issues:
- CLAUDE.md's workflow section references `make simtest` (M10) and a real
  `make itest` (M2) that are still stubs/absent — intentional; the map/workflow
  describe the stable target, not today's partial state.

Notes for next agents:
- CLAUDE.md is now authoritative and supersedes the seed "Conventions" in
  `tasks/_common.md`. Update CLAUDE.md in the SAME commit as any change that alters
  a convention it states, and keep it ≤150 lines.
- When you add an `internal/` package, add its `doc.go` (responsibility + spec
  section) and flip its map line in CLAUDE.md from `(Mn)` to **built**.
- Cold-session spot-check passed: CLAUDE.md alone answers "the three guards + where"
  (§7.2, every processor tx) and "how to run integration tests" (`make itest` +
  `TEST_DATABASE_URL`).
- Gates green: `make lint` (incl. `check-docs`), `make test`. PATH bootstrap from
  the M1 note above still applies on this box.

## M2 — 2026-08-22

Built:
- `migrations/0001_init.sql` — full spec §5.2 DDL verbatim (accounts, transactions,
  operations, balance_snapshots, leader_lease, config, all indexes) + seed rows for
  `leader_lease` and `config` (`INSERT ... ON CONFLICT DO NOTHING`; column defaults
  fill every behavioral value — no env-seeding needed, there are no env vars for the
  config-table knobs). Unqualified names; the runner sets `search_path` first.
- `migrations/` asset package (`migrations.go`, `package migrations`, `//go:embed
  *.sql` → `migrations.FS`). See note below on why the embed lives here.
- `internal/migrate` — the runner (`Run(ctx, databaseURL, schema) (applied []int,
  err error)`): dedicated connection → set search_path → advisory lock → CREATE
  SCHEMA → per-schema `schema_migrations` → apply each pending embedded migration in
  its own tx (recording the version in the same tx). Returns versions applied this
  call (empty on a no-op boot). Unit test covers filename parsing/ordering (no DB).
- `internal/dbtest` — the throwaway-schema integration harness used by every later
  milestone: `URL(t)` (skip if `TEST_DATABASE_URL` unset), `RandomSchema(t)`,
  `NewSchema(t) *pgxpool.Pool` (fresh migrated schema, auto drop+close via
  `t.Cleanup`), `DropSchema(ctx, url, schema)`. Normal (untagged) package so `make
  lint` vets it.
- `internal/migrate/migrate_itest_test.go` (build tag `itest`) — creates-objects +
  idempotent-second-boot, parallel-migrators race (8 goroutines, exactly-once
  apply), two-schemas-one-database independence.
- `cmd/balancedb/main.go` — migrations wired into boot: `migrate` role always runs
  them then exits; `api`/`processor` run them on boot unless
  `BALANCEDB_MIGRATE_ON_START=false`. Runner uses its own connection, so it runs
  before the app pool is built.
- `Makefile` — real `itest` target: requires `TEST_DATABASE_URL` (fails fast if
  unset), runs `go test -tags itest ./...`. `make test` stays DB-free (integration
  files are behind the `itest` tag).

Decisions:
- **ADR-0004** — reordered plan §M2 steps 2/3: take the advisory lock BEFORE
  `CREATE SCHEMA`. `CREATE SCHEMA IF NOT EXISTS` is not concurrency-safe in Postgres
  (racing boots → 23505 on `pg_namespace_nspname_index`); the lock exists precisely
  to serialize concurrent boots, so schema creation must sit inside it. The lock key
  is the schema *name* (a string) and needs no schema object, so lock-first is
  sound. Caught by the parallel-migrator test before the fix. No G1–G6 impact.
- No new dependencies. The race test uses `sync.WaitGroup` (not
  `golang.org/x/sync/errgroup`, which plan §M2 mentions only illustratively) to
  avoid adding a dependency.

Deferred/known issues:
- None blocking. `make simtest` still absent (M10).

Notes for next agents:
- **go:embed placement.** go:embed cannot reach across directories (`../migrations`),
  so the repo-root `migrations/` dir (per plan §0 layout) carries its own tiny
  `package migrations` with `//go:embed *.sql`; `internal/migrate` imports it. This
  keeps migrations at the root as diagrammed while embedding cleanly. To add a
  migration: drop `NNNN_name.sql` into `migrations/` (numeric prefix, unique,
  positive, unqualified SQL) — the runner discovers and orders it automatically.
- **Use `dbtest.NewSchema(t)`** for any DB-touching test from M3 on; put such files
  behind `//go:build itest` so `make test` stays DB-free and `make itest` runs them.
- The `migrate` runner opens its own connection and does NOT touch the app pool, so
  it is safe to call before `db.Connect`. It is safe to call concurrently.
- `migrate` package map line in CLAUDE.md flipped `(M2)` → **built**.

## M3 — 2026-08-22

Built:
- `internal/model` (now **built**): domain model per spec §5. Row types (`Account`,
  `Transaction`, `Operation`, `BalanceSnapshot`, `Config`) mirroring the schema;
  status vocabularies (`OpStatus`, `TxStatus`); reason codes (`ReasonLimitViolated`,
  `LimitSide`, and the `Rejection` detail struct with `Marshal`/`ParseRejection`
  for the reason columns); `ParseAmount(json.Number) (int64,error)` — THE single
  money entry point, strict (rejects decimals/exponents/whitespace, accepts 0 so the
  DB CHECK stays the authority); `HashPayload([]CanonicalOp)` — canonical JSON (UTC-
  normalised effective_at, struct encoding) → SHA-256 for idempotency.
- `internal/api` insertion core (**built (M3)**; HTTP is still M7–M8): `Insert(ctx,
  tx, req)` as a pure function over a `pgx.Tx` (spec §10.1). Single vs group branch;
  on-demand account upsert by `(owner_id, external_id)`; atomic multi-leg group
  insert with an `op_count` cross-check; Stripe-model idempotency; `payload_hash`
  conflict → `ErrPayloadConflict`; `max_group_size` read from the `config` table;
  one-owner-per-group (sharding invariant, spec §2) enforced here and only here.
  Fresh inserts ring `NOTIFY work_available` inside the tx (ADR-0002); replays do
  not. Sentinel errors for every rejection so M7 can map status codes without string
  matching (`ErrNoOperations`, `ErrInvalidIdempotencyKey`, `ErrMixedOwners`,
  `ErrGroupTooLarge`, `ErrPayloadConflict`, `ErrZeroAmount`).
- `internal/api/insert_itest_test.go` (build tag `itest`, via `dbtest.NewSchema`):
  all plan §M3 cases — single, group, idempotent replay (single + group) returns
  original ids, same-key/different-payload conflict, mixed-owner rejection, group-size
  cap, amount=0 rejected by CHECK, doorbell fires on fresh insert, replay commits
  cleanly with no new rows.
- `simtest/` started (plan §M10 says grow it from M3): `Model`, an in-memory
  sequential reference model that reproduces the inserter's insert-level outcomes
  (new / replay / conflict, guards) and assigns ids like identity columns. First §15
  property test asserts G6 — idempotent replays never duplicate and never grow the op
  count, and same-key/different-payload retries conflict and change nothing; prints
  its seed on failure. Runs under `make test` (pure, no DB); a dedicated
  `make simtest` target stays deferred to M10 per CLAUDE.md.

Decisions:
- **ADR-0005** — insertion uses `ON CONFLICT DO NOTHING RETURNING` for the
  probe-and-insert (avoids 23505 poisoning the single insert transaction, so no
  SAVEPOINTs) and rings the doorbell on fresh inserts only (a replay creates no
  PENDING work; the doorbell is a hint, ADR-0002). Same DO-NOTHING idiom for account
  upsert so an existing account's row is never rewritten (no MVCC churn / no lock
  contention with the processor's version CAS). No new dependencies (pgx/pgconn only).

Deferred/known issues:
- None blocking. `op_count` cross-check on insert confirms all legs landed; the
  processor-side load-and-cross-check for partial groups is M6.

Notes for next agents:
- **Doorbell channel is global, not schema-scoped.** LISTEN/NOTIFY channels are
  per-database, so `NOTIFY work_available` (literal, per ADR-0002) crosses schemas:
  if several cells share one Postgres, a processor for schema A is woken by inserts
  to schema B. Harmless (the doorbell is a hint; the leader selects only its own
  schema's PENDING work `ORDER BY id`), but M5 (the LISTEN side) may want to
  schema-scope the channel (e.g. `work_available_<schema>` via `pg_notify`) to avoid
  spurious cross-cell wakeups. Left literal here on purpose; flag for M5, not a
  blocker. In `dbtest` (many schemas per DB) this is why the doorbell test asserts a
  notification *arrives* rather than asserting none arrives on replay.
- The insert core takes an already-parsed `int64` amount; M7's HTTP layer must decode
  the body with `json.Decoder.UseNumber()` and call `model.ParseAmount` — that is the
  only sanctioned path from JSON to money.
- Idempotency keys are validated as canonical UUIDs in Go (`ErrInvalidIdempotencyKey`)
  before the DB sees them; SQL casts the key with `$n::uuid`.
- `simtest` imports `internal/api` for the shared request/result types; M10 will feed
  the same `api.InsertRequest` schedules to both the DB and the reference model.

## M4 — 2026-08-22

Built:
- `internal/lease` (now **built**): the spec §7.1 leader lease. `New(db)` generates a
  boot UUID (crypto/rand v4, no new dependency). `Acquire(ctx, ttl)` runs the spec's
  single-UPDATE acquire/renew verbatim (`owner=:me OR owner IS NULL OR lease_until <
  now()`), returning whether this instance leads the cycle; `RowsAffected()==1` is the
  leader test. `IsLeader()` returns the cached result of the last Acquire — no
  background goroutine, the caller's loop is the heartbeat (plan §M4). `Release(ctx)`
  clears ownership only `WHERE owner=:me` (never stomps a new leader) and marks the
  instance non-leader. `Owner()` exposes the UUID for logging.
- All lease/expiry comparisons run in SQL against `now()` (DB clock). TTL is passed in
  as a `time.Duration` and sent as milliseconds (`ttl.Milliseconds()`); the expiry is
  computed as `now() + ($2 * interval '1 millisecond')` inside the UPDATE. No
  `time.Now()` anywhere in the package. The caller reads `config.lease_ttl_ms` and
  passes it in (per the M4 constraint; the config read is the processor's job in M5).
- SQL is `const` beside its single call site, unqualified (search_path owns the
  schema). The lease takes a minimal unexported `execer` interface (just `Exec`), which
  `*pgxpool.Pool` satisfies — the lease is autocommit, deliberately not transactional
  (arbitrated by the row, not a connection).
- `internal/lease/lease_itest_test.go` (build tag `itest`, via `dbtest.NewSchema`):
  every plan §M4 "Done when" case — empty lease acquired, held lease renewed
  (lease_until extended), foreign unexpired lease NOT acquired, expired foreign lease
  taken over, graceful release (owner cleared + immediate takeover), plus
  release-does-not-stomp-a-new-leader, and the 100-round two-instance
  mutual-exclusion loop (never both leaders, exactly one each round, IsLeader agrees
  with Acquire).

Decisions: no ADRs. No deviation from spec/plan. No new dependencies (UUID via
crypto/rand, not google/uuid). `owner` is passed as a string cast `$1::uuid`, matching
the M3 insertion convention.

Deferred/known issues:
- The two competing instances in the mutual-exclusion test share one pool (each Lease
  has its own owner UUID); the DB row is the arbiter, so this faithfully tests
  exclusion without a second pool. If a future test wants genuinely separate pools on
  one schema, `dbtest` would need a NewSchema variant that also returns the schema
  name — not needed for M4.

Notes for next agents (M5, processor loop):
- The loop owns the heartbeat: read the `config` row (lease_ttl_ms, loop_interval_ms)
  each cycle, call `lease.Acquire(ctx, ttl)`, and only process when it returns true.
  Call `lease.Release(ctx)` on graceful shutdown before exit.
- Guard 1 (the in-transaction lease fence, spec §7.2) is a SEPARATE `SELECT 1 FROM
  leader_lease WHERE owner=:me AND lease_until > now()` inside each processing tx — it
  is NOT `lease.Acquire`. The lease package covers only §7.1 (the loop-level
  acquire/renew/release); M5 writes the fence against the same `owner` value
  (`lease.Owner()`).
- CLAUDE.md package map line for `lease` flipped `(M4)` → **built**.

## M5 — 2026-08-22

Built:
- `internal/snapshot` (now **built**): the spec §8.4 daily cumulative snapshots.
  `Apply(ctx, execer, accountID, effectiveAt, amount)` does two writes — upsert the
  operation's own UTC-day row (a fresh day is seeded from the most recent earlier
  snapshot so it starts at the right running total; an existing day just gains v),
  then the sparse cascade `UPDATE ... WHERE day > :op_day`. Newest-day common case
  touches zero later rows; a k-day backdate touches ≤ k. UTC day computed in Go from
  the op's own effective_at (pure data transform, not a clock read) and passed as a
  literal `YYYY-MM-DD` cast `::date`, so it is independent of DB session tz. Takes an
  `execer` interface (pool or tx). itest covers new-day/increment and seed+cascade.
- `internal/processor` (now **built (M5)**; groups + batching are M6): the spec §8.1
  leader loop and §8.2 single-op processing with all three §7.2 guards verbatim,
  each rowcount-checked.
  - Loop (`Run`): re-read `config` row → `lease.Acquire` (renew) → if leader, `drain`
    PENDING singles `ORDER BY id LIMIT batch_size` until an empty select → idle-wait.
    Standbys don't listen; they `sleepCtx(loop_interval)`. Only ctx cancellation ends
    `Run` (it then Releases the lease); transient errors and guard misses are logged
    and folded back into the loop.
  - Idle wait (leader only): a dedicated `LISTEN work_available` pool connection,
    `WaitForNotification` with deadline `idleDeadline = min(loop_interval, ttl/2)`.
    Doorbell wakes early; the timed wakeup (DeadlineExceeded → nil) is the correctness
    path. Connection loss drops the listener (re-armed next cycle). Ordering is taken
    ONLY from `ORDER BY id`, never from notification arrival (G3).
  - `processSingle` (one `db.WithTx`): Guard 1 lease fence (`SELECT 1 FROM leader_lease
    WHERE owner=:me AND lease_until > now()`, ErrNoRows → miss) — separate from
    `lease.Acquire`, uses `lease.Owner()`; account read (balance, version, limits,
    external_id); §6 binary validation; ACCEPT → Guard 2 conditional flip to CONFIRMED
    (rowcount≠1 → miss), Guard 3 account version CAS (rowcount≠1 → miss),
    `snapshot.Apply`, `NOTIFY outcomes 'op:<id>'`; REJECT → Guard 2 flip to INVALID
    with `model.Rejection` detail (no account write, so no Guard 3), notify. Any guard
    miss returns `errGuardMiss`, WithTx rolls back, the loop re-acquires and continues.
  - Wired the `processor` role in `cmd/balancedb/main.go` (builds a lease, runs the
    loop). The `api` role still just holds until shutdown (M7).
- `internal/processor/processor_itest_test.go` (internal `package processor`, tag
  `itest`): accept (balance/version/snapshot/status/confirmed_at), reject (INVALID +
  reason detail, no change), unbounded limits, backdated cascade through existing later
  snapshots, future-dated entering final balance + forward cascade (N5), and the three
  guard-miss scenarios (Guard 1 non-leader; Guard 2 re-process already-decided; Guard 3
  stale-version via a test seam, then clean retry). Doorbell wakeup (~0.09s under a 60s
  interval) and doorbell-loss timed wakeup (~1.08s ≈ one 1s interval) both pass.
- `simtest/` reference model extended (single-op validation + confirmed-balance
  evolution): accounts now track limits + balance; `SetLimits` (enforces §6),
  `ProcessNext`/`ProcessAll` decide PENDING singles in registration order and evolve
  balances, `CheckG1`. New property test asserts G1 after every single decision and
  that each decision matches the validation rule (300 seeds; seed printed on failure).

Decisions (no new ADR — all within milestone scope or adherence to existing ADRs):
- **M5 processes singles only.** The work select filters `transaction_id IS NULL`;
  group legs stay PENDING for M6, which replaces this with the full §8.1 single/group
  dispatch + skip-by-status. This is the milestone boundary, not a spec deviation. NB:
  once groups exist, interleaving singles ahead of an earlier-registered group would
  break G3 — M6 must restore the "select all PENDING ORDER BY id, dispatch" form.
- **Doorbell channel kept literal `work_available`** (matches the api NOTIFY side and
  ADR-0002). Not schema-scoped despite the M3 handoff's suggestion, because scoping
  would require changing the api side too (out of M5 scope) and cross-schema wakeups
  are harmless (a spurious wake just re-selects and finds nothing). Left as a possible
  future refinement needing BOTH sides changed together.
- **`idleDeadline = min(loop_interval, ttl/2)`** concretizes plan §M5's "time to next
  safe lease renewal": ttl/2 guarantees the loop wakes to renew well before expiry even
  if loop_interval is configured longer than the TTL.
- Two unexported test seams on `Processor` (`afterAccountRead`, `enteredIdleWait`),
  always nil in production, make the Guard 3 version-CAS miss and the doorbell timing
  deterministic. Not behavior.

Deferred/known issues:
- Groups + batching (§8.3/§8.5) are M6, as planned.
- A standby that never becomes leader still holds its armed listen conn once armed;
  harmless (released on shutdown). Not worth dropping-on-demotion for one conn.

Notes for next agents (M6):
- Replace `fetchPendingSingles` (the `transaction_id IS NULL` filter) with the full
  §8.1 dispatch: `SELECT ... WHERE status='PENDING' ORDER BY id LIMIT :chunk`, then
  `process_single` (exists) vs `process_group` (new), with skip-by-status for later
  legs of an already-decided group. This is what restores G3 when groups are present.
- `process_group` reuses the same guard shape: Guard 1 fence once, Guard 2 flips all
  legs + the transaction row conditionally, Guard 3 per involved account (net per
  account), `snapshot.Apply` per leg, `NOTIFY outcomes 'tx:<id>'`. §8.5 batching packs
  up to `batch_size` decisions per tx with guards per decision — extend `drain`.
- `snapshot.Apply` is leg-agnostic; call it once per confirmed leg inside the group tx.
- CLAUDE.md package map: `processor` → **built (M5)**, `snapshot` → **built**.

## M6 — 2026-08-22

Built:
- `internal/processor` groups + batching (now fully **built**):
  - Restored the full spec §8.1 work select — `fetchPendingWork` selects every
    PENDING operation (singles and group legs) `ORDER BY id LIMIT batch_size`. This
    fixes the M5 note: interleaving singles ahead of an earlier-registered group can
    no longer break G3.
  - `internal/processor/group.go` — `processGroupTx` (spec §8.3): Guard 1 fence,
    read the transaction row (op_count + status for skip-by-status), load all legs by
    transaction_id, cross-check `len(legs) == op_count`, net per account, read/validate
    every involved account in ascending id order (first violation rejects, naming that
    account + shortfall), then all-or-nothing — Guard 2 flips all legs + the tx row
    (rowcount checks: legs == leg count, tx == 1), Guard 3 per involved account with
    its net, `snapshot.Apply` per leg, `NOTIFY outcomes 'tx:<id>'`. A group is decided
    at its first leg by id; later legs are skipped (in-batch via a `decided` set,
    across batches because a decided group's legs are no longer PENDING).
  - Batching (spec §8.5): `drain` → `processBatch` packs one batch (`LIMIT batch_size`
    ops' worth of decisions) into a single `db.WithTx`; guards run per decision; any
    miss rolls back the whole batch and `drain` bubbles `errGuardMiss` so `Run`
    re-acquires and reprocesses — idempotent by construction (Guard 2 conditional
    flips no-op on decided rows).
  - Refactor: `processSingle`/`processGroup` are thin `db.WithTx` wrappers over
    `processSingleTx`/`processGroupTx`, so the guarded body runs either standalone
    (single-decision, used by the itests) or inside the shared batch tx. The
    `afterAccountRead` seam now fires in the group path too.
- `internal/processor/group_itest_test.go` (tag `itest`): multi-account commit,
  one-leg-fails whole-group reject (offending account + shortfall), net-per-account,
  skip-by-status, group Guard 1 / Guard 3 misses, mixed single/group batch, and
  whole-batch rollback + idempotent reprocess on a guard miss.
- `simtest/` groups in the reference model (pure): `decideGroup` (net per account,
  all-or-nothing, first offending account by ascending id — matches the processor),
  `ProcessNext`/`ProcessAll` decide singles and groups in strict registration order,
  `CheckG2`; `refAccount` gained `ExternalID`. Pure property tests in
  `simtest/group_test.go`.
- `simtest/sim_db_test.go` (build tag `simtest`): the DB-backed simulation — drives
  the real processor over a throwaway schema and asserts the DB against the reference
  model. `TestSimGroupsBatchedMatchReference` (large batches → G1/G2/G3 match) and
  `TestSimBatchCrashEqualsSequential` (crash the processor mid-flight via ctx cancel,
  restart, then finish → final state == sequential reference). G2 is checked with
  invariant queries at every restart boundary.
- `Makefile`: added the `make simtest` target (`go test -tags simtest ./simtest/...`,
  requires `TEST_DATABASE_URL`).

Decisions (no new ADR — all within milestone scope / existing ADRs):
- **Guards per decision within a batch.** `now()` is fixed within a Postgres tx, so
  Guard 1 is logically once-per-batch, but the task/spec §8.5 call for guards per
  decision and it keeps `processSingleTx`/`processGroupTx` individually complete and
  reusable — kept the fence in each. Guard 3 (the version CAS) is genuinely
  per-account and is what makes whole-batch rollback safe.
- **Group Guard 3 applies to every involved account, including net 0** (bumps
  version uniformly). Simpler and uniformly race-safe; a spurious retry is harmless.
- **Offending account = smallest involved account_id that violates.** Deterministic
  tie-break shared by the processor (accounts read `ORDER BY id`, first violation) and
  the reference model, so G3 comparison is exact.
- **Batch-crash test uses ctx-cancel + restart** rather than a commit seam: cancelling
  during `db.WithTx` rolls back the open batch (a real mid-batch crash) using only the
  exported `processor.New`/`Run`. No test-only public API added.
- **`make simtest` is DB-backed** (tag `simtest`, requires `TEST_DATABASE_URL`). The
  pure reference-model property tests still run under `make test`; `-tags simtest`
  additionally compiles the DB simulation. This is the M10 harness seed, scoped to M6.

Deferred/known issues:
- `op_count` mismatch in a group is treated as a hard error (not `errGuardMiss`); it
  is unreachable in practice (groups insert atomically, spec §10.1) and would loop if
  it ever happened. Left as a loud failure rather than swallowed. Not a blocker.
- `make simtest` runs ~25s (20 batched seeds + 8 crash-churn seeds). Fine for now;
  M10 will parallelize / tune when it scales to 1,000+ scenarios.

Notes for next agents:
- CLAUDE.md package map: `processor` → **built**.
- The DB simulation relies on **id alignment** between the reference model and the
  DB: accounts are created in identical order (before any ops) and inserts happen in
  lockstep, so identity ids match and outcomes can be compared row for row. Keep that
  invariant if you extend the harness (M10) — or compare by a stable key instead.
- The reference model's group rejection picks the offending account by ascending
  account_id; if a future spec change redefines the tie-break, change both sides
  together or G3 comparison breaks.

## M7 — 2026-08-22

Built:
- `internal/api` HTTP layer (now **built (M7)**) on Huma v2 (`v2.39.1`) over chi
  (`v5.3.2`), code-first per ADR-0003, confined to this package. Every spec §10
  endpoint is a typed operation registered in `server.go`:
  - `POST /transactions` — wraps the M3 `Insert` core inside `db.WithTx`; maps the
    insert sentinels to status codes (`errors.go`); always 202 (fire-and-forget).
  - `GET /transactions/{id}`, `GET /operations/{id}` — status + per-leg statuses +
    machine-readable rejection detail (reuses `model.Rejection` as the typed schema).
  - `GET /accounts/{ext}/balance` (+ `?at=T` point-in-time, §9/N2),
    `GET /accounts/{ext}/statement` (keyset pagination, snapshot-seeded running
    balance, §9), `POST /accounts` (201; 409 on duplicate),
    `PUT /accounts/{ext}/limits` (§6 rules, version CAS, retry-once → 409).
  - `/docs` (Stoplight Elements, request-executing) and `/openapi.{yaml,json}`
    served by Huma via `humachi`/`DefaultConfig`.
- `amount.go` — the `Amount` int64 transport type: a `huma.SchemaProvider` that
  reports `integer/int64` (with the minor-units + JS-precision descriptions) AND an
  `UnmarshalJSON` that decodes via `json.Number` → `model.ParseAmount`. This is how
  the ADR-0003 "int64 fields" requirement and the "all money through
  `model.ParseAmount`, no float64" inviolable are both satisfied at once — see
  Decisions.
- `owner.go` — `OwnerResolver` interface + `HeaderOwnerResolver` (X-Owner-Id →
  positive int64). Declared on every operation; read paths join through
  `accounts.owner_id`, so cross-owner reads 404. This is the pluggable seam the plan
  asks for (a directory-backed resolver drops in without touching handlers).
- `cmd/openapigen` + `make openapi` — regenerates the committed `api/openapi.yaml`
  from the code (no DB needed), fails on drift (CI diff gate), then runs an
  `oasdiff` breaking-change check vs `HEAD:api/openapi.yaml`.
- `cmd/balancedb` api role now serves the router with graceful shutdown (`runAPI`).
- Tests: `api_test.go` (no DB — Amount parsing, spec generation, owner resolver) and
  `api_itest_test.go` (`package api`, tag `itest`; httptest over a throwaway schema)
  covering every endpoint contract, the float-amount / missing-field 7807 rejections,
  payload conflict, owner scoping, N2 point-in-time, statement pagination across a
  snapshot-day boundary, and the limit-update version conflict.

Decisions (no new ADR — this reconciles two existing authorities, it does not
deviate from spec/plan; ADR-0003 governs and pre-approves the Huma/chi stack):
- **Amount routes through `model.ParseAmount`.** ADR-0003 says "int64 fields, floats
  rejected by type"; the CLAUDE.md inviolable + M3 handoff say every JSON→money
  conversion goes through the one helper with a `json.Number` decode (never float64).
  The `Amount` type does both. Note Huma's schema-validation pass parses the body
  into `any` (numbers → float64), but the *authoritative* value comes from a second
  unmarshal into the struct, which invokes `Amount.UnmarshalJSON` (json.Number →
  `ParseAmount`) — so no float64 ever holds a stored money value, and `15.00`/`1e3`
  are rejected there (whole-number floats slip past the integer schema check but not
  past `ParseAmount`). Verified live: `-5.5` → 422 `application/problem+json`.
- **PUT /limits is a full replace.** Both `min_balance` and `max_balance` are set;
  an omitted/null field means unbounded (documented in the field docs and the
  endpoint description). JSON can't distinguish "absent" from "null" for a pointer,
  so partial-update semantics were not attempted; PUT = replace is the clean choice.
- **UTC day buckets computed in Go for reads.** Point-in-time and statement seeds
  compute the UTC day boundary in Go (matching `internal/snapshot`'s write side) and
  bind it as a timestamptz / `::date`, so results are independent of the DB session
  timezone — the snapshot buckets are UTC-fixed forever (spec §5.2).
- **`afterLimitsRead` test seam** on `Server` (nil in production) forces the version
  CAS to miss deterministically, exercising the retry-then-409 path. Mirrors the M5
  `afterAccountRead` seam; not behavior.
- **Dependencies:** `danielgtaylor/huma/v2` + `go-chi/chi/v5` (both pre-approved by
  ADR-0003) and their transitive deps (`google/uuid`, `fxamacker/cbor/v2`,
  `x448/float16`). No new ADR needed.

Deferred/known issues:
- **`oasdiff` is not installed on this box**, so `make openapi`'s breaking-change
  check is skipped with a message (the diff gate still runs and passes). CI must
  install `oasdiff` for the breaking-change gate to be active. The target is wired
  correctly (compares `HEAD:api/openapi.yaml` vs the regenerated file).
- **`wait_ms > 0` is stubbed as 0** (always 202) — synchronous waiting is M8. The
  field is documented as such in the request schema. M8 wires `internal/notify` +
  the wait path into `createTransaction`.
- The `/docs` page uses Huma's default Stoplight Elements renderer, which loads its
  JS from a CDN in the browser. Automated tests assert `/docs` returns HTML that
  references the spec URL and that real requests execute against the live server
  (all itests do); full in-browser "click Try It" execution was verified only via
  the reasoning that Stoplight calls the same endpoints — treat browser execution as
  a manual check if needed. No CSP is set (this is a first-party server, not an
  Artifact), so the CDN load is fine.

Notes for next agents (M8):
- Reuse `createTransaction` in `transactions.go`: when `wait_ms > 0`, register a
  waiter on `tx:<id>`/`op:<id>` before/around the insert, then wait (with the
  1–2 s poll fallback) capped by `api_max_wait_ms` from the `config` row. Outcome →
  200; expiry → 202 + current state. The processor already emits
  `NOTIFY outcomes 'op:<id>'|'tx:<id>'` inside the deciding commit (M5/M6).
- CLAUDE.md package map: `api` → **built (M7)**; `notify` stays `(M8)`.
- Any change to an `internal/api` request/response struct changes the contract:
  run `make openapi` and commit the regenerated `api/openapi.yaml` in the same
  change (the diff gate enforces it).

## M8 — 2026-08-22

Built:
- `internal/notify` (now **built**): the outcome fan-out half of ADR-0002. `New`
  builds a `Notifier`; `Run(ctx)` holds one dedicated `LISTEN outcomes` connection
  per API process and demultiplexes each `op:<id>` / `tx:<id>` payload to the waiters
  registered for that key. `Register(key)` returns a size-1 buffered signal channel
  (coalesced; the dispatcher never blocks) plus an unregister func. On any connection
  failure `Run` reconnects-with-resubscribe: the in-memory registry is untouched, only
  `LISTEN` is re-issued — notifications lost during the gap are the API poll path's
  job (NOTIFY is a hint, correctness is the poll). `BackendPID()` exposes the listen
  backend (M9 metrics + tests). Depends only on pgxpool — no cycle with `api`.
- `internal/api` synchronous wait (now fully **built**): replaced M7's stubbed
  `wait_ms>0` inside the existing `createTransaction`. New `wait.go`:
  `waitForOutcome` registers the waiter BEFORE one immediate status check (closes the
  decided-before-registration race), then `select`s over the deciding NOTIFY, a
  status-poll ticker (`defaultPollInterval` 1s; the durability fallback), and the
  deadline. Budget = `min(wait_ms, api_max_wait_ms)` read from the config row per
  request. Outcome → 200 with decided status; expiry → 202 with current state.
  `wait_ms<=0` is unchanged fire-and-forget (202) and never calls the notifier.
- Wiring: `NewServer(pool, resolver, notifier)` gained the notifier param (an
  `outcomeWaiter` interface — nil = poll-only, used by `cmd/openapigen`). `runAPI`
  in `cmd/balancedb` starts one `notify.Notifier` per api process in a goroutine.
- `api/openapi.yaml` regenerated: description-only changes (endpoint + `wait_ms`
  field); no shape change (see Decisions). CLAUDE.md map: `api` and `notify` → **built**.
- itests: `internal/notify` (delivery, reconnect-with-resubscribe after the listen
  backend is terminated, key-scoped dispatch, coalescing); `internal/api`
  (`wait_itest_test.go`: decided-before-registration, expiry→202+PENDING, cap by
  api_max_wait_ms, poll fallback after the listen backend is killed with reconnect
  held off so only the poll resolves, wait_ms=0 registers no waiter) and
  `wait_e2e_itest_test.go` (external `package api_test`: real processor + real
  notifier, `loop_interval=10s`, synchronous insert resolved in ~25 ms → proves the
  doorbell+notify fast path, not a timed fallback).

Decisions (no new ADR — implements ADR-0002 as planned; reconciles a Huma detail):
- **200/202 via runtime status override, contract unchanged.** Huma reads the
  output struct's special `Status int` field to set the response code at runtime but
  (v2.39.1) does NOT register it as an extra response — so the OpenAPI still declares
  only 202 for `POST /transactions`. The task required "no shape change, only
  descriptions"; this delivers exactly that. NB: because the `Status` field now
  exists on `CreateTransactionOutput`, every return path MUST set it explicitly
  (0 would emit HTTP 0) — the fire-and-forget path sets 202.
- **Poll interval is a fixed 1s default with an unexported `Server.pollInterval`
  test seam** (mirrors the existing `afterAccountRead`/`afterLimitsRead` seams).
  Not a config knob: spec §10.1 pins 1–2 s and it is a durability fallback, not a
  behavioral tuning surface.
- **`outcomeWaiter` interface in `api`, not a hard dep on `notify`.** Keeps `api`
  decoupled (it still imports no notify) and lets the wait tests use a fake. A nil
  notifier degrades cleanly to poll-only.
- **Notify test shortcut:** the api wait itests flip op status directly (past the
  processor guards) because they test the API wait *mechanics*, not the decision
  engine; the end-to-end test uses the real processor.

Deferred/known issues:
- The `outcomes` channel is global (per-database, like the doorbell): a Notifier may
  receive another cell's payloads sharing the DB — harmless (dispatched to zero local
  waiters). Same latent cross-cell wakeup noted for the doorbell in M3/M5; if ever
  scoped, scope both channels together (needs the processor NOTIFY side too).
- On request-context cancellation mid-wait the handler returns `ctx.Err()` (→ 500-ish
  via Huma). Fine for a disconnected client / shutdown; not worth a special code.

Notes for next agents (M9 observability):
- `Notifier.BackendPID()` is already exported for metrics. ADR-0002 asks M9 to
  measure doorbell wakeup lag and NOTIFY→outcome lag — the wait path in `wait.go` is
  the natural place to time register→resolve, and `notify.Run` for reconnect counts.
- The wait path reads `api_max_wait_ms` once per waiting request (`selectAPIMaxWait`);
  if M9 adds a cached/observed config read, route it through there too.

## M9 — 2026-08-22

Built:
- `internal/obs` (now **built**): the cell's observability surface (spec §13).
  - `metrics.go` — `Metrics` on a private `prometheus.Registry` (Go runtime +
    process collectors included). All record methods are **nil-safe**, so a
    component with metrics unwired (tests, the spec exporter) runs unchanged. Metric
    set: `queue_depth`, `oldest_pending_age_seconds`, `loop_busy_seconds_total` +
    `loop_wall_seconds_total` (ρ = rate/rate), `snapshot_rows_touched` (histogram),
    `leadership_changes_total`, `leader`, `decisions_total{kind,outcome}`,
    `doorbell_wakeup_lag_seconds`, `api_wait_seconds{source,result}`,
    `slow_queries_total`.
  - `pool.go` — a custom `prometheus.Collector` reading `pool.Stat()` live on each
    scrape (`balancedb_pool_*`), so pgxpool stats never go stale and need no sampler.
  - `http.go` — `NewHealthHandler` (`/metrics`, `/healthz`, `/readyz`) + `WithRecovery`
    and `WithRequestLog` net/http middleware. `/readyz` pings the DB (2 s timeout →
    503 on failure); for the processor it reports `leader` as info only — a standby is
    ready (plan §M9).
  - `tracer.go` — `SlowQueryTracer` (a `pgx.QueryTracer`) logs + counts any query over
    `DefaultSlowQueryThreshold` (200 ms). Carries the SQL text through the trace ctx,
    never the arg values.
- Instrumentation wired into existing packages (metrics are pure observation; **no
  guard, ordering, validation, or decision logic changed**):
  - `internal/processor` — `SetMetrics(*obs.Metrics)` setter (kept New unchanged to
    avoid test churn). Loop samples `queue_depth`/`oldest_pending_age` each cycle
    (`selectQueueStats`, both age endpoints on the DB clock); counts leadership
    transitions; measures loop busy/wall for ρ. `fetchPendingWork` now also selects
    `registered_at, now()` and observes doorbell wakeup lag per op (both ends DB
    clock). Decision + snapshot-rows metrics buffer in a per-batch `batchAccum` and
    flush **only on a committed batch** (`withBatchTx`), so a guard-miss rollback
    counts nothing. `IsLeader()` exposes leadership via an `atomic.Bool` for the
    cross-goroutine `/readyz` handler (the lease itself stays single-threaded).
  - `internal/snapshot` — `Apply` now returns `(rowsTouched int64, err error)` = 1 +
    cascade rowcount, feeding `snapshot_rows_touched`. All call sites updated.
  - `internal/api` — `WaitObserver` interface + `SetWaitObserver` (mirrors the M8
    `outcomeWaiter` decoupling; api does not hard-depend on obs for the field). The
    wait path times register→return and labels the resolution `source`
    (immediate|notify|poll|timeout) and `result`.
  - `internal/db` — `Connect` gained variadic `Option`s (`WithTracer`); existing
    callers unchanged.
  - `cmd/balancedb` — both roles start the metrics/health server on
    `BALANCEDB_METRICS_ADDR` via `serveMetrics` (best-effort: a bind clash is logged,
    never fatal). The API handler is wrapped in recovery + request logging. The
    slow-query tracer is attached to the pool and its metrics sink set after the
    registry exists.
- `cmd/loadgen` + `make loadgen` — a small load generator (configurable rate,
  accounts, group size, concurrency) that POSTs inserts with a per-request UUID
  `Idempotency-Key`. Developer tool only.
- `README.md` — new; documents every metric with its §13 / ADR-0002 rationale, the
  health endpoints, middleware, clock discipline, and `make loadgen`.

Decisions:
- **No ADR for Prometheus.** `prometheus/client_golang` is pre-sanctioned by plan §0
  ("`prometheus/client_golang` for metrics (§13)"), so it needed no ADR. `go mod tidy`
  pulled its transitive deps; it is now a direct require.
- **NOTIFY→outcome lag interpreted as synchronous-wait latency** (`api_wait_seconds`)
  with a `source` label separating the ADR-0002 notify fast path from the poll
  fallback. A stricter "decision-commit → delivery" measurement was rejected: only
  CONFIRMED singles and decided groups carry a decision timestamp (INVALID singles
  have none), so a DB-clock decision→delivery lag can't be measured uniformly across
  outcomes. The register→return latency, labeled by source, is the honest,
  cheap "API wait health" signal §13 asks for.
- **Slow-query threshold is a fixed 200 ms constant, not an env/config knob.** It is a
  diagnostic aid, not a behavioral surface; adding a `BALANCEDB_*` var was out of M9
  scope. `SlowQueryTracer.Threshold` is settable in code if a future milestone wants it.
- **Metrics via setters (`SetMetrics`, `SetWaitObserver`), not constructor params.**
  Minimizes churn to M5–M8 tests and keeps a metric-less run/test a first-class path
  (nil = no-op). Wiring lives in `cmd/balancedb`.

Deferred/known issues:
- Both roles default `BALANCEDB_METRICS_ADDR` to `:9090`; running an api and a
  processor on the same host needs one overridden (`serveMetrics` logs the bind
  failure and continues, so the ledger still runs). In the smoke test I used `:9091`
  for the processor. Compose/devcontainer (M12) should set distinct ports.
- Raw `DB row-writes/s` / WAL / fsync (§13) are Postgres-server metrics, left to a
  Postgres exporter (documented in the README), not emitted by the app — matches the
  plan §M9 scope (app metrics + pgxpool stats).
- No simulation-model change: M9 adds only observation, so the decision engine the
  reference model mirrors is byte-identical. `make simtest` stays green.

Notes for next agents:
- CLAUDE.md package map: `obs` → **built**.
- Every processing path already flows through `withBatchTx`; if you add a new decision
  outcome, record it via `p.recordDecision(...)` inside the tx so it flushes on commit
  (never `p.metrics.RecordDecision` directly from a tx body — that would count a
  rolled-back batch).
- Verified end-to-end (plan §M9 "Done when"): api + processor booted, `make loadgen`
  drove 1,200 singles + 320 groups, and `:9090`/`:9091` `/metrics` showed
  `decisions_total`, `doorbell_wakeup_lag_seconds` (2,156 ops = 1,199 singles +
  319×3 legs), `snapshot_rows_touched`, ρ counters, pool stats, and a notify-sourced
  `api_wait_seconds` (~8 ms) from a synchronous insert. `/readyz` reported the
  processor's leader state.
- Gates green: `make lint`, `make test`, `make itest`, and `make simtest` (~24 s).

## M10 — 2026-08-22

Built (deterministic simulation hardening; completes the spec §15 harness — see
ADR-0006):
- `simtest/generate.go` — the seeded full-scenario generator. `Generator` emits rounds
  of `Action`s shaped by the current reference state: fresh singles/groups (2–4 legs,
  mixed accounts, same-account multi-leg, non-zero-sum), reversals (double-reversals
  arise because a confirmed reversal is itself an eligible target; reversal-after-reject
  because a rejected reversal frees its original), state-aware limit changes (always
  bracket the confirmed balance, §6), and idempotent replays/payload conflicts.
  Aggressive back/future-dating across 7 UTC days with midnight-boundary hits and
  exact-timestamp ties. Fully seed-determined.
- `simtest/reference.go` — refOp now carries EffectiveAt + ReversalOf; added
  `Timeline` (G4 order), `CheckG6`, `confirmedReversalTargets`, and account/limit
  snapshot helpers the generator reads.
- `simtest/generate_test.go` — breadth tier (pure, runs under `make test` and
  `make simtest`): `TestFullScenarioReference` sweeps **1,200 seeds** (env
  `SIMTEST_SEEDS`) asserting G1/G2/G4/G6; `TestFullScenarioReproduces` asserts G3
  reproducibility (same seed → identical decision sequence). ~1 s.
- `simtest/sim_db_test.go` — rewritten around a `harness` with an explicit
  reference→database **id map** (idempotent replays consume Postgres IDENTITY values
  the reference does not, so raw ids no longer align — the map also translates a
  reversal's reversal_of). `TestSimFullScenarioMatchReference` (full scenario, real
  processor, asserts G1/G3/G6 + G4 timeline + snapshot-cascade invariant);
  `TestSimBatchCrashEqualsSequential` (batch-boundary crashes).
- `simtest/sim_faults_test.go` — `TestSimCompetingLeaders` (3 processors, short TTL,
  kill/restart failover), `TestSimZombieLeader` (real processors + direct lease
  expire/steal → stale-lease-write windows the guards must catch),
  `TestSimVersionRaceGuard3` (out-of-band version bumps racing the processor —
  the Guard-3 API-race stressor), `TestSimReversalConstraints` (one-live-reversal-
  per-op + reversal-after-reject, N3).
- `docs/decisions/0006-simulation-harness-design.md`; README "Simulation testing"
  section incl. the one-time Guard-3 mutation smoke-test write-up; `simtest/doc.go`
  updated.

Decisions (ADR-0006): two-tier harness (breadth in-memory 1,000+ / fidelity
DB-backed), reference→DB id map instead of id alignment, zombie-leader via lease
tampering under real processors, and a dedicated Guard-3 version-race stressor.
Env-overridable seed counts (`SIMTEST_SEEDS`, `SIMTEST_DB_SEEDS`,
`SIMTEST_CRASH_SEEDS`, `SIMTEST_FAULT_SEEDS`, `SIMTEST_GUARD_SEEDS`).

Mutation smoke test (one-time, done manually, documented in README): disabling
Guard 3's rowcount check in `internal/processor/single.go` (`_ = tag`) makes
`TestSimVersionRaceGuard3` fail immediately on every seed with a G1 balance
divergence (a CONFIRMED op whose amount never reached the balance). Reverted; the
committed tree keeps all three guard rowcount checks (verified `git diff` clean).

Key finding for next agents:
- **Pure leadership churn does NOT reliably catch a disabled Guard 3.** Guard 2 plus
  the lowest-id-first work select already serialize same-operation work, so the
  competing/zombie tests (correctness under failover) rarely open Guard 3's window.
  Guard 3's spec'd job is arbitrating API races — the concurrent account-version write
  between the processor's read and its CAS — which `TestSimVersionRaceGuard3`
  reproduces directly. That is the mutation-catch vehicle; keep it if you refactor.
- The reference↔DB id map must stay faithful: record every fresh insert's legs (and
  the tx) in registration order. A new insert path that consumes ids differently, or
  a new insert outcome, needs the map updated or the comparison silently breaks.
- Limit changes are out-of-band (not registered operations), so a DB schedule must
  apply them while drained (no in-flight decisions) to match the reference — the
  full-scenario test applies all of a round's moves before draining for exactly this
  reason.

Gates green: `make lint`, `make test` (simtest breadth ~1.25 s), `make itest`, and
`make simtest` (full, ~52 s: 1,200 breadth seeds + 40 reproducibility + 30 DB-backed
fidelity seeds/cases across full-scenario/crash/competing/zombie/version-race/reversal).

## M11 — 2026-08-22

Built:
- `Dockerfile` — the plan §M11 multi-stage build **verbatim**: `golang:1.25`
  builder → static `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` binary on
  `gcr.io/distroless/static-debian12:nonroot` (no shell, non-root). Migrations are
  embedded (go:embed), so the image is self-contained. `ENTRYPOINT ["/balancedb"]`,
  `CMD ["api"]` — the role is the container command.
- `.dockerignore` — trims the build context to the Go module (source + go.mod/go.sum
  + `migrations/`); excludes docs/scripts/tasks/etc. Verified nothing under go:embed
  lives in an excluded path (only `migrations/*.sql` is embedded).
- `docker-compose.yml` — prod-like reference deployment, all `BALANCEDB_*` env, one
  image reused by every role: `postgres:18` (healthchecked), a one-shot `migrate`
  **deploy gate**, `api`, `processor`, and `processor-standby`. The long-running
  roles set `BALANCEDB_MIGRATE_ON_START=false` and `depends_on` the migrate gate
  (`service_completed_successfully`) + postgres (`service_healthy`). YAML anchors
  share the image/build and env blocks.
- `scripts/smoke.sh` + `make smoke` (and `make image`) — the plan §M11 "Done when"
  end to end: `compose up -d --build` → wait for readiness → insert a 2-leg group
  with `wait_ms` (asserts HTTP 200 + COMMITTED + 2 CONFIRMED legs) → read both
  balances → kill the active processor → assert the standby acquires leadership
  within the lease TTL → prove the new leader drains (a fresh insert CONFIRMS).
  Self-contained (tears the stack down on exit; `KEEP_UP=1` to keep it). No jq/python
  dependency (grep-based JSON checks) so M13 CI can reuse it as-is.
- README "Deployment (Docker + compose)" section: the full `BALANCEDB_*` env
  contract table, role semantics, curl examples, multi-cell via `BALANCEDB_SCHEMA`,
  and the two upgrade paths (migrate-on-boot vs. gated `migrate` role).

Decisions (no ADR — all within milestone scope, no spec/plan deviation):
- **postgres:18 volume mount is `/var/lib/postgresql`, NOT `.../data`.** The 18 image
  refuses a mount at the legacy `.../data` path (docker-library/postgres#1259 — 18
  stores data in a major-version subdir). First attempt with `.../data` left postgres
  permanently `unhealthy` and the stack never came up; the fix is the whole-dir mount.
- **Health endpoints are on `:9090` (metrics port), not the api's `:8080`.** The api
  role's Huma server (`:8080`) has no `/readyz` (that returns 404); `/healthz`
  `/readyz` `/metrics` are served by the obs surface on `BALANCEDB_METRICS_ADDR`
  (M9). The smoke script waits on `:9090/readyz` for the api and hits `:8080` only for
  the ledger endpoints. Documented in the README ports table.
- **Compose uses `restart: unless-stopped` (prod-realistic); the smoke script makes
  failover deterministic by `docker update --restart=no <leader>` before SIGKILL** so
  the killed container stays down and the *survivor* is the only leadership candidate.
  Without that, docker restarts the killed processor within ~2s and it races the
  standby for the lease at TTL expiry (~50/50), making "the standby takes over"
  flaky. The script also proves work resumed (post-failover CONFIRMED insert), which
  is robust regardless of which node wins.
- **Postgres published on host `55432`** (→ container 5432) to avoid colliding with
  the standalone `balancedb-pg` already on host 5432 (per the orchestrator note).
  Service-to-service uses the compose network name `postgres:5432`.
- **Distinct host metrics ports** (api 9090, processor 9091, standby 9092) — inside
  each container the health server binds the default `:9090` (isolated netns, no
  clash), addressing the M9 co-location note without overriding `METRICS_ADDR`.

Deferred/known issues:
- `make smoke` failover leg takes ~16s (config-default `lease_ttl_ms`=15000 + a
  loop). The script's `FAILOVER_TIMEOUT` defaults to 30s; override via env if a CI
  runner is slower. Total `make smoke` ~30–40s after the image is built/cached.
- No code changes to processor/validation/model, so the simulation reference model is
  untouched; `make simtest` stays green (nothing to extend for M11).
- `oasdiff` still not installed (M7 note) — unrelated to M11; `make openapi` skips
  its breaking-change check gracefully.

Notes for next agents (M12 devcontainer, M13 CI):
- **M13 reuses `make smoke` verbatim** as the compose smoke stage. It exits non-zero
  on the first failure and needs only Docker + the compose plugin + `curl` (no jq).
  It builds the image itself (`compose up --build`), so a prior `make image` is
  optional.
- The compose `migrate` gate + `BALANCEDB_MIGRATE_ON_START=false` pattern is the
  reference for the plan §M11 upgrade procedure; M12's devcontainer keeps
  migrate-on-start=true for the single dev DB (simpler), which is fine — both paths
  are advisory-lock safe.
- CLAUDE.md left unchanged: M11 adds no `internal/` package and changes no stated
  convention; `smoke`/`image` are non-gate Makefile targets (same precedent as the
  unlisted `openapi`/`loadgen` targets). `check-docs` stays green.
- Environment left as found: the standalone `balancedb-pg` on host 5432 is still
  running (used by `make itest`/`simtest` via `TEST_DATABASE_URL`); the M11 compose
  stack was torn down (`docker compose down -v`).

## M12 — 2026-08-22

Built:
- `.devcontainer/docker-compose.yml` — dev infrastructure (distinct from the prod-like
  root `docker-compose.yml`): a `dev` service (`mcr.microsoft.com/devcontainers/go:1.25`,
  `sleep infinity`, workspace bind-mount at `/workspaces/balancedb`, `depends_on`
  postgres `service_healthy`) and a `postgres` service (`postgres:18`, `pg_isready`
  healthcheck, named `pgdata` volume mounted at `/var/lib/postgresql` — the pg18 mount
  lesson from M11). Postgres is NOT published to the host (VS Code `forwardPorts`
  handles host exposure), so it can't collide with a host-bound 5432.
- `.devcontainer/devcontainer.json` — `dockerComposeFile`+`service: dev`,
  `workspaceFolder: /workspaces/balancedb`, `forwardPorts: [8080, 9090, 5432]`,
  `containerEnv` (BALANCEDB_DATABASE_URL / BALANCEDB_SCHEMA / TEST_DATABASE_URL [same
  URL — tests self-isolate in throwaway schemas] / BALANCEDB_LOG_FORMAT=text),
  features `docker-in-docker:2` + `robbert229/.../postgresql-client:1`,
  `postCreateCommand: go mod download && make tools`, and VS Code customizations (Go
  extension, gopls `formatting.gofumpt` + `ui.diagnostic.staticcheck`, format-on-save,
  `.sql` association for `migrations/`).
- `Makefile` — added the three missing maintenance-interface targets: `run-api`,
  `run-processor` (`go run ./cmd/balancedb <role>`), and `psql` (opens a shell on
  `$PSQL_URL`/`$BALANCEDB_DATABASE_URL`). The other §M12 targets (test, itest, simtest,
  lint, image, loadgen) already existed from M1/M2/M6/M9/M11 — not duplicated.

Decisions (no ADR — within milestone scope, no spec/plan deviation):
- **postgres-client feature = `ghcr.io/robbert229/devcontainer-features/postgresql-client:1`,
  client-only, `version: "15"`.** Plan §M12 asks for "postgres-client (psql for poking
  at state)"; the client-only feature is the faithful match (the DB server is the
  `postgres` compose service — no redundant server). Its version proposals cap at 15; a
  v15 psql talks to the v18 server fine for interactive inspection. The alternative
  `itsmechlark/features/postgresql:1` supports v18 but installs a full server (heavier,
  unnecessary here) — rejected. Both feature refs verified present on ghcr (200).
- **`make tools` unchanged (installs gofumpt only).** Plan §M12's postCreateCommand
  parenthetical says `make tools` installs "golangci-lint, gofumpt", but golangci-lint's
  config lands in M13 and `make lint` does not use it yet. Adding it now would be M13
  scope, so `postCreateCommand` runs `make tools` exactly as written and it installs
  gofumpt today; M13 extends `make tools` when it adds the golangci-lint config.
- **CLAUDE.md left unchanged.** M12 adds no `internal/` package and alters no stated
  convention. CLAUDE.md §4 names the gate targets, not every Makefile target (same
  precedent as the unlisted openapi/loadgen/smoke targets from M7/M9/M11); run-api /
  run-processor / psql are non-gate convenience targets. `check-docs` stays green.

Validation (the interactive "Reopen in Container" path can't run in this non-interactive
box, so it was validated by construction + a live compose bring-up, NOT a VS Code
devcontainer session):
- `docker compose -f .devcontainer/docker-compose.yml config` parses; services =
  {postgres, dev}. `devcontainer.json` parses as JSONC with all §M12 keys present.
- Referenced images/features confirmed to exist: `mcr.microsoft.com/devcontainers/go`
  tag `1.25` (MCR tags list), `docker-in-docker:2` and the postgresql-client feature
  (ghcr manifests → 200). Pulled the go:1.25 image and verified it ships go 1.25.12 +
  make 4.4.1 + git + gcc (no psql → hence the client feature; no gofumpt → hence
  `make tools` in postCreate), so the "zero manual setup" toolchain claim holds.
- **End-to-end (no VS Code):** brought the `.devcontainer` compose up under a throwaway
  project name, waited for postgres `healthy`, then inside the real `dev` service (env =
  what `containerEnv` provides, DB reached as `postgres:5432`) ran `go mod download &&
  make itest` → all packages green. Also booted `make run-processor` + `make run-api`
  in the dev container: api `/readyz` → 200, `POST /accounts` → 201, processor logged
  "running processor loop" as leader with migrate_on_start=true. Torn down with
  `down -v`; the standalone `balancedb-pg` on host 5432 was untouched.

Deferred/known issues:
- The devcontainer keeps `BALANCEDB_MIGRATE_ON_START` at its default `true` (single dev
  DB, simplest UX) — the opposite of the root prod compose's gated `migrate` role. Both
  are advisory-lock safe (ADR-0001/0004).
- No Go code changed, so the simulation reference model is untouched; `make simtest`
  needs nothing added for M12.

Notes for next agents (M13 CI):
- The GitHub Actions matrix should install `golangci-lint` and add its config, then
  extend `make tools`/`make lint` to use it (the M12 postCreateCommand already calls
  `make tools`, so the devcontainer picks it up for free once M13 lands it).
- CI's integration/simulation stage can mirror the devcontainer wiring: a
  `postgres:18` service container + `TEST_DATABASE_URL` = the same URL (throwaway
  schemas isolate). The compose smoke stage reuses `make smoke` verbatim (M11 note).
- `oasdiff` still not installed on this box (M7/M11 note) — `make openapi` skips its
  breaking-change check gracefully; CI must install it to activate that gate.

## M13 — 2026-08-22

Built (CI + release; the final milestone):
- `.golangci.yml` (schema v2) — the committed golangci-lint config plan §M13 asks
  for. Deliberately narrow: golangci-lint is the `go vet` runner (`default: none`,
  enable `govet`), with `build-tags: [itest, simtest]` so it vets EVERY .go file —
  including the integration/simulation sources that the previous bare `go vet ./...`
  silently skipped. gofumpt (the other §M13 gate) stays enforced by the standalone
  binary the repo already standardizes on — see the gofumpt decision below.
- `Makefile` — `make lint` now runs `golangci-lint run` in place of bare `go vet`
  (still gofumpt check + check-docs); `make tools` now also installs the pinned
  golangci-lint (`GOLANGCI_LINT_VERSION=v2.5.0`), so the M12 devcontainer postCreate
  (`make tools`) picks it up for free — closes the M12 handoff's open item.
- `CLAUDE.md` §4 — updated IN THIS milestone's commit (build(lint) commit) to
  describe the new `make lint` (gofumpt + golangci-lint go vet + check-docs) and its
  tool requirement, replacing the old "go vet … no external deps" line. check-docs
  stays green (AGENTS.md is the symlink). File is 103 lines (≤150 cap).
- `.github/workflows/ci.yml` — the GitHub Actions pipeline (plan §M13). Jobs fan in
  via `needs:`  — `lint` + `test` (parallel) → `integration` → `image`:
  - lint: `make lint` (gofumpt via `go install`, golangci-lint via its official
    pinned installer script — identical to what `make tools`/local use).
  - test: `make test` (pure unit + in-memory sim reference-model property tests).
  - integration: `make itest` + `make simtest` against a `postgres:18` **service
    container**; `TEST_DATABASE_URL` points at it and tests self-isolate in throwaway
    schemas (internal/dbtest), so one DB serves both targets.
  - image: `make smoke` builds `balancedb:local` (docker compose up --build) and runs
    the M11 end-to-end + failover smoke test; the **exact image that passed smoke** is
    then tagged and pushed to GHCR — always `:sha-<12>`, and additionally
    `:<semver>` + `:latest` on `refs/tags/v*`. Push steps are gated to
    `github.event_name == 'push'` (never PRs/forks; job has `packages: write`).
    Smoke runs on PRs too (build+e2e validation) but does not push.

Decisions (no ADR — plan §M13 pre-sanctions "golangci-lint config committed", so this
is planned, not a deviation; golangci-lint is a dev tool, not a go.mod dependency, so
the "new dependency ⇒ ADR" convention does not apply):
- **gofumpt authority = standalone gofumpt, NOT golangci-lint's bundled gofumpt.**
  golangci-lint v2.5.0 bundles an older gofumpt that wants to expand ONE naked return
  (`internal/processor/processor_itest_test.go`) that standalone gofumpt v0.11.0 (and
  the devcontainer's gopls gofumpt) consider already-formatted. Routing the format
  gate through golangci-lint would have (a) forced a reformat of an M5 test file for a
  cosmetic tool-version quirk and (b) created a two-gofumpt drift where a dev's editor
  says "clean" and CI says "dirty". Keeping the standalone gofumpt gate (unchanged
  from M1) avoids both. Consequence: `.golangci.yml` intentionally declares NO
  formatter — golangci-lint does vet only; gofumpt is the separate, stable gate.
- **golangci-lint linter set kept to govet only** (not the broader `default: standard`
  errcheck/staticcheck/unused set). Enabling the standard set surfaces ~22 pre-existing
  findings across earlier milestones (e.g. `obs/metrics.go` uses the deprecated
  `prometheus.NewGoCollector`/`NewProcessCollector`; a couple of dead assignments /
  unused helpers in itest files). Fixing those is out of a CI milestone's scope
  (tasks/_common.md scope discipline). A future cleanup milestone can flip to
  `default: standard` once they are addressed — the config comment says so.
- **CI installs golangci-lint via its official installer script (pinned v2.5.0), not
  the golangci-lint-action.** Fewer third-party actions to version-verify, and the CI
  command becomes byte-identical to the local `make lint` I validated. The Makefile
  `make tools` uses `go install …@v2.5.0` for the same version (both were verified to
  work on this box).
- **Push target = GHCR** (`ghcr.io/<repo>`, lowercased), authenticated with the
  built-in `GITHUB_TOKEN` (no extra secret). SHA tag always; semver + latest on tags.
  The image is built once (by the smoke stage) and that same image is shipped — build
  once, prove, push.

Validation — the live GHA run was validated by LOCAL EQUIVALENT + INSPECTION, NOT an
actual pipeline execution (this box cannot trigger GitHub Actions):
- Workflow parses (`python3 yaml.safe_load`) and passes **actionlint** (which also
  shellchecks the `run:` scripts) with zero findings.
- All referenced action versions confirmed to exist on GitHub (git ls-remote):
  `actions/checkout@v4`, `actions/setup-go@v5`, `docker/login-action@v3`.
- **Every command the CI invokes was run locally and passed**: `make lint`,
  `make test`, `make itest`, `make simtest` (fresh `-count=1`: 42.9s), and
  `make smoke` (built `balancedb:local` 16.1MB, group COMMITTED w/ 2 CONFIRMED legs,
  balances correct, killed leader → standby took over in ~17s, post-failover insert
  CONFIRMED — "SMOKE TEST PASSED"). golangci-lint v2.5.0 installed via the same
  official installer the workflow uses.
- The image job's tag/push shell logic was exercised under bash (the GHA `run:`
  shell): branch push → one `sha-…` tag; `refs/tags/v1.4.2` → `sha-…` + `1.4.2` +
  `latest`. `docker tag balancedb:local …` verified against the real smoke image.

Deferred/known issues:
- **`oasdiff` breaking-change gate is NOT wired into CI.** M7/M11 noted oasdiff isn't
  installed here; `make openapi` skips it gracefully. I did not add an oasdiff step to
  ci.yml because plan §M13's scope is lint/test/image/smoke/push and I could not
  validate an oasdiff step end-to-end locally (tool absent). A follow-up can add an
  `openapi` job that installs oasdiff and runs `make openapi` (the target is already
  correct — compares `HEAD:api/openapi.yaml`). Flagged, not blocking §M13's "Done when".
- CI failover budget is generous (`BOOT_TIMEOUT=300`, `FAILOVER_TIMEOUT=60`) because
  shared runners are slower than a dev box (config-default lease_ttl_ms=15000). Local
  smoke used the script defaults (180/30) and passed with ~17s failover.
- No Go/processor/validation code changed, so the simulation reference model is
  untouched; `make simtest` needed nothing extended for M13.

Notes for next agents:
- To cut a release: push a `vX.Y.Z` tag. CI runs the full pipeline and, on green,
  pushes `ghcr.io/magnomp/balancedb:X.Y.Z` + `:latest` + `:sha-<12>` — a pullable
  image that passed the compose smoke test (plan §M13 "Done when").
- If you widen linting to `default: standard`, fix the pre-existing findings listed
  above FIRST (they live in earlier-milestone code) or the lint gate goes red.
- Environment: golangci-lint v2.5.0 was installed to `$HOME/go/bin` on this box (via
  the official installer script) to validate the config; the PATH bootstrap from the
  M1 note still applies. The standalone `balancedb-pg` on host 5432 was left running
  and untouched; the M13 smoke compose stack was torn down (`down -v`).


## Caller-controlled insertion transactions — 2026-09-13

The user explicitly chose existing caller-owned transactions with no wrapper or
before-commit hook, and advice to insert at the very end of a short transaction
instead of controlling transaction duration or insertion order. ADR-0008
supersedes ADR-0007's registration lock and unconditional G3 claims. Removed that
lock from the shared ledger core. Immediate insertion, savepoint error isolation,
schema restoration, migrations, processor lease and all three guards remain.

Updated the current spec G3/N6, plan §0 refinement, README, embedding guide,
package comments and CLAUDE.md: work queries select visible committed rows by ID,
but overlapping insertion commits can change decision order, acceptance and final
balances. Prompt commit reduces risk without guaranteeing order. Terminal
rejections are not revisited. No all-writers registration-lock rollout is needed.

Replaced the serial-registration simulation with independent transaction
visibility: IDs are allocated before commit; ProcessNext skips uncommitted rows;
rollback discards staged operations while preserving sequence gaps. The staging
seam is explicitly scoped to fresh keys and preexisting accounts (constraint-lock
waits remain covered by integration tests). Eight DB schedules cover an embedded
credit held open while an HTTP-core debit commits, processing before/after credit
visibility, credit commit/rollback, and single/group debit outcomes. They prove
both the allowed overtaking and the terminal rejection consequence, plus compare
balances, operation/group statuses and group atomicity to the reference.

Validation: make lint test itest simtest openapi passed; full simulation ~56s.
OpenAPI stayed identical; optional oasdiff check skipped because it is absent.
The isolated PostgreSQL 18 container on localhost:55439 was removed afterward.
No new dependencies, migrations, wrappers, hooks or queues. Existing unrelated
user changes were preserved.


## Embedded account and query API — 2026-09-13

The initial embedding work was committed as c9d47e0 and published to the new public
repository github.com/magnomp/balancedb. This follow-up uses the isolated worktree
embedded-account-queries and branch feat/embedded-account-queries.

ADR-0009 adds CreateAccount and UpdateLimits using caller-owned pgx transactions,
plus account, final/historical balance, paginated statement, and typed operation/
group outcome queries. HTTP and Go share the internal/ledger core. Composite reads
use a read-only REPEATABLE READ transaction; individual pages are consistent but
subsequent pages can reflect new confirmations. Account creation now rejects bounds
that exclude its initial zero balance (HTTP 422); the simulation reference model
and DB fidelity cases cover this G1 correction. Checked projection arithmetic
returns errors instead of wrapping int64.

Validation: make lint test itest simtest passed against an isolated PostgreSQL 18
instance; simulation completed in 59s. Race tests passed for the public package,
internal/ledger and internal/api with the itest tag. OpenAPI regeneration changes
only the overview's ordering description to match ADR-0008; request/response shapes
are unchanged. Local review covered schema/savepoint restoration, owner isolation,
CAS retry/revalidation, exact large amounts, paging, and concurrent snapshot reads.
No dependencies, migrations, processor guards, or insertion coordination changed.

## Operation editing — 2026-09-16

Shipped (feature spec `.compozy/tasks/operation-editing/`, tasks 01–06; ADR-0010
`docs/decisions/0010-operation-editing.md`, accepted): confirmed operations can be
edited in place — `amount`, `effective_at`, `account` (same owner) — with an
append-only revision history, as single edits (`PATCH /operations/{id}`), grouped
or mixed items of `POST /transactions` (`edit_of`, optional `expected_revision`),
and through the embedded core (`InsertOp.EditOf`/`ExpectedRevision`). An edit is a
registration row in `operations` (same id sequence, PENDING queue, idempotency and
grouping); the leader decides it as two virtual legs (`−old`, `+new`) netted per
account under the three guards plus a fourth rowcount-checked write, the revision
CAS on the target, appending the superseded state to `operation_revisions` in the
same transaction. `GET /operations/{id}` gains the current state + `revision`,
`GET /operations/{id}/history` is the audit trail (revisions + pending + rejected
edits), statement entries and group legs carry `revision`/`edit_of`. `apiVersion`
is 1.1.0. Migration `0002_operation_edits.sql` is additive (no row rewritten;
IT-002 asserts unchanged `xmin`). Hot-reloaded `config.allow_edits` (default true)
keeps a cell on the reversal-only contract.

Decisions (details in ADR-0010 and the task memory files under
`.compozy/tasks/operation-editing/memory/`):
- CLAUDE.md inviolable #2 restated from "never mutated" to "history append-only";
  #1 gains the revision CAS. Spec header note, Principle 5, G4 ("moved by a
  confirmed edit"), new N7 (edits and reversals do not track each other),
  §5.2–§5.4, §7.2, §8.2–§8.4, §10.2–§10.3, §12, §13 refined.
- Idempotency hashing: `model.HashPayload` takes `[]any` of `CanonicalOp` |
  `CanonicalEditOp`; legacy payloads encode byte-identically (golden hash
  `f5bceba8…48ee8`, UT-001). Edits hash the request *as sent* (omitted fields stay
  omitted), while the stored edit row carries the fully resolved proposed state.
- Ledger validation order: structural edit checks (zero DB calls) → one config
  query (`max_group_size`, `allow_edits`) → hash → resolve every edit target
  (owner-scoped, read-only) → write. A bad target in the last item of a group
  writes nothing. `ErrEditChangesNothing` fires structurally, before the target
  lookup. Seven `errors.Is`-able sentinels, re-exported from the root package and
  `internal/api`.
- Processor: `netAndReadAccounts` works over virtual legs for singles, edits and
  groups; Guard 3 CAS runs for every involved account even at net 0; group Guard 2
  flips are split by `edit_of IS NULL / IS NOT NULL`, each rowcount-checked against
  its own class. Two edits of one target in a batch are decided sequentially in one
  transaction. Same-bucket snapshot coalescing: one apply of the delta, or none for a
  zero delta (`snapshot_rows_touched` observed once per edit).
- Deferral (edit reaches the leader while its target is still PENDING) is
  implemented and counted (`balancedb_edit_deferrals_total`), with a drain cursor so
  a deferred head never starves later work — but it is **unreachable through real
  inserts**: the `edit_of` FK refuses an uncommitted target outright (no FK wait),
  and a later-committed edit always has a higher id. Group deferral is checked
  before any rejection so leg order never changes the outcome. IT-042 /
  `TestGroupDeferredWhileTargetPending` drive it through `processBatch`; SIM-003
  asserts the refusal.
- Decided edit rows (APPLIED **and** INVALID) stamp `confirmed_at` as their decision
  instant (history `decided_at`); regular INVALID rows keep it NULL as before.
- HTTP: PATCH and POST structural checks run before any DB access (nil-pool unit
  tests); PATCH maps an unknown/foreign target to `404`, the same case in a group
  item is `422 edit target N not found`; `403` when editing is disabled. Replay with
  `wait_ms > 0` and a decided edit returns `200` (mirrors POST /transactions).
  `POST /transactions` item schema now has `account`/`amount`/`effective_at`
  optional (edit items may omit them); the handler refuses a *new* item missing one
  with `400` — previously the schema's `422` (recorded in ADR-0010; release note).
- OpenAPI: `Rejection.account/limit_side/shortfall` became optional (`omitempty`;
  the new codes `TARGET_NOT_EDITABLE`/`STALE_REVISION` cannot carry them).
  `oasdiff breaking` against the pre-feature baseline reports exactly nine
  `response-property-became-optional` — accepted in ADR-0010 (`LIMIT_VIOLATED` JSON
  is byte-identical). Everything else is additive (2 endpoints, optional
  request/response properties).
- Simulation: the generator emits single edits (`actEdit`) and grouped/mixed edits
  (`actEditGroup`); the reference mirrors ledger registration and both decision paths
  (`decideEdit`, `decideGroup` with edit legs); every tier prints its action mix and
  fails if any edit class is absent; the fidelity tier compares current columns,
  `revision`, `edit_of` and every `operation_revisions` row through the id map
  (ADR-0006 updated).
- Docs (this task): README "Editing operations" section, behavioral `config` knob
  table with `allow_edits`, metric rows (`decisions_total{kind=edit}`,
  `edit_deferrals_total`), simulation coverage; spec consistency pass (§8.3 Guard 2
  wording matched to the code, §10.1 item shape pointer, §12 deferral row);
  CLAUDE.md §1/§4/§5 refreshed (119 lines, AGENTS.md symlink intact);
  `docs/embedding.md` had gained its edit section in task 05.

Validation — full gate set on the final tree (all tasks 01–06 applied), fresh
throwaway `postgres:17-alpine` on 127.0.0.1:55445, `GOFLAGS=-count=1` so nothing
was served from the Go test cache (a first pass without it returned 21 cached
packages in under a second — not evidence):
- `make lint` → `0 issues.` exit 0, 0.4s (gofumpt + golangci-lint go vet across
  `itest`/`simtest` tags + `check-docs`).
- `make test` → exit 0, 2.9s; in-memory breadth tier 1,200 seeds, action mix
  `insert=86743 replay=19185 conflict=14168 reversal=33604 set_limits=29079
  edit=33434 edit_group=7901 edit_group_mixed=15886`.
- `make itest` → exit 0, 29.0s (api 28.2s, processor 6.5s, root 2.2s, ledger 1.9s,
  lease 1.4s, migrate 1.2s, notify 0.7s, snapshot 0.4s).
- `make simtest` → exit 0, 49.0s (`ok simtest 48.6s`: full-scenario DB fidelity,
  batch-boundary crash, competing leaders, zombie leader, Guard-3 race, SIM-003
  host-registration edit visibility).
- `make openapi` → exit 2 at the **drift step only**: `api/openapi.yaml` is
  regenerated but uncommitted, so `git diff --quiet` vs the index fails by
  construction until the feature is committed (tasks 01/04/05 saw the same). The two
  facts the gate protects were proven directly: `make openapi-gen` is idempotent
  (sha256 `a28c86f5c7574482…` before and after), and `oasdiff breaking
  HEAD:api/openapi.yaml → api/openapi.yaml` = the 9 accepted Rejection relaxations,
  0 others (exit 0 — the Makefile passes no `--fail-on`, so the check is
  informational). Expected to be green on the first `make openapi` after commit.
- Every task's own evidence (unit/integration/simulation IDs) is listed per task in
  `.compozy/tasks/operation-editing/memory/task_0N.md`; all were re-run here as part
  of the full suites above.

Deferred/known issues:
- `go build ./...` fails in `simtest` at HEAD and after this feature (the non-test
  file `generate.go` uses `ptr`/`uuid` helpers defined in `_test.go` files; the
  package only ever compiles with tests). `go vet`/`make lint` (which vet with test
  files) and every `make` gate are unaffected. Pre-existing; a small cleanup for
  whoever next touches `simtest`.
- `oasdiff breaking` is still not wired into CI (M13 deferral) and, locally, does not
  fail `make openapi` (no `--fail-on`). If a future change wants it blocking, add
  `--fail-on ERR` and an exclusion (or a regenerated baseline) for the nine accepted
  Rejection relaxations first.
- The processor never reads `allow_edits` (by design: registered edits are always
  decided); the ledger reads it in its own `selectConfig` into a local. The
  `model.Config.AllowEdits` field exists but no `readConfig` scans it yet.
- Deferral remains a defensive path with no organic reproduction; keep IT-042 and
  `TestGroupDeferredWhileTargetPending` as its only proof.

Notes for next agents:
- Release notes for 1.1.0: PATCH/history endpoints and `edit_of` items are additive;
  `Rejection` optional fields; **behavior change** — a `POST /transactions` new item
  missing `account`/`amount`/`effective_at` is now `400` (was `422`); migration 0002
  is additive and forward-only, safe under both upgrade paths (README).
- Adding migration `0003+`: bump `latestVersion` in
  `internal/migrate/migrate_itest_test.go`.
- Any change to `HashPayload` must keep the UT-001 golden hash; any change to
  `model.Rejection` needs `make openapi-gen` + commit of `api/openapi.yaml`.
- `simtest/reference.go:canonicalOps` must stay a mirror of
  `internal/ledger/insert.go:canonicalOps`.
- The `outcomes` NOTIFY channel is per database: itest listeners must skip foreign
  payloads because packages run in parallel against one `TEST_DATABASE_URL`.
- Environment: toolchain bootstrap is `export PATH="$HOME/sdk/go/bin:$HOME/go/bin:$HOME/bin:$PATH"`
  (go 1.25, make, gofumpt, golangci-lint v2.5.0); `oasdiff` is now installed in
  `$HOME/go/bin` (`go install github.com/oasdiff/oasdiff@latest`, task 04). No
  Postgres listens on 5432 here; every task used a throwaway `postgres:17-alpine`
  container (ports 55440–55445) and removed it afterwards — this task's
  `balancedb-task06-pg` on 55445 included. The unrelated Postgres/compose containers
  on this box were left untouched. The whole feature is uncommitted on `master`;
  commit and PR belong to the next surface.

Rebase note (2026-09-16): this work was rebased onto the merged embedded account and
query API (ADR-0009), which is why editing is ADR-0010. The edit fields (`edit_of`,
`expected_revision`, `revision`, current `account`/`amount`/`effective_at`) were
folded into the shared `ledger.OperationOutcome` / `TransactionOutcome` /
`StatementEntry` types so HTTP and embedded Go read them from one query core; the
API-side SQL that task 04 had added for those reads was dropped in favour of it.
All gates were re-run green on the rebased tree.
