package model

import (
	"encoding/json"
	"fmt"
)

// ReasonCode is the machine-readable class of a rejection. Kept small and
// stable: clients switch on it.
type ReasonCode string

const (
	// ReasonLimitViolated — a candidate (operation, group, or edit's virtual legs)
	// would push an account's final balance outside its limits (spec §6).
	ReasonLimitViolated ReasonCode = "LIMIT_VIOLATED"
	// ReasonTargetNotEditable — an edit whose target is INVALID at decision time
	// (ADR-0010). Terminal targets cannot be revised; the client re-submits.
	ReasonTargetNotEditable ReasonCode = "TARGET_NOT_EDITABLE"
	// ReasonStaleRevision — an edit carrying expected_revision that no longer
	// matches the target's current revision at decision time (ADR-0010).
	ReasonStaleRevision ReasonCode = "STALE_REVISION"
)

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
// The processor writes it; the API reads it. It is defined here so both sides
// share one shape. Shortfall is a positive magnitude in minor units.
//
// Only Code is always present. LIMIT_VIOLATED fills Account/LimitSide/Shortfall
// (all non-zero, so its stored JSON is unchanged by omitempty). The edit codes
// (ADR-0010) fill OperationID — the edit target — and STALE_REVISION adds the
// expected and actual revisions.
type Rejection struct {
	Code             ReasonCode `json:"code"`
	Account          string     `json:"account,omitempty"`
	LimitSide        LimitSide  `json:"limit_side,omitempty"`
	Shortfall        int64      `json:"shortfall,omitempty"`
	OperationID      *int64     `json:"operation_id,omitempty"`
	ExpectedRevision *int32     `json:"expected_revision,omitempty"`
	ActualRevision   *int32     `json:"actual_revision,omitempty"`
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
