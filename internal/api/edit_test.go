package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// patchNoDB issues a PATCH /operations/{id} against a Server built without a pool:
// every case here must be refused before the handler touches the database, or the
// nil pool would panic — which is the point of the test.
func patchNoDB(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	return requestNoDB(t, http.MethodPatch, "/operations/41", body)
}

// postNoDB is patchNoDB for POST /transactions.
func postNoDB(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	return requestNoDB(t, http.MethodPost, "/transactions", body)
}

func requestNoDB(t *testing.T, method, path, body string) (*http.Response, string) {
	t.Helper()
	srv := NewServer(nil, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Owner-Id", "7")
	req.Header.Set("Idempotency-Key", "4e1d0b1a-9a7c-4b2e-8f10-2c9d5b6a7e01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(b)
}

// UT-020: a PATCH body that changes nothing, or carries expected_revision < 1, is
// refused with 400 and the contract's message — before any DB access.
func TestPatchStructuralValidation(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{}`, "edit changes nothing"},
		{`{"expected_revision": 3}`, "edit changes nothing"},
		{`{"wait_ms": 500}`, "edit changes nothing"},
		{`{"expected_revision": 0}`, "expected_revision must be >= 1"},
		{`{"amount": -1200, "expected_revision": -1}`, "expected_revision must be >= 1"},
	}
	for _, tc := range cases {
		resp, body := patchNoDB(t, tc.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %s: status = %d, want 400; body %s", tc.body, resp.StatusCode, body)
			continue
		}
		if !strings.Contains(body, `"detail":"`+tc.want+`"`) {
			t.Errorf("PATCH %s: body %s, want detail %q", tc.body, body, tc.want)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("PATCH %s: content-type = %q, want RFC 7807 problem+json", tc.body, ct)
		}
	}
}

// UT-021: amount goes through the same Amount type as insertion, so a fractional,
// exponent, string or zero amount fails exactly as POST /transactions does today
// (422, problem+json) — never a float64, never a DB round-trip.
func TestPatchAmountParsingMatchesInsert(t *testing.T) {
	for _, body := range []string{
		`{"amount": 12.5}`, `{"amount": "12.50"}`, `{"amount": 1e3}`, `{"amount": 0}`,
	} {
		resp, b := patchNoDB(t, body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("PATCH %s: status = %d, want 422; body %s", body, resp.StatusCode, b)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("PATCH %s: content-type = %q, want RFC 7807 problem+json", body, ct)
		}
	}
}

// UT-022: POST /transactions with two items editing the same operation is refused
// with 422 `duplicate edit target 41` before any DB call (nil pool). The other
// structural rules of the group form are refused the same way: a new operation
// missing account/amount/effective_at (400), an edit item that changes nothing
// (400), expected_revision < 1 (400), reversal_of on an edit item (400), a zero
// amount (422, the insertion error).
func TestPostTransactionsStructuralValidation(t *testing.T) {
	cases := []struct {
		body string
		code int
		want string
	}{
		{`{"operations":[{"edit_of":41,"amount":-1200},{"edit_of":41,"effective_at":"2026-02-10T09:30:00Z"}]}`, 422, "duplicate edit target 41"},
		{`{"operations":[{"account":"cash","amount":300}]}`, 400, "operations[0]: account, amount and effective_at are required on a new operation"},
		{`{"operations":[{"edit_of":41,"amount":-1200},{"amount":300,"effective_at":"2026-03-01T10:00:00Z"}]}`, 400, "operations[1]: account, amount and effective_at are required on a new operation"},
		{`{"operations":[{"edit_of":41}]}`, 400, "edit changes nothing"},
		{`{"operations":[{"edit_of":41,"expected_revision":3}]}`, 400, "edit changes nothing"},
		{`{"operations":[{"edit_of":41,"amount":5,"expected_revision":0}]}`, 400, "expected_revision must be >= 1"},
		{`{"operations":[{"edit_of":41,"amount":5,"reversal_of":9}]}`, 400, "reversal_of is not allowed on an edit item"},
		{`{"operations":[{"edit_of":41,"amount":0}]}`, 422, "amount must be non-zero"},
		{`{"operations":[{"account":"cash","amount":0,"effective_at":"2026-03-01T10:00:00Z"}]}`, 422, "amount must be non-zero"},
	}
	for _, tc := range cases {
		resp, body := postNoDB(t, tc.body)
		if resp.StatusCode != tc.code {
			t.Errorf("POST %s: status = %d, want %d; body %s", tc.body, resp.StatusCode, tc.code, body)
			continue
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("POST %s: body %s, want detail containing %q", tc.body, body, tc.want)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("POST %s: content-type = %q, want RFC 7807 problem+json", tc.body, ct)
		}
	}
}

// IT-032 (static half, Safety Invariant 3): no SQL text in internal/api or
// internal/ledger updates or deletes operation_revisions. Every string literal of
// every non-test file is inspected, so a new const cannot slip past the check.
func TestNoRevisionMutationSQLInAPIOrLedger(t *testing.T) {
	for _, dir := range []string{".", filepath.Join("..", "ledger")} {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for path, f := range pkg.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					text, err := strconv.Unquote(lit.Value)
					if err != nil {
						return true
					}
					up := strings.ToUpper(text)
					if !strings.Contains(up, "OPERATION_REVISIONS") {
						return true
					}
					if strings.Contains(up, "UPDATE ") || strings.Contains(up, "DELETE ") {
						t.Errorf("%s: string literal at %s mutates operation_revisions:\n%s", path, fset.Position(lit.Pos()), text)
					}
					return true
				})
			}
		}
	}
}
