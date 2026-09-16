package obs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// gather returns the names of every metric family currently in the registry.
func gather(t *testing.T, m *Metrics) map[string]bool {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}
	return names
}

func TestNewMetricsRegistersTheSpecSet(t *testing.T) {
	m := NewMetrics(nil) // nil pool: pool collector is skipped, everything else present.
	// Exercise the label-vec metrics so they materialize at least one series.
	m.RecordDecision(KindSingle, OutcomeConfirmed)
	m.ObserveWait(0.01, WaitSourceNotify, WaitResultDecided)

	names := gather(t, m)
	want := []string{
		"balancedb_queue_depth",
		"balancedb_oldest_pending_age_seconds",
		"balancedb_loop_busy_seconds_total",
		"balancedb_loop_wall_seconds_total",
		"balancedb_leadership_changes_total",
		"balancedb_leader",
		"balancedb_decisions_total",
		"balancedb_edit_deferrals_total",
		"balancedb_snapshot_rows_touched",
		"balancedb_doorbell_wakeup_lag_seconds",
		"balancedb_api_wait_seconds",
		"balancedb_slow_queries_total",
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("metric %q not registered", w)
		}
	}
	// The pgxpool collector must NOT be present when the pool is nil.
	if names["balancedb_pool_total_conns"] {
		t.Error("pool collector registered despite nil pool")
	}
}

func TestNilMetricsRecordsAreNoOps(t *testing.T) {
	var m *Metrics // nil receiver
	// None of these must panic.
	m.SetQueueDepth(1)
	m.SetOldestPendingAge(1)
	m.AddLoopBusy(1)
	m.AddLoopWall(1)
	m.LeadershipChanged()
	m.SetLeader(true)
	m.RecordDecision(KindGroup, OutcomeRejected)
	m.ObserveSnapshotRows(3)
	m.ObserveDoorbellLag(0.5)
	m.ObserveWait(0.5, WaitSourcePoll, WaitResultDecided)
	m.IncSlowQuery()
}

func TestNegativeDoorbellLagFlooredToZero(t *testing.T) {
	m := NewMetrics(nil)
	m.ObserveDoorbellLag(-5) // must not panic and must not record a negative value
	families, _ := m.Registry().Gather()
	for _, f := range families {
		if f.GetName() != "balancedb_doorbell_wakeup_lag_seconds" {
			continue
		}
		h := f.GetMetric()[0].GetHistogram()
		if got := h.GetSampleSum(); got < 0 {
			t.Errorf("doorbell lag sample sum is negative: %v", got)
		}
	}
}

func TestHealthHandlerHealthz(t *testing.T) {
	m := NewMetrics(nil)
	h := NewHealthHandler(HealthConfig{Registry: m.Registry(), Role: "api"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("/healthz body = %q", rec.Body.String())
	}
}

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestReadyzReflectsDBReachability(t *testing.T) {
	m := NewMetrics(nil)

	t.Run("db ok", func(t *testing.T) {
		h := NewHealthHandler(HealthConfig{Registry: m.Registry(), DB: fakePinger{}, Role: "api"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("/readyz status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
			t.Errorf("body = %q", rec.Body.String())
		}
	})

	t.Run("db down", func(t *testing.T) {
		h := NewHealthHandler(HealthConfig{Registry: m.Registry(), DB: fakePinger{err: errors.New("boom")}, Role: "api"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz status = %d, want 503", rec.Code)
		}
	})
}

func TestReadyzStandbyIsHealthy(t *testing.T) {
	m := NewMetrics(nil)
	// A standby: leader=false, DB reachable ⇒ still ready (plan §M9).
	h := NewHealthHandler(HealthConfig{
		Registry: m.Registry(), DB: fakePinger{}, Role: "processor",
		Leader: func() bool { return false },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("standby /readyz status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"leader":false`) || !strings.Contains(body, `"status":"ready"`) {
		t.Errorf("standby body = %q", body)
	}
}

func TestMetricsEndpointServes(t *testing.T) {
	m := NewMetrics(nil)
	m.SetQueueDepth(7)
	h := NewHealthHandler(HealthConfig{Registry: m.Registry(), Role: "api"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "balancedb_queue_depth 7") {
		t.Errorf("/metrics did not expose queue_depth: %q", rec.Body.String())
	}
}

func TestWithRecoveryTurnsPanicInto500(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := WithRecovery(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWithRequestLogPassesThrough(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := WithRequestLog(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hi"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if rec.Body.String() != "hi" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestSlowQueryTracerCountsOverThreshold(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewMetrics(nil)
	tr := NewSlowQueryTracer(logger, m)
	tr.Threshold = time.Millisecond

	// Start, then simulate an elapsed of > threshold by rewinding the stored start.
	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	started := ctx.Value(slowQueryCtxKey{}).(slowQueryStart)
	started.at = started.at.Add(-time.Second)
	ctx = context.WithValue(ctx, slowQueryCtxKey{}, started)
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	if got := slowQueryCount(t, m); got != 1 {
		t.Fatalf("slow_queries_total = %d, want 1", got)
	}

	// A fast query must not be counted.
	ctx2 := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 2"})
	tr.TraceQueryEnd(ctx2, nil, pgx.TraceQueryEndData{})
	if got := slowQueryCount(t, m); got != 1 {
		t.Fatalf("slow_queries_total = %d after fast query, want 1", got)
	}
}

func slowQueryCount(t *testing.T, m *Metrics) int {
	t.Helper()
	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() == "balancedb_slow_queries_total" {
			return int(f.GetMetric()[0].GetCounter().GetValue())
		}
	}
	return 0
}

// Ensure *Metrics satisfies the api.WaitObserver shape (compile-time check of the
// ObserveWait signature the api package depends on).
var _ interface {
	ObserveWait(seconds float64, source, result string)
} = (*Metrics)(nil)
