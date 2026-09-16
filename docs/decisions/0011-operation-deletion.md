# ADR-0011 — Deletes are registrations decided by the leader; a deleted operation is a terminal status, never a removed row

Status: accepted (user direction 2026-09-16: "an operation can be deleted, though
internally we must be able to still trace it for audit purposes; delete can be part
of a group just like edit; a group inserted together doesn't have to be deleted
together"; accepted with the first processor commit that applies a delete)

## Context

ADR-0010 made corrections in-place edits with an append-only history. The remaining
correction the user's non-bank-grade clients perform daily is *removal*: an entry that
should never have existed. Today that is a reversal (spec §5.4, N3): a new opposite
operation, a second id, and "was it reversed?" derived from an index. Clients want
`DELETE` semantics with the same audit guarantees editing has.

Forces: G1 (no final balance outside limits) must survive deletion — removing a spent
credit is a debit without funds; G2 atomicity must extend to grouped deletes; the three
guards and single-writer model stay; every read of §9 selects `status = 'CONFIRMED'` and
must exclude deleted work without query changes; nothing may ever be physically deleted
from `operations` or `operation_revisions`; idempotency hashes of existing keys must not
change; a group inserted atomically must *not* have to be deleted atomically.

Options considered:

- **Physical `DELETE FROM operations` with a tombstone table.** Reads need no new
  status, but it breaks the `operation_revisions`, `reversal_of`, `edit_of` and
  `superseded_by` links (or forces cascading copies), ids stop resolving, and it
  violates the append-only inviolable. Rejected: the user requires full traceability;
  a status is strictly simpler.
- **Deletion as an edit to amount 0.** No new column or status, but `amount <> 0` is a
  CHECK and a product rule, a zero row would still appear in statements, and "deleted"
  and "edited to nothing" are different facts to an auditor. Rejected.
- **A separate `delete_of` column.** Storage would name the public field, but every
  `edit_of IS NOT NULL` predicate (Guard 2 split, target lookups, work fetch, history
  lists, simulation) doubles, and two indexes serve one concept. Rejected: a delete
  *is* an edit whose proposed state is "none"; one marker column keeps every path.
- **Append a revision row and bump `revision` on delete.** Uniform "every change
  appends", but the appended row and the operation row would hold identical values,
  the "current" history entry would describe a state that no longer counts, and
  `expected_revision` gains nothing on a terminal row. Rejected: `deleted_by` and
  `deleted_at` on the row carry the event without redundancy.

## Decision

1. **A delete is an edit-class registration row in `operations`**: `edit_of = <target>`
   plus `is_delete = TRUE`. It reuses the id sequence, PENDING work queue,
   `transactions` grouping, idempotency columns, `idx_ops_edit_of`, the deferral rule and
   the Guard 2 split (`edit_of IS NULL / IS NOT NULL`) verbatim. Its `account_id`,
   `amount`, `effective_at` are filled from the target's current row at submission
   (the columns are NOT NULL; the values are informational and never read by the
   leader). The public surface names it `delete_of` (`DELETE /operations/{id}`,
   `POST /transactions` items, outcomes, operation and leg views); the storage marker is
   `edit_of + is_delete`. The idempotency hash of a delete item is a
   `CanonicalDeleteOp{owner, delete_of, expected_revision}`; legacy and edit encodings
   are untouched.
2. **The leader decides a delete as one virtual leg**: `−current_amount` on
   `current_account` at `current_effective_at`, read inside the deciding transaction.
   Groups net it with every other virtual leg per account and run the unchanged §6
   check, Guard 3 per account and `snapshot.Apply` per leg (a delete leg is never
   coalesced — it has no pair). On accept the delete row flips `PENDING → APPLIED`
   (Guard 2) and the target flips `CONFIRMED → DELETED` under the **delete CAS**
   (`UPDATE operations SET status = 'DELETED', deleted_by = <delete id>, deleted_at =
   now() WHERE id = target AND revision = read AND status = 'CONFIRMED'`, rowcount 1
   else guard miss). No revision row is appended and `revision` is not bumped: the
   row's last values *are* the final state and stay readable; `operation_revisions`
   keeps holding exactly the superseded states. On reject it flips to INVALID with
   `LIMIT_VIOLATED`, `TARGET_NOT_EDITABLE` (target INVALID **or DELETED**) or
   `STALE_REVISION`, stamping `confirmed_at` as its decision instant like a rejected
   edit. A PENDING target defers the unit exactly as for edits, counted on the shared
   `balancedb_edit_deferrals_total`. Decisions are counted under kind `delete`.
3. **DELETED is a terminal status of a regular operation.** The regular state machine
   becomes `PENDING → CONFIRMED → DELETED | PENDING → INVALID`; DELETED, INVALID,
   APPLIED and REJECTED are terminal. A DELETED row is never CONFIRMED again, so every
   §9 read excludes it without a query change. Edits and deletes of a DELETED target
   are registered and decided `INVALID` with `TARGET_NOT_EDITABLE` — at decision time
   only, never refused at submission (user decision 2026-09-16: one place for callers
   to handle it; no new rejection code). The leader's `checkTarget` is the single
   enforcement point, and the CAS predicate `status = 'CONFIRMED'` makes a stale
   leader's apply impossible. There is no restore; a client re-inserts.
4. **Group membership is not a deletion unit.** A leg of a COMMITTED group may be
   deleted alone; the `transactions` row stays COMMITTED with that leg DELETED —
   `COMMITTED` means every leg was applied or confirmed *at decision time*. Deleting a
   whole group is simply a group of delete items.
5. `config.allow_deletes` (default true, hot-reloaded, independent of `allow_edits`)
   refuses new delete registrations with `ErrDeletesDisabled`; registered ones are
   still decided.

## Consequences

- Clients get `DELETE /operations/{id}` and `delete_of` items with stable ids and a
  complete trail; mixed groups (new + edit + delete) come for free from the group
  machinery.
- G1, G2, G3, G5, G6 unchanged; the guards are reused verbatim plus one CAS write on
  the target, as for edits. G4 unaffected: a deleted operation leaves every CONFIRMED
  read but keeps its `(effective_at, id)` position in the row for audit views.
- CLAUDE.md inviolable #2 and spec Principle 5 are widened once more: a regular row's
  status flips *twice at most* (`PENDING → CONFIRMED → DELETED`), and `deleted_by`,
  `deleted_at` join the columns only the leader writes — exactly once, under the CAS.
  After deletion `account_id`, `amount`, `effective_at` and `revision` never change
  again (Safety Invariant 7).
- `TARGET_NOT_EDITABLE` now covers two target states (INVALID, DELETED); clients read
  the target to tell them apart. N7 extends to deletes: reversals, edits and deletes
  never track each other; a reversal of a DELETED operation is an ordinary operation,
  and `idx_ops_reversal` (`status <> 'INVALID'`) intentionally keeps a DELETED
  reversal "live" — deleting a reversal does not restore its original, so a second
  live reversal of that original must still be blocked.
- Migration `0003_operation_deletes.sql` is additive (`is_delete`, `deleted_by`,
  `deleted_at`, two CHECKs, `config.allow_deletes`). The simulation reference model
  gains delete registration, decisions and the invariants "a DELETED row has exactly
  one APPLIED delete" / "a DELETED row never reads CONFIRMED again" in the same commit
  series as the processor change; the DB fidelity tier runs single, grouped and mixed
  deletes in the action mix and compares statuses (DELETED included), current
  columns, `is_delete`, `deleted_by`/`deleted_at`, revisions and balances. `apiVersion` becomes 1.2.0; `oasdiff` must report no breaking change (all
  additions; `DELETED` is a new value of a status documented as free text).
- Risks: a stale leader deleting after a concurrent edit is caught by the CAS (the edit
  bumped `revision`) and Guard 3; a concurrent delete apply is caught by the CAS's
  `status = 'CONFIRMED'`. Reads keyed on `status <> 'INVALID'` rather than
  `= 'CONFIRMED'` were audited: only `idx_ops_reversal`, behavior kept on purpose
  (above). Hash drift: only items with `delete_of` use the new canonical form
  (golden-hash unit test).

Invariants preserved: every processor transaction that decides a delete runs Guard 1,
Guard 2 (on the delete row and, for a group, the transaction row) and Guard 3 per
involved account, each rowcount-checked, plus the delete CAS on the target; any miss
rolls the whole batch back. No code path physically deletes from `operations` or
`operation_revisions` (a source-level test scans every non-test Go file), and
`status = 'DELETED'`, `deleted_by`, `deleted_at` are written only by
`internal/processor`. A delete row is never CONFIRMED; it flips `PENDING → APPLIED |
INVALID` once. A regular operation's `id`, `transaction_id`, `reversal_of`,
`registered_at` are never changed by a delete; only `status`, `deleted_by` and
`deleted_at` change, and only together.

## References

- `docs/architecture-spec.md` §3, §4.1–§4.2, §5.2–§5.4, §6, §7.2, §8, §9, §10, §12, §13
- ADR-0005 (probe-and-insert reused by delete rows), ADR-0008 (why a target can still be
  PENDING when a delete is reached), ADR-0010 (edits as registrations — the pattern this
  ADR extends)
- `.compozy/tasks/operation-deletion/_spec.md` (feature spec; this ADR was drafted there
  as ADR-001)
