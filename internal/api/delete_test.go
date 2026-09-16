package api

import (
	"net/http"
	"strings"
	"testing"
)

// deleteNoDB is patchNoDB for DELETE /operations/41: every case must be refused
// before the handler touches the (nil) pool.
func deleteNoDB(t *testing.T, query, body string) (*http.Response, string) {
	t.Helper()
	return requestNoDB(t, http.MethodDelete, "/operations/41"+query, body)
}

// The DELETE contract's structural rules are enforced by the schema, before the
// handler runs: expected_revision below 1 and a negative wait_ms are refused
// with Huma's 422 problem document (the ledger's own >= 1 check never fires);
// a non-numeric guard is refused the same way. Nothing here reaches the DB.
func TestDeleteQueryValidation(t *testing.T) {
	for _, q := range []string{"?expected_revision=0", "?expected_revision=-1", "?wait_ms=-5", "?expected_revision=abc"} {
		resp, body := deleteNoDB(t, q, "")
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("DELETE %s: status = %d, want 422; body %s", q, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("DELETE %s: content-type = %q, want RFC 7807 problem+json", q, ct)
		}
	}
}

// IT-002 (structural half, no DB): a POST /transactions delete item that carries
// anything but delete_of and expected_revision is refused with 400 and the
// contract's message; a delete item with expected_revision < 1 is 400 like an
// edit item; two items targeting one operation in any mix of kinds are 422
// "duplicate edit target"; a delete item alone is well-formed (it reaches the
// nil pool, which is how the test knows validation passed).
func TestPostTransactionsDeleteItemValidation(t *testing.T) {
	const extra = "a delete item carries only delete_of and expected_revision"
	cases := []struct {
		body string
		code int
		want string
	}{
		{`{"operations":[{"delete_of":41,"amount":-1}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"account":"cash"}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"effective_at":"2026-03-01T10:00:00Z"}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"reversal_of":9}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"edit_of":41}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"amount":0}]}`, 400, extra},
		{`{"operations":[{"delete_of":41,"expected_revision":0}]}`, 400, "expected_revision must be >= 1"},
		{`{"operations":[{"delete_of":41},{"edit_of":41,"amount":-1}]}`, 422, "duplicate edit target 41"},
		{`{"operations":[{"edit_of":41,"amount":-1},{"delete_of":41}]}`, 422, "duplicate edit target 41"},
		{`{"operations":[{"delete_of":41},{"delete_of":41,"expected_revision":2}]}`, 422, "duplicate edit target 41"},
	}
	for _, tc := range cases {
		resp, body := postNoDB(t, tc.body)
		if resp.StatusCode != tc.code {
			t.Errorf("POST %s: status = %d, want %d; body %s", tc.body, resp.StatusCode, tc.code, body)
			continue
		}
		if !strings.Contains(body, `"detail":"`+tc.want+`"`) {
			t.Errorf("POST %s: body %s, want detail %q", tc.body, body, tc.want)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "problem+json") {
			t.Errorf("POST %s: content-type = %q, want RFC 7807 problem+json", tc.body, ct)
		}
	}
}

// mapInsertErr names the offending item's kind from the ledger's TargetError
// wrap: the same sentinel reads "edit target" or "delete target"; the duplicate
// wording is shared. An unwrapped sentinel still maps to its status.
func TestMapInsertErrRendersTargetKind(t *testing.T) {
	cases := []struct {
		err  error
		code int
		want string
	}{
		{&TargetError{Kind: TargetDelete, Target: 41, Err: ErrEditTargetNotFound}, 422, "delete target 41 not found"},
		{&TargetError{Kind: TargetEdit, Target: 41, Err: ErrEditTargetNotFound}, 422, "edit target 41 not found"},
		{&TargetError{Kind: TargetDelete, Target: 57, Err: ErrEditTargetNotOperation}, 422, "delete target 57 is an edit, not an operation"},
		{&TargetError{Kind: TargetEdit, Target: 57, Err: ErrEditTargetNotOperation}, 422, "edit target 57 is an edit, not an operation"},
		{&TargetError{Kind: TargetDelete, Target: 41, Err: ErrDuplicateEditTarget}, 422, "duplicate edit target 41"},
		{ErrDeleteWithFields, 400, "a delete item carries only delete_of and expected_revision"},
		{ErrDeletesDisabled, 403, "deleting is disabled for this cell"},
		{ErrEditsDisabled, 403, "editing is disabled for this cell"},
	}
	for _, tc := range cases {
		got := mapInsertErr(tc.err)
		se, ok := got.(interface {
			GetStatus() int
			Error() string
		})
		if !ok {
			t.Fatalf("mapInsertErr(%v) = %T, want a status error", tc.err, got)
		}
		if se.GetStatus() != tc.code || !strings.Contains(se.Error(), tc.want) {
			t.Errorf("mapInsertErr(%v) = %d %q, want %d %q", tc.err, se.GetStatus(), se.Error(), tc.code, tc.want)
		}
	}
	// DELETE /operations/{id}: the path target not found is the resource itself.
	if se := mapDeleteErr(&TargetError{Kind: TargetDelete, Target: 41, Err: ErrEditTargetNotFound}).(interface{ GetStatus() int }); se.GetStatus() != 404 {
		t.Errorf("mapDeleteErr(not found) status = %d, want 404", se.GetStatus())
	}
}
