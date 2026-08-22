package obs

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector reports pgxpool.Stat on each scrape (plan §M9 "pgxpool stats"). A
// custom collector avoids a background sampler: the values are read straight from
// the live pool when Prometheus scrapes, so they never go stale.
type poolCollector struct {
	pool *pgxpool.Pool

	acquiredConns     *prometheus.Desc
	idleConns         *prometheus.Desc
	constructingConns *prometheus.Desc
	totalConns        *prometheus.Desc
	maxConns          *prometheus.Desc
	acquireCount      *prometheus.Desc
	acquireDuration   *prometheus.Desc
	emptyAcquireCount *prometheus.Desc
	canceledAcquire   *prometheus.Desc
	newConnsCount     *prometheus.Desc
}

func newPoolCollector(pool *pgxpool.Pool) *poolCollector {
	d := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "pool", name), help, nil, nil)
	}
	return &poolCollector{
		pool:              pool,
		acquiredConns:     d("acquired_conns", "Connections currently in use."),
		idleConns:         d("idle_conns", "Idle connections in the pool."),
		constructingConns: d("constructing_conns", "Connections currently being established."),
		totalConns:        d("total_conns", "Total connections (acquired + idle + constructing)."),
		maxConns:          d("max_conns", "Configured maximum pool size."),
		acquireCount:      d("acquire_count_total", "Cumulative successful connection acquisitions."),
		acquireDuration:   d("acquire_duration_seconds_total", "Cumulative time spent blocking on Acquire."),
		emptyAcquireCount: d("empty_acquire_count_total", "Acquisitions that had to wait for a connection (pool was empty)."),
		canceledAcquire:   d("canceled_acquire_count_total", "Acquisitions canceled by context before completing."),
		newConnsCount:     d("new_conns_count_total", "Cumulative new connections opened."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.acquiredConns
	ch <- c.idleConns
	ch <- c.constructingConns
	ch <- c.totalConns
	ch <- c.maxConns
	ch <- c.acquireCount
	ch <- c.acquireDuration
	ch <- c.emptyAcquireCount
	ch <- c.canceledAcquire
	ch <- c.newConnsCount
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	g := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
	}
	ct := func(desc *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v)
	}
	g(c.acquiredConns, float64(s.AcquiredConns()))
	g(c.idleConns, float64(s.IdleConns()))
	g(c.constructingConns, float64(s.ConstructingConns()))
	g(c.totalConns, float64(s.TotalConns()))
	g(c.maxConns, float64(s.MaxConns()))
	ct(c.acquireCount, float64(s.AcquireCount()))
	ct(c.acquireDuration, s.AcquireDuration().Seconds())
	ct(c.emptyAcquireCount, float64(s.EmptyAcquireCount()))
	ct(c.canceledAcquire, float64(s.CanceledAcquireCount()))
	ct(c.newConnsCount, float64(s.NewConnsCount()))
}
