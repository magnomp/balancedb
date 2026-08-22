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
