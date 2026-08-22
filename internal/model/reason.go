package model

import (
	"encoding/json"
	"fmt"
)

// ReasonCode is the machine-readable class of a rejection. Kept small and
// stable: clients switch on it.
type ReasonCode string

// ReasonLimitViolated is the only rejection cause in the current contract — a
// candidate that would push an account's final balance outside its limits
// (spec §6).
const ReasonLimitViolated ReasonCode = "LIMIT_VIOLATED"

// LimitSide names which configured bound a rejection crossed.
type LimitSide string

const (
	LimitMin LimitSide = "min"
	LimitMax LimitSide = "max"
)

// Rejection is the machine-readable detail stored in operations.invalidation_reason
// and transactions.reject_reason and surfaced by the query API (spec §10.2). It
// carries enough for a budgeting UI to render "envelope short by 12.00" directly:
// the offending account's external id, the bound crossed, and by how much.
//
// The processor (M5/M6) writes it; the API (M7) reads it. It is defined here so
// both sides share one shape. Shortfall is a positive magnitude in minor units.
type Rejection struct {
	Code      ReasonCode `json:"code"`
	Account   string     `json:"account"`
	LimitSide LimitSide  `json:"limit_side"`
	Shortfall int64      `json:"shortfall"`
}

// Marshal renders the rejection as the JSON text stored in the reason columns.
func (r Rejection) Marshal() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal rejection: %w", err)
	}
	return string(b), nil
}

// ParseRejection parses a reason column's JSON text back into a Rejection.
func ParseRejection(s string) (Rejection, error) {
	var r Rejection
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Rejection{}, fmt.Errorf("parse rejection %q: %w", s, err)
	}
	return r, nil
}
