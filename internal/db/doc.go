// Package db owns the pgx connection pool: it sets search_path on every pooled
// connection so all SQL can stay unqualified (plan §0), pings on boot with clear
// error text, and provides the WithTx helper used by every writer.
package db
