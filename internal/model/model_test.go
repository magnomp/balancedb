package model

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestParseAmount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "1500", want: 1500},
		{in: "-1500", want: -1500},
		{in: "+7", want: 7},
		{in: "9223372036854775807", want: 9223372036854775807},
		{in: "-9223372036854775808", want: -9223372036854775808},
		{in: "9223372036854775808", wantErr: true}, // overflow
		{in: "15.00", wantErr: true},
		{in: "1.5", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "", wantErr: true},
		{in: " 5", wantErr: true},
		{in: "0x10", wantErr: true},
		{in: "abc", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseAmount(json.Number(c.in))
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseAmount(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAmount(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAmount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestParseAmountRejectsFloatDecode guards the inviolable directly: a JSON body
// decoded with UseNumber never yields a float, and ParseAmount rejects a
// fractional token.
func TestParseAmountRejectsFloatDecode(t *testing.T) {
	t.Parallel()
	dec := json.NewDecoder(bytes.NewReader([]byte(`{"amount": 15.00}`)))
	dec.UseNumber()
	var body struct {
		Amount json.Number `json:"amount"`
	}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := ParseAmount(body.Amount); err == nil {
		t.Fatalf("ParseAmount(%q): expected rejection of fractional amount", body.Amount)
	}
}

func TestHashPayloadDeterministicAndUTCNormalised(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("plus2", 2*60*60)
	utc := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	shifted := utc.In(loc) // same instant, different offset

	a, err := HashPayload([]CanonicalOp{{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: utc}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPayload([]CanonicalOp{{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: shifted}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("same instant in different zones must hash equal")
	}
	if len(a) != 32 {
		t.Fatalf("expected 32-byte sha-256, got %d", len(a))
	}
}

func TestHashPayloadSensitiveToContent(t *testing.T) {
	t.Parallel()
	base := []CanonicalOp{{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}}
	h0, _ := HashPayload(base)

	mutations := [][]CanonicalOp{
		{{OwnerID: 2, Account: "acct", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}},
		{{OwnerID: 1, Account: "other", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}},
		{{OwnerID: 1, Account: "acct", Amount: -501, EffectiveAt: time.Unix(0, 0).UTC()}},
		{{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: time.Unix(1, 0).UTC()}},
	}
	for i, m := range mutations {
		h, _ := HashPayload(m)
		if bytes.Equal(h0, h) {
			t.Errorf("mutation %d must change the hash", i)
		}
	}
}

func TestRejectionRoundTrip(t *testing.T) {
	t.Parallel()
	r := Rejection{Code: ReasonLimitViolated, Account: "wallet", LimitSide: LimitMin, Shortfall: 1200}
	s, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRejection(s)
	if err != nil {
		t.Fatal(err)
	}
	if got != r {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, r)
	}
}
