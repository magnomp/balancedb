// Package notify implements the outcome fan-out half of the bidirectional
// LISTEN/NOTIFY path (ADR-0002; spec §11): one dedicated LISTEN outcomes
// connection per API process demultiplexing the processor's per-decision
// notifications ('op:<id>' | 'tx:<id>', emitted inside the deciding commit,
// spec §10.1/§8) to the API waiters registered for those keys.
//
// The doorbell half (NOTIFY work_available; the leader's LISTEN) lives with the
// insert path (internal/api) and the processor (internal/processor). This
// package is only the API-side receiver.
//
// NOTIFY is a hint, never a guarantee: notifications that arrive while the listen
// connection is down (reconnect window) are simply dropped, and the API wait path
// falls back to its status poll as the durability path (spec §10.1). A Notifier is
// safe for concurrent use by many in-flight request handlers.
package notify
