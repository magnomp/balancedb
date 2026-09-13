package balancedb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/magnomp/balancedb"
)

func TestInvalidDeploymentConfig(t *testing.T) {
	for _, cfg := range []balancedb.Config{
		{},
		{DatabaseURL: "postgres://localhost/app", Schema: "ledger;DROP"},
		{DatabaseURL: "postgres://localhost/app", Schema: strings.Repeat("a", 64)},
		{DatabaseURL: "postgres://localhost/app", MaxConns: 1},
		{DatabaseURL: "postgres://localhost/app", MaxConns: -1},
	} {
		if d, err := balancedb.Open(context.Background(), cfg); err == nil {
			d.Close()
			t.Fatalf("Open accepted invalid config: %+v", cfg)
		}
		if _, err := balancedb.Migrate(context.Background(), cfg); err == nil {
			t.Fatalf("Migrate accepted invalid config: %+v", cfg)
		}
	}
}
