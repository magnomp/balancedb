package config

import (
	"strings"
	"testing"
)

// mapEnv returns a Getenv backed by a map, so tests never touch the process
// environment.
func mapEnv(m map[string]string) Getenv {
	return func(key string) string { return m[key] }
}

const validURL = "postgres://user:secret@localhost:5432/db?sslmode=disable"

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom(mapEnv(map[string]string{
		"BALANCEDB_DATABASE_URL": validURL,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Schema != defaultSchema {
		t.Errorf("Schema = %q, want %q", cfg.Schema, defaultSchema)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.MetricsAddr != defaultMetricsAddr {
		t.Errorf("MetricsAddr = %q, want %q", cfg.MetricsAddr, defaultMetricsAddr)
	}
	if cfg.LogLevel != defaultLogLevel {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, defaultLogLevel)
	}
	if cfg.LogFormat != defaultLogFormat {
		t.Errorf("LogFormat = %q, want %q", cfg.LogFormat, defaultLogFormat)
	}
	if !cfg.MigrateOnStart {
		t.Errorf("MigrateOnStart = false, want true (default)")
	}
	if cfg.PoolMaxConns != defaultPoolMax {
		t.Errorf("PoolMaxConns = %d, want %d", cfg.PoolMaxConns, defaultPoolMax)
	}
}

func TestLoadMissingURL(t *testing.T) {
	_, err := LoadFrom(mapEnv(map[string]string{}))
	if err == nil {
		t.Fatal("expected error for missing BALANCEDB_DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "BALANCEDB_DATABASE_URL") {
		t.Errorf("error = %q, want mention of BALANCEDB_DATABASE_URL", err)
	}
}

func TestValidateDatabaseURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"postgres scheme", "postgres://h/db", true},
		{"postgresql scheme", "postgresql://h/db", true},
		{"wrong scheme", "mysql://h/db", false},
		{"no host", "postgres:///db", false},
		{"garbage", "://::::", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{}
			if tc.url != "" {
				m["BALANCEDB_DATABASE_URL"] = tc.url
			}
			_, err := LoadFrom(mapEnv(m))
			if tc.ok && err != nil {
				t.Errorf("url %q: unexpected error %v", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("url %q: expected error, got none", tc.url)
			}
		})
	}
}

func TestSchemaNameValidation(t *testing.T) {
	cases := []struct {
		schema string
		ok     bool
	}{
		{"balancedb", true},
		{"b", true},
		{"_private", true},
		{"tenant_42", true},
		{"1leading_digit", false},
		{"Upper", false},
		{"with-dash", false},
		{"with space", false},
		{"with.dot", false},
		{"drop;table", false},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			_, err := LoadFrom(mapEnv(map[string]string{
				"BALANCEDB_DATABASE_URL": validURL,
				"BALANCEDB_SCHEMA":       tc.schema,
			}))
			if tc.ok && err != nil {
				t.Errorf("schema %q: unexpected error %v", tc.schema, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("schema %q: expected error, got none", tc.schema)
			}
		})
	}
}

func TestLogLevelAndFormat(t *testing.T) {
	cases := []struct {
		level, format string
		ok            bool
	}{
		{"info", "json", true},
		{"DEBUG", "TEXT", true}, // case-insensitive
		{"warn", "text", true},
		{"error", "json", true},
		{"trace", "json", false},
		{"info", "yaml", false},
	}
	for _, tc := range cases {
		t.Run(tc.level+"/"+tc.format, func(t *testing.T) {
			_, err := LoadFrom(mapEnv(map[string]string{
				"BALANCEDB_DATABASE_URL": validURL,
				"BALANCEDB_LOG_LEVEL":    tc.level,
				"BALANCEDB_LOG_FORMAT":   tc.format,
			}))
			if tc.ok && err != nil {
				t.Errorf("unexpected error %v", err)
			}
			if !tc.ok && err == nil {
				t.Errorf("expected error, got none")
			}
		})
	}
}

func TestMigrateOnStartParsing(t *testing.T) {
	cases := []struct {
		val  string
		want bool
		ok   bool
	}{
		{"true", true, true},
		{"false", false, true},
		{"1", true, true},
		{"0", false, true},
		{"yes", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			cfg, err := LoadFrom(mapEnv(map[string]string{
				"BALANCEDB_DATABASE_URL":     validURL,
				"BALANCEDB_MIGRATE_ON_START": tc.val,
			}))
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if cfg.MigrateOnStart != tc.want {
					t.Errorf("MigrateOnStart = %v, want %v", cfg.MigrateOnStart, tc.want)
				}
			} else if err == nil {
				t.Errorf("expected error for %q", tc.val)
			}
		})
	}
}

func TestPoolMaxConnsParsing(t *testing.T) {
	cases := []struct {
		val  string
		want int32
		ok   bool
	}{
		{"1", 1, true},
		{"25", 25, true},
		{"0", 0, false},
		{"-3", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			cfg, err := LoadFrom(mapEnv(map[string]string{
				"BALANCEDB_DATABASE_URL":   validURL,
				"BALANCEDB_POOL_MAX_CONNS": tc.val,
			}))
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if cfg.PoolMaxConns != tc.want {
					t.Errorf("PoolMaxConns = %d, want %d", cfg.PoolMaxConns, tc.want)
				}
			} else if err == nil {
				t.Errorf("expected error for %q", tc.val)
			}
		})
	}
}

func TestRedactedDSN(t *testing.T) {
	cfg, err := LoadFrom(mapEnv(map[string]string{
		"BALANCEDB_DATABASE_URL": validURL,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	red := cfg.RedactedDSN()
	if strings.Contains(red, "secret") {
		t.Errorf("redacted DSN still contains password: %q", red)
	}
	if !strings.Contains(red, "localhost:5432") {
		t.Errorf("redacted DSN lost host info: %q", red)
	}
}
