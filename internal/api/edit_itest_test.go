//go:build itest

// External test package (like wait_e2e_itest_test.go) so a real processor can
// decide the edits these tests register: processor imports api, so the tests
// cannot live in package api without an import cycle.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/notify"
	"github.com/magnomp/balancedb/internal/processor"
)

// Integration and end-to-end tests for the HTTP editing surface (ADR-0010):
// IT-030–IT-041, IT-060, IT-061, E2E-001, E2E-002 and E2E-003 of the
// operation-editing test contract. Every test runs a whole cell — the real ledger, processor and
// (when asked) notifier — over a throwaway schema, so what is asserted is the
// contract an integrator sees, not a seeded projection.

const (
	owner   = "7"
	stepKey = "4e1d0b1a-9a7c-4b2e-8f10-%012d"
)

// cell is one throwaway BalanceDB cell: schema, HTTP server, optional real
// notifier, and a processor that tests start and stop to control exactly when
// registrations are decided.
type cell struct {
	t     *testing.T
	pool  *pgxpool.Pool
	n     *notify.Notifier
	ts    *httptest.Server
	log   *slog.Logger
	stop  context.CancelFunc
	done  chan struct{}
	nextK int
}

// newCell builds the cell. withNotifier wires the real LISTEN/NOTIFY fan-out into
// the server; without it the wait path resolves by status poll only. The
// processor is not started — call startProcessor.
func newCell(t *testing.T, withNotifier bool) *cell {
	t.Helper()
	c := &cell{t: t, pool: dbtest.NewSchema(t)}
	c.log = slog.New(slog.NewTextHandler(e2eWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))

	// A nil *Notifier must not be passed as the interface (it would be a non-nil
	// interface holding nil), so the two constructions stay separate.
	srv := api.NewServer(c.pool, nil, nil)
	if withNotifier {
		nctx, ncancel := context.WithCancel(context.Background())
		t.Cleanup(ncancel)
		c.n = notify.New(c.pool, c.log)
		go func() { _ = c.n.Run(nctx) }()
		waitNotifierArmed(t, c.n)
		srv = api.NewServer(c.pool, nil, c.n)
	}
	c.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(c.ts.Close)
	return c
}

// startProcessor runs a real processor loop and returns once it holds the lease
// (and has armed its doorbell), so an insert after this point is decided promptly.
func (c *cell) startProcessor() {
	c.t.Helper()
	if c.stop != nil {
		c.t.Fatal("processor already running")
	}
	l, err := lease.New(c.pool)
	if err != nil {
		c.t.Fatalf("lease.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proc := processor.New(c.pool, l, c.log)
	c.done = make(chan struct{})
	c.stop = cancel
	go func() { _ = proc.Run(ctx); close(c.done) }()
	waitLeader(c.t, c.pool)
	time.Sleep(200 * time.Millisecond)
	c.t.Cleanup(c.stopProcessor)
}

// stopProcessor cancels the loop and waits for it to release the lease.
func (c *cell) stopProcessor() {
	if c.stop == nil {
		return
	}
	c.stop()
	<-c.done
	c.stop = nil
}

func (c *cell) key() string {
	c.nextK++
	return fmt.Sprintf(stepKey, c.nextK)
}

// do issues one request and returns the status and decoded JSON body ($schema
// stripped so bodies can be compared against the contract transcripts).
func (c *cell) do(method, path, ownerID, idem, body string) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.ts.URL+path, r)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if ownerID != "" {
		req.Header.Set("X-Owner-Id", ownerID)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.ts.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		c.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
	}
	delete(m, "$schema")
	return resp.StatusCode, m
}

// insert registers one regular operation (owner 7) and returns its id. With wait
// it expects the decided status; without it, 202 PENDING.
func (c *cell) insert(acct string, amount int64, eff string, waitMs int, wantStatus string) int64 {
	c.t.Helper()
	body := fmt.Sprintf(`{"operations":[{"account":%q,"amount":%d,"effective_at":%q}],"wait_ms":%d}`, acct, amount, eff, waitMs)
	code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), body)
	if (waitMs > 0 && code != http.StatusOK) || (waitMs == 0 && code != http.StatusAccepted) {
		c.t.Fatalf("POST /transactions: status %d, body %v", code, m)
	}
	op := m["operations"].([]any)[0].(map[string]any)
	if op["status"] != wantStatus {
		c.t.Fatalf("POST /transactions: status %v, want %s", op["status"], wantStatus)
	}
	return int64(op["id"].(float64))
}

// confirm inserts and waits for CONFIRMED (the processor must be running).
func (c *cell) confirm(acct string, amount int64, eff string) int64 {
	c.t.Helper()
	return c.insert(acct, amount, eff, 5000, "CONFIRMED")
}

// patch edits an operation and returns the status code and the edit object.
func (c *cell) patch(id int64, body string) (int, map[string]any, map[string]any) {
	c.t.Helper()
	return c.patchAs(owner, c.key(), id, body)
}

func (c *cell) patchAs(ownerID, idem string, id int64, body string) (int, map[string]any, map[string]any) {
	c.t.Helper()
	code, m := c.do(http.MethodPatch, fmt.Sprintf("/operations/%d", id), ownerID, idem, body)
	edit, _ := m["edit"].(map[string]any)
	return code, edit, m
}

// editApplied edits with a wait and requires 200 APPLIED; returns the edit id.
func (c *cell) editApplied(id int64, fields string) int64 {
	c.t.Helper()
	code, edit, m := c.patch(id, fmt.Sprintf(`{%s,"wait_ms":5000}`, fields))
	if code != http.StatusOK || edit["status"] != "APPLIED" {
		c.t.Fatalf("PATCH %d %s: status %d, body %v; want 200 APPLIED", id, fields, code, m)
	}
	return int64(edit["id"].(float64))
}

func (c *cell) getOp(id int64) (int, map[string]any) {
	c.t.Helper()
	return c.do(http.MethodGet, fmt.Sprintf("/operations/%d", id), owner, "", "")
}

func (c *cell) history(id int64) (int, map[string]any) {
	c.t.Helper()
	return c.do(http.MethodGet, fmt.Sprintf("/operations/%d/history", id), owner, "", "")
}

func (c *cell) balance(acct, at string) int64 {
	c.t.Helper()
	path := "/accounts/" + acct + "/balance"
	if at != "" {
		path += "?at=" + at
	}
	code, m := c.do(http.MethodGet, path, owner, "", "")
	if code != http.StatusOK {
		c.t.Fatalf("GET %s: status %d, body %v", path, code, m)
	}
	return int64(m["balance"].(float64))
}

func (c *cell) statement(acct string) []map[string]any {
	c.t.Helper()
	code, m := c.do(http.MethodGet, "/accounts/"+acct+"/statement", owner, "", "")
	if code != http.StatusOK {
		c.t.Fatalf("GET statement: status %d, body %v", code, m)
	}
	var out []map[string]any
	for _, e := range m["entries"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// waitStatus polls GET /operations/{id} until it reports status (or fails after 5s).
func (c *cell) waitStatus(id int64, status string) map[string]any {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, m := c.getOp(id)
		if m["status"] == status {
			return m
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("operation %d: status %v, want %s within 5s", id, m["status"], status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *cell) exec(sql string, args ...any) {
	c.t.Helper()
	if _, err := c.pool.Exec(context.Background(), sql, args...); err != nil {
		c.t.Fatalf("exec %s: %v", sql, err)
	}
}

func (c *cell) createAccount(ext string, minB *int64) {
	c.t.Helper()
	body := fmt.Sprintf(`{"external_id":%q}`, ext)
	if minB != nil {
		body = fmt.Sprintf(`{"external_id":%q,"min_balance":%d}`, ext, *minB)
	}
	if code, m := c.do(http.MethodPost, "/accounts", owner, "", body); code != http.StatusCreated {
		c.t.Fatalf("POST /accounts: status %d, body %v", code, m)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := keys(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: keys %v, want %v (body %v)", what, got, want, m)
	}
}

func num(v any) int64 { return int64(v.(float64)) }

func i64p(v int64) *int64 { return &v }

// --- IT-030 ------------------------------------------------------------------

// IT-030: a PATCH against another owner's operation is 404 with the same body as
// an unknown id — the API never reveals that the id exists.
func TestEditForeignTargetIs404(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")

	codeForeign, _, foreign := c.patchAs("8", c.key(), target, `{"amount":-1200}`)
	codeUnknown, _, unknown := c.patchAs(owner, c.key(), 999999, `{"amount":-1200}`)
	if codeForeign != http.StatusNotFound || codeUnknown != http.StatusNotFound {
		t.Fatalf("status foreign=%d unknown=%d, want 404/404", codeForeign, codeUnknown)
	}
	if !reflect.DeepEqual(foreign, unknown) {
		t.Fatalf("bodies differ: foreign %v, unknown %v", foreign, unknown)
	}
	if foreign["detail"] != "operation not found" {
		t.Fatalf("detail = %v, want %q", foreign["detail"], "operation not found")
	}
	// Nothing was registered for either request.
	var n int
	if err := c.pool.QueryRow(context.Background(), `SELECT count(*) FROM operations WHERE edit_of IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("edit rows = %d (%v), want 0", n, err)
	}
}

// --- IT-031 ------------------------------------------------------------------

// IT-031: after two applied edits the history holds three ordered revisions; each
// superseded one names its edit and instant, the current one carries neither, and
// the instant a revision was superseded is exactly when the next became current.
func TestEditHistoryAfterTwoAppliedEdits(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	e1 := c.editApplied(target, `"amount":-1200`)
	e2 := c.editApplied(target, `"account":"wallet"`)

	code, h := c.history(target)
	if code != http.StatusOK {
		t.Fatalf("history: status %d, body %v", code, h)
	}
	wantKeys(t, "history", h, "operation_id", "status", "current_revision", "revisions", "pending_edits", "rejected_edits")
	if num(h["operation_id"]) != target || h["status"] != "CONFIRMED" || num(h["current_revision"]) != 3 {
		t.Fatalf("head = %v", h)
	}
	if len(h["pending_edits"].([]any)) != 0 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("lists must be empty: %v", h)
	}
	revs := h["revisions"].([]any)
	if len(revs) != 3 {
		t.Fatalf("revisions = %d, want 3: %v", len(revs), revs)
	}
	r1, r2, r3 := revs[0].(map[string]any), revs[1].(map[string]any), revs[2].(map[string]any)
	wantKeys(t, "revision 1", r1, "revision", "account", "amount", "effective_at", "recorded_at", "superseded_at", "superseded_by")
	wantKeys(t, "revision 2", r2, "revision", "account", "amount", "effective_at", "recorded_at", "superseded_at", "superseded_by")
	wantKeys(t, "revision 3", r3, "revision", "account", "amount", "effective_at", "recorded_at")
	if num(r1["revision"]) != 1 || r1["account"] != "cash" || num(r1["amount"]) != -1500 || num(r1["superseded_by"]) != e1 {
		t.Fatalf("revision 1 = %v", r1)
	}
	if num(r2["revision"]) != 2 || r2["account"] != "cash" || num(r2["amount"]) != -1200 || num(r2["superseded_by"]) != e2 {
		t.Fatalf("revision 2 = %v", r2)
	}
	if num(r3["revision"]) != 3 || r3["account"] != "wallet" || num(r3["amount"]) != -1200 {
		t.Fatalf("revision 3 = %v", r3)
	}
	for _, r := range []map[string]any{r1, r2, r3} {
		if r["effective_at"] != "2026-03-01T10:00:00Z" {
			t.Fatalf("effective_at = %v, want unchanged", r["effective_at"])
		}
	}
	// Continuity: superseded_at of N == recorded_at of N+1 (same deciding
	// transaction, same now()), and recorded_at never goes backwards.
	if r1["superseded_at"] != r2["recorded_at"] || r2["superseded_at"] != r3["recorded_at"] {
		t.Fatalf("instants are not continuous: %v / %v / %v", r1, r2, r3)
	}
	if r1["recorded_at"].(string) > r2["recorded_at"].(string) {
		t.Fatalf("recorded_at not monotonic: %v then %v", r1["recorded_at"], r2["recorded_at"])
	}
}

// --- IT-033 ------------------------------------------------------------------

// IT-033: a PENDING edit is listed under pending_edits, leaves the list once
// decided, an INVALID edit lands under rejected_edits with its rejection and
// decided_at, and an APPLIED edit is only ever visible as superseded_by.
func TestEditHistoryPendingAndRejectedLists(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	c.stopProcessor()

	code, edit, _ := c.patch(target, `{"amount":-1200,"expected_revision":1}`)
	if code != http.StatusAccepted || edit["status"] != "PENDING" {
		t.Fatalf("PATCH: status %d edit %v, want 202 PENDING", code, edit)
	}
	e1 := num(edit["id"])

	_, h := c.history(target)
	pend := h["pending_edits"].([]any)
	if len(pend) != 1 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("history while pending = %v", h)
	}
	p := pend[0].(map[string]any)
	wantKeys(t, "pending edit", p, "id", "kind", "account", "amount", "effective_at", "expected_revision")
	if num(p["id"]) != e1 || p["kind"] != "edit" || p["account"] != "cash" || num(p["amount"]) != -1200 || p["effective_at"] != "2026-03-01T10:00:00Z" || num(p["expected_revision"]) != 1 {
		t.Fatalf("pending edit = %v", p)
	}

	c.startProcessor()
	c.waitStatus(e1, "APPLIED")
	_, h = c.history(target)
	if len(h["pending_edits"].([]any)) != 0 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("history after apply = %v", h)
	}
	revs := h["revisions"].([]any)
	if len(revs) != 2 || num(revs[0].(map[string]any)["superseded_by"]) != e1 {
		t.Fatalf("revisions after apply = %v", revs)
	}

	// A stale guard: rejected, listed with its rejection and decision instant.
	code, edit, _ = c.patch(target, `{"amount":20,"expected_revision":1,"wait_ms":5000}`)
	if code != http.StatusOK || edit["status"] != "INVALID" {
		t.Fatalf("stale PATCH: status %d edit %v, want 200 INVALID", code, edit)
	}
	e2 := num(edit["id"])
	_, h = c.history(target)
	rej := h["rejected_edits"].([]any)
	if len(rej) != 1 || len(h["pending_edits"].([]any)) != 0 {
		t.Fatalf("history after rejection = %v", h)
	}
	r := rej[0].(map[string]any)
	wantKeys(t, "rejected edit", r, "id", "kind", "account", "amount", "effective_at", "expected_revision", "decided_at", "rejection")
	if num(r["id"]) != e2 || r["kind"] != "edit" || num(r["amount"]) != 20 || num(r["expected_revision"]) != 1 {
		t.Fatalf("rejected edit = %v", r)
	}
	if decided, _ := time.Parse(time.RFC3339Nano, r["decided_at"].(string)); decided.IsZero() || time.Since(decided) > time.Minute {
		t.Fatalf("decided_at = %v, want a recent instant", r["decided_at"])
	}
	rj := r["rejection"].(map[string]any)
	if rj["code"] != "STALE_REVISION" || num(rj["operation_id"]) != target || num(rj["expected_revision"]) != 1 || num(rj["actual_revision"]) != 2 {
		t.Fatalf("rejection = %v", rj)
	}
	// The applied edit appears nowhere but as superseded_by.
	for _, list := range []string{"pending_edits", "rejected_edits"} {
		for _, e := range h[list].([]any) {
			if num(e.(map[string]any)["id"]) == e1 {
				t.Fatalf("applied edit %d listed under %s", e1, list)
			}
		}
	}
}

// --- IT-034 / IT-035 -------------------------------------------------------------

// IT-034: the history of an edit registration id is 404.
func TestEditHistoryOfEditIdIs404(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	e1 := c.editApplied(target, `"amount":-1200`)
	if code, m := c.history(e1); code != http.StatusNotFound || m["detail"] != "operation not found" {
		t.Fatalf("history of edit %d: status %d, body %v; want 404", e1, code, m)
	}
	if code, _ := c.history(target); code != http.StatusOK {
		t.Fatalf("history of target: status %d, want 200", code)
	}
	// And another owner's history is 404 too.
	if code, _ := c.do(http.MethodGet, fmt.Sprintf("/operations/%d/history", target), "8", "", ""); code != http.StatusNotFound {
		t.Fatalf("foreign history: status %d, want 404", code)
	}
}

// IT-035: an INVALID operation has a history: one revision, status INVALID.
func TestEditHistoryOfInvalidOperation(t *testing.T) {
	c := newCell(t, false)
	c.createAccount("cash", i64p(0))
	c.startProcessor()
	op := c.insert("cash", -100, "2026-03-01T10:00:00Z", 5000, "INVALID")
	code, h := c.history(op)
	if code != http.StatusOK || h["status"] != "INVALID" || num(h["current_revision"]) != 1 {
		t.Fatalf("history: status %d, body %v", code, h)
	}
	revs := h["revisions"].([]any)
	if len(revs) != 1 || num(revs[0].(map[string]any)["revision"]) != 1 || num(revs[0].(map[string]any)["amount"]) != -100 {
		t.Fatalf("revisions = %v", revs)
	}
	if len(h["pending_edits"].([]any)) != 0 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("lists must be empty: %v", h)
	}
}

// --- IT-036 ------------------------------------------------------------------

// IT-036: PATCH with wait_ms resolves through NOTIFY (fast, well under the loop
// interval), through the status poll when no notifier is wired, and expires to
// 202 PENDING when nothing decides.
func TestEditWaitPaths(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	// Raise loop_interval so only the doorbell + NOTIFY fast path can be quick.
	c.exec(`UPDATE config SET loop_interval_ms = 10000, lease_ttl_ms = 30000`)
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	code, edit, _ := c.patch(target, `{"amount":-1200,"wait_ms":9000}`)
	if code != http.StatusOK || edit["status"] != "APPLIED" {
		t.Fatalf("notify path: status %d edit %v, want 200 APPLIED", code, edit)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("notify path took %s with loop_interval=10s", el)
	}

	// Poll fallback: a server without a notifier over the same cell.
	polled := api.NewServer(c.pool, nil, nil)
	pts := httptest.NewServer(polled.Handler())
	defer pts.Close()
	req, _ := http.NewRequest(http.MethodPatch, pts.URL+fmt.Sprintf("/operations/%d", target), strings.NewReader(`{"amount":-1100,"wait_ms":9000}`))
	req.Header.Set("X-Owner-Id", owner)
	req.Header.Set("Idempotency-Key", c.key())
	req.Header.Set("Content-Type", "application/json")
	resp, err := pts.Client().Do(req)
	if err != nil {
		t.Fatalf("poll PATCH: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"status":"APPLIED"`) {
		t.Fatalf("poll path: status %d body %s, want 200 APPLIED", resp.StatusCode, b)
	}

	// Expiry: no processor → still PENDING after the budget.
	c.stopProcessor()
	code, edit, m := c.patch(target, `{"amount":-1000,"wait_ms":300}`)
	if code != http.StatusAccepted || edit["status"] != "PENDING" || m["replayed"] != false {
		t.Fatalf("expiry: status %d body %v, want 202 PENDING", code, m)
	}
}

// --- IT-037 ------------------------------------------------------------------

// IT-037: GET /operations/{id} shapes for a regular row and for edit rows are
// exactly the contract's: regular rows carry account/amount/effective_at/revision
// and never edit_of; edit rows carry edit_of and expected_revision, never
// revision, and the rejection once INVALID.
func TestEditGetOperationShapes(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")

	_, m := c.getOp(target)
	wantKeys(t, "regular op", m, "id", "status", "account", "amount", "effective_at", "revision")
	if m["status"] != "CONFIRMED" || m["account"] != "cash" || num(m["amount"]) != -1500 || m["effective_at"] != "2026-03-01T10:00:00Z" || num(m["revision"]) != 1 {
		t.Fatalf("regular op = %v", m)
	}

	code, edit, _ := c.patch(target, `{"amount":-1200,"expected_revision":1,"wait_ms":5000}`)
	if code != http.StatusOK || edit["status"] != "APPLIED" {
		t.Fatalf("PATCH: status %d edit %v", code, edit)
	}
	wantKeys(t, "edit outcome", edit, "id", "status", "operation_id")
	e1 := num(edit["id"])
	_, m = c.getOp(e1)
	wantKeys(t, "applied edit", m, "id", "status", "edit_of", "account", "amount", "effective_at", "expected_revision")
	if m["status"] != "APPLIED" || num(m["edit_of"]) != target || m["account"] != "cash" || num(m["amount"]) != -1200 || m["effective_at"] != "2026-03-01T10:00:00Z" || num(m["expected_revision"]) != 1 {
		t.Fatalf("applied edit = %v", m)
	}

	_, m = c.getOp(target)
	if num(m["revision"]) != 2 || num(m["amount"]) != -1200 {
		t.Fatalf("target after apply = %v", m)
	}

	code, edit, _ = c.patch(target, `{"amount":20,"expected_revision":1,"wait_ms":5000}`)
	if code != http.StatusOK || edit["status"] != "INVALID" {
		t.Fatalf("stale PATCH: status %d edit %v", code, edit)
	}
	wantKeys(t, "invalid edit outcome", edit, "id", "status", "operation_id", "rejection")
	e2 := num(edit["id"])
	_, m = c.getOp(e2)
	wantKeys(t, "invalid edit", m, "id", "status", "edit_of", "account", "amount", "effective_at", "expected_revision", "rejection")
	rj := m["rejection"].(map[string]any)
	wantKeys(t, "stale rejection", rj, "code", "operation_id", "expected_revision", "actual_revision")
	if rj["code"] != "STALE_REVISION" || num(rj["operation_id"]) != target || num(rj["expected_revision"]) != 1 || num(rj["actual_revision"]) != 2 {
		t.Fatalf("rejection = %v", rj)
	}

	// An edit without a guard omits expected_revision.
	e3 := c.editApplied(target, `"effective_at":"2026-03-02T10:00:00Z"`)
	_, m = c.getOp(e3)
	wantKeys(t, "unguarded edit", m, "id", "status", "edit_of", "account", "amount", "effective_at")
}

// --- IT-038 / IT-039 ---------------------------------------------------------

// IT-038: after an effective_at move the statement shows the operation once, at
// its new position, with its revision; running balances agree with the
// point-in-time balances on the old day, the days between and the new day.
func TestEditStatementAfterMove(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	op1 := c.confirm("cash", 1000, "2026-03-01T09:00:00Z")
	op2 := c.confirm("cash", -300, "2026-03-02T09:00:00Z")
	op3 := c.confirm("cash", 500, "2026-03-04T09:00:00Z")
	c.editApplied(op2, `"effective_at":"2026-03-05T10:00:00Z"`)

	entries := c.statement("cash")
	if len(entries) != 3 {
		t.Fatalf("entries = %v, want 3", entries)
	}
	type want struct {
		id, running, rev int64
		eff              string
	}
	wants := []want{{op1, 1000, 1, "2026-03-01T09:00:00Z"}, {op3, 1500, 1, "2026-03-04T09:00:00Z"}, {op2, 1200, 2, "2026-03-05T10:00:00Z"}}
	for i, w := range wants {
		e := entries[i]
		if num(e["id"]) != w.id || num(e["running_balance"]) != w.running || num(e["revision"]) != w.rev || e["effective_at"] != w.eff {
			t.Fatalf("entry %d = %v, want %+v", i, e, w)
		}
	}
	for at, want := range map[string]int64{
		"2026-03-01T23:59:59Z": 1000, // old day: the move is gone
		"2026-03-02T23:59:59Z": 1000,
		"2026-03-03T12:00:00Z": 1000,
		"2026-03-04T23:59:59Z": 1500,
		"2026-03-05T23:59:59Z": 1200, // new day
	} {
		if got := c.balance("cash", at); got != want {
			t.Fatalf("balance at %s = %d, want %d", at, got, want)
		}
	}
	if got := c.balance("cash", ""); got != 1200 {
		t.Fatalf("final balance = %d, want 1200", got)
	}
}

// IT-039: edit registrations (pending, applied or rejected) never appear in the
// statement or in any balance sum; the edited operation appears once with its
// current amount.
func TestEditRegistrationsNeverInStatementOrSums(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	op := c.confirm("cash", 1000, "2026-03-01T09:00:00Z")
	applied := c.editApplied(op, `"amount":1200`)
	_, rejected, _ := c.patch(op, `{"amount":900,"expected_revision":1,"wait_ms":5000}`)
	c.stopProcessor()
	_, pending, _ := c.patch(op, `{"amount":800}`)
	editIDs := map[int64]bool{applied: true, num(rejected["id"]): true, num(pending["id"]): true}

	entries := c.statement("cash")
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want exactly the edited operation", entries)
	}
	if e := entries[0]; num(e["id"]) != op || num(e["amount"]) != 1200 || num(e["revision"]) != 2 || num(e["running_balance"]) != 1200 {
		t.Fatalf("entry = %v", e)
	}
	for _, e := range entries {
		if editIDs[num(e["id"])] {
			t.Fatalf("edit registration %v listed in the statement", e["id"])
		}
	}
	if got := c.balance("cash", ""); got != 1200 {
		t.Fatalf("final balance = %d, want 1200", got)
	}
	if got := c.balance("cash", "2026-03-01T23:59:59Z"); got != 1200 {
		t.Fatalf("point-in-time balance = %d, want 1200", got)
	}
}

// --- IT-040 / IT-041 ---------------------------------------------------------

// IT-040: an edit of a PENDING target is accepted (202) and, once the processor
// runs, the target is decided first and the edit applied after it.
func TestEditPendingTargetAppliedAfterTarget(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	code, edit, _ := c.patch(target, `{"amount":-1200}`)
	if code != http.StatusAccepted || edit["status"] != "PENDING" {
		t.Fatalf("PATCH pending target: status %d edit %v, want 202 PENDING", code, edit)
	}
	e1 := num(edit["id"])
	if e1 <= target {
		t.Fatalf("edit id %d must follow target id %d", e1, target)
	}

	c.startProcessor()
	c.waitStatus(e1, "APPLIED")
	_, m := c.getOp(target)
	if m["status"] != "CONFIRMED" || num(m["revision"]) != 2 || num(m["amount"]) != -1200 {
		t.Fatalf("target = %v, want CONFIRMED at revision 2 with -1200", m)
	}
	var targetAt, editAt time.Time
	if err := c.pool.QueryRow(context.Background(),
		`SELECT t.confirmed_at, e.confirmed_at FROM operations t, operations e WHERE t.id = $1 AND e.id = $2`, target, e1).
		Scan(&targetAt, &editAt); err != nil {
		t.Fatalf("read decision instants: %v", err)
	}
	if editAt.Before(targetAt) {
		t.Fatalf("edit decided at %s before its target at %s", editAt, targetAt)
	}
}

// IT-041: when the target ends INVALID the edit is rejected TARGET_NOT_EDITABLE.
func TestEditTargetInvalidRejectsEdit(t *testing.T) {
	c := newCell(t, false)
	c.createAccount("cash", i64p(0))
	target := c.insert("cash", -100, "2026-03-01T10:00:00Z", 0, "PENDING")
	code, edit, _ := c.patch(target, `{"amount":-50}`)
	if code != http.StatusAccepted {
		t.Fatalf("PATCH: status %d edit %v", code, edit)
	}
	e1 := num(edit["id"])

	c.startProcessor()
	m := c.waitStatus(e1, "INVALID")
	rj := m["rejection"].(map[string]any)
	wantKeys(t, "rejection", rj, "code", "operation_id")
	if rj["code"] != "TARGET_NOT_EDITABLE" || num(rj["operation_id"]) != target {
		t.Fatalf("rejection = %v", rj)
	}
	if _, tm := c.getOp(target); tm["status"] != "INVALID" || num(tm["revision"]) != 1 || num(tm["amount"]) != -100 {
		t.Fatalf("target = %v, want INVALID, untouched", tm)
	}
	_, h := c.history(target)
	if h["status"] != "INVALID" || len(h["rejected_edits"].([]any)) != 1 {
		t.Fatalf("history = %v", h)
	}
}

// --- IT-060 / IT-061 ---------------------------------------------------------

// IT-060: allow_edits = false refuses PATCH with 403 and the contract's message;
// plain inserts are unaffected.
func TestEditsDisabledPolicy(t *testing.T) {
	c := newCell(t, false)
	target := c.insert("cash", -1500, "2026-03-01T10:00:00Z", 0, "PENDING")
	c.exec(`UPDATE config SET allow_edits = false`)

	code, _, m := c.patch(target, `{"amount":-1200}`)
	if code != http.StatusForbidden || m["detail"] != "editing is disabled for this cell" {
		t.Fatalf("PATCH with edits disabled: status %d body %v, want 403", code, m)
	}
	// A group with an edit item is refused the same way — before any row is written.
	groupBody := fmt.Sprintf(`{"operations":[{"edit_of":%d,"amount":-1200},{"account":"cash","amount":5,"effective_at":"2026-03-01T12:00:00Z"}]}`, target)
	if code, m := c.do(http.MethodPost, "/transactions", owner, c.key(), groupBody); code != http.StatusForbidden || m["detail"] != "editing is disabled for this cell" {
		t.Fatalf("POST /transactions with an edit item and edits disabled: status %d body %v, want 403", code, m)
	}
	var txCount int
	if err := c.pool.QueryRow(context.Background(), `SELECT count(*) FROM transactions`).Scan(&txCount); err != nil || txCount != 0 {
		t.Fatalf("refused group left %d transactions rows (%v)", txCount, err)
	}
	c.insert("cash", 10, "2026-03-01T11:00:00Z", 0, "PENDING")

	c.exec(`UPDATE config SET allow_edits = true`)
	if code, edit, _ := c.patch(target, `{"amount":-1200}`); code != http.StatusAccepted || edit["status"] != "PENDING" {
		t.Fatalf("PATCH after re-enable: status %d edit %v, want 202", code, edit)
	}
}

// IT-061: an edit registered before the flag flips to false is still decided.
func TestEditRegisteredBeforeDisableStillDecided(t *testing.T) {
	c := newCell(t, false)
	c.startProcessor()
	target := c.confirm("cash", -1500, "2026-03-01T10:00:00Z")
	c.stopProcessor()

	_, edit, _ := c.patch(target, `{"amount":-1200}`)
	e1 := num(edit["id"])
	c.exec(`UPDATE config SET allow_edits = false`)

	c.startProcessor()
	c.waitStatus(e1, "APPLIED")
	if _, m := c.getOp(target); num(m["revision"]) != 2 || num(m["amount"]) != -1200 {
		t.Fatalf("target = %v, want revision 2 with -1200", m)
	}
}

// --- E2E-001 / E2E-002 -------------------------------------------------------

// E2E-001: the _dx.md Golden Path verbatim. The identity sequence is restarted so
// the ids are the transcript's (41 for the operation, 57 for the edit).
func TestGoldenPath(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 41`)
	if id := c.confirm("cash", -1500, "2026-03-01T10:00:00Z"); id != 41 {
		t.Fatalf("operation id = %d, want 41", id)
	}

	// The operation as it stands today (id 41, confirmed).
	_, m := c.getOp(41)
	want := map[string]any{"id": 41.0, "status": "CONFIRMED", "account": "cash", "amount": -1500.0, "effective_at": "2026-03-01T10:00:00Z", "revision": 1.0}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("GET /operations/41 = %v, want %v", m, want)
	}
	if bal := c.balance("cash", ""); bal != -1500 {
		t.Fatalf("balance = %d, want -1500", bal)
	}

	// Edit it in place. Same id, new amount. Optional wait for the decision.
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 57`)
	const idem = "4e1d0b1a-9a7c-4b2e-8f10-2c9d5b6a7e01"
	code, _, body := c.patchAs(owner, idem, 41, `{"amount": -1200, "wait_ms": 2000}`)
	wantBody := map[string]any{"edit": map[string]any{"id": 57.0, "status": "APPLIED", "operation_id": 41.0}, "replayed": false}
	if code != http.StatusOK || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("PATCH = %d %v, want 200 %v", code, body, wantBody)
	}

	// The operation now reads the new value at revision 2; the balance moved by +300.
	_, m = c.getOp(41)
	want["amount"], want["revision"] = -1200.0, 2.0
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("GET /operations/41 after edit = %v, want %v", m, want)
	}
	if bal := c.balance("cash", ""); bal != -1200 {
		t.Fatalf("balance = %d, want -1200", bal)
	}

	// The old value is still there, append-only.
	_, h := c.history(41)
	if num(h["operation_id"]) != 41 || h["status"] != "CONFIRMED" || num(h["current_revision"]) != 2 {
		t.Fatalf("history head = %v", h)
	}
	revs := h["revisions"].([]any)
	if len(revs) != 2 {
		t.Fatalf("revisions = %v", revs)
	}
	r1, r2 := revs[0].(map[string]any), revs[1].(map[string]any)
	if num(r1["revision"]) != 1 || r1["account"] != "cash" || num(r1["amount"]) != -1500 || r1["effective_at"] != "2026-03-01T10:00:00Z" || num(r1["superseded_by"]) != 57 || r1["superseded_at"] == nil || r1["recorded_at"] == nil {
		t.Fatalf("revision 1 = %v", r1)
	}
	wantKeys(t, "revision 2", r2, "revision", "account", "amount", "effective_at", "recorded_at")
	if num(r2["revision"]) != 2 || r2["account"] != "cash" || num(r2["amount"]) != -1200 || r2["effective_at"] != "2026-03-01T10:00:00Z" || r2["recorded_at"] != r1["superseded_at"] {
		t.Fatalf("revision 2 = %v", r2)
	}
	if len(h["pending_edits"].([]any)) != 0 || len(h["rejected_edits"].([]any)) != 0 {
		t.Fatalf("lists = %v", h)
	}

	// Idempotent replay: same key, same edit → replayed with the current status:
	// 202 without a wait (wait_ms is not part of the payload), 200 with one since
	// the outcome is already decided — exactly the POST /transactions semantics.
	code, _, body = c.patchAs(owner, idem, 41, `{"amount": -1200}`)
	wantBody["replayed"] = true
	if code != http.StatusAccepted || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("replay = %d %v, want 202 %v", code, body, wantBody)
	}
	if code, _, body = c.patchAs(owner, idem, 41, `{"amount": -1200, "wait_ms": 2000}`); code != http.StatusOK || !reflect.DeepEqual(body, wantBody) {
		t.Fatalf("replay with wait = %d %v, want 200 %v", code, body, wantBody)
	}
	// Same key, different body → 422 payload conflict.
	if code, _, body = c.patchAs(owner, idem, 41, `{"amount": -1100}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("payload conflict = %d %v, want 422", code, body)
	}
}

// E2E-002: with cash at min 0, an edit whose delta would breach the limit is
// rejected LIMIT_VIOLATED synchronously and the operation is unchanged.
func TestRejectedEdit(t *testing.T) {
	c := newCell(t, true)
	c.createAccount("cash", i64p(0))
	c.startProcessor()
	c.confirm("cash", 1000, "2026-03-01T09:00:00Z")
	op := c.confirm("cash", -700, "2026-03-01T10:00:00Z") // balance 300

	code, edit, body := c.patch(op, `{"amount":-1200,"wait_ms":5000}`)
	if code != http.StatusOK || edit["status"] != "INVALID" || num(edit["operation_id"]) != op || body["replayed"] != false {
		t.Fatalf("PATCH = %d %v, want 200 INVALID", code, body)
	}
	rj := edit["rejection"].(map[string]any)
	wantRej := map[string]any{"code": "LIMIT_VIOLATED", "account": "cash", "limit_side": "min", "shortfall": 200.0}
	if !reflect.DeepEqual(rj, wantRej) {
		t.Fatalf("rejection = %v, want %v", rj, wantRej)
	}

	_, m := c.getOp(op)
	if m["status"] != "CONFIRMED" || num(m["amount"]) != -700 || num(m["revision"]) != 1 {
		t.Fatalf("target = %v, want unchanged", m)
	}
	if bal := c.balance("cash", ""); bal != 300 {
		t.Fatalf("balance = %d, want 300", bal)
	}
	_, h := c.history(op)
	if len(h["revisions"].([]any)) != 1 || len(h["rejected_edits"].([]any)) != 1 {
		t.Fatalf("history = %v", h)
	}
}

// --- E2E-003 -----------------------------------------------------------------

// E2E-003: the _dx.md POST /transactions transcript verbatim — two edits and one
// new operation as one atomic unit. The identity sequences are restarted so the
// ids are the transcript's (41/42 for the targets, 9 for the group, 58–60 for
// its items): 202 with edit_of per outcome, then GET /transactions/9 COMMITTED
// with APPLIED edit legs (edit_of) and a CONFIRMED regular leg (revision 1); the
// targets read their new values at revision 2 and the balances moved by the net.
func TestGroupedEditsTranscript(t *testing.T) {
	c := newCell(t, true)
	c.startProcessor()
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 41`)
	if id := c.confirm("cash", -1500, "2026-03-01T10:00:00Z"); id != 41 {
		t.Fatalf("operation id = %d, want 41", id)
	}
	if id := c.confirm("wallet", 1500, "2026-02-09T18:00:00Z"); id != 42 {
		t.Fatalf("operation id = %d, want 42", id)
	}
	c.exec(`ALTER TABLE operations ALTER COLUMN id RESTART WITH 58`)
	c.exec(`ALTER TABLE transactions ALTER COLUMN id RESTART WITH 9`)

	const idem = "0b6f1c8e-2d3a-4f5b-9c7d-1e2f3a4b5c6d"
	body := `{
  "operations": [
    {"edit_of": 41, "amount": -1200},
    {"edit_of": 42, "effective_at": "2026-02-10T09:30:00Z", "expected_revision": 1},
    {"account": "cash", "amount": 300, "effective_at": "2026-03-01T10:00:00Z"}
  ],
  "wait_ms": 0}`
	code, m := c.do(http.MethodPost, "/transactions", owner, idem, body)
	want := map[string]any{
		"transaction_id": 9.0, "transaction_status": "PENDING",
		"operations": []any{
			map[string]any{"id": 58.0, "status": "PENDING", "edit_of": 41.0},
			map[string]any{"id": 59.0, "status": "PENDING", "edit_of": 42.0},
			map[string]any{"id": 60.0, "status": "PENDING"},
		},
		"replayed": false,
	}
	if code != http.StatusAccepted || !reflect.DeepEqual(m, want) {
		t.Fatalf("POST /transactions = %d %v, want 202 %v", code, m, want)
	}

	// When decided: COMMITTED, edit items APPLIED, the new item CONFIRMED.
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
		"id": 9.0, "status": "COMMITTED", "op_count": 3.0,
		"operations": []any{
			map[string]any{"id": 58.0, "status": "APPLIED", "edit_of": 41.0},
			map[string]any{"id": 59.0, "status": "APPLIED", "edit_of": 42.0},
			map[string]any{"id": 60.0, "status": "CONFIRMED", "revision": 1.0},
		},
	}
	if !reflect.DeepEqual(tx, wantTx) {
		t.Fatalf("GET /transactions/9 = %v, want %v", tx, wantTx)
	}

	// The targets read their new state at revision 2; balances moved by the net
	// (cash: +300 from the edit, +300 from the new operation; wallet: unchanged).
	_, op41 := c.getOp(41)
	if num(op41["amount"]) != -1200 || num(op41["revision"]) != 2 || op41["effective_at"] != "2026-03-01T10:00:00Z" {
		t.Fatalf("GET /operations/41 = %v", op41)
	}
	_, op42 := c.getOp(42)
	if num(op42["amount"]) != 1500 || num(op42["revision"]) != 2 || op42["effective_at"] != "2026-02-10T09:30:00Z" {
		t.Fatalf("GET /operations/42 = %v", op42)
	}
	_, op60 := c.getOp(60)
	if op60["status"] != "CONFIRMED" || num(op60["transaction_id"]) != 9 || num(op60["revision"]) != 1 {
		t.Fatalf("GET /operations/60 = %v", op60)
	}
	if bal := c.balance("cash", ""); bal != -900 {
		t.Fatalf("cash balance = %d, want -900", bal)
	}
	if bal := c.balance("wallet", ""); bal != 1500 {
		t.Fatalf("wallet balance = %d, want 1500", bal)
	}
	_, h := c.history(42)
	revs := h["revisions"].([]any)
	if len(revs) != 2 || num(revs[0].(map[string]any)["superseded_by"]) != 59 || revs[1].(map[string]any)["effective_at"] != "2026-02-10T09:30:00Z" {
		t.Fatalf("history of 42 = %v", h)
	}

	// Replay: same key, same body → 202 replayed with the current statuses
	// (outcomes carry edit_of, never revision — that is a leg-read field).
	code, m = c.do(http.MethodPost, "/transactions", owner, idem, body)
	want["transaction_status"], want["replayed"] = "COMMITTED", true
	want["operations"] = []any{
		map[string]any{"id": 58.0, "status": "APPLIED", "edit_of": 41.0},
		map[string]any{"id": 59.0, "status": "APPLIED", "edit_of": 42.0},
		map[string]any{"id": 60.0, "status": "CONFIRMED"},
	}
	if code != http.StatusAccepted || !reflect.DeepEqual(m, want) {
		t.Fatalf("replay = %d %v, want 202 %v", code, m, want)
	}

	// A violating unit rejects the whole group and leaves every target unchanged:
	// cash at min −1000 (balance −900); edit 41 to −1400 (net −200) plus a new −5.
	c.exec(`UPDATE accounts SET min_balance = -1000 WHERE external_id = 'cash'`)
	before41, before42 := op41, op42
	body = `{"operations":[{"edit_of":41,"amount":-1400},{"edit_of":42,"amount":1600},{"account":"cash","amount":-5,"effective_at":"2026-03-02T10:00:00Z"}],"wait_ms":5000}`
	code, m = c.do(http.MethodPost, "/transactions", owner, c.key(), body)
	if code != http.StatusOK || m["transaction_status"] != "REJECTED" {
		t.Fatalf("violating group = %d %v, want 200 REJECTED", code, m)
	}
	for _, o := range m["operations"].([]any) {
		if o.(map[string]any)["status"] != "INVALID" {
			t.Fatalf("violating group item %v, want INVALID", o)
		}
	}
	txID := num(m["transaction_id"])
	_, tx = c.do(http.MethodGet, fmt.Sprintf("/transactions/%d", int64(txID)), owner, "", "")
	rej, _ := tx["rejection"].(map[string]any)
	if rej["code"] != "LIMIT_VIOLATED" || rej["account"] != "cash" || rej["limit_side"] != "min" || num(rej["shortfall"]) != 105 {
		t.Fatalf("rejection = %v, want LIMIT_VIOLATED cash/min/105", rej)
	}
	for _, leg := range tx["operations"].([]any) {
		l := leg.(map[string]any)
		if l["status"] != "INVALID" || !reflect.DeepEqual(l["rejection"], rej) {
			t.Fatalf("rejected leg %v, want INVALID with the group's rejection", l)
		}
	}
	if _, after := c.getOp(41); !reflect.DeepEqual(after, before41) {
		t.Fatalf("41 changed by a rejected group: %v → %v", before41, after)
	}
	if _, after := c.getOp(42); !reflect.DeepEqual(after, before42) {
		t.Fatalf("42 changed by a rejected group: %v → %v", before42, after)
	}
	if bal := c.balance("cash", ""); bal != -900 {
		t.Fatalf("cash balance after the rejected group = %d, want -900", bal)
	}
}
