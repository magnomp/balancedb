# CLAUDE.md — how to work in this repo

Read before touching code. This file points at the authorities; it does not restate
them. **`docs/architecture-spec.md`** is what the system does (never contradict it
silently); **`docs/plan.md`** is how it is built (§0 global decisions apply to every
change); **`docs/decisions/`** (ADRs) records every deviation or refinement — spec +
ADR log is the complete current truth. `docs/handoff.md` is the running log between
sessions. Deviating from spec or plan requires a new ADR, not a quiet edit.

## 1. Orientation

BalanceDB is a Postgres-backed ledger: clients insert money operations (singles or
atomic groups) and a single elected leader selects visible committed work in ID
order and validates final balances against configured min/max limits. Concurrent
insertion commits can reorder decisions (ADR-0008). Confirmed operations can be
edited in place with an append-only revision history (ADR-0010) and deleted to a
terminal `DELETED` status that keeps the row for audit (ADR-0011); both are
registrations decided by the leader, alone or inside a group. The root `balancedb`
package embeds insertion, migrations and the processor (ADR-0007;
`docs/embedding.md`). One Go binary also runs as `api`, `processor`, or `migrate`;
all SQL is hand-written against a configurable schema. **Read `docs/architecture-spec.md`
§2–§4 before any non-trivial change** — the consistency contract G1–G6 (spec §4.1)
is the product; everything here exists to protect it.

## 2. Inviolables

Stated imperatively. Breaking one breaks the product — stop and write an ADR before
you do.

- **All three processing guards stay** (spec §7.2), each with its rowcount check, in
  *every* processor transaction: lease fence (Guard 1), conditional status flip
  (Guard 2), account-version CAS (Guard 3) — plus the revision CAS on every edit
  target (ADR-0010) and the delete CAS on every delete target (ADR-0011). Any guard
  matching 0/mismatched rows ⇒ `ROLLBACK`, re-acquire lease, continue. Never
  "optimize" a guard away.
- **An operation's history is append-only (ADR-0010, ADR-0011).** Status moves
  forward only — regular rows `PENDING → CONFIRMED → DELETED` or `PENDING → INVALID`,
  edit and delete rows `PENDING → APPLIED|INVALID`; `INVALID`, `APPLIED`, `DELETED`
  and (transactions) `REJECTED` are terminal. A CONFIRMED row's
  `account_id`/`amount`/`effective_at` are overwritten *only* by the leader applying
  an edit under the revision CAS, appending the superseded state to
  `operation_revisions` in the same transaction; `status = 'DELETED'`, `deleted_by`
  and `deleted_at` are written *only* by the leader applying a delete under the same
  CAS, once, with no revision appended. Nothing ever physically deletes from
  `operations` or updates/deletes `operation_revisions`; `id`, `transaction_id`,
  `reversal_of`, `registered_at` are immutable forever.
- **Money is `int64` minor units end to end.** No floats, no `float64` JSON
  decoding, ever. All amount parsing goes through the one designated helper in
  `internal/model` — nothing else touches the conversion (spec §5.1, plan §0).
- **Time comparisons for lease and ordering use the DB clock (`now()`), never
  `time.Now()`.** Timeline order is `(effective_at, id)`, fixed at insert (spec §5.4).
- **No atomic work spanning two owners.** One owner per group is the sharding
  invariant (spec §2/§10.1); a feature that touches two owners atomically is out of
  contract — stop and design, write an ADR, do not improvise.
- **Host transactions stay caller-owned, without insertion serialization.** Use
  `internal/ledger` for all insertions; advise inserting just before commit.
  Concurrent commits can change acceptance order (G3 refined by ADR-0008).
  Embedded calls isolate errors with a savepoint and restore `search_path`.

## 3. Code conventions

- **SQL** is either in `migrations/` or a `const` next to its single call site —
  greppable. Written **unqualified** (`search_path` owns the schema, plan §0). No
  ORM, no query builders, no string-built SQL.
- **Migrations are forward-only.** New numbered file only; never edit an applied
  migration; no down migrations (a rollback is a new migration).
- **Config split:** new *behavioral* knobs go in the DB `config` table (hot-reloaded,
  spec §5.2); *deployment* knobs use `BALANCEDB_*` env for the CLI (plan §0 table)
  or typed Go `balancedb.Config` for embedding (ADR-0007).
- **Errors are handled explicitly at every SQL call.** No panics in library code. A
  guard miss rolls back and continues — it is normal control flow, not an error.
- **New dependencies require an ADR.** Default answer is no; the stack is boring on
  purpose (plan §0).
- Go, gofumpt-formatted (`make fmt`). Each `internal/` package's `doc.go` states its
  single responsibility and spec section.

## 4. Workflow — the Makefile is the interface

- `make lint` — gofumpt check + `golangci-lint run` (go vet across all build tags,
  incl. `itest`/`simtest`; config in `.golangci.yml`) + `check-docs` (CLAUDE.md/
  AGENTS.md sync). Needs `gofumpt` + `golangci-lint` on PATH (`make tools`).
- `make test` — unit tests, no external deps.
- `make itest` — integration tests against **`TEST_DATABASE_URL`**; each test
  self-isolates in a throwaway schema. *(Stub until M2 lands the migration/schema
  harness.)*
- `make simtest` — deterministic simulation harness (spec §15): 1,000+ seeded
  scenarios, in-memory breadth + DB-backed fidelity with fault injection (ADR-0006).
  Requires `TEST_DATABASE_URL`.
- `make openapi` — regenerate `api/openapi.yaml` from the code (server is the
  authority, ADR-0003), fail on drift, and run the `oasdiff` breaking-change check
  (informational, no `--fail-on`; skipped when `oasdiff` is absent). Run it after
  any `internal/api` change and commit the regenerated spec.
- `make bench` — throughput benchmark (`cmd/bench`, `docs/benchmarking.md`): sweeps
  inserter counts against **`TEST_DATABASE_URL`** in a throwaway schema and reports
  the leader's sustained decisions/s, backlog, latency and ρ. Run it before and
  after any processor or ledger change that could move the ceiling.

Definition of done for any change: `make lint test` green, plus `make itest` if it
touches the DB and `make simtest` once that exists. **Changes to processor or
validation logic must extend the simulation reference model in the same commit
series** — the proof travels with the code.

Environment note (from M1 handoff): this box may lack `go`/`make`/`gofumpt` on PATH;
see `docs/handoff.md` for the bootstrap before running the Makefile.

## 5. Package map

One line per `internal/` package (plan §0 layout). Navigate here, don't tree-scan.
Packages marked *(Mn)* are not built yet — the map is the stable target.

- `config` — env parsing + validation of `BALANCEDB_*` (plan §0). **built**
- `db` — pgxpool, `search_path`, ping-on-boot, write transactions + consistent read snapshots (plan §0, ADR-0009). **built**
- `migrate` — embedded migrations + purpose-built runner (spec §5.2, ADR-0001). **built**
- `model` — row types (incl. `operation_revisions`, the `is_delete`/`deleted_*` columns), status enums, reason codes, the one amount helper, payload hashing (spec §5). **built**
- `ledger` — shared accounts, insertion, idempotency, edit/delete registration + `allow_edits`/`allow_deletes`, balances/statements and outcomes (spec §6/§9/§10, ADR-0007–0011). **built**
- `lease` — leader lease acquire/renew/release (spec §7.1). **built**
- `processor` — main loop, single/group/edit/delete processing (delete legs in any group mix), the three guards + revision CAS + delete CAS, deferral, batching (spec §7–§8, ADR-0010/0011). **built**
- `snapshot` — snapshot upsert + cascade update (spec §8.4). **built**
- `api` — Huma/chi HTTP handlers for every §10 endpoint incl. `PATCH`/`DELETE /operations/{id}` + `/history` (ADR-0003/0009–0011), shared ledger insertion + synchronous waiting (ADR-0002/0007). **built**
- `notify` — LISTEN/NOTIFY: work doorbell (api/processor) + outcome fan-out (spec §11, ADR-0002). **built**
- `obs` — metrics registry, health endpoints, HTTP middleware, slow-query tracer (spec §13). **built**

---

**Maintain this file.** Update CLAUDE.md in the *same commit* as any change that
alters a convention it states — a stale instruction file is worse than none. Keep it
under ~150 lines; details live in the spec and ADRs, not here. `AGENTS.md` is a
symlink to this file; `make lint` fails if they drift.
