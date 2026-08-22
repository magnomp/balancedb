// Package config parses and validates BalanceDB's deployment configuration from
// BALANCEDB_* environment variables (plan §0). Deployment knobs live here;
// behavioral knobs live in the DB config table (spec §5.2), not in this package.
// The process refuses to start on any invalid value.
package config
