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
// Each decision — a single (spec §8.2) or a whole group (spec §8.3) — carries all
// three guards of spec §7.2 verbatim, each with its rowcount check: Guard 1 the
// lease fence (SELECT against leader_lease by the DB clock — separate from
// lease.Acquire), Guard 2 the conditional PENDING→CONFIRMED|INVALID status flip
// (for a group, of every leg and the transaction row), Guard 3 the versioned
// account-balance CAS (for a group, once per involved account with its net). Any
// guard matching 0/mismatched rows rolls the transaction back; the loop re-acquires
// the lease and continues — normal control flow, not an error. Confirmed operations
// update the daily snapshots (internal/snapshot) in the same commit, which also
// emits NOTIFY outcomes (op:<id> / tx:<id>) so a waiting API node (M8) learns the
// result.
//
// A group is decided at its first leg by registration order; its remaining legs are
// skipped by status (they are no longer PENDING). Decisions are packed into batches
// (spec §8.5): up to batch_size operations' worth per DB transaction, guards
// evaluated per decision, a whole-batch rollback on any miss and a reprocess that is
// idempotent by construction (the Guard 2 conditional flips no-op on decided rows).
//
// An edit registration (edit_of set, ADR-0010) sits in the same queue and is
// decided as two virtual legs (−current, +proposed) netted per account under the
// same three guards plus a revision CAS on the target; on accept the edit row
// flips PENDING→APPLIED, the superseded state is appended to operation_revisions
// and the target's current columns are overwritten. A group may carry edit legs
// next to regular ones: every leg expands into virtual legs, the net is validated
// per account, Guard 2 flips regular legs to CONFIRMED and edit legs to APPLIED
// (split by edit_of, rowcounts summing to the leg count), each edit target gets
// its history row and revision CAS, and Guard 3 runs once per account; a target
// that ended INVALID or is not at the expected revision rejects the whole group.
// A unit whose edit target is still PENDING is deferred — skipped, left PENDING,
// counted — and the batch continues.
package processor
