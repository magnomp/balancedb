// Package simtest is the deterministic simulation harness (spec §15). It grows
// one milestone at a time, per plan §M10's guidance to start it early and let it
// accrete rather than bolting it on at the end.
//
// At M3 it holds the seed of the sequential reference model: an in-memory replay
// of insertion in registration order that computes the same insert-level outcomes
// as the real inserter (internal/api) — new vs replay vs payload conflict,
// max_group_size, the one-owner-per-group invariant — so later milestones can
// cross-check the database against it. Confirmation/rejection outcomes and fault
// injection arrive with the processor (M5/M6) and the full harness (M10).
//
// The reference model is pure and deterministic: given the same schedule it
// produces the same outcomes, and every property test prints its seed so a
// failure reproduces exactly.
package simtest
