// Package simtest is the deterministic simulation harness (spec §15). It grew one
// milestone at a time — the reference model seeded at M3, decisions at M5/M6 — and
// M10 completed it into the full spec §15 investment.
//
// It has two tiers (ADR-0006):
//
//   - Breadth: the seeded generator (generate.go) drives the full scenario space —
//     limits (tight/loose/one-sided/unbounded), singles, groups (mixed sizes,
//     same-account multi-leg, non-zero-sum), aggressive back/future-dating with day
//     crossings and exact-timestamp ties, reversals (double-reversals and
//     reversal-after-reject retries), edits (ADR-0010: single, grouped and mixed
//     with new operations; amount, account and day-crossing instant changes,
//     expected_revision guards, CONFIRMED / INVALID / already-edited / same-round
//     PENDING targets), state-aware limit changes, and idempotent
//     replays/conflicts — through the in-memory sequential reference model across
//     1,000+ seeds under `make test`, asserting G1/G2/G4/G6, the editing
//     invariants (edits never CONFIRMED, dense append-only histories) and G3
//     reproducibility, and printing the action mix (every edit class must occur).
//   - Fidelity: the DB-backed tests (build tag `simtest`, run by `make simtest`)
//     drive the real processor over throwaway schemas and assert the database equals
//     the reference model — statuses, current columns, revisions and the
//     append-only operation_revisions rows, transactions, balances, timelines and
//     snapshots — interleaved with fault injection: batch-boundary crashes
//     (sim_db_test.go), competing-leader failover, zombie-leader stale-lease writes,
//     and the Guard-3 version-race stressor (sim_faults_test.go), all with edits in
//     the mix; the ADR-0008 visibility schedules (registration_db_test.go) include
//     an edit registered against a held-open, then PENDING, target. Because
//     identity ids do not line up (idempotent replays consume Postgres IDENTITY
//     values the reference does not), the harness compares through an explicit
//     reference→database id map.
//
// The reference model is pure and deterministic: given the same seed it produces the
// same schedule and outcomes, and every property test prints its seed so a failure
// reproduces exactly. The README documents the one-time Guard-3 mutation smoke test.
package simtest
