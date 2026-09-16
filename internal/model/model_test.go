package model

import (
	"bytes"
	"encoding/hex"
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

	a, err := HashPayload([]any{CanonicalOp{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: utc}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPayload([]any{CanonicalOp{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: shifted}})
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
	base := []any{CanonicalOp{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}}
	h0, _ := HashPayload(base)

	mutations := [][]any{
		{CanonicalOp{OwnerID: 2, Account: "acct", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}},
		{CanonicalOp{OwnerID: 1, Account: "other", Amount: -500, EffectiveAt: time.Unix(0, 0).UTC()}},
		{CanonicalOp{OwnerID: 1, Account: "acct", Amount: -501, EffectiveAt: time.Unix(0, 0).UTC()}},
		{CanonicalOp{OwnerID: 1, Account: "acct", Amount: -500, EffectiveAt: time.Unix(1, 0).UTC()}},
	}
	for i, m := range mutations {
		h, _ := HashPayload(m)
		if bytes.Equal(h0, h) {
			t.Errorf("mutation %d must change the hash", i)
		}
	}
}

// legacyPayloadHash is the SHA-256 the pre-editing encoder ([]CanonicalOp
// marshalled directly) produced for legacyPayloadFixture. It was captured from
// that encoder before HashPayload learned about edit items and must never
// change: every idempotency key registered before the upgrade replays against
// it (spec §10.1; ADR-0010 risk "idempotency hash drift").
const legacyPayloadHash = "f5bceba8adf9826325209f103eb6887ccef3038ec07e331e4158a821bc648ee8"

func legacyPayloadFixture() []any {
	rev := int64(41)
	at := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	return []any{
		CanonicalOp{OwnerID: 7, Account: "cash", Amount: -1500, EffectiveAt: at},
		CanonicalOp{
			OwnerID: 7, Account: "wallet", Amount: 1500,
			EffectiveAt: at.In(time.FixedZone("plus2", 2*60*60)), ReversalOf: &rev,
		},
	}
}

// UT-001: a request of plain operations hashes byte-identically to the
// pre-feature encoder.
func TestHashPayloadLegacyGolden(t *testing.T) {
	t.Parallel()
	h, err := HashPayload(legacyPayloadFixture())
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h); got != legacyPayloadHash {
		t.Fatalf("legacy hash drifted:\n got %s\nwant %s", got, legacyPayloadHash)
	}
}

// editPayloadHash and deletePayloadHash are the SHA-256s HashPayload produced
// for editPayloadFixture and deletePayloadFixture when each item kind was
// introduced (ADR-0010 and ADR-0011). Like legacyPayloadHash they must never
// change: every idempotency key registered with an edit or a delete replays
// against them. Adding an item kind adds a golden here; it never touches an
// existing encoding.
const (
	editPayloadHash   = "194e94a72435349254f8053ca94763a6bea5012cdf26f0aa7d7bf7b0c5929f00"
	deletePayloadHash = "9ac1da8ce2b3fa6163fb463df74b02211315e2ae1dda5a16396bddfbe23b9aa9"
)

func editPayloadFixture() []any   { return []any{CanonicalEditOp{OwnerID: 7, EditOf: 41}} }
func deletePayloadFixture() []any { return []any{CanonicalDeleteOp{OwnerID: 7, DeleteOf: 41}} }

// UT-001 (deletion): the edit encoding is untouched by the delete item kind and
// a delete item has its own pinned golden. A delete of a target hashes
// differently from an edit of the same target with every field omitted, and
// from the same delete with a revision guard.
func TestHashPayloadEditAndDeleteGolden(t *testing.T) {
	t.Parallel()
	h, err := HashPayload(editPayloadFixture())
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h); got != editPayloadHash {
		t.Fatalf("edit hash drifted:\n got %s\nwant %s", got, editPayloadHash)
	}
	hDelete, err := HashPayload(deletePayloadFixture())
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hDelete); got != deletePayloadHash {
		t.Fatalf("delete hash drifted:\n got %s\nwant %s", got, deletePayloadHash)
	}
	if bytes.Equal(h, hDelete) {
		t.Fatalf("delete of 41 and all-omitted edit of 41 must hash differently")
	}

	rev := int32(2)
	hGuarded, err := HashPayload([]any{CanonicalDeleteOp{OwnerID: 7, DeleteOf: 41, ExpectedRevision: &rev}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hDelete, hGuarded) {
		t.Fatalf("expected_revision must be part of the delete payload")
	}
}

// UT-002: edit items hash as sent — omitting a field and sending the target's
// current value are different payloads — while the transport shape (PATCH on
// one operation vs a POST item) does not matter: both build the same
// CanonicalEditOp.
func TestHashPayloadEditAsSent(t *testing.T) {
	t.Parallel()
	current := int64(-1500)
	omitted := CanonicalEditOp{OwnerID: 7, EditOf: 41}
	explicit := CanonicalEditOp{OwnerID: 7, EditOf: 41, Amount: &current}

	hOmitted, err := HashPayload([]any{omitted})
	if err != nil {
		t.Fatal(err)
	}
	hExplicit, err := HashPayload([]any{explicit})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hOmitted, hExplicit) {
		t.Fatalf("omitted amount and explicit current amount must hash differently (as-sent semantics)")
	}

	// PATCH-shaped: the handler knows the target id and one changed field.
	// POST-shaped: an item in a transactions request with edit_of set. Both are
	// the same canonical item; the offset of effective_at is irrelevant.
	newAmount := int64(-1200)
	utc := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	shifted := utc.In(time.FixedZone("minus3", -3*60*60))
	patchShaped := CanonicalEditOp{OwnerID: 7, EditOf: 41, Amount: &newAmount, EffectiveAt: &utc}
	postShaped := CanonicalEditOp{OwnerID: 7, EditOf: 41, Amount: &newAmount, EffectiveAt: &shifted}
	hPatch, err := HashPayload([]any{patchShaped})
	if err != nil {
		t.Fatal(err)
	}
	hPost, err := HashPayload([]any{postShaped})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hPatch, hPost) {
		t.Fatalf("PATCH-shaped and POST-shaped edit items must hash equal")
	}
	if shifted.Location() == time.UTC {
		t.Fatalf("HashPayload must not mutate the caller's time through the pointer")
	}

	// An edit item never collides with a plain operation, and an unsupported
	// item type is a programming error, not a silent hash.
	hOp, err := HashPayload([]any{CanonicalOp{OwnerID: 7, Account: "cash", Amount: -1200, EffectiveAt: utc}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hOp, hPatch) {
		t.Fatalf("edit item and new operation must hash differently")
	}
	if _, err := HashPayload([]any{"not an item"}); err == nil {
		t.Fatalf("unsupported item type must be rejected")
	}
}

// The stored JSON of a LIMIT_VIOLATED rejection is unchanged by the omitempty
// tags (every field is non-zero), and the edit codes omit the limit fields.
func TestRejectionJSONShape(t *testing.T) {
	t.Parallel()
	limit := Rejection{Code: ReasonLimitViolated, Account: "wallet", LimitSide: LimitMin, Shortfall: 1200}
	s, err := limit.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const wantLimit = `{"code":"LIMIT_VIOLATED","account":"wallet","limit_side":"min","shortfall":1200}`
	if s != wantLimit {
		t.Fatalf("LIMIT_VIOLATED json changed:\n got %s\nwant %s", s, wantLimit)
	}

	target := int64(41)
	expected, actual := int32(2), int32(3)
	stale := Rejection{Code: ReasonStaleRevision, OperationID: &target, ExpectedRevision: &expected, ActualRevision: &actual}
	s, err = stale.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const wantStale = `{"code":"STALE_REVISION","operation_id":41,"expected_revision":2,"actual_revision":3}`
	if s != wantStale {
		t.Fatalf("STALE_REVISION json:\n got %s\nwant %s", s, wantStale)
	}
	got, err := ParseRejection(s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != ReasonStaleRevision || got.OperationID == nil || *got.OperationID != 41 ||
		got.ExpectedRevision == nil || *got.ExpectedRevision != 2 ||
		got.ActualRevision == nil || *got.ActualRevision != 3 {
		t.Fatalf("STALE_REVISION round trip mismatch: %+v", got)
	}

	notEditable := Rejection{Code: ReasonTargetNotEditable, OperationID: &target}
	s, err = notEditable.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	const wantNotEditable = `{"code":"TARGET_NOT_EDITABLE","operation_id":41}`
	if s != wantNotEditable {
		t.Fatalf("TARGET_NOT_EDITABLE json:\n got %s\nwant %s", s, wantNotEditable)
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
