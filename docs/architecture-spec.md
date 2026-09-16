# BalanceDB — Architecture Specification

**Final revision — 2026-08-19**

**Contract refinement — 2026-09-13:** [ADR-0008](decisions/0008-caller-controlled-insertion-transactions.md)
limits G3 to ID-ordered selection of committed work visible to each work query;
overlapping insertion transactions may change decision order and acceptance.

**Contract refinement — 2026-09-15:** [ADR-0010](decisions/0010-operation-editing.md)
adds in-place editing: an edit is a registration row decided by the leader as two
virtual legs; the target's current columns are overwritten under a revision CAS and
every superseded state is appended to `operation_revisions`. Principle 5 becomes
"history append-only", G4 gains the "moved by a confirmed edit" clause, N7 is added,
and §5.2–§5.4, §7.2 and §8 gain the edit rules.

**Contract refinement — 2026-09-16:** [ADR-0011](decisions/0011-operation-deletion.md)
adds deletion: a delete is an edit-class registration row (`edit_of` + `is_delete`)
decided by the leader as one virtual leg (`−current`); the target flips
`CONFIRMED → DELETED` under the revision CAS with `deleted_by`/`deleted_at`, keeps its
last values and revision, and no row is ever physically deleted. Principle 5 gains the
DELETED terminal, N7 extends to deletes, `TARGET_NOT_EDITABLE` covers DELETED targets,
and §5.2–§5.4, §7.2, §8.2, §8.3, §9, §10, §12 and §13 gain the delete rules (§10.4 is
the HTTP surface, §8.3 the grouped decision — delete legs in any mix, group
membership not a deletion unit).

---

## 1. Purpose

BalanceDB is a balance-maintenance engine. Its single job is to keep account balances correctly computed over time, given an immutable stream of operations — including **backdated** and future-dated operations, its distinguishing requirement. Target uses: ledgers, banking systems, budgeting/financial assistants (including envelope systems), ERP inventory.

It is **asynchronous and eventually consistent**: insertion is decoupled from processing; an operation is typically decided within about one polling cycle (~1 s by default).

Out of scope: scheduling, currency conversion, authentication.

## 2. Architecture Overview

The deployment unit is a **cell**. A cell serves a disjoint set of owners (users/tenants) and is fully independent of every other cell.

```

                        ┌─ directory: owner → cell ─┐

 clients ──► API nodes ─┤                           │

             (N, stateless)                         ▼

                        ┌───────────── cell ─────────────┐

                        │  PostgreSQL  ◄── Processor      │

                        │  (sole store &   (1 active +    │

                        │   coordinator)    standby)      │

                        └────────────────────────────────┘

                          ... more cells, added linearly ...

```

- **API nodes** — any number, stateless. Insert operations, answer queries, wait on outcomes. Route each request to the owner's cell via the directory.

- **Processor** — exactly one active per cell (leader lease, §7), plus standby instances for failover. Selects visible committed pending work in registration ID order (G3, ADR-0008).

- **PostgreSQL** — one per cell; the sole authoritative store *and* the coordination medium. The per-cell throughput ceiling.

- **Directory** — a small `owner → cell` mapping (a table; cacheable). Using an explicit directory (not hashing) makes moving an owner between cells an ordinary per-owner data migration plus a directory update.

**The sharding invariant (load-bearing):** *all operations of a group belong to accounts of one owner.* Enforced at the API. Because no group ever crosses a cell, cells share nothing — no distributed transactions, no cross-cell reads — and system capacity scales linearly by adding cells. Any future feature that would atomically span two owners (e.g., user-to-user transfers) violates this invariant and requires a deliberate design decision (application-level sagas, or a cross-cell commit protocol); it must not be improvised.

## 3. Design Principles

1. **The database is the serialization point.** All authoritative state and all coordination (leader lease, outcomes) live in the cell's ACID database. Processes around it are stateless and disposable.

2. **Single writer per cell.** One processor performs all balance writes, making all cross-account logic — including atomic groups — plain sequential arithmetic inside ordinary DB transactions.

3. **Correctness never depends on coordination.** The leader lease makes double-processing *rare*; conditional (optimistic) writes make it *harmless*. Coordination failures degrade latency, never correctness.

4. **Facts are written once; derivations are computed on read.** No back-pointers, no flags on existing rows, no stored values a query can derive.

5. **Immutability of history (ADR-0010, ADR-0011).** An operation's history is append-only: its registration id, `transaction_id`, `reversal_of` and `registered_at` never change, its status only moves forward (`PENDING → CONFIRMED → DELETED` or `PENDING → INVALID`, at most two flips), and every superseded state is kept forever in `operation_revisions`. The `operations` row is the *current projection*: a CONFIRMED operation's `account_id`, `amount` and `effective_at` may be overwritten only by the leader applying an edit registration under the revision CAS, in the same transaction that appends the superseded state; its `status`, `deleted_by` and `deleted_at` may be flipped to DELETED only by the leader applying a delete registration under the same CAS, once, with no revision appended — the row's last values are its final state and stay readable. Nothing is ever physically deleted from `operations` or `operation_revisions`. Cancellation is a new opposite operation (reversal), correction is an edit, removal is a delete.

## 4. Consistency Contract

Publish to client teams verbatim.

### 4.1 Guarantees

- **G1 — Final-balance invariant.** An account's *final balance* (sum of all CONFIRMED operations regardless of effective timestamp — the balance at the end of the timeline) never violates its configured `min_balance` / `max_balance`.

- **G2 — Group atomicity.** All operations of a group are confirmed together in one database transaction, or all are rejected together. Partial application is impossible, even transiently.

- **G3 — Ordered visible work (ADR-0008).** Each work query selects committed PENDING operations visible to that query in ascending registration ID order. A group is selected at its first leg. Across overlapping insertion transactions, this does not guarantee global decision order by ID or commit time: lower IDs committed after a work query may be decided after higher IDs it already fetched. Acceptance can depend on transaction visibility and processor timing. Sequential insertion commits retain ID-ordered acceptance.

- **G4 — Deterministic, immutable ordering.** Timeline order is `(effective_at, registration id)` — total, unique, fixed at insert. An operation moves on the timeline only when a confirmed edit changes its `effective_at` (ADR-0010); it keeps its registration id as the tiebreaker, so the order stays total and unique.

- **G5 — Eventual decision, no timeouts.** Every operation is eventually CONFIRMED or INVALID; nothing is ever aborted for taking too long.

- **G6 — Idempotent insertion.** Re-sending a request with the same idempotency key never duplicates operations.

### 4.2 Explicit non-guarantees

- **N1 — No decision deadline.** Typical latency is ~one polling cycle; it is unbounded under backlog.

- **N2 — Point-in-time balances may violate limits.** A backdated debit may permanently make history show a below-minimum balance at a past instant. With future-dated operations, "now" is just another timeline point with the same property. Only the **final** balance is protected.

- **N3 — Reversals may be refused.** A reversal is an ordinary operation validated against limits; reversing a spent credit is a debit without funds.

- **N4 — Intra-timestamp adjacency is not guaranteed** for reversals sharing the original's timestamp; end-of-instant balances are identical regardless.

- **N5 — Future-dated operations enter the final balance immediately upon confirmation** (and are invisible to point-in-time reads at "now"). BalanceDB does not schedule; the client inserts when the date arrives if scheduling semantics are wanted.

- **N6 — No global decision ordering across overlapping insertion transactions (ADR-0008).** Inserting at the end of a short transaction reduces the risk of a later ID being decided first, but cannot eliminate it. A late commit never revisits terminal rejections, including rejected groups. Balance limits, group atomicity, and `(effective_at, id)` timeline ordering remain enforced.

- **N7 — Reversals, edits and deletes do not track each other (ADR-0010, ADR-0011).** `reversal_of` is inert metadata: editing or deleting an operation never adjusts a reversal that points at it, and editing or deleting a reversal never adjusts its original. A reversal of a DELETED operation is an ordinary operation. A client that edits or deletes a reversed operation owns the follow-up.

## 5. Data Model

### 5.1 Conventions

- All amounts are **`BIGINT` in minor units**; floating point is forbidden everywhere. `amount > 0` credit, `< 0` debit.

- Groups are **not required to be zero-sum**: a group is an atomic *set* of operations, not a double-entry pair. (An envelope system's "debit physical account + debit envelope" group is a supported first-class case.)

- Accounts have an internal `id` (sequence) and a client-defined `external_id` (unique per owner). The API speaks external ids; the mapping is immutable and cacheable forever.

- Accounts are created explicitly (with limits) or on demand (upserted at insertion; `NULL` limits = unbounded).

### 5.2 Schema (PostgreSQL; per cell)

```sql

CREATE TABLE accounts (

  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  owner_id          BIGINT NOT NULL,           -- sharding key; all group legs share one owner

  external_id       TEXT   NOT NULL,

  min_balance       BIGINT NULL,               -- NULL = unbounded

  max_balance       BIGINT NULL,

  confirmed_balance BIGINT NOT NULL DEFAULT 0, -- FINAL balance (sum of all CONFIRMED ops)

  version           BIGINT NOT NULL DEFAULT 0, -- optimistic lock (processor vs API races)

  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

  UNIQUE (owner_id, external_id)

);

CREATE TABLE transactions (          -- group records; ONLY for groups (>= 2 operations)

  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

  idempotency_key UUID  NOT NULL UNIQUE,

  payload_hash    BYTEA NOT NULL,

  op_count        INT   NOT NULL,

  status          TEXT  NOT NULL DEFAULT 'PENDING',  -- PENDING | COMMITTED | REJECTED

  reject_reason   TEXT  NULL,                        -- LIMIT_VIOLATED + detail

  registered_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

  decided_at      TIMESTAMPTZ NULL

);

CREATE TABLE operations (

  id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

                      -- registration id: global insertion order; ordering tiebreaker

  account_id          BIGINT NOT NULL REFERENCES accounts(id),

  amount              BIGINT NOT NULL CHECK (amount <> 0),

  effective_at        TIMESTAMPTZ NOT NULL,   -- client-supplied; past AND future allowed

  transaction_id      BIGINT NULL REFERENCES transactions(id),  -- NULL for singles

  reversal_of         BIGINT NULL REFERENCES operations(id),

  status              TEXT NOT NULL DEFAULT 'PENDING',

                      -- PENDING | CONFIRMED | INVALID   (no intermediate states)

  invalidation_reason TEXT NULL,              -- LIMIT_VIOLATED + offending account/limit

  idempotency_key     UUID  NULL,             -- singles only (groups: on transactions)

  payload_hash        BYTEA NULL,             -- singles only

  registered_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

  confirmed_at        TIMESTAMPTZ NULL,       -- decision instant of CONFIRMED and APPLIED rows, and of INVALID edit rows

  -- Editing (ADR-0010, migration 0002):

  edit_of             BIGINT NULL REFERENCES operations(id),  -- set: this row is an edit registration of that operation

  expected_revision   INT    NULL,            -- optional optimistic guard on an edit row (>= 1)

  revision            INT    NOT NULL DEFAULT 1,  -- current revision of a regular operation (unused, 1, on edit rows)

  revised_at          TIMESTAMPTZ NULL,       -- when the current revision became current (NULL = never edited)

  CHECK (edit_of IS NULL OR reversal_of IS NULL),

  CHECK (edit_of IS NULL OR edit_of <> id),

  CHECK (expected_revision IS NULL OR expected_revision >= 1),

  -- Deletion (ADR-0011, migration 0003):

  is_delete           BOOLEAN NOT NULL DEFAULT FALSE,  -- set with edit_of: this row is a delete registration of that operation

  deleted_by          BIGINT NULL REFERENCES operations(id),  -- regular row: the APPLIED delete registration that removed it

  deleted_at          TIMESTAMPTZ NULL,       -- regular row: when it became DELETED (= the delete row's confirmed_at)

  CHECK (NOT is_delete OR edit_of IS NOT NULL),

  CHECK ((deleted_by IS NULL) = (deleted_at IS NULL))

);

CREATE INDEX idx_ops_work     ON operations (id) WHERE status = 'PENDING';   -- work queue

CREATE INDEX idx_ops_timeline ON operations (account_id, effective_at, id);  -- reads

CREATE INDEX idx_ops_tx       ON operations (transaction_id) WHERE transaction_id IS NOT NULL;

CREATE UNIQUE INDEX idx_ops_reversal ON operations (reversal_of)

  WHERE reversal_of IS NOT NULL AND status <> 'INVALID';  -- one live reversal per op;

                                                          -- a rejected reversal allows retry

CREATE UNIQUE INDEX idx_ops_idem ON operations (idempotency_key)

  WHERE idempotency_key IS NOT NULL;

CREATE INDEX idx_ops_edit_of ON operations (edit_of) WHERE edit_of IS NOT NULL;  -- "which edits target X" (reads only)

CREATE TABLE operation_revisions (   -- append-only: one row per SUPERSEDED state (ADR-0010)

  operation_id  BIGINT NOT NULL REFERENCES operations(id),

  revision      INT    NOT NULL,                 -- the revision being superseded

  account_id    BIGINT NOT NULL REFERENCES accounts(id),

  amount        BIGINT NOT NULL,

  effective_at  TIMESTAMPTZ NOT NULL,

  recorded_at   TIMESTAMPTZ NOT NULL,            -- when this revision became current

  superseded_by BIGINT NOT NULL REFERENCES operations(id),  -- the APPLIED edit row

  superseded_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (operation_id, revision)

);

CREATE TABLE balance_snapshots (

  account_id BIGINT NOT NULL,

  day        DATE   NOT NULL,     -- UTC bucket of effective_at (timezone fixed forever)

  balance    BIGINT NOT NULL,     -- CUMULATIVE balance at end of that day

  PRIMARY KEY (account_id, day)

);

CREATE TABLE leader_lease (       -- single row, created at cell setup

  singleton   BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),

  owner       UUID NULL,          -- processor instance UUID (generated at boot)

  lease_until TIMESTAMPTZ NULL

);

CREATE TABLE config (             -- single row; hot-reloaded every cycle

  singleton         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),

  lease_ttl_ms      INT NOT NULL DEFAULT 15000,

  loop_interval_ms  INT NOT NULL DEFAULT 1000,

  batch_size        INT NOT NULL DEFAULT 200,   -- decisions per DB commit (§8.4)

  max_group_size    INT NOT NULL DEFAULT 10,

  api_max_wait_ms   INT NOT NULL DEFAULT 30000,

  allow_edits       BOOLEAN NOT NULL DEFAULT TRUE,  -- FALSE refuses new edit registrations (ADR-0010)

  allow_deletes     BOOLEAN NOT NULL DEFAULT TRUE   -- FALSE refuses new delete registrations (ADR-0011), independent of allow_edits

);

```

### 5.3 Status state machines

```

Operation:    PENDING ──► CONFIRMED ──► DELETED (terminal, ADR-0011)    Transaction: PENDING ──► COMMITTED

                      └─► INVALID  (terminal)                                          └─► REJECTED (terminal)

Edit / delete registration (edit_of set; is_delete marks a delete — ADR-0010, ADR-0011):

              PENDING ──► APPLIED  (terminal)

                      └─► INVALID  (terminal)

```

`INVALID` / `APPLIED` / `DELETED` / `REJECTED` are terminal; recovery is client re-submission (there is no restore of a DELETED operation — re-insert instead). All operations of a group flip together with their transaction row, in one DB transaction (`COMMITTED` means every regular leg was CONFIRMED and every edit or delete leg APPLIED *at decision time*; a regular leg may be DELETED later by its own delete registration — group membership is not a deletion unit). An edit or delete registration is never CONFIRMED and a DELETED row is no longer CONFIRMED, so every read that selects CONFIRMED rows excludes both without a query change. Applying an edit bumps the target's `revision` and sets `revised_at`; applying a delete flips the target to DELETED and stamps `deleted_by`/`deleted_at`, leaving its values and `revision` untouched — the only post-decision changes a regular row ever sees.

### 5.4 Ordering and reversals

Timeline order is the composite key `(effective_at, id)` — deterministic, unique, free (the registration id already exists at insert). There is no rank generation, no linked list, and **no per-operation running-balance column**: running balances are derived on read (§9), which is what makes backdating cheap (§8.4).

A reversal is an ordinary operation with the opposite amount, typically the original's `effective_at`, and `reversal_of` set at insertion — immutable metadata, unused by the engine. Nothing is ever written to the reversed operation; "was it reversed?" is derived by querying the index. The partial unique index blocks double reversal while allowing a retry after a *rejected* reversal.

**Edits (ADR-0010).** An edit is a registration row with `edit_of = <target>` carrying the full proposed state (`account_id`, `amount`, `effective_at`; omitted fields are resolved from the target's current row at submission, so the hash covers the request as sent) and an optional `expected_revision`. The target must exist, belong to the same owner, and be a regular operation; one request may edit a given operation at most once; `reversal_of` is never set on an edit. Registration id, owner, `transaction_id`, `reversal_of` and `registered_at` are never editable. Editing and reversing are independent (N7): a client corrects by editing and cancels by reversing. When an applied edit changes `effective_at` the operation moves to its new timeline position with its original id as the tiebreaker (G4). `config.allow_edits = false` refuses new edit registrations; edits already registered are still decided.

**Deletes (ADR-0011).** A delete is an edit-class registration row with `edit_of = <target>` and `is_delete = TRUE`, carrying only its target and an optional `expected_revision` (the row's `account_id`, `amount`, `effective_at` are an informational copy of the target at submission that the leader never reads; the hash covers `owner`, `delete_of`, `expected_revision`). The same target rules as an edit apply at submission — exists, same owner, a regular operation, named at most once per request across edits and deletes — and the target's *status* is deliberately not checked there: a DELETED, INVALID or PENDING target is a decision-time outcome. A DELETED operation keeps its `(effective_at, id)` position on its row for audit views but is excluded from every CONFIRMED read; a reversal of a DELETED operation is an ordinary operation, and `idx_ops_reversal` keeps a DELETED reversal "live" (its predicate is `status <> 'INVALID'`) because deleting a reversal does not restore its original. `config.allow_deletes = false` refuses new delete registrations independently of `allow_edits`; deletes already registered are still decided.

## 6. Balances & Validation

Because a cell has a single processor and groups apply atomically, **there are never in-flight groups during validation**. Two balance notions exist:

- **Final balance** — `accounts.confirmed_balance`, O(1); the object of G1.

- **Balance at instant T** — derived on read (§9); a projection, not covered by G1 (N2).

**Validation is binary.** For a candidate with net amount `v` on an account with limits `[min, max]`:

```

ACCEPT  iff  min <= confirmed_balance + v <= max      (unbounded sides skipped)

```

For a group: compute the **net per account** (sum of the group's legs on that account) and test every involved account. All pass → the whole group commits; any fail → the whole group rejects with `LIMIT_VIOLATED` plus the offending account and shortfall. Per-account netting is sound precisely because all legs apply in one DB transaction (G2).

`effective_at` plays no role in validation: an operation's effect on the *final* balance is its amount, wherever it lands on the timeline — so the current-balance check is exactly G1. This is also why backdated inserts need no special validation.

**Limit changes** (mutable account configuration, written directly by the API): a new `min` is accepted only if `min <= confirmed_balance`; a new `max` only if `confirmed_balance <= max`. Optimistically guarded: `UPDATE ... WHERE version = :read_version`, retry on conflict — the same `version` the processor bumps, so API-vs-processor races are detected on either side. Pending operations are validated later against whatever limits hold at their processing time (consistent with G3).

## 7. Leader Election & Safety Guards

### 7.1 Leader lease

Every processor instance generates a UUID at boot and runs the same loop:

```sql

UPDATE leader_lease SET owner = :me, lease_until = now() + :ttl

 WHERE owner = :me OR owner IS NULL OR lease_until < now();

```

Row updated → leader this cycle: process work. Not updated → standby: sleep one interval, retry. Graceful shutdown sets `owner = NULL`; crash is covered by TTL expiry (failover ≈ `lease_ttl`, default 15 s). All time comparisons use the **database clock** — node clocks are never trusted. Do not tune the TTL below several loop intervals, or a long GC pause costs leadership needlessly.

### 7.2 Safety guards

The lease makes double-processing *rare*; the guards make it *harmless*. Residual scenario: a leader checks its lease, stalls past expiry (GC/VM pause), and writes after a standby took over — without guards, a silent lost update on a balance, the one unforgivable failure in a ledger. Every processing transaction carries three guards; any guard matching 0 rows ⇒ `ROLLBACK`, re-acquire lease, continue:

```sql

BEGIN;

-- Guard 1: lease fence (DB clock)

SELECT 1 FROM leader_lease WHERE owner = :me AND lease_until > now();   -- empty? rollback

-- Guard 2: conditional status flips (idempotent retry & crash recovery)

UPDATE operations SET status = 'CONFIRMED', confirmed_at = now()

 WHERE id = ANY(:ops) AND status = 'PENDING';         -- rowcount mismatch? rollback

-- Guard 3: account version (closes the fence-to-commit window; arbitrates API races)

UPDATE accounts SET confirmed_balance = confirmed_balance + :net, version = version + 1

 WHERE id = :acct AND version = :version_read;         -- 0 rows? rollback

-- ... snapshot updates ...

COMMIT;

```

Deciding an edit registration (ADR-0010) keeps all three guards — Guard 2 flips the edit row `PENDING → APPLIED` and Guard 3 runs per involved account with the net of the edit's virtual legs — and adds a fourth rowcount-checked write on the target, the **revision CAS**:

```sql

UPDATE operations SET account_id = :new, amount = :new, effective_at = :new,

                      revision = revision + 1, revised_at = now()

 WHERE id = :target AND revision = :revision_read AND status = 'CONFIRMED';   -- 0 rows? rollback

```

Deciding a delete registration (ADR-0011) keeps the same three guards — Guard 2 flips the delete row `PENDING → APPLIED`, Guard 3 runs on the target's account with the net of the one virtual leg — and its fourth rowcount-checked write is the **delete CAS**, the same predicate with a different payload:

```sql

UPDATE operations SET status = 'DELETED', deleted_by = :delete_id, deleted_at = now()

 WHERE id = :target AND revision = :revision_read AND status = 'CONFIRMED';   -- 0 rows? rollback

```

A concurrent edit apply bumped `revision`, a concurrent delete apply left the row non-CONFIRMED: either way the CAS matches 0 rows and the whole batch rolls back. `revision` is not bumped and no revision row is appended by a delete.

## 8. Processing Algorithm

### 8.1 Main loop (leader only)

```

every loop_interval:

  re-read config; renew lease

  work = SELECT * FROM operations WHERE status = 'PENDING' ORDER BY id LIMIT :chunk

  for op in work:

    if op.transaction_id IS NULL: process_single(op)

    else:                         process_group(op.transaction_id)

                                  -- processed at its FIRST leg by registration order;

                                  -- remaining legs of a decided group are skipped by status

  emit NOTIFY for each outcome (inside its commit)

```

Work selection orders the committed rows visible to each query by registration ID (G3, ADR-0008). Uncommitted lower IDs cannot be seen or waited for; later commits do not reorder already fetched work or revisit terminal decisions. Clients should insert immediately before committing a short transaction, without treating that advice as a global ordering guarantee.

### 8.2 Single operation

One DB transaction: read account (balance, version, limits) → binary validation → ACCEPT: apply (§8.4) and flip to CONFIRMED; REJECT: flip to INVALID with reason. No other states exist.

**Edit registration (ADR-0010).** Same transaction shape, with the item expanded into two *virtual legs*. Read the target (`account_id, amount, effective_at, status, revision, registered_at, revised_at`):

- target `PENDING` → the unit is **deferred**: skipped, left PENDING, counted (`balancedb_edit_deferrals_total`), and the batch continues with the next unit; the next select of the same drain starts past it so a deferred head-of-queue never starves later work, and the next cycle decides target and edit in id order. Only reachable when a work query sees an edit whose target it has not decided (overlapping insertion transactions, ADR-0008).
- target `INVALID` or `DELETED` (ADR-0011) → REJECT with `TARGET_NOT_EDITABLE{operation_id}`.
- `expected_revision` set and ≠ `revision` → REJECT with `STALE_REVISION{operation_id, expected_revision, actual_revision}`.
- otherwise the legs are `(−current_amount, current_account, current_effective_at)` and `(+new_amount, new_account, new_effective_at)`, netted per account; every involved account is read and validated in ascending id order (§6), the first violation rejecting with `LIMIT_VIOLATED`.

ACCEPT: flip the edit row `PENDING → APPLIED` (Guard 2); append the superseded state to `operation_revisions` with `recorded_at = COALESCE(target.revised_at, target.registered_at)` and `superseded_by = <edit id>`; overwrite the target under the revision CAS (§7.2); apply each account's net under Guard 3; update snapshots per virtual leg with same-bucket coalescing (§8.4); NOTIFY `op:<edit id>`. REJECT: flip the edit row to INVALID with the reason — no balance, snapshot or revision write, the target untouched. Two edits of one target in one batch are decided sequentially in the same transaction; the second reads the first's result, so its CAS holds.

**Delete registration (ADR-0011).** The same transaction shape with the item expanded into *one* virtual leg. Read the target exactly as for an edit; the same three target rules apply verbatim (`PENDING` → deferred, counted on the shared `balancedb_edit_deferrals_total`; `INVALID` or `DELETED` → `TARGET_NOT_EDITABLE{operation_id}`; a mismatched `expected_revision` → `STALE_REVISION`); otherwise the leg is `(−current_amount, current_account, current_effective_at)` — the values as they stand when the leader decides, never the delete row's informational copy — and the account is read and validated (§6) against the limits in force at processing time, a violation rejecting with `LIMIT_VIOLATED`. ACCEPT: flip the delete row `PENDING → APPLIED` (Guard 2); flip the target `CONFIRMED → DELETED` under the delete CAS (§7.2) with `deleted_by = <delete id>`, `deleted_at = now()`; apply the account's net under Guard 3; exactly one `snapshot.Apply(−current_amount)` at the target's current instant (§8.4, no coalescing — there is no pair); NOTIFY `op:<delete id>`. No revision row is appended and `revision` is untouched. REJECT: flip the delete row to INVALID with the reason and its decision instant — nothing else written. A delete and an edit of one target in one batch are decided in id order: delete first leaves the edit `TARGET_NOT_EDITABLE`; edit first lets the delete remove the *edited* values.

### 8.3 Group

One DB transaction for the entire group: load all legs by `transaction_id` (the universe is complete — groups are inserted atomically, §10.1; cross-check `op_count`) → compute net per account → read and validate every involved account → all pass: apply every account's net (each under Guard 3), update snapshots for every leg, flip all legs to CONFIRMED and the transaction to COMMITTED; any fail: flip all legs to INVALID and the transaction to REJECTED with reason. **Group atomicity is simply the DB transaction** — there is no distributed protocol.

**Edit legs (ADR-0010).** A group may mix edit registrations with regular legs. Every edit leg's target is read as in §8.2; any target still PENDING defers the whole group (every leg stays PENDING, one deferral counted, the drain cursor moves past all its legs); then, in leg order, a target that ended INVALID or is not at the leg's `expected_revision` rejects the whole group with `TARGET_NOT_EDITABLE` / `STALE_REVISION` — every leg INVALID with that one reason, no target touched. Otherwise the net per account is taken over all *virtual* legs (one per regular leg, two per edit leg) and validated as above. ACCEPT: Guard 2 flips regular legs `PENDING → CONFIRMED` and edit legs `PENDING → APPLIED` as two conditional updates split by `edit_of IS NULL / IS NOT NULL`, each rowcount-checked against its own class's leg count (so together they cover every leg), then the transaction to COMMITTED; for each edit leg, in leg order, the history row is appended and the target overwritten under the revision CAS (two legs never share a target — refused at registration); Guard 3 runs once per involved account with its net; snapshots update per regular leg and per coalesced edit-leg pair (§8.4); NOTIFY `tx:<id>`. REJECT: the same split flips to INVALID (a rejected edit leg stamps `confirmed_at` as its decision instant, like a rejected single edit) and the transaction to REJECTED. Either every regular leg is CONFIRMED and every edit leg APPLIED, or every leg is INVALID.

**Delete legs (ADR-0011).** A group may also carry delete registrations, in any mix with edit and regular legs (two legs never target one operation — refused at registration). A delete leg is an edit-class leg: its target is read with the edit legs' (`selectGroupLegs` adds `is_delete`), the whole-group deferral and the leg-order `checkTarget` rules above apply verbatim (a DELETED target is `TARGET_NOT_EDITABLE`), and it expands to *one* virtual leg `(−current_amount, current_account, current_effective_at)` — the target as read in the deciding transaction, never the leg's informational copy — so the net per account runs over one leg per regular leg, two per edit leg and one per delete leg. ACCEPT: the Guard 2 split is unchanged — delete legs flip `PENDING → APPLIED` with the edit legs (`edit_of IS NOT NULL`), the rowcount still matching the edit-class count; then, per edit-class leg in leg order, an edit leg appends its history row and runs the revision CAS, a delete leg runs the delete CAS of §7.2 (`status = 'DELETED', deleted_by = <leg id>, deleted_at = now()` at the revision read, rowcount 1 or rollback), appending nothing and leaving `revision` untouched; Guard 3 runs once per involved account with its net; snapshots update per regular leg, per coalesced edit-leg pair and per delete leg as-is (one `snapshot.Apply(−current_amount)` at the target's current instant, no pair to coalesce); NOTIFY `tx:<id>`. REJECT: unchanged — the same split flips every leg to INVALID with the one shared reason (a rejected delete leg stamps `confirmed_at` like a rejected edit leg), the transaction to REJECTED, and no target is touched. Group atomicity now reads: either every regular leg is CONFIRMED, every edit leg APPLIED and every delete leg APPLIED (each target DELETED), or every leg is INVALID and no target is touched (Safety Invariant 6, ADR-0011). **Group membership is not a deletion unit**: deleting any leg of a COMMITTED group — alone or in a later group — leaves the transaction COMMITTED with its `op_count`; the deleted leg keeps its `transaction_id` and reads `DELETED` among its siblings.

### 8.4 Applying amounts — snapshots

For each confirmed leg: upsert the cumulative snapshot for the leg's `effective_at` UTC day, then:

```sql

UPDATE balance_snapshots SET balance = balance + :v

 WHERE account_id = :a AND day > :op_day;    -- sparse: only days that exist are touched

```

Cost: O(existing snapshot-days after the operation's day). The newest-timestamp common case touches zero rows (O(1)); a 30-day backdate touches ≤ 30 rows. Backdated and future-dated operations need no special code — the day arithmetic places them.

**Edits — same-bucket coalescing (ADR-0010).** An edit's two virtual legs are collapsed before the snapshot writes when they land in the same bucket, i.e. the same account and the same UTC day of `effective_at` (a time change within the day keeps the bucket): a non-zero `new − old` becomes one apply of the delta at the new instant, so the cascade touches each later day once instead of twice; a zero delta writes no snapshot row at all. Different account or different UTC day keeps the two-apply form (`−old` at the old bucket, `+new` at the new one), each cascading over its own day range. Coalescing is an arithmetic identity on the daily cumulative balances; the Guard 3 net on the account is unaffected.

### 8.5 Batching

Commit fsync (~2–5 ms) dominates service time. The leader packs up to `batch_size` decisions into one DB transaction (guards evaluated per decision). Safe because decisions are independent and idempotent under reprocessing: a crash loses the whole batch, which is simply reprocessed. 10–50× throughput.

## 9. Read Paths

- **Final balance**: `accounts.confirmed_balance`. O(1).

- **Balance at instant T**: last snapshot with `day < T::date` + sum of CONFIRMED operations of T's day with `(effective_at, id) ≤ T`. May violate limits (N2), including T = now() when future-dated operations exist (N5).

- **Statement with running balance**: page over `idx_ops_timeline` (CONFIRMED only), seeded by the snapshot preceding the page, prefix-summed within the page.

- **Deleted operations (ADR-0011)**: every read above selects `status = 'CONFIRMED'`, so a DELETED operation leaves the final balance (its `−current` leg was applied), the point-in-time sums and the statement without any query change; snapshots were moved by the same leg. The row itself stays readable by id (operation and transaction views, history) as `DELETED` with its last values, `deleted_by` and `deleted_at`.

- **Retention**: full history kept in the database indefinitely — no archival tier. If volume ever demands it, native time-based table partitioning applies without design changes.

## 10. HTTP API

API nodes are stateless; any node serves any request after directory lookup (`owner → cell`).

### 10.1 Insertion

```

POST /transactions

Idempotency-Key: <client-generated UUID>          (required)

{ "operations": [ { "account": "<external_id>", "amount": -1500,

                    "effective_at": "...", "reversal_of": <op_id>? }, ... ],

  "wait_ms": 0 }

-- an item may instead be an edit: { "edit_of": <op_id>, "expected_revision"?, "account"?, "amount"?, "effective_at"? }  (§10.3)
-- or a delete:                    { "delete_of": <op_id>, "expected_revision"? }  (§10.4)

```

- One request = one atomic unit. 1 operation → **single** (no transaction row; key stored on the operation). 2..`max_group_size` → **group** (transaction row + legs, one ACID insert). All legs must belong to **one owner** (the sharding invariant, §2) — enforced here.

- Accounts are resolved/upserted by `(owner, external_id)` in the same insert transaction. `effective_at` may be past or future. Amounts are bigint minor units. Groups need not be zero-sum.

- **Idempotency (Stripe model).** Retry reuses the key; a new business action uses a new key. The unique index arbitrates concurrent retries atomically; a replay returns the original's current state (probe `transactions`, then `operations`, by key) — never an error, never a duplicate. Same key with a different `payload_hash` → `422`. Keys are retained forever.

- **Waiting.** `wait_ms = 0` → immediate `202` (fire-and-forget). Otherwise the API waits via `LISTEN/NOTIFY` (`NOTIFY outcomes, 'tx:<id>' | 'op:<id>'`, emitted inside the deciding commit), with a 1–2 s status poll as durability fallback. Outcome → `200`; expiry (capped by `api_max_wait_ms`) → `202` with current state. **Sync mode means "wait up to T", never "guaranteed outcome".**

### 10.2 Queries & account management

```

GET /transactions/{id}      → group status + per-leg statuses

GET /operations/{id}        → status, current account/amount/effective_at, revision (edit rows:
                              edit_of, expected_revision, proposed state; delete rows: delete_of,
                              expected_revision, the target's values at submission; DELETED rows:
                              last values + deleted_at, deleted_by); if INVALID: reason + detail

GET /operations/{id}/history → append-only revisions (oldest first, last = current) + pending and
                              rejected edits and deletes, each with kind (ADR-0010, ADR-0011);
                              deleted_at/deleted_by once DELETED; 404 for an edit or delete
                              registration id

GET /accounts/{ext}/balance             → final balance

GET /accounts/{ext}/balance?at=T        → point-in-time projection (N2 applies)

GET /accounts/{ext}/statement?...       → paged timeline with derived running balance

POST /accounts                          → explicit creation with limits

PUT  /accounts/{ext}/limits             → §6 rules, version-guarded

```

Rejections always carry a machine-readable reason and detail — a budgeting UI can render "envelope short by 12.00" directly from the API.

### 10.3 Editing (ADR-0010)

```

PATCH /operations/{id}      { "amount"?, "effective_at"?, "account"?, "expected_revision"?, "wait_ms"? }

```

Registers one edit (`Idempotency-Key` required, same rules as §10.1); omitted fields are unchanged and at least one of the first three is required. Returns `{"edit": {id, status, operation_id, rejection?}, "replayed"}` — `202` immediately, or with `wait_ms > 0` the §10.1 wait on `op:<edit id>` → `200` with `APPLIED | INVALID`. Statement entries carry `revision`; edit registrations never appear in statements or balances. `403` when `config.allow_edits` is false; `404` for an unknown or another owner's target.

**Grouped and mixed edits.** A `POST /transactions` item with `edit_of` (plus optional `expected_revision`) is an edit item; `account`, `amount` and `effective_at` are then optional (omitted = unchanged, at least one required) and `reversal_of` is not allowed. Items without `edit_of` are new operations exactly as before — the handler refuses one missing `account`, `amount` or `effective_at` with `400`. One request is one atomic unit with the §10.1 rules (one owner, `max_group_size`, all-or-nothing, net per account, §8.3); two items may not edit the same operation (`422`); an unknown or foreign target is `422` in this context; `403` when `config.allow_edits` is false. Outcomes carry `edit_of` on edit items; `GET /transactions/{id}` legs carry `edit_of` (edit legs) or `revision` (regular legs). A decided group is `COMMITTED` (edit items `APPLIED`, new items `CONFIRMED`) or `REJECTED` (every item `INVALID` with one shared rejection).

### 10.4 Deleting (ADR-0011)

```

DELETE /operations/{id}?expected_revision=&wait_ms=      (no body)

```

Registers one delete (`Idempotency-Key` required, same rules as §10.1; the target and `expected_revision` are the payload, `wait_ms` is not). Returns `{"deletion": {id, status, operation_id, rejection?}, "replayed"}` — `202` immediately, or with `wait_ms > 0` the §10.1 wait on `op:<deletion id>` → `200` with `APPLIED | INVALID`. When applied the operation reads `DELETED` with its last values, `deleted_at` and `deleted_by` (`GET /operations/{id}`, `/history`, the group's legs) and leaves every statement, balance and point-in-time projection; the id never changes and nothing is physically removed. `expected_revision` (`>= 1`, schema-checked) makes the delete `STALE_REVISION` unless the operation is at that revision when decided; a target that ended `INVALID` or is already `DELETED` is `TARGET_NOT_EDITABLE` at decision time, never at submission. `403` when `config.allow_deletes` is false (independent of `allow_edits`); `404` for an unknown or another owner's target; `422` when the id is an edit or delete registration (`delete target 57 is an edit, not an operation`).

**Grouped and mixed deletes.** A `POST /transactions` item with `delete_of` (plus optional `expected_revision`) is a delete item; any other field on it is `400` (`a delete item carries only delete_of and expected_revision`). It mixes freely with edit items and new operations under the §10.1/§10.3 rules: one owner, `max_group_size`, all-or-nothing, net per account (§8.3); two items may not target the same operation in any mix of kinds (`422 duplicate edit target 41`); an unknown or foreign target is `422 delete target 999 not found`; `403` when `config.allow_deletes` is false. Outcomes carry `delete_of` on delete items; `GET /transactions/{id}` legs carry `delete_of` (delete legs), `edit_of` (edit legs) or `revision` (regular legs), and a regular leg deleted after its group committed reads `DELETED` while the group stays `COMMITTED`. A decided group is `COMMITTED` (delete and edit items `APPLIED`, targets `DELETED` / at their next revision, new items `CONFIRMED`) or `REJECTED` (every item `INVALID` with one shared rejection, no target touched). A single `delete_of` item is the same registration as `DELETE /operations/{id}` (one idempotency key covers both forms).

## 11. Scaling Model

**Per-cell ceiling.** The cell's database bounds throughput: WAL/fsync and row-write volume. Each decided operation costs a handful of row writes (status flip, account update, 1+ snapshot touches; groups: × legs + transaction row). With batching, a cell sustains thousands of decisions/second — on solid hardware, roughly **hundreds of thousands to ~1M active users per cell** for a budgeting-class workload (tens of group-operations per user per day). Loop utilization ρ and DB write metrics (§13) tell you where a cell actually stands; capacity planning is arithmetic, not faith.

**Horizontal scale = more cells.** Because of the sharding invariant (no group crosses owners), cells share nothing and capacity grows linearly with cell count. Adding a cell requires no migration of existing cells. Moving an owner between cells is a per-owner copy (accounts, operations, snapshots) plus a directory flip.

**Latency is flat across scale**: ~one polling cycle (~1 s default; p99 a small multiple) at any number of cells, provided each cell is run within its ceiling. For lower latency, shrink `loop_interval` (the idle poll is one cheap indexed query) or wake the leader with a NOTIFY from the insert path — tens of milliseconds are reachable without architectural change.

**What would break the model** — and must trigger a design pass, never an improvisation: any feature requiring atomicity across owners (e.g., user-to-user transfers). Options then: application-level sagas (two groups + compensation), or a cross-cell commit protocol. The invariant exists so that day is a conversation, not an incident.

## 12. Failure Matrix

| Failure | Consequence | Recovery |

|---|---|---|

| Leader crash | Cell processing pauses ≤ lease TTL | Standby claims; PENDING work resumes; no state lost (all state in DB) |

| Crash mid-transaction | DB rollback | Work re-selected next cycle; idempotent by guards |

| Zombie leader (stall past lease expiry) | Guards hit 0 rows | Rollback; instance re-acquires or becomes standby |

| API crash mid-insert | ACID rollback of the insert | Client retries with the same idempotency key |

| Cell DB down | That cell pauses entirely (others unaffected) | Resumes with DB; no split-brain possible — the DB *is* the coordinator |

| Clock skew | None — all time comparisons use the DB clock | — |

| Lost NOTIFY | A waiting API request | Covered by the polling fallback |

| Edit or delete reaches the leader before its target is decided (overlapping insertion transactions, ADR-0008/0010/0011) | That unit is deferred, counted, left PENDING; work behind it proceeds | Decided in a later cycle in id order once the target is; nothing to operate |

| Stale leader applies a delete after a concurrent edit or delete of the same target (ADR-0011) | Delete CAS matches 0 rows (revision moved or status no longer CONFIRMED) | Batch rolled back; re-decided next cycle against the target's new state |

## 13. Observability (per cell, from day one)

| Metric | Why |

|---|---|

| Oldest PENDING age & queue depth | The system's only latency promise is "~cycle when healthy"; this is its health |

| Loop utilization ρ (busy time / cycle) | Queue-latency predictor; the cell-splitting trigger |

| Snapshot rows touched per confirmation | Measures the backdating workload in production |

| Leadership changes/hour | Flapping lease = tuning or infrastructure problem |

| DB row-writes/s, WAL throughput, fsync latency | Distance to the cell ceiling |

| NOTIFY→outcome lag | API wait health |

| Decisions by kind and outcome (`single`/`group`/`edit`/`delete` × `confirmed`/`invalid`/`committed`/`rejected`/`applied`) | Edit and delete adoption and rejection mix (ADR-0010, ADR-0011) |

| Edit deferrals (`balancedb_edit_deferrals_total`; counts deferred deletes too) | A sustained rate means overlapping insertion transactions (ADR-0008); zero is the norm |

## 14. Key Rationale (why it is this way)

- **Ordering by `(effective_at, id)`** instead of ranks/linked lists: deterministic, unique, zero maintenance; viable because no per-row running balance is stored.

- **Snapshots (daily/cumulative/sparse)** instead of per-row balances: turns backdating from O(all subsequent rows) into O(days spanned), and reads stay O(1)/O(page).

- **Validation against the final balance only**: an operation's effect on the final balance is its amount regardless of timeline position, so the check is exactly the guarantee (G1) — and point-in-time guarantees are explicitly not offered (N2).

- **Single processor per cell + atomic group transactions**: with one owner of all the cell's accounts, a group commits in one DB transaction — eliminating any need for two-phase commit, reservations, deferred states, or timeouts, and strengthening the contract (G2 absolute, G5 timeout-free).

- **Leader lease + optimistic guards**: the lease provides availability; the guards provide safety. Safety never rests on the lease (Principle 3).

- **One-way reversal link**: a back-pointer would mutate the original and is derivable from an index (Principles 4–5).

- **Non-zero-sum groups**: a group is an atomic set, not a double-entry constraint — required by allocation-style accounting (envelopes) where one real movement legitimately hits two balances the same way.

- **Cells sharded by owner**: the workload's natural boundary; zero cross-cell coordination makes scaling linear and keeps every cell as simple as a single-node system.

## 15. Implementation Notes

First investment: **deterministic simulation testing** — seeded generators of operations, groups, aggressive backdating (crossing snapshot days, tying timestamps), leader failover, and zombie leaders, checking G1–G6 against a sequential reference model. Also: authorization/tenancy plumbing around `owner_id`, pagination details, config tuning, and the directory service (a table + cache is sufficient initially).
