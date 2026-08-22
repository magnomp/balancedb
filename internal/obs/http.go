package obs

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// readyTimeout bounds the DB reachability probe on /readyz so a stalled database
// fails the check fast rather than hanging the load balancer's health request.
const readyTimeout = 2 * time.Second

// Pinger is the slice of *pgxpool.Pool that /readyz needs: a bounded DB round
// trip. Kept narrow so obs does not depend on the pool's full surface and tests
// can substitute a fake.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthConfig configures the operational HTTP surface served on
// BALANCEDB_METRICS_ADDR (plan §0, both roles).
type HealthConfig struct {
	// Registry is served at /metrics. Required.
	Registry *prometheus.Registry
	// DB backs /readyz. A nil DB makes /readyz report ready without a probe.
	DB Pinger
	// Role labels the readiness body ("api" | "processor").
	Role string
	// Leader, when non-nil, adds this processor's current lease state to the
	// /readyz body as information only — a standby is still ready (plan §M9).
	Leader func() bool
}

// NewHealthHandler builds the /metrics, /healthz, and /readyz handler (spec §13,
// plan §M9). /healthz reports process liveness (always 200 while serving);
// /readyz reports DB reachability (200 ready, 503 not) and, for the processor,
// includes lease state as information, not readiness.
func NewHealthHandler(cfg HealthConfig) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(cfg.Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": cfg.Role})
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{"role": cfg.Role}
		if cfg.Leader != nil {
			// Info only: a standby (leader=false) is still ready.
			body["leader"] = cfg.Leader()
		}
		if cfg.DB != nil {
			ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
			defer cancel()
			if err := cfg.DB.Ping(ctx); err != nil {
				body["status"] = "not_ready"
				body["db"] = "unreachable"
				writeJSON(w, http.StatusServiceUnavailable, body)
				return
			}
		}
		body["status"] = "ready"
		body["db"] = "ok"
		writeJSON(w, http.StatusOK, body)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// statusRecorder captures the response status and byte count for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// WithRecovery wraps h so a panic in a handler becomes a logged 500 instead of a
// crashed process (plan §M9 panic-recovery middleware). Library code never panics
// (CLAUDE.md); this is the last line of defense at the HTTP boundary.
func WithRecovery(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				logger.Error("panic in http handler", "panic", p, "method", r.Method, "path", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// WithRequestLog wraps h to log every request with its method, path, status, and
// latency (plan §M9 request logging). Logged at Info; 5xx at Warn.
func WithRequestLog(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelWarn
		}
		logger.LogAttrs(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("latency", time.Since(start)),
		)
	})
}
