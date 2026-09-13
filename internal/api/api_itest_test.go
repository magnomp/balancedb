//go:build itest

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/dbtest"
	"github.com/magnomp/balancedb/internal/model"
)

// newTestServer builds a Server over a throwaway schema and serves it via
// httptest, returning the Server (so tests may set seams) and the base URL.
func newTestServer(t *testing.T) (*Server, *httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewSchema(t)
	srv := NewServer(pool, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, pool
}

func keyN(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

// doReq issues an HTTP request. owner/idem set the X-Owner-Id / Idempotency-Key
// headers when non-empty; body (raw JSON) sets the request body when non-empty.
func doReq(t *testing.T, ts *httptest.Server, method, path, owner, idem, body string) (*http.Response, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if owner != "" {
		req.Header.Set("X-Owner-Id", owner)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, b
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body %q: %v", string(b), err)
	}
	return m
}

// --- seeding helpers (direct SQL; read paths are what these tests exercise) ---

func seedAccount(t *testing.T, pool *pgxpool.Pool, owner int64, ext string, minB, maxB *int64, confirmed int64) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO accounts (owner_id, external_id, min_balance, max_balance, confirmed_balance)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		owner, ext, minB, maxB, confirmed).Scan(&id)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

func seedConfirmedOp(t *testing.T, pool *pgxpool.Pool, accountID, amount int64, eff time.Time) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO operations (account_id, amount, effective_at, status, confirmed_at)
		 VALUES ($1,$2,$3,'CONFIRMED', now()) RETURNING id`,
		accountID, amount, eff).Scan(&id)
	if err != nil {
		t.Fatalf("seed confirmed op: %v", err)
	}
	return id
}

func seedSnapshot(t *testing.T, pool *pgxpool.Pool, accountID int64, day string, balance int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO balance_snapshots (account_id, day, balance) VALUES ($1, $2::date, $3)`,
		accountID, day, balance)
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
}

func ptr(v int64) *int64 { return &v }

// --- insertion + query round-trips ------------------------------------------

func TestHTTPInsertSingleAndQuery(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"operations":[{"account":"wallet","amount":-1500,"effective_at":"2026-08-22T12:00:00Z"}]}`
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(1), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /transactions status = %d, body %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if m["transaction_id"] != nil {
		t.Fatalf("single must have no transaction_id: %v", m["transaction_id"])
	}
	ops := m["operations"].([]any)
	if len(ops) != 1 {
		t.Fatalf("want 1 operation, got %d", len(ops))
	}
	op0 := ops[0].(map[string]any)
	if op0["status"] != "PENDING" {
		t.Fatalf("want PENDING, got %v", op0["status"])
	}
	opID := int64(op0["id"].(float64))

	// GET /operations/{id}
	resp, b = doReq(t, ts, http.MethodGet, fmt.Sprintf("/operations/%d", opID), "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /operations status = %d, body %s", resp.StatusCode, b)
	}
	if got := decode(t, b)["status"]; got != "PENDING" {
		t.Fatalf("operation status = %v, want PENDING", got)
	}

	// Owner scoping: a different owner cannot see it.
	resp, _ = doReq(t, ts, http.MethodGet, fmt.Sprintf("/operations/%d", opID), "999", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner GET /operations status = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPInsertGroupAndQuery(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"operations":[
		{"account":"checking","amount":-1000,"effective_at":"2026-08-22T12:00:00Z"},
		{"account":"envelope","amount":-1000,"effective_at":"2026-08-22T12:00:00Z"}
	]}`
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "7", keyN(2), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST group status = %d, body %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if m["transaction_id"] == nil {
		t.Fatal("group must have a transaction_id")
	}
	txID := int64(m["transaction_id"].(float64))

	resp, b = doReq(t, ts, http.MethodGet, fmt.Sprintf("/transactions/%d", txID), "7", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /transactions status = %d, body %s", resp.StatusCode, b)
	}
	tm := decode(t, b)
	if int(tm["op_count"].(float64)) != 2 || len(tm["operations"].([]any)) != 2 {
		t.Fatalf("unexpected transaction body: %s", b)
	}
	if tm["status"] != "PENDING" {
		t.Fatalf("group status = %v, want PENDING", tm["status"])
	}

	// Cross-owner 404.
	resp, _ = doReq(t, ts, http.MethodGet, fmt.Sprintf("/transactions/%d", txID), "8", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner GET /transactions = %d, want 404", resp.StatusCode)
	}
}

func TestHTTPIdempotentReplay(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"operations":[{"account":"w","amount":500,"effective_at":"2026-08-22T12:00:00Z"}]}`
	_, b1 := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(10), body)
	resp, b2 := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(10), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status = %d", resp.StatusCode)
	}
	m2 := decode(t, b2)
	if m2["replayed"] != true {
		t.Fatalf("second insert must be a replay: %s", b2)
	}
	id1 := decode(t, b1)["operations"].([]any)[0].(map[string]any)["id"]
	id2 := m2["operations"].([]any)[0].(map[string]any)["id"]
	if id1 != id2 {
		t.Fatalf("replay returned different id: %v != %v", id1, id2)
	}
}

// --- contract enforcement (7807) --------------------------------------------

func TestHTTPFloatAmountRejected(t *testing.T) {
	_, ts, _ := newTestServer(t)
	for _, amt := range []string{"15.50", "15.00", "1e3"} {
		body := fmt.Sprintf(`{"operations":[{"account":"w","amount":%s,"effective_at":"2026-08-22T12:00:00Z"}]}`, amt)
		resp, b := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(20), body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("amount %s: status = %d, want 422; body %s", amt, resp.StatusCode, b)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Fatalf("amount %s: content-type = %q, want RFC7807 problem+json", amt, ct)
		}
	}
}

func TestHTTPMissingFieldRejected(t *testing.T) {
	_, ts, _ := newTestServer(t)
	// Missing "amount".
	body := `{"operations":[{"account":"w","effective_at":"2026-08-22T12:00:00Z"}]}`
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(21), body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing amount: status = %d, want 422; body %s", resp.StatusCode, b)
	}
}

func TestHTTPMissingOwnerHeaderRejected(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"operations":[{"account":"w","amount":1,"effective_at":"2026-08-22T12:00:00Z"}]}`
	resp, _ := doReq(t, ts, http.MethodPost, "/transactions", "", keyN(22), body)
	if resp.StatusCode < 400 {
		t.Fatalf("missing X-Owner-Id: status = %d, want 4xx", resp.StatusCode)
	}
}

func TestHTTPPayloadConflict(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"operations":[{"account":"w","amount":500,"effective_at":"2026-08-22T12:00:00Z"}]}`
	doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(23), body)
	other := `{"operations":[{"account":"w","amount":501,"effective_at":"2026-08-22T12:00:00Z"}]}`
	resp, b := doReq(t, ts, http.MethodPost, "/transactions", "1", keyN(23), other)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("payload conflict: status = %d, want 422; body %s", resp.StatusCode, b)
	}
}

// --- rejection detail surfaces ----------------------------------------------

func TestHTTPOperationRejectionDetail(t *testing.T) {
	_, ts, pool := newTestServer(t)
	acct := seedAccount(t, pool, 1, "a", ptr(0), nil, 0)
	// Seed an INVALID single op with a rejection detail.
	rej := model.Rejection{Code: model.ReasonLimitViolated, Account: "a", LimitSide: model.LimitMin, Shortfall: 1200}
	reason, err := rej.Marshal()
	if err != nil {
		t.Fatalf("marshal rejection: %v", err)
	}
	var opID int64
	err = pool.QueryRow(context.Background(),
		`INSERT INTO operations (account_id, amount, effective_at, status, invalidation_reason)
		 VALUES ($1,-1200,'2026-08-22T12:00:00Z','INVALID',$2) RETURNING id`, acct, reason).Scan(&opID)
	if err != nil {
		t.Fatalf("seed invalid op: %v", err)
	}

	resp, b := doReq(t, ts, http.MethodGet, fmt.Sprintf("/operations/%d", opID), "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET invalid op status = %d", resp.StatusCode)
	}
	m := decode(t, b)
	if m["status"] != "INVALID" {
		t.Fatalf("status = %v, want INVALID", m["status"])
	}
	r := m["rejection"].(map[string]any)
	if r["code"] != "LIMIT_VIOLATED" || r["account"] != "a" || r["limit_side"] != "min" || int64(r["shortfall"].(float64)) != 1200 {
		t.Fatalf("unexpected rejection detail: %v", r)
	}
}

// --- accounts + limits ------------------------------------------------------

func TestHTTPCreateAccountAndConflict(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body := `{"external_id":"savings","min_balance":0,"max_balance":100000}`
	resp, b := doReq(t, ts, http.MethodPost, "/accounts", "1", "", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /accounts status = %d, body %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if m["account"] != "savings" || int64(m["min_balance"].(float64)) != 0 || int64(m["max_balance"].(float64)) != 100000 {
		t.Fatalf("unexpected account body: %s", b)
	}

	// Duplicate → 409.
	resp, _ = doReq(t, ts, http.MethodPost, "/accounts", "1", "", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate account status = %d, want 409", resp.StatusCode)
	}

	// Inverted range → 422.
	resp, _ = doReq(t, ts, http.MethodPost, "/accounts", "1", "", `{"external_id":"bad","min_balance":10,"max_balance":5}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("inverted range status = %d, want 422", resp.StatusCode)
	}
}

func TestHTTPUpdateLimits(t *testing.T) {
	_, ts, pool := newTestServer(t)
	seedAccount(t, pool, 1, "acct", nil, nil, 50)

	// Valid: min <= 50 <= max.
	resp, b := doReq(t, ts, http.MethodPut, "/accounts/acct/limits", "1", "", `{"min_balance":0,"max_balance":100}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT limits status = %d, body %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if int64(m["version"].(float64)) != 1 {
		t.Fatalf("version = %v, want 1 after one update", m["version"])
	}

	// §6 violation: new min above confirmed_balance.
	resp, _ = doReq(t, ts, http.MethodPut, "/accounts/acct/limits", "1", "", `{"min_balance":60}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("min > balance status = %d, want 422", resp.StatusCode)
	}

	// §6 violation: new max below confirmed_balance.
	resp, _ = doReq(t, ts, http.MethodPut, "/accounts/acct/limits", "1", "", `{"max_balance":40}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("max < balance status = %d, want 422", resp.StatusCode)
	}

	// Unknown account → 404.
	resp, _ = doReq(t, ts, http.MethodPut, "/accounts/nope/limits", "1", "", `{"min_balance":0}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d, want 404", resp.StatusCode)
	}
}

// TestHTTPUpdateLimitsVersionConflict forces the version CAS to miss on every
// attempt via the afterLimitsRead seam (a stand-in for a processor confirmation
// racing the limit update, spec §6) and asserts the retry-then-409 path.
func TestHTTPUpdateLimitsVersionConflict(t *testing.T) {
	srv, ts, pool := newTestServer(t)
	id := seedAccount(t, pool, 1, "race", nil, nil, 50)
	srv.afterLimitsRead = func(ctx context.Context) {
		// Bump the version between the handler's read and its CAS, every time.
		if _, err := pool.Exec(ctx, `UPDATE accounts SET version = version + 1 WHERE id = $1`, id); err != nil {
			t.Errorf("seam bump: %v", err)
		}
	}
	resp, b := doReq(t, ts, http.MethodPut, "/accounts/race/limits", "1", "", `{"min_balance":0,"max_balance":100}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("racing limit update status = %d, want 409; body %s", resp.StatusCode, b)
	}
}

// --- balances (final + point-in-time, N2) -----------------------------------

func TestHTTPBalanceFinalAndPointInTime(t *testing.T) {
	_, ts, pool := newTestServer(t)
	acct := seedAccount(t, pool, 1, "a", ptr(0), nil, 50) // min=0, final=50 (within limits)
	seedConfirmedOp(t, pool, acct, -100, mustTime("2026-03-10T10:00:00Z"))
	seedConfirmedOp(t, pool, acct, 150, mustTime("2026-03-10T12:00:00Z"))
	seedSnapshot(t, pool, acct, "2026-03-10", 50)

	// Final balance = 50.
	resp, b := doReq(t, ts, http.MethodGet, "/accounts/a/balance", "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET balance status = %d", resp.StatusCode)
	}
	if got := int64(decode(t, b)["balance"].(float64)); got != 50 {
		t.Fatalf("final balance = %d, want 50", got)
	}

	// Point-in-time between the debit and the credit: -100, below min 0 (N2 visible).
	resp, b = doReq(t, ts, http.MethodGet, "/accounts/a/balance?at=2026-03-10T11:00:00Z", "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET balance?at status = %d", resp.StatusCode)
	}
	m := decode(t, b)
	if got := int64(m["balance"].(float64)); got != -100 {
		t.Fatalf("point-in-time balance = %d, want -100 (N2)", got)
	}
	if m["at"] == nil {
		t.Fatal("point-in-time response must echo at")
	}

	// Next day: seeded from the snapshot → 50.
	resp, b = doReq(t, ts, http.MethodGet, "/accounts/a/balance?at=2026-03-11T00:00:00Z", "1", "", "")
	if got := int64(decode(t, b)["balance"].(float64)); got != 50 {
		t.Fatalf("next-day point-in-time = %d, want 50; body %s", got, b)
	}
}

// --- statement (pagination across a snapshot-day boundary) ------------------

func TestHTTPStatementPaginationAcrossSnapshotDay(t *testing.T) {
	_, ts, pool := newTestServer(t)
	acct := seedAccount(t, pool, 1, "s", nil, nil, 420)
	seedConfirmedOp(t, pool, acct, 100, mustTime("2026-01-01T09:00:00Z"))
	seedConfirmedOp(t, pool, acct, 200, mustTime("2026-01-01T15:00:00Z"))
	seedConfirmedOp(t, pool, acct, 50, mustTime("2026-01-02T09:00:00Z"))
	seedConfirmedOp(t, pool, acct, 70, mustTime("2026-01-02T15:00:00Z"))
	seedSnapshot(t, pool, acct, "2026-01-01", 300)
	seedSnapshot(t, pool, acct, "2026-01-02", 420)

	// Page 1 (limit 2): running 100, 300.
	resp, b := doReq(t, ts, http.MethodGet, "/accounts/s/statement?limit=2", "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statement page1 status = %d, body %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	entries := m["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("page1 entries = %d, want 2", len(entries))
	}
	if rb := int64(entries[0].(map[string]any)["running_balance"].(float64)); rb != 100 {
		t.Fatalf("page1[0] running = %d, want 100", rb)
	}
	if rb := int64(entries[1].(map[string]any)["running_balance"].(float64)); rb != 300 {
		t.Fatalf("page1[1] running = %d, want 300", rb)
	}
	cursor, ok := m["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("page1 must have a next_cursor; body %s", b)
	}

	// Page 2: crosses the day boundary; running seeded from the day-1 snapshot → 350, 420.
	resp, b = doReq(t, ts, http.MethodGet, "/accounts/s/statement?limit=2&cursor="+cursor, "1", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statement page2 status = %d", resp.StatusCode)
	}
	m = decode(t, b)
	entries = m["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("page2 entries = %d, want 2", len(entries))
	}
	if rb := int64(entries[0].(map[string]any)["running_balance"].(float64)); rb != 350 {
		t.Fatalf("page2[0] running = %d, want 350 (snapshot-seeded across day boundary)", rb)
	}
	if rb := int64(entries[1].(map[string]any)["running_balance"].(float64)); rb != 420 {
		t.Fatalf("page2[1] running = %d, want 420", rb)
	}
	if nc, _ := m["next_cursor"].(string); nc != "" {
		t.Fatalf("page2 must be the last page, got next_cursor %q", nc)
	}
}

// --- docs + spec served ------------------------------------------------------

func TestHTTPDocsAndSpecServed(t *testing.T) {
	_, ts, _ := newTestServer(t)

	resp, b := doReq(t, ts, http.MethodGet, "/docs", "", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /docs status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("/docs content-type = %q, want html", ct)
	}
	if !strings.Contains(string(b), "openapi.yaml") && !strings.Contains(string(b), "openapi.json") {
		t.Fatalf("/docs page does not reference the spec URL")
	}

	resp, b = doReq(t, ts, http.MethodGet, "/openapi.yaml", "", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /openapi.yaml status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(b), "operationId: createTransaction") {
		t.Fatalf("/openapi.yaml missing operations")
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// G1 applies at account creation, including before the first operation.
func TestHTTPCreateAccountLimitsMustContainZero(t *testing.T) {
	_, ts, pool := newTestServer(t)
	for _, body := range []string{
		`{"external_id":"bad_min","min_balance":1}`,
		`{"external_id":"bad_max","max_balance":-1}`,
	} {
		response, _ := doReq(t, ts, http.MethodPost, "/accounts", "1", "", body)
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("initial balance outside limits: status=%d", response.StatusCode)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid accounts persisted: %d", count)
	}
}
