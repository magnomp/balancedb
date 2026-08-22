# BalanceDB — Agent Orchestration Guide

One milestone = one agent with clean context, running on Opus. Each agent's entire world is: its task file in `tasks/`, the files that task tells it to read, git history, and `docs/handoff.md`. That constraint is by design — it forces the docs to be complete, which is exactly what long-term AI maintenance needs.

## The kit

```
docs/architecture-spec.md   The spec, verbatim. Behavior authority.
docs/plan.md                The implementation plan. Structure/scope authority.
docs/decisions/             ADRs (created by agents as they go).
docs/handoff.md             Inter-agent log (created by the M1 agent).
tasks/_common.md            Rules every agent reads first.
tasks/m1.md … m13.md        One self-contained brief per milestone.
.claude/agents/balancedb-implementer.md   Opus-pinned subagent definition.
```

## Running it (Claude Code)

Two equivalent modes — pick per taste:

**A. One fresh session per task (simplest, fully clean context).**
From the repo root:

```
claude --model opus
> Read tasks/m1.md and execute it.
```

When it finishes and gates are green, review + merge, then open a *new* session for the next task. Never continue to the next milestone in the same session — the clean-context discipline is the point.

**B. Delegate from a main session (you keep one supervisory conversation).**
The `.claude/agents/balancedb-implementer.md` definition pins `model: opus` and encodes the execution protocol. From any session in the repo:

```
> Use the balancedb-implementer subagent to execute tasks/m2.md.
```

Subagents start with fresh, isolated context — they don't see your main conversation — so this gives the same isolation while your main session tracks progress. Manage/inspect agents with `/agents`. (If your main session runs a cheaper model, note that some surfaces cap a subagent's model at the main session's tier — mode A avoids the question entirely.)

## Order and gating

```
m1 → m1_5 → m2 → m12* → m3 → m4 → m5 → m6 → m7 → m8 → m9 → m10 → m11 → m13
```

\* m12 (devcontainer) is sequenced early on purpose — the plan recommends it right after M2 so every later agent has `make itest` working against the bundled Postgres. Its brief accounts for this.

Gate between tasks = the task's "Done when" satisfied + its verification commands green + your review of the diff. The agent's finishing protocol (in `tasks/_common.md`) makes it commit on a branch, append to `docs/handoff.md`, and report — merge is yours.

## Decision checkpoints (agents will stop and ask)

- **m1**: Go module path (if you haven't set one, it leaves a TODO).
- **m13**: CI system, if not inferable from the repo host.

Answer these in the session, and have the agent record the answer as an ADR — that's how decisions survive context resets.

Already decided by the operator (pre-recorded, agents just follow them): ADR-0002 — bidirectional LISTEN/NOTIFY (insert-path doorbell wakes the leader; deciding commits notify waiters; waiting itself remains optional via `wait_ms`), and ADR-0003 — code-first OpenAPI via Huma v2 (the server's typed operations are authoritative; the generated spec is exported to `api/openapi.yaml`, committed, and diff/breaking-change-checked in CI; docs at `/docs`).

## Rules for you, the operator

- Don't hand two agents overlapping scopes in parallel; the sequence is a dependency chain, and `docs/handoff.md` is append-only single-writer by convention.
- If an agent reports an open question instead of finishing: that's the protocol working, not a failure. Answer, then re-invoke the same task.
- After merging each task, skim `docs/handoff.md` — deferred items accumulate there and should be triaged every few milestones.
