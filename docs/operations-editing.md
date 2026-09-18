# Editing and deleting operations

HTTP reference for in-place edits (ADR-0010) and deletes (ADR-0011). Embedded Go
hosts use the same registrations through `InsertOp.EditOf` / `InsertOp.DeleteOf`
([embedding guide](embedding.md)).

## Editing operations (ADR-0010)

A confirmed operation can be corrected **in place** — same id, new `amount`,
`effective_at` and/or `account` (same owner) — with an append-only history. An edit
is a *registration* like an insert: it enters the same PENDING queue, is decided by
the leader (validated against the final-balance limits as `−old` + `+new`, netted per
account), and is idempotent under the same `Idempotency-Key` rules. `revision`
starts at 1 and increments with every applied edit; registration id, owner,
`transaction_id`, `reversal_of` and `registered_at` are never editable
(`decisions/0010-operation-editing.md`, spec §10.3).

```sh
# One edit: fix -15.00 to -12.00 and wait for the decision (200 = decided).
curl -s -X PATCH localhost:8080/operations/41 \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' \
  -d '{"amount": -1200, "expected_revision": 1, "wait_ms": 10000}'
# {"edit":{"id":57,"status":"APPLIED","operation_id":41},"replayed":false}

# Grouped / mixed: edits are POST /transactions items with edit_of — one atomic
# unit, one owner, max_group_size items, all applied or all rejected.
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"edit_of": 41, "amount": -1200},
      {"edit_of": 42, "effective_at": "2026-02-10T09:30:00Z"},
      {"account":"cash","amount":300,"effective_at":"2026-03-01T10:00:00Z"}
    ], "wait_ms": 10000 }'

# History: every superseded revision (oldest first, last = current) plus the
# pending and rejected edits — nothing is ever updated or deleted.
curl -s localhost:8080/operations/41/history -H 'X-Owner-Id: 1'
```

- **Statuses.** An edit registration goes `PENDING → APPLIED | INVALID` (terminal);
  the target stays `CONFIRMED` and only its current columns, `revision` and
  `revised_at` change. Rejections are `LIMIT_VIOLATED` (as for inserts),
  `TARGET_NOT_EDITABLE` (the target ended INVALID or is DELETED) or `STALE_REVISION`
  (`expected_revision` did not match at decision time). A group with any failing
  item is `REJECTED` as a whole; `COMMITTED` means every new item CONFIRMED and every
  edit item APPLIED.
- **Reads.** `GET /operations/{id}` returns the current state and `revision` (an edit
  registration id returns `edit_of` and the proposed state); statement entries carry
  `revision`, `GET /transactions/{id}` legs carry `revision` (regular legs) or
  `edit_of` (edit legs); `GET /operations/{id}/history` is the audit trail (`404`
  for an edit registration id). Edit registrations never
  appear in statements, balances or point-in-time sums.
- **Turning it off.** `UPDATE config SET allow_edits = false;` — hot-reloaded, so the
  next edit registration is refused with `403` (embedded: `ErrEditsDisabled`);
  edits already registered are still decided.
- **Non-guarantee N7.** Edits and reversals do not track each other: editing an
  operation never adjusts a reversal that points at it, and editing a reversal never
  adjusts its original — the client owns the follow-up. Reverse to cancel, edit to
  correct, delete to remove (next section).
- **Embedded hosts** register edits through the same `InsertOp.EditOf` /
  `ExpectedRevision` fields inside their own transaction
  ([embedding guide](embedding.md#edit-an-operation-in-a-business-transaction)).

## Deleting operations (ADR-0011)

A confirmed operation can be **deleted**: same id, terminal status `DELETED`, last
values kept on the row with `deleted_at` and `deleted_by` stamped — nothing is ever
physically removed, so the audit trail stays complete. A delete is a *registration*
exactly like an edit: it enters the PENDING queue, is decided by the leader as one
virtual leg (`−current` on the target's account, validated against the final-balance
limits — removing a spent credit is a debit without funds), and is idempotent under
the same `Idempotency-Key` rules. `revision` is not bumped and no history row is
appended; the row's last values *are* its final state
(`decisions/0011-operation-deletion.md`, spec §10.4).

```sh
# One delete, guarded by the revision the client last read, waiting for the decision.
curl -s -X DELETE 'localhost:8080/operations/41?expected_revision=2&wait_ms=10000' \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)"
# {"deletion":{"id":63,"status":"APPLIED","operation_id":41},"replayed":false}

# Grouped / mixed: deletes are POST /transactions items with delete_of (nothing
# else on the item) — they mix freely with edits and new operations in one unit.
curl -s localhost:8080/transactions \
  -H 'X-Owner-Id: 1' -H "Idempotency-Key: $(cat /proc/sys/kernel/random/uuid)" \
  -H 'Content-Type: application/json' -d '{
    "operations": [
      {"delete_of": 42},
      {"delete_of": 43, "expected_revision": 1},
      {"edit_of": 44, "amount": -500},
      {"account":"cash","amount":300,"effective_at":"2026-03-01T10:00:00Z"}
    ], "wait_ms": 10000 }'

# The operation now reads DELETED with its last values; its history lists the
# revisions plus every edit *and* delete registration, each with a kind.
curl -s localhost:8080/operations/41 -H 'X-Owner-Id: 1'
curl -s localhost:8080/operations/41/history -H 'X-Owner-Id: 1'
```

- **Statuses.** A delete registration goes `PENDING → APPLIED | INVALID` (terminal);
  on APPLIED the target flips `CONFIRMED → DELETED` (terminal — there is no restore,
  re-insert instead). Rejections are `LIMIT_VIOLATED`, `TARGET_NOT_EDITABLE` (the
  target ended INVALID or is already DELETED — decided by the leader, never refused
  at submission, so a client handles it in one place) or `STALE_REVISION`. A group is
  all-or-nothing: `COMMITTED` means delete and edit items APPLIED and new items
  CONFIRMED; `REJECTED` touches no target.
- **Groups are not deletion units.** A leg of a COMMITTED group can be deleted alone;
  the transaction stays `COMMITTED` and that leg reads `DELETED` among its siblings.
  Deleting a whole group is just a group of `delete_of` items.
- **Reads.** A DELETED operation leaves every balance, statement and point-in-time
  sum (its `−current` leg was applied) but stays readable by id: `GET /operations/{id}`
  returns `DELETED`, its last values, `deleted_at` and `deleted_by`; a delete
  registration id returns `delete_of`; `GET /transactions/{id}` legs carry
  `delete_of` (delete legs), `edit_of` (edit legs) or `revision` (regular legs).
- **`expected_revision`** is a query parameter (`>= 1`); `0` or a non-integer is the
  schema's `422`, unlike the body-path `400` of `PATCH` — deliberate, both are
  client errors before any DB access.
- **Turning it off.** `UPDATE config SET allow_deletes = false;` — hot-reloaded and
  independent of `allow_edits`; the next delete registration is refused with `403`
  (embedded: `ErrDeletesDisabled`); deletes already registered are still decided.
- **Non-guarantee N7** extends to deletes: deleting an operation never adjusts a
  reversal that points at it, deleting a reversal never restores its original, and a
  reversal of a DELETED operation is an ordinary operation.
- **Embedded hosts** register deletes with `InsertOp.DeleteOf` (plus optional
  `ExpectedRevision`) and read `OperationOutcome.DeleteOf` / `DeletedAt` / `DeletedBy`
  ([embedding guide](embedding.md#delete-an-operation-in-a-business-transaction)).

