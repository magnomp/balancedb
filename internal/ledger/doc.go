// Package ledger owns the shared account, insertion, edit- and
// delete-registration and query semantics (spec §6/§9/§10, ADR-0007/0009/0010/
// 0011). It owns insertion validation, edit-class target resolution (an edit
// fills its omitted fields from the target, a delete copies the target's state
// as an informational snapshot), idempotency, and the transactional work
// doorbell for both HTTP and embedded callers. It does not serialize insertion
// transactions; work visibility follows host commits (ADR-0008).
package ledger
