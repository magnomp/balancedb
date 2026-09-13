// Package ledger owns the shared account, insertion and query semantics
// (spec §6/§9/§10, ADR-0007/0009). It owns insertion validation, idempotency,
// the transactional work doorbell for both HTTP and embedded callers. It does
// not serialize insertion transactions; work visibility follows host commits
// (ADR-0008).
package ledger
