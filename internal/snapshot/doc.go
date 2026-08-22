// Package snapshot maintains the daily cumulative balance snapshots (spec §8.4).
//
// A snapshot row (account_id, day, balance) holds the CUMULATIVE confirmed
// balance at the end of a UTC day. Applying a confirmed operation of amount v on
// its effective_at UTC day is two writes: upsert the operation's own day (seeded
// from the most recent earlier snapshot so a brand-new day starts at the right
// running total), then a sparse cascade that adds v to every existing later day.
// The newest-timestamp common case touches zero later rows (O(1)); a k-day
// backdate touches at most k rows — which is what keeps backdating cheap and
// leaves reads O(1)/O(page) (spec §14). Backdated and future-dated operations
// need no special code: the UTC-day arithmetic places them.
//
// The processor (M5 for singles, M6 for group legs) calls Apply inside the same
// transaction that flips the operation to CONFIRMED, under the three guards of
// spec §7.2 — so a snapshot write is only ever committed together with its
// balance change.
package snapshot
