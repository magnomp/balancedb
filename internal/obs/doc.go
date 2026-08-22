// Package obs is the observability surface of a cell (spec §13): the Prometheus
// metrics registry, the /metrics + /healthz + /readyz HTTP handler, HTTP
// panic-recovery and request-logging middleware, and the pgx slow-query tracer.
//
// It is a leaf package — the processor and api packages record into a *Metrics
// (or a narrow observer interface) that is nil-safe, so a component with no
// metrics wired keeps working unchanged and tests need not construct one. Every
// metric here is documented, with its §13 / ADR-0002 rationale, in the project
// README.
package obs
