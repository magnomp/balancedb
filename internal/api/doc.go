// Package api is the client-facing surface of a cell (spec §10).
//
// M3 builds its correctness core with no HTTP: Insert is a pure function over a
// pgx.Tx implementing the insertion semantics of spec §10.1 — single vs group,
// on-demand account upsert, atomic multi-leg group insert, the Stripe-model
// idempotency probe-and-insert, payload-hash conflict detection, max_group_size
// enforcement, and the one-owner-per-group sharding invariant (spec §2), which is
// validated here and only here. Every successful *new* insert transaction rings
// the processor doorbell (NOTIFY work_available, ADR-0002) before it commits.
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
// Synchronous waiting (wait_ms > 0) is deferred to M8; M7 documents the field and
// treats every insert as fire-and-forget (202).
package api
