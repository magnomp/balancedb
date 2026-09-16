package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

// CanonicalOp is the business content of one operation for idempotency hashing.
// It is deliberately narrow: only the fields that define what the operation *is*
// participate in the hash. The owner is included so a reused idempotency key
// across owners is detected as a payload conflict rather than replaying another
// owner's result.
//
// Its encoding is frozen: every idempotency key registered before operation
// editing existed was hashed with exactly this shape, and those hashes must keep
// matching on replay (UT-001 pins a golden value). Never add a field here — new
// item kinds get their own canonical type, like CanonicalEditOp.
type CanonicalOp struct {
	OwnerID     int64     `json:"owner_id"`
	Account     string    `json:"account"`
	Amount      int64     `json:"amount"`
	EffectiveAt time.Time `json:"effective_at"`
	ReversalOf  *int64    `json:"reversal_of"`
}

// CanonicalEditOp is the business content of one edit item (ADR-0010) for
// idempotency hashing. The request is hashed *as sent*: a field the client
// omitted is nil, and stays nil even though insertion later resolves it from the
// target's current value — so "omit amount" and "send the current amount" are
// different payloads under the same key (spec §10.1 payload conflict). EffectiveAt
// is normalised to UTC like CanonicalOp's.
type CanonicalEditOp struct {
	OwnerID          int64      `json:"owner_id"`
	EditOf           int64      `json:"edit_of"`
	Account          *string    `json:"account"`      // nil when omitted
	Amount           *int64     `json:"amount"`       // nil when omitted
	EffectiveAt      *time.Time `json:"effective_at"` // nil when omitted (UTC)
	ExpectedRevision *int32     `json:"expected_revision"`
}

// HashPayload computes the canonical SHA-256 of a request's items, used to
// detect same-key/different-payload retries (spec §10.1: same key + different
// payload → 422). Each item is a CanonicalOp (a new operation) or a
// CanonicalEditOp (an edit); any other type is a programming error.
//
// Canonicalisation: item order is significant (it is part of the request), each
// effective_at is normalised to UTC so the same instant hashes identically
// regardless of the client's offset, and the encoding is a struct per item (fixed
// field order, no maps) so json.Marshal is deterministic. The slice is []any so a
// request of plain CanonicalOp items marshals byte-for-byte as the pre-editing
// []CanonicalOp did: existing hashes stay valid across the upgrade.
func HashPayload(items []any) ([]byte, error) {
	norm := make([]any, len(items))
	for i, item := range items {
		switch v := item.(type) {
		case CanonicalOp:
			v.EffectiveAt = v.EffectiveAt.UTC()
			norm[i] = v
		case CanonicalEditOp:
			if v.EffectiveAt != nil {
				at := v.EffectiveAt.UTC()
				v.EffectiveAt = &at
			}
			norm[i] = v
		default:
			return nil, fmt.Errorf("hash payload: item %d has unsupported type %T", i, item)
		}
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("hash payload: marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
