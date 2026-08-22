package api

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/magnomp/balancedb/internal/model"
)

// Amount is the transport type for money at the HTTP boundary. It is an int64 of
// minor units (spec §5.1) and it exists to reconcile two authorities:
//
//   - ADR-0003 wants amount fields to be int64 in the request/response structs so
//     Huma derives an `integer/int64` schema and fractional JSON is rejected by
//     type.
//   - The inviolable (CLAUDE.md, plan §0) requires every JSON→money conversion to
//     pass through the single helper model.ParseAmount — no float64 decoding, ever.
//
// Amount does both: its Schema reports integer/int64 (with the minor-units and
// JS-precision warnings), and its UnmarshalJSON decodes via json.Number and hands
// the token to model.ParseAmount, so a fractional or exponent amount ("15.00",
// "1e3") is rejected at the contract boundary and no float64 ever holds a money
// value. Marshalling is the plain int64 form (a JSON number).
type Amount int64

// amountDoc is the shared field description; it carries the two client warnings
// the contract must state (ADR-0003): minor units and JS integer precision.
const amountDoc = "Signed amount in minor units (e.g. cents); positive is a credit, " +
	"negative a debit. Integer only — fractional or exponent values are rejected. " +
	"Values are int64 and may exceed JavaScript's safe-integer range (2^53); clients " +
	"that use JS numbers can lose precision and should treat the field as a string or bigint."

// Schema makes Amount reflect as an OpenAPI integer/int64, overriding Huma's
// default numeric reflection so the description (and the format) are attached
// wherever an Amount appears (huma.SchemaProvider).
func (Amount) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{
		Type:        huma.TypeInteger,
		Format:      "int64",
		Description: amountDoc,
	}
}

// UnmarshalJSON routes the raw JSON token through model.ParseAmount — the one
// sanctioned path from JSON to money (M3 handoff, CLAUDE.md inviolable). The token
// is decoded as a json.Number (never float64), so precision is preserved and any
// fractional/exponent value is rejected here rather than silently truncated.
func (a *Amount) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("amount must be an integer number in minor units: %w", err)
	}
	v, err := model.ParseAmount(n)
	if err != nil {
		return err
	}
	*a = Amount(v)
	return nil
}
