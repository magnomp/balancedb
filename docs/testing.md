# Testing BalanceDB

Developer workflow and the simulation harness. The [README](../README.md) is the
overview; spec §15 and ADR-0006 are the authorities.

## Development

The `Makefile` is the interface:

| Target | What it does |
|---|---|
| `make lint` | gofumpt check + `golangci-lint run` (go vet across all build tags) + CLAUDE.md/AGENTS.md sync. |
| `make test` | unit tests (no external dependencies). |
| `make itest` | integration tests against `TEST_DATABASE_URL` (each test in a throwaway schema). |
| `make simtest` | deterministic simulation harness against `TEST_DATABASE_URL` (spec §15). |
| `make openapi` | regenerate `api/openapi.yaml` from the code, fail on drift, then `oasdiff breaking` vs `HEAD` (informational — no `--fail-on`; skipped when `oasdiff` is absent). |
| `make loadgen` | run the local load generator (see [operations.md](operations.md#watching-it-locally)). |
| `make bench` | throughput benchmark against `TEST_DATABASE_URL`: sustained decisions/s, insert latency, backlog and ρ per inserter level (see [benchmarking.md](benchmarking.md)). |

This box has no toolchain on the default PATH; see `handoff.md` for the
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
