// Package processor is the cell's decision engine: the single-threaded leader
// loop that validates PENDING operations in strict registration order and commits
// their outcomes under the three safety guards (spec §7–§8).
//
// The loop (spec §8.1) re-reads the config row, renews the lease, and — only while
// it holds the lease — fetches PENDING work ordered by id and drains it until an
// empty select. When idle it sleeps on the work doorbell (LISTEN work_available,
// ADR-0002) with a deadline of min(loop_interval, time-to-safe-lease-renewal): a
// doorbell wakes it early, but the timed wakeup is the correctness path (a lost
// doorbell costs at most one interval). Ordering never comes from notification
// arrival — it comes exclusively from ORDER BY id (G3). Standbys do not listen;
// they poll for the lease on the normal interval.
//
// Each single operation is decided in one transaction carrying all three guards of
// spec §7.2 verbatim, each with its rowcount check: Guard 1 the lease fence
// (SELECT against leader_lease by the DB clock — separate from lease.Acquire),
// Guard 2 the conditional PENDING→CONFIRMED|INVALID status flip, Guard 3 the
// versioned account-balance CAS. Any guard matching 0/mismatched rows rolls the
// transaction back; the loop re-acquires the lease and continues — normal control
// flow, not an error. Confirmed operations update the daily snapshots
// (internal/snapshot) in the same commit, which also emits NOTIFY outcomes so a
// waiting API node (M8) learns the result.
//
// M5 decides singles; group processing and batching (spec §8.3/§8.5) arrive in M6,
// which replaces the singles-only work select with the full single/group dispatch.
package processor
