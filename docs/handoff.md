# Handoff notes

Running log left by each milestone agent for the next. Newest section at the
bottom. See `tasks/_common.md` for the protocol.

## OPEN QUESTIONS

_(none)_

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
