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
type CanonicalOp struct {
	OwnerID     int64     `json:"owner_id"`
	Account     string    `json:"account"`
	Amount      int64     `json:"amount"`
	EffectiveAt time.Time `json:"effective_at"`
	ReversalOf  *int64    `json:"reversal_of"`
}

// HashPayload computes the canonical SHA-256 of a request's operations, used to
// detect same-key/different-payload retries (spec §10.1: same key + different
// payload → 422). Canonicalisation: operation order is significant (it is part of
// the request), each effective_at is normalised to UTC so the same instant hashes
// identically regardless of the client's offset, and the encoding is a struct
// (fixed field order, no maps) so json.Marshal is deterministic.
func HashPayload(ops []CanonicalOp) ([]byte, error) {
	norm := make([]CanonicalOp, len(ops))
	for i, op := range ops {
		op.EffectiveAt = op.EffectiveAt.UTC()
		norm[i] = op
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("hash payload: marshal: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
