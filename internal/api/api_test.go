package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAmountUnmarshalRoutesThroughParseAmount confirms the Amount type is the
// single JSON→money path: integers parse, fractional/exponent/string tokens are
// rejected (no float64 ever holds a money value).
func TestAmountUnmarshalRoutesThroughParseAmount(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: `0`, want: 0},
		{in: `-1500`, want: -1500},
		{in: `9007199254740993`, want: 9007199254740993}, // beyond 2^53, must stay exact
		{in: `15.00`, wantErr: true},
		{in: `15.5`, wantErr: true},
		{in: `1e3`, wantErr: true},
		{in: `null`, wantErr: true},
		// A quoted string ("15") is rejected at the HTTP boundary by the integer
		// schema, not here — this test unmarshals directly, bypassing that gate.
	}
	for _, tc := range cases {
		var a Amount
		err := json.Unmarshal([]byte(tc.in), &a)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Amount(%s): expected error, got %d", tc.in, int64(a))
			}
			continue
		}
		if err != nil {
			t.Errorf("Amount(%s): unexpected error: %v", tc.in, err)
			continue
		}
		if int64(a) != tc.want {
			t.Errorf("Amount(%s) = %d, want %d", tc.in, int64(a), tc.want)
		}
	}
}

// TestOpenAPIYAMLContract checks the code-first spec generates and carries every
// §10 operation and the int64 amount schema. This is the no-DB guard that the
// contract compiles; `make openapi` is the diff-clean guard against the committed
// api/openapi.yaml (ADR-0003).
func TestOpenAPIYAMLContract(t *testing.T) {
	yaml, err := OpenAPIYAML()
	if err != nil {
		t.Fatalf("OpenAPIYAML: %v", err)
	}
	s := string(yaml)

	for _, id := range []string{
		"createTransaction", "getTransaction", "getOperation",
		"getBalance", "getStatement", "createAccount", "updateAccountLimits",
	} {
		if !strings.Contains(s, "operationId: "+id) {
			t.Errorf("openapi missing operationId %q", id)
		}
	}
	if !strings.Contains(s, "format: int64") {
		t.Error("openapi missing int64 amount schema")
	}
	if !strings.Contains(s, "X-Owner-Id") {
		t.Error("openapi missing X-Owner-Id header parameter")
	}
	if !strings.Contains(s, "openapi: 3.1.0") {
		t.Error("openapi document is not 3.1.0")
	}
}

// TestHeaderOwnerResolver covers the single-header owner resolution seam.
func TestHeaderOwnerResolver(t *testing.T) {
	r := HeaderOwnerResolver{}
	if id, err := r.Resolve(t.Context(), " 42 "); err != nil || id != 42 {
		t.Fatalf("Resolve(42) = %d, %v; want 42, nil", id, err)
	}
	for _, bad := range []string{"", "0", "-1", "abc", "1.5"} {
		if _, err := r.Resolve(t.Context(), bad); err == nil {
			t.Errorf("Resolve(%q): expected error", bad)
		}
	}
}
