//go:build itest

package api_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// Integration and end-to-end tests for the HTTP deletion surface (ADR-0011):
// IT-002–IT-005, IT-009, IT-031–IT-034, IT-036–IT-038, IT-060, IT-061, E2E-001,
// E2E-002 and E2E-003 of the operation-deletion test contract. The cell helpers live in
// edit_itest_test.go; every test runs the real ledger, processor and (when
// asked) notifier over a throwaway schema.

// del deletes an operation (owner 7, fresh key) with optional query parameters
// and returns the status code, the deletion object and the whole body.
func (c *cell) del(id int64, query string) (int, map[string]any, map[string]any) {
	c.t.Helper()
	return c.delAs(owner, c.key(), id, query, "")
}

func (c *cell) delAs(ownerID, idem string, id int64, query, body string) (int, map[string]any, map[string]any) {
	c.t.Helper()
	code, m := c.do(http.MethodDelete, fmt.Sprintf("/operations/%d%s", id, query), ownerID, idem, body)
	deletion, _ := m["deletion"].(map[string]any)
	return code, deletion, m
}

// deleteApplied deletes with a wait and requires 200 APPLIED; returns the
// deletion id.
func (c *cell) deleteApplied(id int64) int64 {
	c.t.Helper()
	code, d, m := c.del(id, "?wait_ms=5000")
	if code != http.StatusOK || d["status"] != "APPLIED" {
		c.t.Fatalf("DELETE %d: status %d, body %v; want 200 APPLIED", id, code, m)
	}
	return int64(d["id"].(float64))
}

// count runs a scalar count query against the cell's schema.
func (c *cell) count(sql string) int {
	c.t.Helper()
	var n int
	if err := c.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		c.t.Fatalf("count %s: %v", sql, err)
	}
	return n
}

// group inserts one atomic group of regular legs and returns the transaction
// id and the leg ids.
func (c *cell) group(body string) (int64, []int64) {
	c.t.Helper()
	code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), body)
	if code != http.StatusAccepted && code != http.StatusOK {
		c.t.Fatalf("POST /transactions: status %d, body %v", code, m)
	}
	var legs []int64
	for _, o := range m["operations"].([]any) {
		legs = append(legs, num(o.(map[string]any)["id"]))
	}
	return num(m["transaction_id"]), legs
}

// --- IT-002 / IT-003 ---------------------------------------------------------

// IT-002: a delete item that carries any other field is refused with 400 and
// the contract's message before any write — no operations or transactions row
// appears, whether the item is alone or in a group.
func TestDeleteItemWithFieldsIs400(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	ops, txs := c.count(`SELECT count(*) FROM operations`), c.count(`SELECT count(*) FROM transactions`)

	for _, body := range []string{
		fmt.Sprintf(`{"operations":[{"delete_of":%d,"amount":-1}]}`, target),
		fmt.Sprintf(`{"operations":[{"delete_of":%d,"amount":-1},{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`, target),
	} {
		code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), body)
		if code != http.StatusBadRequest || m["detail"] != "a delete item carries only delete_of and expected_revision" {
			t.Fatalf("POST %s: status %d body %v, want 400", body, code, m)
		}
	}
	if got := c.count(`SELECT count(*) FROM operations`); got != ops {
		t.Fatalf("operations rows = %d, want %d (unchanged)", got, ops)
	}
	if got := c.count(`SELECT count(*) FROM transactions`); got != txs {
		t.Fatalf("transactions rows = %d, want %d (unchanged)", got, txs)
	}
}

// IT-003: DELETE has no body; one sent along is ignored and the delete is
// registered exactly as without it (202 PENDING, delete_of on the row).
func TestDeleteIgnoresBody(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")

	code, d, m := c.delAs(owner, c.key(), target, "", `{"amount": -1200, "wait_ms": 5000, "expected_revision": 9}`)
	if code != http.StatusAccepted || d["status"] != "PENDING" || num(d["operation_id"]) != target || m["replayed"] != false {
		t.Fatalf("DELETE with body: status %d body %v, want 202 PENDING", code, m)
	}
	wantKeys(t, "deletion", d, "id", "status", "operation_id")
	_, reg := c.getOp(num(d["id"]))
	wantKeys(t, "delete registration", reg, "id", "status", "delete_of", "account", "amount", "effective_at")
	if num(reg["delete_of"]) != target || num(reg["amount"]) != -1500 {
		t.Fatalf("registration = %v", reg)
	}
}

// --- IT-004 / IT-005 ---------------------------------------------------------

// IT-004: DELETE of an unknown id and of another owner's id are both 404 with
// the same body — the API never reveals that the id exists.
func TestDeleteForeignTargetIs404(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")

	codeForeign, _, foreign := c.delAs("8", c.key(), target, "", "")
	codeUnknown, _, unknown := c.delAs(owner, c.key(), 999, "", "")
	if codeForeign != http.StatusNotFound || codeUnknown != http.StatusNotFound {
		t.Fatalf("status foreign=%d unknown=%d, want 404/404", codeForeign, codeUnknown)
	}
	if !reflect.DeepEqual(foreign, unknown) {
		t.Fatalf("bodies differ: foreign %v, unknown %v", foreign, unknown)
	}
	if foreign["detail"] != "operation not found" {
		t.Fatalf("detail = %v, want %q", foreign["detail"], "operation not found")
	}
	if n := c.count(`SELECT count(*) FROM operations WHERE is_delete`); n != 0 {
		t.Fatalf("delete rows = %d, want 0", n)
	}
}

// IT-005: the target of a DELETE must be a regular operation — an edit id and a
// deletion id are both 422 with the contract's message (the ids are restarted
// so the edit is the transcript's 57).
func TestDeleteOfRegistrationIs422(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 57`)
	_, edit, _ := c.patch(target, `{"amount":-1200}`)
	editID := num(edit["id"])
	_, d, _ := c.del(target, "")
	delID := num(d["id"])
	if editID != 57 || delID != 58 {
		t.Fatalf("ids = %d/%d, want 57/58", editID, delID)
	}

	code, _, m := c.del(editID, "")
	if code != http.StatusUnprocessableEntity || m["detail"] != "delete target 57 is an edit, not an operation" {
		t.Fatalf("DELETE of an edit id: status %d body %v, want 422", code, m)
	}
	code, _, m = c.del(delID, "")
	if code != http.StatusUnprocessableEntity || m["detail"] != "delete target 58 is an edit, not an operation" {
		t.Fatalf("DELETE of a deletion id: status %d body %v, want 422", code, m)
	}
	// Grouped, the same rule names the kind of the offending item.
	body := fmt.Sprintf(`{"operations":[{"delete_of":%d},{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`, editID)
	if code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), body); code != http.StatusUnprocessableEntity || m["detail"] != "delete target 57 is an edit, not an operation" {
		t.Fatalf("grouped delete of an edit id: status %d body %v, want 422", code, m)
	}
	body = `{"operations":[{"delete_of":999},{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`
	if code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), body); code != http.StatusUnprocessableEntity || m["detail"] != "delete target 999 not found" {
		t.Fatalf("grouped delete of an unknown id: status %d body %v, want 422", code, m)
	}
	if n := c.count(`SELECT count(*) FROM transactions`); n != 0 {
		t.Fatalf("refused groups left %d transactions rows", n)
	}
}

// --- IT-009 ------------------------------------------------------------------

// IT-009: DELETE /operations/{id} and a single POST /transactions delete item
// are the same registration — the second under the same key replays the first
// (same id, one row); the key reused by another owner with its own payload is
// a payload conflict.
func TestDeleteAndPostDeleteItemShareIdempotency(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	const k = "9c2f6d31-5b8e-4a0f-9d47-3e1a8b7c6d05"

	code, d, m := c.delAs(owner, k, target, "?expected_revision=1", "")
	if code != http.StatusAccepted || m["replayed"] != false {
		t.Fatalf("DELETE: status %d body %v, want 202 fresh", code, m)
	}
	delID := num(d["id"])

	body := fmt.Sprintf(`{"operations":[{"delete_of":%d,"expected_revision":1}]}`, target)
	code, m = c.do(http.MethodPost, "/transactions", owner, k, body)
	if code != http.StatusAccepted || m["replayed"] != true {
		t.Fatalf("POST replay: status %d body %v, want 202 replayed", code, m)
	}
	op := m["operations"].([]any)[0].(map[string]any)
	if num(op["id"]) != delID || num(op["delete_of"]) != target || op["status"] != "PENDING" {
		t.Fatalf("POST replay outcome = %v, want id %d delete_of %d", op, delID, target)
	}
	// And the other way round: the DELETE replays too.
	if code, d, m := c.delAs(owner, k, target, "?expected_revision=1", ""); code != http.StatusAccepted || m["replayed"] != true || num(d["id"]) != delID {
		t.Fatalf("DELETE replay: status %d body %v", code, m)
	}
	if n := c.count(`SELECT count(*) FROM operations WHERE is_delete`); n != 1 {
		t.Fatalf("delete rows = %d, want 1", n)
	}

	// The same key under another owner (with that owner's own payload) conflicts.
	other := `{"operations":[{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`
	if code, m := c.do(http.MethodPost, "/transactions", "8", k, other); code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-owner key: status %d body %v, want 422", code, m)
	}
	// Same key, guard removed → a different payload → conflict (IT-008 over HTTP).
	if code, _, m := c.delAs(owner, k, target, "", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("guard removed: status %d body %v, want 422", code, m)
	}
}

// --- IT-031 / IT-032 ---------------------------------------------------------

// IT-031: after a deletion the history reads DELETED with the markers; the
// revisions are intact and the last one (the last values) has no superseded_*.
func TestDeleteHistoryShowsMarkers(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	e1 := c.editApplied(target, `"amount":-1200`)
	delID := c.deleteApplied(target)

	code, h := c.history(target)
	if code != http.StatusOK {
		t.Fatalf("history: status %d body %v", code, h)
	}
	wantKeys(t, "history", h, "operation_id", "status", "current_revision", "deleted_at", "deleted_by", "revisions", "pending_edits", "rejected_edits")
	if h["status"] != "DELETED" || num(h["current_revision"]) != 2 || num(h["deleted_by"]) != delID {
		t.Fatalf("history head = %v", h)
	}
	if at, _ := time.Parse(time.RFC3339Nano, h["deleted_at"].(string)); at.IsZero() || time.Since(at) > time.Minute {
		t.Fatalf("deleted_at = %v, want a recent instant", h["deleted_at"])
	}
	revs := h["revisions"].([]any)
	if len(revs) != 2 {
		t.Fatalf("revisions = %v, want 2", revs)
	}
	r1, r2 := revs[0].(map[string]any), revs[1].(map[string]any)
	if num(r1["revision"]) != 1 || num(r1["amount"]) != -1500 || num(r1["superseded_by"]) != e1 {
		t.Fatalf("revision 1 = %v", r1)
	}
	wantKeys(t, "last revision", r2, "revision", "account", "amount", "effective_at", "recorded_at")
	if num(r2["revision"]) != 2 || num(r2["amount"]) != -1200 {
		t.Fatalf("revision 2 = %v", r2)
	}
	if len(h["pending_edits"].([]any)) != 0 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("lists = %v", h)
	}
	// The operation view agrees: DELETED, last values, revision frozen, markers.
	_, m := c.getOp(target)
	wantKeys(t, "deleted op", m, "id", "status", "account", "amount", "effective_at", "revision", "deleted_at", "deleted_by")
	if m["status"] != "DELETED" || num(m["amount"]) != -1200 || num(m["revision"]) != 2 || num(m["deleted_by"]) != delID || m["deleted_at"] != h["deleted_at"] {
		t.Fatalf("GET /operations/%d = %v", target, m)
	}
}

// IT-032: a deletion registration reads back with delete_of and the target's
// values at submission (informational), the guard if any, never a revision, and
// the rejection once INVALID.
func TestDeleteRegistrationView(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")

	delID := c.deleteApplied(target)
	_, m := c.getOp(delID)
	want := map[string]any{"id": float64(delID), "status": "APPLIED", "delete_of": float64(target), "account": "cash", "amount": -1500.0, "effective_at": "2026-03-01T10:00:00Z"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("applied deletion = %v, want %v", m, want)
	}

	// A second delete of the (now DELETED) target: registered, then rejected.
	code, d, _ := c.del(target, "?expected_revision=2&wait_ms=5000")
	if code != http.StatusOK || d["status"] != "INVALID" {
		t.Fatalf("second DELETE: status %d deletion %v, want 200 INVALID", code, d)
	}
	wantKeys(t, "invalid deletion outcome", d, "id", "status", "operation_id", "rejection")
	_, m = c.getOp(num(d["id"]))
	wantKeys(t, "invalid deletion", m, "id", "status", "delete_of", "expected_revision", "account", "amount", "effective_at", "rejection")
	rj := m["rejection"].(map[string]any)
	if m["status"] != "INVALID" || num(m["expected_revision"]) != 2 || rj["code"] != "TARGET_NOT_EDITABLE" || num(rj["operation_id"]) != target {
		t.Fatalf("invalid deletion = %v", m)
	}
}

// --- IT-033 / IT-034 ---------------------------------------------------------

// IT-033: pending and rejected deletes and edits against one operation are
// listed together, each with its kind, in registration-id order.
func TestDeleteHistoryKinds(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")

	// Rejected: a stale edit, then a stale delete.
	code, edit, _ := c.patch(target, `{"amount":-1200,"expected_revision":5,"wait_ms":5000}`)
	if code != http.StatusOK || edit["status"] != "INVALID" {
		t.Fatalf("stale PATCH: %d %v", code, edit)
	}
	code, d, _ := c.del(target, "?expected_revision=5&wait_ms=5000")
	if code != http.StatusOK || d["status"] != "INVALID" {
		t.Fatalf("stale DELETE: %d %v", code, d)
	}
	// Pending: a delete, then an edit, registered while nothing decides.
	c.stopProcessor()
	_, d2, _ := c.del(target, "")
	_, e2, _ := c.patch(target, `{"amount":-1000}`)

	_, h := c.history(target)
	if h["status"] != "CONFIRMED" || h["deleted_at"] != nil || h["deleted_by"] != nil {
		t.Fatalf("history head = %v", h)
	}
	pend, rej := h["pending_edits"].([]any), h["rejected_edits"].([]any)
	if len(pend) != 2 || len(rej) != 2 {
		t.Fatalf("lists = %v", h)
	}
	p0, p1 := pend[0].(map[string]any), pend[1].(map[string]any)
	wantKeys(t, "pending delete", p0, "id", "kind", "account", "amount", "effective_at")
	if num(p0["id"]) != num(d2["id"]) || p0["kind"] != "delete" || num(p0["amount"]) != -1500 {
		t.Fatalf("pending delete = %v", p0)
	}
	if num(p1["id"]) != num(e2["id"]) || p1["kind"] != "edit" || num(p1["amount"]) != -1000 {
		t.Fatalf("pending edit = %v", p1)
	}
	r0, r1 := rej[0].(map[string]any), rej[1].(map[string]any)
	wantKeys(t, "rejected delete", r1, "id", "kind", "account", "amount", "effective_at", "expected_revision", "decided_at", "rejection")
	if num(r0["id"]) != num(edit["id"]) || r0["kind"] != "edit" || num(r0["amount"]) != -1200 {
		t.Fatalf("rejected edit = %v", r0)
	}
	if num(r1["id"]) != num(d["id"]) || r1["kind"] != "delete" || num(r1["amount"]) != -1500 || num(r1["expected_revision"]) != 5 {
		t.Fatalf("rejected delete = %v", r1)
	}
	if rj := r1["rejection"].(map[string]any); rj["code"] != "STALE_REVISION" || num(rj["actual_revision"]) != 1 {
		t.Fatalf("rejected delete rejection = %v", rj)
	}
}

// IT-034: the history of a deletion registration id is 404 — only regular
// operations have a history.
func TestDeleteHistoryOfDeletionIdIs404(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	_, d, _ := c.del(target, "")
	code, m := c.history(num(d["id"]))
	if code != http.StatusNotFound || m["detail"] != "operation not found" {
		t.Fatalf("history of a deletion id: status %d body %v, want 404", code, m)
	}
}

// --- IT-036 ------------------------------------------------------------------

// IT-036: DELETE with wait_ms resolves through NOTIFY with the processor running
// (fast, well under the loop interval) and expires to 202 PENDING when nothing
// decides.
func TestDeleteWaitPaths(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	t1 := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	t2 := c.confirm("cash", -700, "2026-03-02T10:00:00Z")
	c.exec(`UPDATE config SET loop_interval_ms = 10000, lease_ttl_ms = 30000`)
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	code, d, m := c.del(t1, "?wait_ms=2000")
	if code != http.StatusOK || d["status"] != "APPLIED" || num(d["operation_id"]) != t1 || m["replayed"] != false {
		t.Fatalf("notify path: status %d body %v, want 200 APPLIED", code, m)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("notify path took %s with loop_interval=10s", el)
	}

	c.stopProcessor()
	start = time.Now()
	code, d, m = c.del(t2, "?wait_ms=300")
	if code != http.StatusAccepted || d["status"] != "PENDING" || m["replayed"] != false {
		t.Fatalf("expiry: status %d body %v, want 202 PENDING", code, m)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Fatalf("expiry returned after %s, before the budget", el)
	}
}

// --- IT-037 / IT-038 ---------------------------------------------------------

// IT-037: a deleted operation leaves the statement, every point-in-time
// projection and the final balance; the others are untouched.
func TestDeleteExcludedFromReads(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	op1 := c.confirm("cash", 1000, "2026-03-01T09:00:00Z")
	op2 := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	op3 := c.confirm("cash", 500, "2026-03-04T09:00:00Z")
	if bal := c.balance("cash", ""); bal != 0 {
		t.Fatalf("balance before = %d, want 0", bal)
	}
	c.deleteApplied(op2)

	entries := c.statement("cash")
	if len(entries) != 2 || num(entries[0]["id"]) != op1 || num(entries[1]["id"]) != op3 {
		t.Fatalf("statement = %v, want [%d %d]", entries, op1, op3)
	}
	if bal := c.balance("cash", "2026-03-01T09:30:00Z"); bal != 1000 {
		t.Fatalf("balance before op2's instant = %d, want 1000", bal)
	}
	if bal := c.balance("cash", "2026-03-02T00:00:00Z"); bal != 1000 {
		t.Fatalf("balance after op2's instant = %d, want 1000 (op2 excluded)", bal)
	}
	if bal := c.balance("cash", ""); bal != 1500 {
		t.Fatalf("final balance = %d, want 1500", bal)
	}
}

// IT-038: the operation and transaction views still return a deleted leg — as
// DELETED, with its revision — while the group stays COMMITTED.
func TestDeleteStillVisibleInOperationAndTransactionViews(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	txID, legs := c.group(`{"operations":[{"account":"cash","amount":-1500,"effective_at":"2026-03-01T10:00:00Z"},{"account":"wallet","amount":1500,"effective_at":"2026-03-01T10:00:00Z"}],"wait_ms":5000}`)
	delID := c.deleteApplied(legs[0])

	_, m := c.getOp(legs[0])
	if m["status"] != "DELETED" || num(m["transaction_id"]) != txID || num(m["revision"]) != 1 || num(m["deleted_by"]) != delID {
		t.Fatalf("GET /operations/%d = %v", legs[0], m)
	}
	_, tx := c.do(http.MethodGet, fmt.Sprintf("/transactions/%d", txID), owner, "", "")
	want := map[string]any{
		"id": float64(txID), "status": "COMMITTED", "op_count": 2.0,
		"operations": []any{
			map[string]any{"id": float64(legs[0]), "status": "DELETED", "revision": 1.0},
			map[string]any{"id": float64(legs[1]), "status": "CONFIRMED", "revision": 1.0},
		},
	}
	if !reflect.DeepEqual(tx, want) {
		t.Fatalf("GET /transactions/%d = %v, want %v", txID, tx, want)
	}
	if bal := c.balance("cash", ""); bal != 0 {
		t.Fatalf("cash balance = %d, want 0", bal)
	}
}

// --- IT-060 / IT-061 ---------------------------------------------------------

// IT-060: allow_deletes = false refuses DELETE and any group with a delete item
// with 403 and the contract's message, before any row is written; edits and
// plain inserts are unaffected. (The embedded ErrDeletesDisabled half is
// covered by the ledger's own itests.)
func TestDeletesDisabledPolicy(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	c.exec(`UPDATE config SET allow_deletes = false`)

	code, _, m := c.del(target, "")
	if code != http.StatusForbidden || m["detail"] != "deleting is disabled for this cell" {
		t.Fatalf("DELETE with deletes disabled: status %d body %v, want 403", code, m)
	}
	groupBody := fmt.Sprintf(`{"operations":[{"delete_of":%d},{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`, target)
	if code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), groupBody); code != http.StatusForbidden || m["detail"] != "deleting is disabled for this cell" {
		t.Fatalf("POST /transactions with a delete item and deletes disabled: status %d body %v, want 403", code, m)
	}
	if n := c.count(`SELECT count(*) FROM transactions`); n != 0 {
		t.Fatalf("refused group left %d transactions rows", n)
	}
	if n := c.count(`SELECT count(*) FROM operations WHERE is_delete`); n != 0 {
		t.Fatalf("refused deletes left %d rows", n)
	}
	// Edits and inserts still go through.
	if code, edit, _ := c.patch(target, `{"amount":-1200}`); code != http.StatusAccepted || edit["status"] != "PENDING" {
		t.Fatalf("PATCH with deletes disabled: status %d edit %v, want 202", code, edit)
	}
	c.insert("cash", 10, "2026-03-01T11:00:00Z", 0, "PENDING")

	c.exec(`UPDATE config SET allow_deletes = true`)
	if code, d, _ := c.del(target, ""); code != http.StatusAccepted || d["status"] != "PENDING" {
		t.Fatalf("DELETE after re-enable: status %d deletion %v, want 202", code, d)
	}
}

// IT-061: a delete registered before allow_deletes flips to false is still
// decided, and allow_edits = false does not refuse deletes.
func TestDeleteRegisteredBeforeDisableStillDecided(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	t1 := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	t2 := c.confirm("cash", -700, "2026-03-02T10:00:00Z")
	c.stopProcessor()

	_, d, _ := c.del(t1, "")
	c.exec(`UPDATE config SET allow_deletes = false`)
	c.startProcessor()
	c.waitStatus(num(d["id"]), "APPLIED")
	if m := c.waitStatus(t1, "DELETED"); num(m["deleted_by"]) != num(d["id"]) {
		t.Fatalf("target = %v, want deleted_by %v", m, d["id"])
	}

	c.exec(`UPDATE config SET allow_deletes = true, allow_edits = false`)
	if code, _, m := c.patch(t2, `{"amount":-600}`); code != http.StatusForbidden {
		t.Fatalf("PATCH with edits disabled: status %d body %v, want 403", code, m)
	}
	if code, d, m := c.del(t2, "?wait_ms=5000"); code != http.StatusOK || d["status"] != "APPLIED" {
		t.Fatalf("DELETE with only edits disabled: status %d body %v, want 200 APPLIED", code, m)
	}
}

// --- E2E-001 / E2E-002 -------------------------------------------------------

// E2E-001: the _dx.md Golden Path verbatim. The identity sequence is restarted
// so the ids are the transcript's (41 for the operation, 63 for the deletion).
func TestDeleteGoldenPath(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 41`)
	if id := c.confirm("cash", -1500, "2026-03-01T10:00:00Z"); id != 41 {
		t.Fatalf("operation id = %d, want 41", id)
	}
	c.confirm("cash", 8500, "2026-02-01T10:00:00Z") // the rest of the balance: 7000 today, 8500 once 41 is gone

	// The operation as it stands today.
	_, m := c.getOp(41)
	want := map[string]any{"id": 41.0, "status": "CONFIRMED", "account": "cash", "amount": -1500.0, "effective_at": "2026-03-01T10:00:00Z", "revision": 1.0}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("GET /operations/41 = %v, want %v", m, want)
	}
	if bal := c.balance("cash", ""); bal != 7000 {
		t.Fatalf("balance = %d, want 7000", bal)
	}

	// Delete it. Same id stays valid. Optional wait for the decision.
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 63`)
	const idem = "9c2f6d31-5b8e-4a0f-9d47-3e1a8b7c6d05"
	code, _, body := c.delAs(owner, idem, 41, "?wait_ms=2000", "")
	wantBody := map[string]any{"deletion": map[string]any{"id": 63.0, "status": "APPLIED", "operation_id": 41.0}, "replayed": false}
	if code != http.StatusOK || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("DELETE = %d %v, want 200 %v", code, body, wantBody)
	}

	// Gone from the ledger but still readable: DELETED, last values kept.
	_, m = c.getOp(41)
	want["status"] = "DELETED"
	want["deleted_by"] = 63.0
	deletedAt, ok := m["deleted_at"].(string)
	if !ok {
		t.Fatalf("GET /operations/41 after delete = %v, want deleted_at", m)
	}
	want["deleted_at"] = deletedAt
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("GET /operations/41 after delete = %v, want %v", m, want)
	}

	// The balance moved by +1500; the statement no longer lists 41.
	if bal := c.balance("cash", ""); bal != 8500 {
		t.Fatalf("balance = %d, want 8500", bal)
	}
	for _, e := range c.statement("cash") {
		if num(e["id"]) == 41 {
			t.Fatalf("statement still lists 41: %v", e)
		}
	}

	// Full trail, append-only.
	_, h := c.history(41)
	revs := h["revisions"].([]any)
	if len(revs) != 1 {
		t.Fatalf("revisions = %v, want 1", revs)
	}
	r1 := revs[0].(map[string]any)
	wantH := map[string]any{
		"operation_id": 41.0, "status": "DELETED", "current_revision": 1.0,
		"deleted_at": deletedAt, "deleted_by": 63.0,
		"revisions":     []any{map[string]any{"revision": 1.0, "account": "cash", "amount": -1500.0, "effective_at": "2026-03-01T10:00:00Z", "recorded_at": r1["recorded_at"]}},
		"pending_edits": []any{}, "rejected_edits": []any{},
	}
	if !reflect.DeepEqual(h, wantH) {
		t.Fatalf("history = %v, want %v", h, wantH)
	}

	// Idempotent replay: same key and payload → replayed, current status; 202
	// without a wait, 200 with one (already decided). A changed guard conflicts.
	wantBody["replayed"] = true
	if code, _, body = c.delAs(owner, idem, 41, "", ""); code != http.StatusAccepted || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("replay = %d %v, want 202 %v", code, body, wantBody)
	}
	if code, _, body = c.delAs(owner, idem, 41, "?wait_ms=2000", ""); code != http.StatusOK || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("replay with wait = %d %v, want 200 %v", code, body, wantBody)
	}
	if code, _, body = c.delAs(owner, idem, 41, "?expected_revision=1", ""); code != http.StatusUnprocessableEntity {
		t.Fatalf("payload conflict = %d %v, want 422", code, body)
	}
}

// E2E-002: with cash at min 0 and its credit partly spent, deleting the credit
// is rejected LIMIT_VIOLATED synchronously and the operation is unchanged.
func TestDeleteRefused(t *testing.T) {
	c := newCell(t, true)
	c.createAccount("cash", i64p(0))
	c.startProcessor()
	credit := c.confirm("cash", 1000, "2026-03-01T09:00:00Z")
	c.confirm("cash", -500, "2026-03-01T10:00:00Z") // balance 500

	code, d, body := c.del(credit, "?wait_ms=2000")
	if code != http.StatusOK || d["status"] != "INVALID" || num(d["operation_id"]) != credit || body["replayed"] != false {
		t.Fatalf("DELETE = %d %v, want 200 INVALID", code, body)
	}
	wantRej := map[string]any{"code": "LIMIT_VIOLATED", "account": "cash", "limit_side": "min", "shortfall": 500.0}
	if !reflect.DeepEqual(d["rejection"], wantRej) {
		t.Fatalf("rejection = %v, want %v", d["rejection"], wantRej)
	}

	_, m := c.getOp(credit)
	wantKeys(t, "credit", m, "id", "status", "account", "amount", "effective_at", "revision")
	if m["status"] != "CONFIRMED" || num(m["amount"]) != 1000 || num(m["revision"]) != 1 {
		t.Fatalf("target = %v, want unchanged", m)
	}
	if bal := c.balance("cash", ""); bal != 500 {
		t.Fatalf("balance = %d, want 500", bal)
	}
	_, h := c.history(credit)
	rej := h["rejected_edits"].([]any)
	if h["status"] != "CONFIRMED" || len(h["revisions"].([]any)) != 1 || len(rej) != 1 || rej[0].(map[string]any)["kind"] != "delete" {
		t.Fatalf("history = %v", h)
	}
}

// --- E2E-003 -----------------------------------------------------------------

// E2E-003: the _dx.md POST /transactions transcript verbatim — two deletes, one
// edit and one new operation as one atomic unit. The identity sequences are
// restarted so the ids are the transcript's: group 7 holds 41 and 42, 44 is a
// wallet credit already edited once (revision 2), the mixed group is 9 with
// items 64–67. 202 with delete_of / edit_of per outcome; GET /transactions/9
// COMMITTED with APPLIED delete and edit legs and a CONFIRMED regular leg; 41
// and 44 DELETED by their legs, 42 at revision 2, group 7 still COMMITTED with
// leg 41 DELETED; balances moved by the nets. Then a violating group of the
// same shape is REJECTED with one shared reason and no target touched.
func TestGroupedDeletesTranscript(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 41`)
	c.exec(`ALTER TABLE transactions ALTER COLUMN id RESTART WITH 7`)
	seven, legs := c.group(`{"operations":[{"account":"cash","amount":-1500,"effective_at":"2026-03-01T10:00:00Z"},{"account":"cash","amount":-800,"effective_at":"2026-03-02T10:00:00Z"}],"wait_ms":5000}`)
	if seven != 7 || len(legs) != 2 || legs[0] != 41 || legs[1] != 42 {
		t.Fatalf("group = %d %v, want 7 [41 42]", seven, legs)
	}
	if id := c.confirm("cash", 8500, "2026-02-01T10:00:00Z"); id != 43 {
		t.Fatalf("operation id = %d, want 43", id)
	}
	if id := c.confirm("wallet", 250, "2026-02-09T18:00:00Z"); id != 44 {
		t.Fatalf("operation id = %d, want 44", id)
	}
	c.editApplied(44, `"amount":200`) // 44 at revision 2
	if _, m := c.getOp(44); num(m["revision"]) != 2 {
		t.Fatalf("GET /operations/44 = %v, want revision 2", m)
	}
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 64`)
	c.exec(`ALTER TABLE transactions ALTER COLUMN id RESTART WITH 9`)

	const idem = "5d0b7e42-1c3f-4a6b-8e9d-2f1a0b3c4d5e"
	body := `{
  "operations": [
    {"delete_of": 41},
    {"delete_of": 44, "expected_revision": 2},
    {"edit_of": 42, "amount": -700},
    {"account": "cash", "amount": 300, "effective_at": "2026-03-01T10:00:00Z"}
  ],
  "wait_ms": 0}`
	code, m := c.do(http.MethodPost, "/transactions", owner, idem, body)
	want := map[string]any{
		"transaction_id": 9.0, "transaction_status": "PENDING",
		"operations": []any{
			map[string]any{"id": 64.0, "status": "PENDING", "delete_of": 41.0},
			map[string]any{"id": 65.0, "status": "PENDING", "delete_of": 44.0},
			map[string]any{"id": 66.0, "status": "PENDING", "edit_of": 42.0},
			map[string]any{"id": 67.0, "status": "PENDING"},
		},
		"replayed": false,
	}
	if code != http.StatusAccepted || !reflect.DeepEqual(m, want) {
		t.Fatalf("POST /transactions = %d %v, want 202 %v", code, m, want)
	}

	// When decided: COMMITTED, delete and edit items APPLIED, the new item CONFIRMED.
	deadline := time.Now().Add(5 * time.Second)
	var tx map[string]any
	for {
		_, tx = c.do(http.MethodGet, "/transactions/9", owner, "", "")
		if tx["status"] == "COMMITTED" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	wantTx := map[string]any{
		"id": 9.0, "status": "COMMITTED", "op_count": 4.0,
		"operations": []any{
			map[string]any{"id": 64.0, "status": "APPLIED", "delete_of": 41.0},
			map[string]any{"id": 65.0, "status": "APPLIED", "delete_of": 44.0},
			map[string]any{"id": 66.0, "status": "APPLIED", "edit_of": 42.0},
			map[string]any{"id": 67.0, "status": "CONFIRMED", "revision": 1.0},
		},
	}
	if !reflect.DeepEqual(tx, wantTx) {
		t.Fatalf("GET /transactions/9 = %v, want %v", tx, wantTx)
	}

	// Targets 41 and 44 DELETED by their legs with their last values; 42 at its
	// next revision; group 7 still COMMITTED with leg 41 DELETED.
	_, op41 := c.getOp(41)
	if op41["status"] != "DELETED" || num(op41["deleted_by"]) != 64 || num(op41["revision"]) != 1 || num(op41["amount"]) != -1500 || num(op41["transaction_id"]) != 7 || op41["deleted_at"] == nil {
		t.Fatalf("GET /operations/41 = %v", op41)
	}
	_, op44 := c.getOp(44)
	if op44["status"] != "DELETED" || num(op44["deleted_by"]) != 65 || num(op44["revision"]) != 2 || num(op44["amount"]) != 200 {
		t.Fatalf("GET /operations/44 = %v", op44)
	}
	_, op42 := c.getOp(42)
	if op42["status"] != "CONFIRMED" || num(op42["amount"]) != -700 || num(op42["revision"]) != 2 {
		t.Fatalf("GET /operations/42 = %v", op42)
	}
	_, op64 := c.getOp(64)
	if op64["status"] != "APPLIED" || num(op64["delete_of"]) != 41 || num(op64["transaction_id"]) != 9 || op64["revision"] != nil {
		t.Fatalf("GET /operations/64 = %v", op64)
	}
	_, seven7 := c.do(http.MethodGet, "/transactions/7", owner, "", "")
	wantSeven := map[string]any{
		"id": 7.0, "status": "COMMITTED", "op_count": 2.0,
		"operations": []any{
			map[string]any{"id": 41.0, "status": "DELETED", "revision": 1.0},
			map[string]any{"id": 42.0, "status": "CONFIRMED", "revision": 2.0},
		},
	}
	if !reflect.DeepEqual(seven7, wantSeven) {
		t.Fatalf("GET /transactions/7 = %v, want %v", seven7, wantSeven)
	}
	// cash: 6200 + 1500 (41 gone) + 100 (42 −800 → −700) + 300 = 8100; wallet: 200 − 200 = 0.
	if bal := c.balance("cash", ""); bal != 8100 {
		t.Fatalf("cash balance = %d, want 8100", bal)
	}
	if bal := c.balance("wallet", ""); bal != 0 {
		t.Fatalf("wallet balance = %d, want 0", bal)
	}
	for _, e := range c.statement("cash") {
		if id := num(e["id"]); id == 41 || id == 64 || id == 66 {
			t.Fatalf("statement lists %d: %v", id, e)
		}
	}
	_, h := c.history(41)
	if h["status"] != "DELETED" || num(h["deleted_by"]) != 64 || len(h["revisions"].([]any)) != 1 {
		t.Fatalf("history of 41 = %v", h)
	}

	// Replay: same key, same body → 202 replayed with the current statuses.
	code, m = c.do(http.MethodPost, "/transactions", owner, idem, body)
	want["transaction_status"], want["replayed"] = "COMMITTED", true
	want["operations"] = []any{
		map[string]any{"id": 64.0, "status": "APPLIED", "delete_of": 41.0},
		map[string]any{"id": 65.0, "status": "APPLIED", "delete_of": 44.0},
		map[string]any{"id": 66.0, "status": "APPLIED", "edit_of": 42.0},
		map[string]any{"id": 67.0, "status": "CONFIRMED"},
	}
	if code != http.StatusAccepted || !reflect.DeepEqual(m, want) {
		t.Fatalf("replay = %d %v, want 202 %v", code, m, want)
	}

	// A violating unit of the same shape is REJECTED as a whole with one shared
	// reason and no target touched: vault at min 0 holds a +100 credit partly
	// spent (balance 60); deleting the credit lands at −40 whatever the other
	// items add.
	c.createAccount("vault", i64p(0))
	credit := c.confirm("vault", 100, "2026-03-01T09:00:00Z")
	c.confirm("vault", -40, "2026-03-01T10:00:00Z")
	_, before42 := c.getOp(42)
	_, beforeCredit := c.getOp(credit)
	body = fmt.Sprintf(`{"operations":[{"delete_of":%d},{"edit_of":42,"amount":-600},{"account":"cash","amount":5,"effective_at":"2026-03-02T10:00:00Z"}],"wait_ms":5000}`, credit)
	code, m = c.do(http.MethodPost, "/transactions", owner, c.key(), body)
	if code != http.StatusOK || m["transaction_status"] != "REJECTED" {
		t.Fatalf("violating group = %d %v, want 200 REJECTED", code, m)
	}
	for _, o := range m["operations"].([]any) {
		if o.(map[string]any)["status"] != "INVALID" {
			t.Fatalf("violating group item %v, want INVALID", o)
		}
	}
	_, tx = c.do(http.MethodGet, fmt.Sprintf("/transactions/%d", num(m["transaction_id"])), owner, "", "")
	rej, _ := tx["rejection"].(map[string]any)
	wantRej := map[string]any{"code": "LIMIT_VIOLATED", "account": "vault", "limit_side": "min", "shortfall": 40.0}
	if !reflect.DeepEqual(rej, wantRej) {
		t.Fatalf("rejection = %v, want %v", rej, wantRej)
	}
	for _, leg := range tx["operations"].([]any) {
		l := leg.(map[string]any)
		if l["status"] != "INVALID" || !reflect.DeepEqual(l["rejection"], rej) {
			t.Fatalf("rejected leg %v, want INVALID with the group's rejection", l)
		}
	}
	if _, after := c.getOp(credit); !reflect.DeepEqual(after, beforeCredit) {
		t.Fatalf("delete target changed by a rejected group: %v → %v", beforeCredit, after)
	}
	if _, after := c.getOp(42); !reflect.DeepEqual(after, before42) {
		t.Fatalf("42 changed by a rejected group: %v → %v", before42, after)
	}
	if bal := c.balance("vault", ""); bal != 60 {
		t.Fatalf("vault balance after the rejected group = %d, want 60", bal)
	}
	if bal := c.balance("cash", ""); bal != 8100 {
		t.Fatalf("cash balance after the rejected group = %d, want 8100", bal)
	}
}
