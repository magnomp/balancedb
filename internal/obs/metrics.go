package obs

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// namespace prefixes every metric name so a scrape groups cleanly (balancedb_*).
const namespace = "balancedb"

// Decision label values (spec §13 "decision counters by outcome"). kind
// distinguishes a single operation from a group transaction; outcome is the
// terminal fact the processor wrote.
const (
	KindSingle = "single"
	KindGroup  = "group"

	OutcomeConfirmed = "confirmed" // single accepted
	OutcomeInvalid   = "invalid"   // single rejected
	OutcomeCommitted = "committed" // group accepted
	OutcomeRejected  = "rejected"  // group rejected
)

// Wait-path label values (spec §13 "NOTIFY→outcome lag", API wait health). source
// records how a synchronous wait resolved, so the ADR-0002 NOTIFY fast path is
// distinguishable from the durability poll fallback; result records whether the
// budget produced a decision or expired still PENDING.
const (
	WaitSourceImmediate = "immediate" // decided before the first poll/notify
	WaitSourceNotify    = "notify"    // resolved by the outcome NOTIFY (the fast path)
	WaitSourcePoll      = "poll"      // resolved by the status-poll fallback
	WaitSourceTimeout   = "timeout"   // budget expired

	WaitResultDecided = "decided"
	WaitResultPending = "pending"
)

// latencyBuckets covers the sub-millisecond doorbell win (ADR-0002 promises tens
// of ms on an idle system) up to multi-second degradation under load.
var latencyBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// snapshotRowBuckets counts rows touched per confirmation: 1 (newest-day common
// case: just the op's own day) up to a deep backdate cascading across many days.
var snapshotRowBuckets = []float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024}

// Metrics is a cell's Prometheus metric set on a private registry (spec §13). All
// record methods are nil-safe: a nil *Metrics is a no-op, so a component with no
// metrics wired (tests, the spec exporter) runs unchanged.
type Metrics struct {
	reg *prometheus.Registry

	// Processor loop + decision engine (§13).
	queueDepth        prometheus.Gauge
	oldestPendingAge  prometheus.Gauge
	loopBusySeconds   prometheus.Counter
	loopWallSeconds   prometheus.Counter
	leadershipChanges prometheus.Counter
	leader            prometheus.Gauge
	decisions         *prometheus.CounterVec
	snapshotRows      prometheus.Histogram
	doorbellLag       prometheus.Histogram

	// API synchronous wait (§13, ADR-0002).
	waitSeconds *prometheus.HistogramVec

	// DB access (slow-query tracer, §13 "DB write metrics").
	slowQueries prometheus.Counter
}

// NewMetrics builds the metric set on a fresh registry. When pool is non-nil a
// pgxpool stats collector is registered (plan §M9), reading pool.Stat() on each
// scrape. Registry() returns the registry to serve at /metrics.
func NewMetrics(pool *pgxpool.Pool) *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		queueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "queue_depth",
			Help: "PENDING operations awaiting a decision (the cell's latency-health signal).",
		}),
		oldestPendingAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "oldest_pending_age_seconds",
			Help: "Age of the oldest PENDING operation, measured on the DB clock (0 when the queue is empty).",
		}),
		loopBusySeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "loop_busy_seconds_total",
			Help: "Cumulative time the leader spent draining work. Loop utilization rho = rate(loop_busy_seconds_total)/rate(loop_wall_seconds_total).",
		}),
		loopWallSeconds: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "loop_wall_seconds_total",
			Help: "Cumulative wall-clock time of leader cycles (drain + idle wait). Denominator of loop utilization rho.",
		}),
		leadershipChanges: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "leadership_changes_total",
			Help: "Lease ownership transitions observed by this instance (gained or lost). rate()*3600 = changes/hour; a high rate is lease flapping.",
		}),
		leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "leader",
			Help: "1 if this processor instance currently holds the leader lease, else 0.",
		}),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "decisions_total",
			Help: "Terminal decisions the processor committed, by kind (single|group) and outcome (confirmed|invalid|committed|rejected).",
		}, []string{"kind", "outcome"}),
		snapshotRows: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "snapshot_rows_touched",
			Help:    "Balance-snapshot rows written per confirmed operation (1 = newest day; more = backdating cascade). Measures the backdating workload (§13).",
			Buckets: snapshotRowBuckets,
		}),
		doorbellLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "doorbell_wakeup_lag_seconds",
			Help:    "Insert-commit to leader-pickup lag per operation, on the DB clock (the ADR-0002 doorbell latency win; grows into queue latency under load).",
			Buckets: latencyBuckets,
		}),
		waitSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "api_wait_seconds",
			Help:    "Synchronous-insert wait latency (register to return), by resolution source and result. NOTIFY->outcome lag / API wait health (§13, ADR-0002).",
			Buckets: latencyBuckets,
		}, []string{"source", "result"}),
		slowQueries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "slow_queries_total",
			Help: "SQL queries whose execution exceeded the slow-query threshold (also logged by the pgx tracer).",
		}),
	}

	reg.MustRegister(
		m.queueDepth, m.oldestPendingAge, m.loopBusySeconds, m.loopWallSeconds,
		m.leadershipChanges, m.leader, m.decisions, m.snapshotRows, m.doorbellLag,
		m.waitSeconds, m.slowQueries,
	)
	// Go runtime + process collectors give GC, goroutine, and FD visibility for free.
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	if pool != nil {
		reg.MustRegister(newPoolCollector(pool))
	}
	return m
}

// Registry returns the underlying registry, to serve at /metrics.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// SetQueueDepth records the current PENDING count (§13 queue depth).
func (m *Metrics) SetQueueDepth(n float64) {
	if m == nil {
		return
	}
	m.queueDepth.Set(n)
}

// SetOldestPendingAge records the oldest PENDING age in seconds (§13).
func (m *Metrics) SetOldestPendingAge(seconds float64) {
	if m == nil {
		return
	}
	m.oldestPendingAge.Set(seconds)
}

// AddLoopBusy adds to the leader's cumulative busy time (§13 loop utilization).
func (m *Metrics) AddLoopBusy(seconds float64) {
	if m == nil {
		return
	}
	m.loopBusySeconds.Add(seconds)
}

// AddLoopWall adds to the leader's cumulative cycle wall-clock (§13).
func (m *Metrics) AddLoopWall(seconds float64) {
	if m == nil {
		return
	}
	m.loopWallSeconds.Add(seconds)
}

// LeadershipChanged counts one lease ownership transition (§13 leadership changes).
func (m *Metrics) LeadershipChanged() {
	if m == nil {
		return
	}
	m.leadershipChanges.Inc()
}

// SetLeader records whether this instance currently holds the lease.
func (m *Metrics) SetLeader(isLeader bool) {
	if m == nil {
		return
	}
	if isLeader {
		m.leader.Set(1)
	} else {
		m.leader.Set(0)
	}
}

// RecordDecision counts one committed terminal decision (§13 decision counters).
func (m *Metrics) RecordDecision(kind, outcome string) {
	if m == nil {
		return
	}
	m.decisions.WithLabelValues(kind, outcome).Inc()
}

// ObserveSnapshotRows records the rows touched by one confirmation (§13).
func (m *Metrics) ObserveSnapshotRows(rows int64) {
	if m == nil {
		return
	}
	m.snapshotRows.Observe(float64(rows))
}

// ObserveDoorbellLag records one insert-commit to pickup lag in seconds (ADR-0002).
func (m *Metrics) ObserveDoorbellLag(seconds float64) {
	if m == nil {
		return
	}
	// A backdated clock or a just-committed row can yield a tiny negative delta;
	// floor at zero so the histogram stays sane.
	if seconds < 0 {
		seconds = 0
	}
	m.doorbellLag.Observe(seconds)
}

// ObserveWait records one synchronous-wait latency (§13 NOTIFY->outcome lag). It
// satisfies the api package's WaitObserver interface.
func (m *Metrics) ObserveWait(seconds float64, source, result string) {
	if m == nil {
		return
	}
	m.waitSeconds.WithLabelValues(source, result).Observe(seconds)
}

// IncSlowQuery counts one query over the slow-query threshold (§13).
func (m *Metrics) IncSlowQuery() {
	if m == nil {
		return
	}
	m.slowQueries.Inc()
}
