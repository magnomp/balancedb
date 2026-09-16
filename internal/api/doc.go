// Package api is the client-facing surface of a cell (spec §10).
//
// Insert delegates to internal/ledger, shared with the public embedded Go API
// (ADR-0007). That core owns spec §10.1 validation, account upserts, idempotency,
// group atomicity and the transactional doorbell. Concurrent insertion commits
// may expose higher IDs before lower ones (ADR-0008). Account management and reads
// also delegate to ledger; composite reads share a consistent snapshot (ADR-0009).
//
// M7 adds the HTTP surface (spec §10) on Huma v2 over a chi mux (ADR-0003), which
// is confined to this package by the ADR boundary rule. Every §10 endpoint is a
// typed operation whose request/response structs are the OpenAPI contract, so the
// generated docs (/docs), spec (/openapi.yaml), and request validation cannot
// drift from the code; `make openapi` exports that spec to the committed
// api/openapi.yaml. Amounts cross the boundary as the Amount type, which reports an
// integer/int64 schema and routes every JSON→money conversion through
// model.ParseAmount (no float64). Every operation is scoped to one owner via the
// pluggable OwnerResolver (X-Owner-Id header today). The Insert core above is
// reused unchanged by POST /transactions.
//
// M8 adds the opt-in synchronous wait (wait_ms > 0, spec §10.1, ADR-0002): after
// the insert commits, POST /transactions registers an outcome waiter with the
// internal/notify Notifier, does one immediate status check to close the
// lost-wakeup race, then waits for the deciding NOTIFY (with a status-poll
// durability fallback) up to the requested budget capped by api_max_wait_ms —
// returning 200 with the decided status if it lands in time, else 202 with the
// current state. wait_ms <= 0 stays fire-and-forget (202) and never touches the
// notify machinery. The 200/202 split is a runtime status override; the OpenAPI
// contract advertises 202 as the declared response.
//
// Operation editing (ADR-0010) adds PATCH /operations/{id} — one edit item through
// the same ledger core, mapped to the contract's statuses (404 for an unknown or
// foreign target, 403 when config.allow_edits is off) and sharing the wait loop
// (awaitDecision) on 'op:<edit id>' — plus GET /operations/{id}/history over the
// append-only operation_revisions table. GET /operations/{id} and the statement
// gain the current values and revision. POST /transactions accepts edit items
// (edit_of, expected_revision) alongside new operations — one atomic group
// through the same core, the handler re-imposing account/amount/effective_at on
// non-edit items before any DB access — and its outcomes and GET
// /transactions/{id} legs carry edit_of (edit items) / revision (regular legs).
// Nothing here writes operation_revisions.
//
// Operation deletion (ADR-0011) adds DELETE /operations/{id} — one delete item
// (no body; expected_revision and wait_ms are query parameters) through the same
// core, the same 404/403 mapping (config.allow_deletes) and the same wait loop
// on 'op:<deletion id>', answering {"deletion": …}. POST /transactions accepts
// delete_of items (nothing but expected_revision may accompany one, 400
// otherwise); outcomes and legs carry delete_of. GET /operations/{id} and the
// history gain deleted_at/deleted_by on a DELETED row, delete registrations read
// back with delete_of, and history entries carry kind ("edit" | "delete"). The
// shared target sentinels arrive wrapped in ledger.TargetError, whose kind the
// error messages name ("delete target 41 not found"). Nothing here writes
// status = 'DELETED' or the deletion markers — the leader does.
package api
