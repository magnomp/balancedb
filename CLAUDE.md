# CLAUDE.md — how to work in this repo

Read before touching code. This file points at the authorities; it does not restate
them. **`docs/architecture-spec.md`** is what the system does (never contradict it
silently); **`docs/plan.md`** is how it is built (§0 global decisions apply to every
change); **`docs/decisions/`** (ADRs) records every deviation or refinement — spec +
ADR log is the complete current truth. `docs/handoff.md` is the running log between
sessions. Deviating from spec or plan requires a new ADR, not a quiet edit.

## 1. Orientation

BalanceDB is a Postgres-backed ledger: clients insert money operations (singles or
atomic groups) and a single elected leader validates and confirms them in strict
registration order, so an account's final balance never breaks its configured
min/max limits. One Go binary runs as `api`, `processor`, or `migrate`; all SQL is
hand-written against a configurable schema. **Read `docs/architecture-spec.md`
§2–§4 before any non-trivial change** — the consistency contract G1–G6 (spec §4.1)
is the product; everything here exists to protect it.

## 2. Inviolables

Stated imperatively. Breaking one breaks the product — stop and write an ADR before
you do.

- **All three processing guards stay** (spec §7.2), each with its rowcount check, in
  *every* processor transaction: lease fence (Guard 1), conditional status flip
  (Guard 2), account-version CAS (Guard 3). Any guard matching 0/mismatched rows ⇒
  `ROLLBACK`, re-acquire lease, continue. Never "optimize" a guard away.
- **Operations are never mutated after their facts are set.** Status flips
  `PENDING → CONFIRMED|INVALID` once; `INVALID` and (transactions) `REJECTED` are
  terminal.
- **Money is `int64` minor units end to end.** No floats, no `float64` JSON
  decoding, ever. All amount parsing goes through the one designated helper in
  `internal/model` — nothing else touches the conversion (spec §5.1, plan §0).
- **Time comparisons for lease and ordering use the DB clock (`now()`), never
  `time.Now()`.** Timeline order is `(effective_at, id)`, fixed at insert (spec §5.4).
- **No atomic work spanning two owners.** One owner per group is the sharding
  invariant (spec §2/§10.1); a feature that touches two owners atomically is out of
  contract — stop and design, write an ADR, do not improvise.

## 3. Code conventions

- **SQL** is either in `migrations/` or a `const` next to its single call site —
  greppable. Written **unqualified** (`search_path` owns the schema, plan §0). No
  ORM, no query builders, no string-built SQL.
- **Migrations are forward-only.** New numbered file only; never edit an applied
  migration; no down migrations (a rollback is a new migration).
- **Config split:** new *behavioral* knobs go in the DB `config` table (hot-reloaded,
  spec §5.2); new *deployment* knobs go in `BALANCEDB_*` env (plan §0 table).
- **Errors are handled explicitly at every SQL call.** No panics in library code. A
  guard miss rolls back and continues — it is normal control flow, not an error.
- **New dependencies require an ADR.** Default answer is no; the stack is boring on
  purpose (plan §0).
- Go, gofumpt-formatted (`make fmt`). Each `internal/` package's `doc.go` states its
  single responsibility and spec section.

## 4. Workflow — the Makefile is the interface

- `make lint` — gofumpt check + `go vet` + `check-docs` (CLAUDE.md/AGENTS.md sync).
  No external deps.
- `make test` — unit tests, no external deps.
- `make itest` — integration tests against **`TEST_DATABASE_URL`**; each test
  self-isolates in a throwaway schema. *(Stub until M2 lands the migration/schema
  harness.)*
- `make simtest` — deterministic simulation harness (spec §15). *(Arrives in M10.)*

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
- `db` — pgxpool, `search_path` per connection, ping-on-boot, `WithTx` (plan §0). **built**
- `migrate` — embedded migrations + purpose-built runner (spec §5.2, ADR-0001). **built**
- `model` — row types, status enums, reason codes, the one amount helper, payload hashing (spec §5). **built**
- `lease` — leader lease acquire/renew/release (spec §7.1). *(M4)*
- `processor` — main loop, single/group processing, the three guards, batching (spec §7–§8). *(M5–M6)*
- `snapshot` — snapshot upsert + cascade update (spec §8.4). *(M5)*
- `api` — insertion core (`Insert` over a `pgx.Tx`, spec §10.1) **built (M3)**; HTTP handlers: waiting, queries (Huma, spec §10, ADR-0003) *(M7–M8)*.
- `notify` — LISTEN/NOTIFY: work doorbell + outcome fan-out (spec §11, ADR-0002). *(M3/M5/M8)*
- `obs` — metrics registry, health endpoints (spec §13). *(M9)*

---

**Maintain this file.** Update CLAUDE.md in the *same commit* as any change that
alters a convention it states — a stale instruction file is worse than none. Keep it
under ~150 lines; details live in the spec and ADRs, not here. `AGENTS.md` is a
symlink to this file; `make lint` fails if they drift.
