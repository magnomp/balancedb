# ADR-0005 — Insertion idempotency via ON CONFLICT DO NOTHING; doorbell on fresh inserts only

Status: accepted (implementer decision, 2026-08-22)

## Context
Plan §M3 describes the idempotency core as "probe-and-insert logic (unique-violation
→ fetch existing → compare `payload_hash` → replay result or conflict error)" and,
via ADR-0002, "every successful insert transaction emits `NOTIFY work_available`".
Two implementation details need pinning down, both within the letter of the plan:

1. **How the unique-violation is detected.** A plain `INSERT` that hits the
   idempotency unique index raises SQLSTATE 23505, which aborts the *current*
   transaction in Postgres — every later statement fails until rollback. But the
   whole insert is one transaction (`db.WithTx`), and the fetch-existing step must
   run *inside* it. Catching 23505 would therefore require a SAVEPOINT/subtransaction
   around every insert. `INSERT ... ON CONFLICT DO NOTHING RETURNING id` expresses
   the same logical flow without ever raising 23505: a returned row is a fresh
   insert, an empty result means the key already exists (and, under a concurrent
   retry, only after the other transaction has committed — so the follow-up SELECT
   sees the row in READ COMMITTED).

2. **Whether an idempotent replay rings the doorbell.** A replay writes no new
   PENDING operation, so ringing `work_available` would signal work that this
   transaction did not create. The doorbell is a pure hint (ADR-0002), so ringing on
   replay cannot cause incorrectness — but it is a false wakeup.

## Decision
1. Use `INSERT ... ON CONFLICT (<key>) [WHERE <partial-index-predicate>] DO NOTHING
   RETURNING id` for both singles (partial index `idx_ops_idem`) and groups (full
   unique constraint on `transactions.idempotency_key`). An empty return triggers
   the fetch-existing + `payload_hash` compare: equal → replay the original state;
   different → `ErrPayloadConflict`. On-demand account upsert uses the same DO
   NOTHING + fallback SELECT idiom, so an existing account's row is never rewritten
   (avoiding MVCC churn and row-lock contention with the processor's version CAS).
2. Ring the doorbell (`NOTIFY work_available`) only when the transaction created new
   operation rows — a fresh single or a fresh group. Replays and payload conflicts
   ring nothing.

## Consequences
- The insert core needs no SAVEPOINTs and never poisons its transaction; error
  handling stays "explicit at every SQL call" (CLAUDE.md).
- M5's leader still treats the doorbell as a hint with the timed wakeup as the
  correctness path, so suppressing the ring on replay changes only latency (of a
  wakeup that would have found no new work anyway).
- **Invariants preserved.** G6 idempotency is unchanged — the unique index remains
  the arbiter; DO NOTHING is just the non-erroring way to observe it. The doorbell
  carries no correctness weight in either direction (ADR-0002), so ringing only on
  fresh inserts is sound. Notifications are still emitted inside the transaction, so
  a rolled-back insert rings nobody.
