// Package ledger registers single operations and atomic groups inside a caller's
// transaction (spec §10.1, ADR-0007). It owns insertion validation, idempotency,
// the transactional work doorbell for both HTTP and embedded callers. It does
// not serialize insertion transactions; work visibility follows host commits
// (ADR-0008).
package ledger
