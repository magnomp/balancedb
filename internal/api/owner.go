package api

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// OwnerResolver maps the raw owner credential presented on a request (today the
// X-Owner-Id header; a token claim later) to the numeric owner_id that scopes
// every query and insert (spec §2 sharding key, §10). It is the pluggable seam
// the plan calls for: a single-cell deployment uses HeaderOwnerResolver, and a
// directory-backed resolver can be dropped in without touching any handler.
type OwnerResolver interface {
	// Resolve turns the raw header value into an owner_id. A malformed or empty
	// credential must return an error; handlers surface it as 400.
	Resolve(ctx context.Context, raw string) (int64, error)
}

// HeaderOwnerResolver is the single-header implementation: the X-Owner-Id header
// carries the owner_id directly as a positive integer. This is the whole owner
// story for a single-cell deployment (one cell = one database); the directory is
// out of scope here, but the interface above is the place it plugs in.
type HeaderOwnerResolver struct{}

// Resolve parses raw as a positive int64 owner_id.
func (HeaderOwnerResolver) Resolve(_ context.Context, raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("X-Owner-Id header is required")
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("X-Owner-Id must be a positive integer, got %q", raw)
	}
	return id, nil
}
