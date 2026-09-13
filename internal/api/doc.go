// Package api is the client-facing surface of a cell (spec §10).
//
// Insert delegates to internal/ledger, shared with the public embedded Go API
// (ADR-0007). That core owns spec §10.1 validation, account upserts, idempotency,
// group atomicity and the transactional doorbell. Concurrent insertion commits
// may expose higher IDs before lower ones (ADR-0008).
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
package api
