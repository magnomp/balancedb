// Command balancedb is the single binary for all three roles: api, processor,
// and migrate. The role is selected by the first CLI argument (plan §0). This
// file owns process lifecycle: config load, logging, signal handling, and
// graceful shutdown. Role bodies (server, loop, migrator) arrive in later
// milestones and consume the shutdown context established here.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/magnomp/balancedb/internal/api"
	"github.com/magnomp/balancedb/internal/config"
	"github.com/magnomp/balancedb/internal/db"
	"github.com/magnomp/balancedb/internal/lease"
	"github.com/magnomp/balancedb/internal/migrate"
	"github.com/magnomp/balancedb/internal/notify"
	"github.com/magnomp/balancedb/internal/obs"
	"github.com/magnomp/balancedb/internal/processor"
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

	// The slow-query tracer is attached to every pooled connection (plan §M9). It is
	// created before Connect (the tracer must exist when connections are built) and
	// its metrics sink is set once the registry exists — IncSlowQuery is nil-safe, so
	// the boot ping traced before then simply counts nothing.
	tracer := obs.NewSlowQueryTracer(logger, nil)
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.Schema, cfg.PoolMaxConns, db.WithTracer(tracer))
	if err != nil {
		return err
	}
	defer pool.Close()
	logger.Info("database connected", "schema", cfg.Schema)

	// One metric registry per process (spec §13); the pgxpool collector reads the
	// live pool on each scrape (plan §M9).
	metrics := obs.NewMetrics(pool)
	tracer.Metrics = metrics

	if role == config.RoleProcessor {
		l, err := lease.New(pool)
		if err != nil {
			return fmt.Errorf("processor: init lease: %w", err)
		}
		proc := processor.New(pool, l, logger)
		proc.SetMetrics(metrics)

		// /readyz includes lease state as information only — a standby is still ready
		// (plan §M9). proc.IsLeader is the goroutine-safe view.
		serveMetrics(ctx, logger, cfg.MetricsAddr, obs.HealthConfig{
			Registry: metrics.Registry(), DB: pool, Role: string(role), Leader: proc.IsLeader,
		})

		logger.Info("running processor loop", "owner", l.Owner())
		if err := proc.Run(ctx); err != nil {
			return fmt.Errorf("processor: %w", err)
		}
		logger.Info("shutting down", "role", string(role))
		return nil
	}

	// api role: serve the Huma-backed HTTP API (spec §10, ADR-0003).
	return runAPI(ctx, logger, cfg, pool, metrics)
}

// serveMetrics starts the operational HTTP surface (/metrics, /healthz, /readyz)
// on BALANCEDB_METRICS_ADDR (plan §0, both roles) and shuts it down when ctx is
// cancelled. It is best-effort: a bind failure is logged, never fatal, so a metrics
// port clash never takes down the ledger.
func serveMetrics(ctx context.Context, logger *slog.Logger, addr string, hc obs.HealthConfig) {
	srv := &http.Server{
		Addr:              addr,
		Handler:           obs.WithRecovery(logger, obs.NewHealthHandler(hc)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	go func() {
		logger.Info("metrics server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server", "err", err)
		}
	}()
}

// runAPI serves the HTTP API and shuts it down gracefully when ctx is cancelled.
func runAPI(ctx context.Context, logger *slog.Logger, cfg config.Config, pool *pgxpool.Pool, metrics *obs.Metrics) error {
	// The operational surface (/metrics, /healthz, /readyz) on its own port (plan §0).
	serveMetrics(ctx, logger, cfg.MetricsAddr, obs.HealthConfig{
		Registry: metrics.Registry(), DB: pool, Role: string(config.RoleAPI),
	})

	// One dedicated LISTEN outcomes connection per API process (ADR-0002); it feeds
	// the synchronous wait path (wait_ms > 0). Run in its own goroutine so it
	// reconnects independently of request handling; it stops when ctx is cancelled.
	notifier := notify.New(pool, logger)
	go func() {
		if err := notifier.Run(ctx); err != nil {
			logger.Warn("outcomes notifier stopped", "err", err)
		}
	}()

	apiServer := api.NewServer(pool, nil, notifier)
	apiServer.SetWaitObserver(metrics)

	// Panic-recovery + request logging wrap the whole API handler (plan §M9).
	handler := obs.WithRecovery(logger, obs.WithRequestLog(logger, apiServer.Handler()))

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.HTTPAddr, "docs", cfg.HTTPAddr+"/docs")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down", "role", "api")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		return nil
	}
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
