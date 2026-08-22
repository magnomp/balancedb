// Package migrations holds the embedded SQL migration files applied by
// internal/migrate. The files live at the repository root (plan §0 layout) and
// are compiled into the binary via go:embed, so the image is self-contained and
// needs no migration files on disk at runtime (ADR-0001).
//
// This package is asset-only: it exposes the embedded file system and nothing
// else. The runner in internal/migrate owns all migration behavior.
package migrations

import "embed"

// FS is the embedded set of numbered migration files (NNNN_name.sql). The
// runner reads and orders them by numeric prefix.
//
//go:embed *.sql
var FS embed.FS
