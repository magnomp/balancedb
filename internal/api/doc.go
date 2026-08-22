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
// The HTTP handlers, waiting, and query endpoints (Huma, ADR-0003) arrive in
// M7–M8 and wrap this core; the core never depends on the transport.
package api
