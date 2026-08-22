// Package dbtest is the integration-test harness shared by every milestone from
// M2 on (plan §M10 test-infra note). Each test isolates itself in a throwaway
// schema (test_<random>) created from TEST_DATABASE_URL and dropped on cleanup —
// cheap isolation, parallel-safe, and a continuous exercise of the
// configurable-schema machinery.
//
// It is a normal (untagged) package so it is vetted by `make lint`; only the
// build-tagged integration test files import it, so it never enters a production
// build.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/migrate"
)

// URL returns TEST_DATABASE_URL, skipping the test if it is unset. `make itest`
// requires it; `make test` never reaches integration tests (they are behind the
// itest build tag), so this only skips when someone runs a tagged test by hand
// without a database.
func URL(t testing.TB) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return url
}

// RandomSchema returns a fresh schema name of the form test_<hex>, valid under
// the BALANCEDB_SCHEMA regex (starts with a letter, only [a-z0-9_], <= 63 chars).
func RandomSchema(t testing.TB) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("dbtest: generate random schema name: %v", err)
	}
	return "test_" + hex.EncodeToString(b[:])
}

// NewSchema creates a fresh throwaway schema, migrates it to the latest version,
// and returns a pool bound to it. The pool is closed and the schema dropped via
// t.Cleanup. This is the common entry point for later milestones' tests.
func NewSchema(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := URL(t)
	schema := RandomSchema(t)

	ctx := context.Background()
	if _, err := migrate.Run(ctx, url, schema); err != nil {
		t.Fatalf("dbtest: migrate schema %q: %v", schema, err)
	}

	pool, err := db.Connect(ctx, url, schema, 4)
	if err != nil {
		// The schema exists at this point; drop it so a connect failure does not
		// leak a schema.
		_ = DropSchema(ctx, url, schema)
		t.Fatalf("dbtest: connect to schema %q: %v", schema, err)
	}

	t.Cleanup(func() {
		pool.Close()
		if err := DropSchema(context.Background(), url, schema); err != nil {
			t.Errorf("dbtest: drop schema %q: %v", schema, err)
		}
	})
	return pool
}

// DropSchema drops a schema and every object in it. It opens and closes its own
// short-lived connection so it is independent of any pool under test.
func DropSchema(ctx context.Context, url, schema string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	_, err = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	return err
}
