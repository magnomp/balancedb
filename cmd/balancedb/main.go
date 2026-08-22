// Command balancedb is the single binary for all three roles: api, processor,
// and migrate. The role is selected by the first CLI argument (plan §0). This
// file owns process lifecycle: config load, logging, signal handling, and
// graceful shutdown. Role bodies (server, loop, migrator) arrive in later
// milestones and consume the shutdown context established here.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/magnomp/balancedb/internal/config"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/migrate"
)

func main() {
	if err := run(); err != nil {
		// Logger may not be up yet; write plainly to stderr.
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return fmt.Errorf("no role given")
	}
	role := config.Role(os.Args[1])
	switch role {
	case config.RoleAPI, config.RoleProcessor, config.RoleMigrate:
	default:
		usage()
		return fmt.Errorf("unknown role %q", os.Args[1])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg)
	slog.SetDefault(logger)

	// Context cancelled on SIGTERM/SIGINT — the single shutdown signal every
	// later role body (loop, server) hangs off of.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	logger.Info("starting",
		"role", string(role),
		"schema", cfg.Schema,
		"dsn", cfg.RedactedDSN(),
		"pool_max_conns", cfg.PoolMaxConns,
		"migrate_on_start", cfg.MigrateOnStart,
	)

	// Migrations: the migrate role always runs them; api and processor run them
	// on boot unless BALANCEDB_MIGRATE_ON_START=false (an explicit migrate gate,
	// plan §0/§M2). The runner opens its own dedicated connection, so it runs
	// before the app pool is built.
	if role == config.RoleMigrate || cfg.MigrateOnStart {
		logger.Info("running migrations", "schema", cfg.Schema)
		applied, err := migrate.Run(ctx, cfg.DatabaseURL, cfg.Schema)
		if err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		logger.Info("migrations up to date", "schema", cfg.Schema, "applied", applied)
	} else {
		logger.Info("skipping migrations on start (BALANCEDB_MIGRATE_ON_START=false)")
	}

	if role == config.RoleMigrate {
		logger.Info("migrate role complete")
		return nil
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.Schema, cfg.PoolMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("database connected", "schema", cfg.Schema)

	// The HTTP server (M7) and processor loop (M5) are not built yet. Boot, hold
	// until a shutdown signal, then exit cleanly so the lifecycle wiring is
	// exercised from day one (plan §M1).
	logger.Info("running; awaiting shutdown signal", "role", string(role))
	<-ctx.Done()

	logger.Info("shutting down", "role", string(role))
	return nil
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.New(handler)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: balancedb <api|processor|migrate>")
}
