# ADR-0008 — Caller-controlled insertion transactions

Status: accepted (2026-09-13, explicit user preference)

## Context

The host should use its existing `pgx.Tx` and choose when to commit, with no
transaction wrapper, before-commit hook, deferred queue, or insertion-order lock.
ADR-0007's registration advisory lock would hold up other insertions until an
arbitrary host transaction ended. The user prefers guidance to insert at the end
of a short transaction and accepts the risk of reordered decisions.

## Decision

Remove the registration advisory lock from the shared insertion core. Immediate
`DB.Insert(ctx, tx, req)` remains the public API. Its savepoint still isolates
errors and restores search_path; it does not change who owns the outer commit.
PostgreSQL's ordinary constraint and row locks remain. Processor leader election,
all three processing guards, and migration advisory locking are unchanged.

This supersedes ADR-0007's insertion serialization and unconditional G3 claims.
G3 now promises **ID-ordered work selection among committed rows visible to each
work query**. It does not promise global decision order by ID or commit time across
overlapping insertion transactions. A lower ID committed after a work query may
be decided after higher IDs already fetched by that query. Groups remain atomic
and are selected by their first visible leg. G1, G2, G4, G5 (committed work), and
G6 remain in force; strict ID-order acceptance still applies to schedules without
late visibility of lower IDs, such as sequential insertion commits.

Document for both HTTP and embedded ingestion that clients should insert ledger
operations as close as possible to commit and avoid holding the transaction open
afterward. This reduces the window; it is not a correctness guarantee and there
is no duration threshold that guarantees ordering. The processor cannot inspect
uncommitted operations and the single leader does not coordinate their commits.

## Consequences

Overlapping transactions may change acceptance and final balances relative to
global ID-order replay. For an account with balance/minimum 0, credit +100 at ID
41 can remain uncommitted while debit -80 at ID 42 commits and is rejected. The
later credit leaves balance 100, whereas processing 41 first would leave 20.
That rejection is terminal; later visibility does not trigger reconsideration.
An affected group can be rejected in its entirety. G4's `(effective_at, id)`
timeline ordering is unchanged and is distinct from decision order.

No new dependency, migration, wrapper, hook, or queue is introduced. No rollout
protocol requiring all writers to share a registration lock remains. The
simulation model and DB schedules explicitly represent independent transaction
visibility, reversed commit order, rollback, and both early and delayed processor
selection. Sequential schedules retain their existing assertions.
