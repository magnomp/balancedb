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
