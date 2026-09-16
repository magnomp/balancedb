# ADR-0010 — Edits are registrations decided by the leader; facts become append-only revisions

Status: accepted (user direction 2026-09-15: "enable operation editing, keep an
append-only edition history, support groups with the same implications as new
operations"; accepted with the first processor commit that applies an edit)

## Context

Spec Principle 5 and CLAUDE.md inviolable #2 said operations are never mutated after
their facts are set; corrections are reversals (§5.4). The user's clients are mostly
not bank-grade and find reversal bookkeeping a burden; they want in-place updates. The
user still wants auditability (append-only history) and wants grouped edits to behave
exactly like grouped inserts (one owner, `max_group_size`, all-or-nothing, net per
account).

Forces: G1 (final balance never outside limits) must survive edits; G2 atomicity must
extend to grouped edits; the three processor guards and the single-writer model must
stay intact; reads (§9) rely on `operations` rows with status CONFIRMED and on daily
cumulative snapshots; idempotency hashes of existing keys must not change on upgrade.

Options considered:

- **Separate `edits`/`edit_items` tables with their own id sequence.** `operations`
  stays "operations only", but two id sequences cannot be merged into one registration
  order without a shared sequence, and idempotency, doorbell, wait keys, replay and
  group machinery would all be duplicated. Rejected: it doubles the surface to protect
  for no contract benefit.
- **Fully append-only rows** (each edit inserts a replacement operation, the old row
  becomes terminal SUPERSEDED). No column of any row is ever overwritten except status,
  but the client's id changes on every edit — exactly the id-chain bookkeeping the user
  wants removed — and reversal links, group membership and statement cursors point at
  stale ids. Rejected: stable identity is the point of "edit" versus "reverse".
- **Mutate in place synchronously in the API** under the account version CAS. One round
  trip, but it breaks the single-writer model (§3.2): the API would write balances and
  snapshots concurrently with the leader, grouped edits would need multi-account locking
  in the API, and G1 would rest on API-side races. Rejected: it moves correctness off the
  one serialization point.

## Decision

1. **An edit is a registration row in `operations`** with `edit_of = <target id>` and
   the full proposed state (`account_id`, `amount`, `effective_at`; omitted fields are
   resolved from the target's current values at submission). It draws its id from the
   same identity sequence, sits in the same PENDING work queue, is grouped by the same
   `transactions` row, and carries the same idempotency columns. Nothing new is needed
   for ordering (G3), doorbell, waiting, or replay.
2. **The leader decides an edit as two virtual legs**: `(−current_amount on
   current_account at current_effective_at)` and `(+new_amount on new_account at
   new_effective_at)`. Groups net all virtual legs of all items per account and apply
   the existing §6 check, Guard 3 CAS per account, and `snapshot.Apply` per virtual leg
   (two same-bucket legs of one edit are coalesced into one apply, or none for a zero
   delta — an arithmetic identity on the daily cumulative balances). On accept, the
   edit row flips `PENDING → APPLIED` (Guard 2), the superseded state is appended to
   `operation_revisions`, and the target row's current columns are overwritten under a
   **revision CAS** (`WHERE id = target AND revision = read_revision AND status =
   'CONFIRMED'`, rowcount 1 else guard miss). On reject it flips to INVALID with
   `LIMIT_VIOLATED`, `TARGET_NOT_EDITABLE` (target INVALID) or `STALE_REVISION`
   (`expected_revision` mismatch). APPLIED and INVALID are terminal. An edit whose
   target is still PENDING at decision time is *deferred*: skipped, left PENDING,
   counted (`balancedb_edit_deferrals_total`), decided in a later cycle once the target
   is — never blocking the work behind it.
3. **Facts are append-only; the `operations` row is the current projection.** The
   immutability inviolable is restated: an operation's *history* (`operation_revisions`
   plus its edit registrations) is never mutated or deleted; the `operations` row of a
   CONFIRMED operation may be overwritten only by the leader applying an edit under the
   revision CAS. Registration id, `transaction_id`, `reversal_of`, `registered_at` are
   immutable forever. G4's timeline key stays `(effective_at, id)`; a confirmed edit
   moves the operation to its new `effective_at` keeping its id.
4. Edit registrations never carry status CONFIRMED, so every existing read (statement,
   point-in-time sums, G1 balance) excludes them without query changes.
5. A hot-reloaded `config.allow_edits` (default true) lets an operator keep a cell on
   the original contract; already registered edits are still decided.

## Consequences

- Clients get `PATCH /operations/{id}` with a stable id and full audit history; grouped
  edits are literally groups, so mixed groups (new + edit) come for free.
- G1, G2, G3, G5, G6 unchanged; the guards are reused verbatim plus one revision CAS.
  G4 gains the "moved by a confirmed edit" clause; a new non-guarantee N7 records that
  editing an operation never touches a reversal that points at it, and vice versa.
- CLAUDE.md inviolable #2 and spec Principle 5 are weakened from "never mutated" to
  "history append-only; current projection overwritten only by the leader's edit
  apply". `operations` holds two kinds of rows; every read that selects by status must
  keep excluding non-CONFIRMED rows (they already do).
- Migration `0002_operation_edits.sql` is additive (new columns with defaults, one new
  table, one partial index, three CHECKs, one config column). No applied migration is
  edited. The simulation reference model gains edit registration and decisions in the
  same commit series as the processor change; the DB fidelity tier compares current
  rows and statuses (APPLIED included) and, with grouped edits, `operation_revisions`.
- The HTTP contract (ADR-0003) is additive — `PATCH /operations/{id}`,
  `GET /operations/{id}/history`, new response fields, `apiVersion` 1.1.0 — with one
  deliberate schema relaxation: `Rejection.account/limit_side/shortfall` become
  optional because the new codes (`TARGET_NOT_EDITABLE`, `STALE_REVISION`) do not carry
  them. `LIMIT_VIOLATED` rejections are byte-identical to before, so no existing flow
  changes; `oasdiff breaking` reports the relaxation as
  `response-property-became-optional` once, against the pre-edit baseline, and that
  report is accepted. Every decided edit row (APPLIED or INVALID) stamps `confirmed_at`
  as its decision instant so the history endpoint can report `decided_at`; rejected
  regular operations keep `confirmed_at` NULL as before. On `POST /transactions`
  the item schema leaves `account`, `amount` and `effective_at` optional so edit
  items can omit them; the handler re-imposes all three on a non-edit item with a
  `400` (previously the schema itself refused a missing field with `422`) — a
  status change only for malformed requests, before any database access.
- Risks: snapshot arithmetic on effective_at moves across days (mitigated by reusing
  `snapshot.Apply` per virtual leg and asserting DB == reference in simulation);
  idempotency hash drift (legacy payloads keep the byte-identical encoding, only items
  with `edit_of` use the extended canonical form — golden-hash unit test); editing
  INVALID operations stays impossible (terminal; clients re-submit).

Invariants preserved: every processor transaction that decides an edit runs Guard 1,
Guard 2 (on the edit row and, for a group, the transaction row) and Guard 3 per
involved account, each rowcount-checked, plus the revision CAS on each target; any miss
rolls the whole batch back. `operation_revisions` rows are inserted only by the leader's
apply path and never updated or deleted; `(operation_id, revision)` is dense from 1 to
`revision − 1`. A regular operation's `id`, `transaction_id`, `reversal_of`,
`registered_at` and decided `status` are never changed by an edit; only `account_id`,
`amount`, `effective_at`, `revision`, `revised_at` change, and only together.

## References

- `docs/architecture-spec.md` §3, §4.1, §5.2–§5.4, §6, §7.2, §8, §9, §10
- ADR-0005 (probe-and-insert reused by edit rows), ADR-0007, ADR-0008 (why a target can
  still be PENDING when an edit is reached)
- `.compozy/tasks/operation-editing/_spec.md` (feature spec; this ADR was drafted there
  as ADR-001)
