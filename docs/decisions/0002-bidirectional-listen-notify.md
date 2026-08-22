# ADR-0002 — Bidirectional LISTEN/NOTIFY (doorbell + outcome fan-out)

Status: accepted (operator decision, 2026-08-21). Predates M1.5's ADR skeleton; numbering reserved.

## Context
Default latency is ~1 polling cycle (insert→decision, dominated by the processor's `loop_interval` sleep) plus up to one poll interval (decision→waiting client). Spec §11 explicitly names both NOTIFY directions as the sanctioned low-latency path. Poll-only was considered (less code) and rejected by the operator in favor of the full path.

## Decision
1. **Processor doorbell.** Every successful insert transaction emits `NOTIFY work_available` (no payload). The *leader* holds a dedicated LISTEN connection and sleeps with deadline `min(loop_interval, time-to-safe-lease-renewal)`; a doorbell wakes it early. Coalesced; drain-until-empty-select; standbys do not listen.
2. **Outcome fan-out.** The deciding commit emits `NOTIFY outcomes, 'op:<id>' | 'tx:<id>'`. API nodes hold one shared LISTEN connection each, demultiplexing to registered waiters.
3. **Waiting stays optional.** `wait_ms = 0`/omitted → immediate `202`; outcomes queryable later. The notify machinery is only touched by opt-in waits.

## Invariants preserved
- NOTIFY is a *hint* in both directions; timed wakeups (processor) and status polls (API waiters) remain the correctness paths. A lost notification costs latency only.
- Ordering/determinism (G3): work is always selected `ORDER BY id`; notification arrival order is never used.
- Notifications are emitted inside their transactions ⇒ delivered only on commit; rolled-back attempts notify nobody.

## Consequences
- M3 gains doorbell emission; M5 gains the leader's listen/deadline wait; M8 implements the fan-out registry as planned; M9 measures doorbell wakeup lag and NOTIFY→outcome lag.
- Idle-system synchronous inserts should resolve in tens of milliseconds; under load, behavior degrades gracefully to plain polling/batching (doorbells coalesce while the leader is busy).
