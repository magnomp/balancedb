package model

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// ParseAmount converts a JSON number token into an int64 amount in minor units.
//
// It is THE single entry point for turning client-supplied money into the
// internal representation (plan §0, CLAUDE.md inviolables): nothing else in the
// codebase converts JSON into a money value. The input is a json.Number (obtained
// via json.Decoder.UseNumber), never a float64 — floating point is forbidden for
// money end to end.
//
// Parsing is strict. Only an optionally signed run of decimal digits is accepted;
// any fractional part ("15.00"), exponent ("1e3"), surrounding whitespace, or
// non-numeric text is rejected. A zero amount parses successfully here — the
// operations.amount CHECK (amount <> 0) is the authority that rejects it (spec
// §5.2), so this helper stays a pure conversion.
func ParseAmount(n json.Number) (int64, error) {
	s := string(n)
	if s == "" {
		return 0, fmt.Errorf("amount: empty value")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: must be an integer in minor units (no decimals, no exponent): %w", s, err)
	}
	return v, nil
}
