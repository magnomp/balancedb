package obs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultSlowQueryThreshold is the duration above which a query is logged as slow
// (plan §M9). It is a fixed default rather than a config knob: it is a diagnostic
// aid, not a behavioral surface. Override SlowQueryTracer.Threshold to tune it.
const DefaultSlowQueryThreshold = 200 * time.Millisecond

// slowQueryCtxKey carries the query start time and SQL from TraceQueryStart to
// TraceQueryEnd. Unexported struct type so it never collides with another
// package's context keys.
type slowQueryCtxKey struct{}

// slowQueryStart is the per-query state threaded through the trace context. Only
// the SQL text is carried, never the argument values (which may be sensitive).
type slowQueryStart struct {
	at  time.Time
	sql string
}

// SlowQueryTracer is a pgx QueryTracer that logs (and counts) any query whose
// execution exceeds Threshold (plan §M9 "slow-query logging via pgx tracer").
// Wire it onto a pool via db.Connect(..., db.WithTracer(tracer)). Metrics may be
// nil (logging only).
type SlowQueryTracer struct {
	Logger    *slog.Logger
	Metrics   *Metrics
	Threshold time.Duration
}

// NewSlowQueryTracer builds a tracer at DefaultSlowQueryThreshold. A nil logger
// falls back to slog.Default; metrics may be nil.
func NewSlowQueryTracer(logger *slog.Logger, metrics *Metrics) *SlowQueryTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlowQueryTracer{Logger: logger, Metrics: metrics, Threshold: DefaultSlowQueryThreshold}
}

// TraceQueryStart stamps the start time and SQL into the context pgx threads to
// the end.
func (t *SlowQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, slowQueryCtxKey{}, slowQueryStart{at: time.Now(), sql: data.SQL})
}

// TraceQueryEnd measures the query and logs/counts it when it exceeds Threshold.
// The elapsed time is a within-process duration (start to end on the same clock),
// so process time is correct here — this is not a lease or ordering comparison.
func (t *SlowQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(slowQueryCtxKey{}).(slowQueryStart)
	if !ok {
		return
	}
	elapsed := time.Since(started.at)
	threshold := t.Threshold
	if threshold <= 0 {
		threshold = DefaultSlowQueryThreshold
	}
	if elapsed < threshold {
		return
	}
	t.Metrics.IncSlowQuery()
	t.Logger.Warn("slow query",
		"elapsed", elapsed,
		"threshold", threshold,
		"sql", started.sql,
		"command_tag", data.CommandTag.String(),
	)
}
