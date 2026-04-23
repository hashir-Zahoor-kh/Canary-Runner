// Package metrics exposes Canary Runner state as Prometheus metrics on
// /metrics and a small JSON /health endpoint.
//
// All collectors live on a *private* prometheus.Registry — we deliberately
// avoid the global default registry so Canary Runner's output doesn't pick
// up unrelated metrics from any library that might auto-register.
package metrics

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/hashir/canary-runner/internal/probe"
)

// LatencyBuckets are the histogram buckets (in seconds) for the
// canary_probe_latency_seconds metric. Exported so anyone wiring up a
// Grafana dashboard can reference the same values in a histogram_quantile.
var LatencyBuckets = []float64{0.05, 0.1, 0.2, 0.5, 1.0, 2.0, 5.0}

// Exporter owns every Prometheus collector Canary Runner publishes. It is
// constructed once at startup and mutated from the main probe loop;
// reads happen concurrently whenever Prometheus scrapes. All of the
// prometheus vec types are safe for concurrent use, so Exporter itself
// needs no mutex.
type Exporter struct {
	registry *prometheus.Registry

	probeSuccess    *prometheus.GaugeVec
	probeLatency    *prometheus.HistogramVec
	sloCompliance   *prometheus.GaugeVec
	budgetRemaining *prometheus.GaugeVec
	budgetConsumed  *prometheus.GaugeVec
	probesTotal     *prometheus.CounterVec
}

// NewExporter constructs a fresh Exporter with its own private registry.
// Every metric family is registered up-front so /metrics responds
// correctly even before the first probe has run.
func NewExporter() *Exporter {
	e := &Exporter{
		registry: prometheus.NewRegistry(),

		// canary_probe_success: latest outcome of the probe (1 or 0).
		// Useful for alerts like "this endpoint is down *right now*".
		probeSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "canary_probe_success",
			Help: "1 if the last probe of the endpoint succeeded (2xx), 0 otherwise.",
		}, []string{"endpoint", "method"}),

		// canary_probe_latency_seconds: histogram of every probe's latency.
		// Prom users compute p50/p95/p99 with histogram_quantile().
		probeLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "canary_probe_latency_seconds",
			Help:    "Probe latency in seconds.",
			Buckets: LatencyBuckets,
		}, []string{"endpoint"}),

		// canary_slo_compliance_percent: current rolling-window compliance
		// for each SLO type (availability vs latency).
		sloCompliance: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "canary_slo_compliance_percent",
			Help: "Current SLO compliance as a percentage (0-100).",
		}, []string{"endpoint", "slo_type"}),

		// canary_error_budget_remaining_percent and
		// canary_error_budget_consumed_minutes: the two numbers humans
		// actually watch on an SLO dashboard.
		budgetRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "canary_error_budget_remaining_percent",
			Help: "Remaining error budget as a percentage of the total budget.",
		}, []string{"endpoint"}),
		budgetConsumed: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "canary_error_budget_consumed_minutes",
			Help: "Consumed error budget in minutes over the current window.",
		}, []string{"endpoint"}),

		// canary_probes_total: counter of every probe ever attempted,
		// split by success/failure. Good for rate() alerts.
		probesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "canary_probes_total",
			Help: "Total number of probes executed, labelled by result (success or failure).",
		}, []string{"endpoint", "result"}),
	}

	// MustRegister panics on duplicate registration — but because the
	// registry is private to this Exporter, duplicates would indicate a
	// programming error that we'd rather crash on than silently ignore.
	e.registry.MustRegister(
		e.probeSuccess,
		e.probeLatency,
		e.sloCompliance,
		e.budgetRemaining,
		e.budgetConsumed,
		e.probesTotal,
	)
	return e
}

// RecordResult updates the per-result metrics from one probe outcome:
// the success gauge, the latency histogram, and the probes_total counter.
// Call this after every probe.
func (e *Exporter) RecordResult(r probe.Result) {
	// probe.Result.Method is always populated by probe.Do (defaulted to
	// GET if the caller didn't set one), but we defend in case someone
	// constructs a Result by hand in a test.
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}

	if r.Success {
		e.probeSuccess.WithLabelValues(r.Name, method).Set(1)
		e.probesTotal.WithLabelValues(r.Name, "success").Inc()
	} else {
		e.probeSuccess.WithLabelValues(r.Name, method).Set(0)
		e.probesTotal.WithLabelValues(r.Name, "failure").Inc()
	}
	// Observe latency even for failures — a 500 at 50 ms and a 500 at
	// 5 s tell very different stories.
	e.probeLatency.WithLabelValues(r.Name).Observe(r.Latency.Seconds())
}

// UpdateSLO sets both SLO compliance gauges for an endpoint.
// Call this after calc.Compliance() in the main loop so the two
// gauges for an endpoint are updated atomically from the caller's POV.
func (e *Exporter) UpdateSLO(endpoint string, availability, latency float64) {
	e.sloCompliance.WithLabelValues(endpoint, "availability").Set(availability)
	e.sloCompliance.WithLabelValues(endpoint, "latency").Set(latency)
}

// UpdateBudget sets the error-budget gauges for an endpoint.
func (e *Exporter) UpdateBudget(endpoint string, consumedMinutes, remainingPercent float64) {
	e.budgetConsumed.WithLabelValues(endpoint).Set(consumedMinutes)
	e.budgetRemaining.WithLabelValues(endpoint).Set(remainingPercent)
}

// Health is the JSON payload returned by /health. Kept as an exported
// struct so external tests (or operators with a scrape-with-curl habit)
// have a stable schema to rely on.
type Health struct {
	Status           string `json:"status"`
	TargetsMonitored int    `json:"targets_monitored"`
	UptimeSeconds    int64  `json:"uptime_seconds"`
}

// Server returns an *http.Server that exposes /metrics and /health on
// addr. Starting and stopping the server is the caller's job — main.go
// runs it in a goroutine alongside the scheduler so SIGTERM can shut
// both down from the same context.
func (e *Exporter) Server(addr string, targetCount int, startTime time.Time) *http.Server {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(e.registry, promhttp.HandlerOpts{
		// Registry is also passed here so promhttp reports errors on
		// e.registry itself rather than a different one.
		Registry: e.registry,
	}))

	// We capture targetCount and startTime by value, so concurrent Updates
	// to main.go's state don't race against reads here. Health responses
	// are therefore cheap (no locks).
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		h := Health{
			Status:           "healthy",
			TargetsMonitored: targetCount,
			// time.Since(startTime) is safe because time.Time is immutable.
			UptimeSeconds: int64(time.Since(startTime).Seconds()),
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(h); err != nil {
			// The response is already partially written by the time
			// json.Encode fails (rare: usually a closed conn), so we
			// can't change the status code. Silently dropping matches
			// the behaviour of every other mature HTTP handler.
			_ = err
		}
	})

	return &http.Server{
		Addr:    addr,
		Handler: mux,
		// ReadHeaderTimeout mitigates slowloris-style attacks — mandatory
		// from go vet in Go 1.21+.
		ReadHeaderTimeout: 5 * time.Second,
	}
}

// Registry exposes the underlying prometheus.Registry for tests and for
// callers who want to mount Canary Runner metrics into an external mux.
func (e *Exporter) Registry() *prometheus.Registry { return e.registry }
