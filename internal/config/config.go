// Package config parses and validates BalanceDB's deployment configuration
// from BALANCEDB_* environment variables (plan §0). Behavioral knobs live in
// the DB config table, not here. The process refuses to start on any invalid
// value.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Role is the process role selected on the command line.
type Role string

const (
	RoleAPI       Role = "api"
	RoleProcessor Role = "processor"
	RoleMigrate   Role = "migrate"
)

// schemaNameRe validates BALANCEDB_SCHEMA. The schema name is applied as a
// quoted identifier in AfterConnect, never string-interpolated, but we still
// constrain it to a conservative Postgres identifier so a misconfiguration is
// caught at boot rather than deep in a query (plan §0).
var schemaNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Config is the fully parsed, validated deployment configuration.
type Config struct {
	DatabaseURL    string // BALANCEDB_DATABASE_URL (required)
	Schema         string // BALANCEDB_SCHEMA
	HTTPAddr       string // BALANCEDB_HTTP_ADDR (api role)
	MetricsAddr    string // BALANCEDB_METRICS_ADDR
	LogLevel       string // BALANCEDB_LOG_LEVEL
	LogFormat      string // BALANCEDB_LOG_FORMAT: json | text
	MigrateOnStart bool   // BALANCEDB_MIGRATE_ON_START
	PoolMaxConns   int32  // BALANCEDB_POOL_MAX_CONNS
}

// Defaults for optional variables, per plan §0.
const (
	defaultSchema      = "balancedb"
	defaultHTTPAddr    = ":8080"
	defaultMetricsAddr = ":9090"
	defaultLogLevel    = "info"
	defaultLogFormat   = "json"
	defaultPoolMax     = 10
)

var validLogLevels = map[string]struct{}{
	"debug": {}, "info": {}, "warn": {}, "error": {},
}

// Getenv abstracts the environment lookup so tests can inject values without
// mutating the process environment.
type Getenv func(key string) string

// Load reads and validates configuration using os.Getenv.
func Load() (Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads and validates configuration using the supplied lookup.
func LoadFrom(getenv Getenv) (Config, error) {
	cfg := Config{
		DatabaseURL:    strings.TrimSpace(getenv("BALANCEDB_DATABASE_URL")),
		Schema:         envDefault(getenv, "BALANCEDB_SCHEMA", defaultSchema),
		HTTPAddr:       envDefault(getenv, "BALANCEDB_HTTP_ADDR", defaultHTTPAddr),
		MetricsAddr:    envDefault(getenv, "BALANCEDB_METRICS_ADDR", defaultMetricsAddr),
		LogLevel:       strings.ToLower(envDefault(getenv, "BALANCEDB_LOG_LEVEL", defaultLogLevel)),
		LogFormat:      strings.ToLower(envDefault(getenv, "BALANCEDB_LOG_FORMAT", defaultLogFormat)),
		MigrateOnStart: true,
		PoolMaxConns:   defaultPoolMax,
	}

	if v := strings.TrimSpace(getenv("BALANCEDB_MIGRATE_ON_START")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return Config{}, fmt.Errorf("BALANCEDB_MIGRATE_ON_START: %q is not a boolean", v)
		}
		cfg.MigrateOnStart = b
	}

	if v := strings.TrimSpace(getenv("BALANCEDB_POOL_MAX_CONNS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("BALANCEDB_POOL_MAX_CONNS: %q is not an integer", v)
		}
		if n < 1 {
			return Config{}, fmt.Errorf("BALANCEDB_POOL_MAX_CONNS: must be >= 1, got %d", n)
		}
		cfg.PoolMaxConns = int32(n)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.DatabaseURL == "" {
		return errors.New("BALANCEDB_DATABASE_URL is required")
	}
	if err := validateDatabaseURL(c.DatabaseURL); err != nil {
		return fmt.Errorf("BALANCEDB_DATABASE_URL: %w", err)
	}
	if !schemaNameRe.MatchString(c.Schema) {
		return fmt.Errorf("BALANCEDB_SCHEMA: %q is not a valid schema name (must match %s)", c.Schema, schemaNameRe.String())
	}
	if _, ok := validLogLevels[c.LogLevel]; !ok {
		return fmt.Errorf("BALANCEDB_LOG_LEVEL: %q is not one of debug|info|warn|error", c.LogLevel)
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return fmt.Errorf("BALANCEDB_LOG_FORMAT: %q is not one of json|text", c.LogFormat)
	}
	return nil
}

// validateDatabaseURL confirms the DSN is a well-formed postgres URL. It does
// not connect — that is db.Connect's ping-on-boot (plan §M1).
func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
	default:
		return fmt.Errorf("scheme must be postgres:// or postgresql://, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// RedactedDSN returns the database URL with any userinfo password replaced by
// "xxxxx", safe to emit in logs (plan §M1 startup line). If the URL cannot be
// parsed it returns a fixed placeholder rather than risk leaking the raw value.
func (c Config) RedactedDSN() string {
	u, err := url.Parse(c.DatabaseURL)
	if err != nil {
		return "<unparseable-dsn>"
	}
	return u.Redacted()
}

func envDefault(getenv Getenv, key, def string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return def
}
