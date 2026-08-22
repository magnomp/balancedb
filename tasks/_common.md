# Common rules for all milestone tasks

You are one agent in a sequence. Your context is clean: you know nothing about this project except what you read from these files. Previous agents left their state in git history and `docs/handoff.md`; you leave yours the same way.

## Read first, always
1. This file.
2. Your task file's "Read first" list (spec sections, plan sections).
3. `docs/handoff.md` — notes from previous agents (may flag surprises, deferred items, environment quirks).
4. `CLAUDE.md` — exists after task M1.5; from then on it is the authoritative conventions file and overrides the "Conventions" section below if they ever differ.
5. `docs/decisions/` — ADRs recording deviations from the spec/plan.

## Authorities
- `docs/architecture-spec.md` — what the system does. Never contradict it silently.
- `docs/plan.md` — how it is structured (§0 global decisions apply to every task) and each milestone's scope + "Done when".
- Deviations from either require an ADR in `docs/decisions/NNNN-title.md` (~half a page: context, decision, consequences).

## Scope discipline
Implement your milestone, nothing else. If you find a bug in earlier milestones' code: fix it only if your milestone cannot pass its gates otherwise (record it in the handoff); else just record it. If you're blocked on an ambiguity or a plan/spec conflict: stop, write the question into `docs/handoff.md` under `## OPEN QUESTIONS`, and report it — do not guess.

## Conventions (seed; CLAUDE.md supersedes once present)
- Go, gofumpt-formatted; errors handled explicitly at every call; no panics in library code.
- SQL as `const` beside its single call site or in `migrations/`; unqualified names (search_path owns the schema); no ORM, no query builders, no string-built SQL.
- Migrations: new numbered file only; never edit an applied one; forward-only.
- Amounts: int64 minor units; all JSON amount parsing through the designated helper (`internal/model`); never float64.
- Behavior knobs → DB `config` table; deployment knobs → `BALANCEDB_*` env.
- New dependencies require an ADR. Default answer is no.

## Gates (every task, in addition to its own "Done when")
- `make lint && make test` green; `make itest` green if the task touches the DB (requires `TEST_DATABASE_URL`).
- From M10 onward (and earlier if the harness already exists): `make simtest` green; changes to processor or validation logic extend the simulation reference model in the same commit series.

## Finishing protocol
1. Conventional commits on a branch named `m<N>-<slug>` (e.g. `m5-processor-singles`); small, reviewable commits.
2. Append to `docs/handoff.md`:
   ```
   ## M<N> — <date>
   Built: ...
   Decisions: ... (ADR refs)
   Deferred/known issues: ...
   Notes for next agents: ...
   ```
3. Report: summary, gate results (paste the passing output tail), open questions.
