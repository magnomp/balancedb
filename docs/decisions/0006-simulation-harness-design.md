# ADR-0006 — Deterministic simulation harness design (M10)

Status: accepted (M10, 2026-08-22)

## Context

Plan §M10 / spec §15 require a seeded-PRNG harness that generates the full scenario
space (limits, singles, groups, aggressive back/future-dating with day crossings and
timestamp ties, reversals incl. double-reversal and retry-after-reject, limit
changes, idempotent retries), interleaved with fault injection (kill/restart,
competing-leader failover, zombie-leader stale-lease writes, batch-boundary crashes),
and asserts G1/G2/G3/G4/G6 against a sequential reference model, with 1,000+
reproducible seeds "in CI-reasonable time" and a one-time Guard-3 mutation check.
Several design forces had to be resolved:

1. **1,000+ seeds vs. DB cost.** Each DB-backed seed creates/migrates/drops a
   throwaway schema and runs the real processor (~1 s+). 1,000+ of those is minutes,
   not CI-reasonable.
2. **Id alignment breaks.** The M6 harness compared the DB to the reference by
   identity id. Once replays/conflicts enter, `INSERT ... ON CONFLICT DO NOTHING`
   consumes (skips) a Postgres IDENTITY value while the reference assigns none, so ids
   no longer line up — and a reversal's `reversal_of` must reference the real op id.
3. **Catching a disabled Guard 3.** Guard 3 (the account-version CAS) is a backstop.
   Guard 2 plus the lowest-id-first work select already serialize same-operation work,
   so pure leadership churn rarely opens the window Guard 3 defends — a black-box
   competing/zombie-leader run does **not** reliably catch a disabled Guard 3.

## Decision

- **Two tiers.** A *breadth* tier drives the full scenario space through the
  in-memory reference model across 1,000+ seeds under `make test`/`make simtest`
  (seconds); it asserts G1/G2/G4/G6 as structural invariants and G3 as
  run-twice-identical reproducibility. A *fidelity* tier (build tag `simtest`) drives
  the real processor over throwaway schemas across a smaller seed set and asserts the
  database equals the reference. Seed counts are env-overridable
  (`SIMTEST_SEEDS`, `SIMTEST_DB_SEEDS`, `SIMTEST_CRASH_SEEDS`, `SIMTEST_FAULT_SEEDS`,
  `SIMTEST_GUARD_SEEDS`) so CI can scale the fidelity tier up.
- **Explicit reference→database id map.** The harness records the map from each fresh
  insert's returned ids (registration order) and compares operations, transactions,
  and timelines through it; balances compare by the stable `(owner_id, external_id)`.
  The map also translates a reversal's `reversal_of`. This is the alternative the M6
  handoff flagged to id alignment.
- **Zombie-leader = lease tampering under real processors.** Rather than a synthetic
  stale write, the harness runs real processors while directly expiring/stealing the
  lease row (short horizons so it stays reclaimable); the proof that Guard 1/Guard 3
  caught every stale attempt is that the drained DB still equals the reference.
- **Guard 3 has its own stressor.** `TestSimVersionRaceGuard3` bumps account
  versions out-of-band (an API-style write) while the processor drains with
  `batch_size=1`, reliably moving the version between the processor's read and its
  CAS. This is the vehicle the README's one-time mutation smoke test uses: disabling
  Guard 3's rowcount check lets a missed CAS commit the status flip without the
  balance, and this test catches it. Verified once manually (documented in the
  README); the guard is unchanged in the committed tree.
- **Reversal generation respects the live-reversal unique index.** The generator only
  reverses a confirmed operation with no live (non-INVALID) reversal, so lockstep
  inserts stay clean; the one-live-reversal and reversal-after-reject rules are
  asserted directly in `TestSimReversalConstraints`.

## Consequences

- The 1,000+-seed requirement is met by the breadth tier; the fidelity tier is the
  teeth (where the mutation bites) at a modest default seed count, scalable in CI.
- Any future change that lets a fresh insert consume ids differently, or that adds a
  new insert outcome, must keep the reference↔DB id map faithful (record every fresh
  leg) or the comparison breaks.
- **Invariants preserved.** The harness only observes; it changes no processor,
  validation, ordering, or guard logic. The three guards (spec §7.2) and G1–G6 are
  exactly as before — the harness is now their proof.
