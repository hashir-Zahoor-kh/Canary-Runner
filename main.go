// Canary Runner entry point.
//
// Stage 5 scope: everything from Stage 4 plus a Prometheus /metrics endpoint
// and a JSON /health endpoint served on :9090 by default. The metrics
// server runs in its own goroutine and shuts down cleanly from the same
// signal context as the scheduler.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/hashir/canary-runner/internal/budget"
	"github.com/hashir/canary-runner/internal/config"
	"github.com/hashir/canary-runner/internal/metrics"
	"github.com/hashir/canary-runner/internal/probe"
	"github.com/hashir/canary-runner/internal/slo"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	// Metrics address is configurable so operators can bind to a specific
	// interface (e.g. 127.0.0.1:9090 for a sidecar pattern) without
	// editing config.yaml. Default matches the Stage 5 spec.
	metricsAddr := flag.String("metrics-addr", ":9090", "address for the Prometheus /metrics and /health server")
	flag.Parse()

	logger, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("failed to construct logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("load config", zap.Error(err))
	}

	// Fan out config.Target into the three runtime shapes (probe, slo,
	// budget). All three share the window duration.
	probeTargets := make([]probe.Target, len(cfg.Targets))
	sloEndpoints := make([]slo.EndpointConfig, len(cfg.Targets))
	budgetEndpoints := make([]budget.EndpointConfig, len(cfg.Targets))
	for i, t := range cfg.Targets {
		window := time.Duration(t.SLO.WindowDays) * 24 * time.Hour
		probeTargets[i] = probe.Target{
			Name:     t.Name,
			URL:      t.URL,
			Method:   t.Method,
			Timeout:  time.Duration(t.TimeoutSeconds) * time.Second,
			Interval: time.Duration(t.IntervalSeconds) * time.Second,
		}
		sloEndpoints[i] = slo.EndpointConfig{
			Name:           t.Name,
			WindowDuration: window,
			LatencyTarget:  time.Duration(t.SLO.LatencyTargetMS) * time.Millisecond,
		}
		budgetEndpoints[i] = budget.EndpointConfig{
			Name:                      t.Name,
			AvailabilityTargetPercent: t.SLO.AvailabilityTargetPercent,
			WindowDuration:            window,
		}
	}

	calc := slo.NewCalculator(slo.RealClock{}, sloEndpoints)
	tracker := budget.NewTracker(budget.RealClock{}, calc, budgetEndpoints)
	exporter := metrics.NewExporter()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Record the process-start time *before* launching anything — the
	// /health endpoint uses this to compute uptime_seconds.
	startedAt := time.Now()

	// Start the metrics HTTP server in its own goroutine. We keep a
	// reference so we can call Shutdown from the main goroutine after
	// the probe loop ends.
	server := exporter.Server(*metricsAddr, len(probeTargets), startedAt)
	go func() {
		// http.ErrServerClosed is the "normal" shutdown signal — not an
		// error we should wake an operator up for.
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped unexpectedly", zap.Error(err))
		}
	}()

	logger.Info("canary-runner starting",
		zap.Int("targets", len(probeTargets)),
		zap.String("config_path", *configPath),
		zap.String("metrics_addr", *metricsAddr),
	)

	s := probe.NewScheduler(probe.NewProber(), logger)
	results := s.Run(ctx, probeTargets)

	// Main loop: for every probe result, update every downstream — the
	// rolling SLO window, the budget tracker, and the three Prometheus
	// metric families that change per-probe. Order is important:
	//   1. Record into calc so its rolling view includes this probe
	//   2. Update tracker so its exhaustion timestamp is current
	//   3. Only then read Compliance / Status and publish to metrics
	for r := range results {
		calc.Record(r)
		tracker.Update(r.Name)

		avail, lat := calc.Compliance(r.Name)
		bud := tracker.Status(r.Name)

		exporter.RecordResult(r)
		exporter.UpdateSLO(r.Name, avail, lat)
		exporter.UpdateBudget(r.Name, bud.ConsumedMinutes, bud.RemainingPercent)

		logger.Info("probe complete",
			zap.String("endpoint", r.Name),
			zap.String("url", r.URL),
			zap.String("method", r.Method),
			zap.Bool("success", r.Success),
			zap.Int("status_code", r.StatusCode),
			zap.Int64("latency_ms", r.Latency.Milliseconds()),
			zap.Time("timestamp", r.Timestamp),
			zap.Float64("availability_slo_percent", avail),
			zap.Float64("latency_slo_percent", lat),
			zap.Float64("error_budget_total_minutes", bud.TotalBudgetMinutes),
			zap.Float64("error_budget_consumed_minutes", bud.ConsumedMinutes),
			zap.Float64("error_budget_remaining_minutes", bud.RemainingMinutes),
			zap.Float64("error_budget_remaining_percent", bud.RemainingPercent),
			zap.Bool("error_budget_exhausted", bud.Exhausted),
			zap.Error(r.Err),
		)
	}

	// Probe loop has exited (signal received, scheduler shut down). Now
	// stop the HTTP server with a short deadline so the process can
	// terminate promptly even if a scraper is mid-request.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("metrics server shutdown", zap.Error(err))
	}

	logger.Info("canary-runner stopped")
}
