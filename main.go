// Canary Runner entry point.
//
// Stage 4 scope: Stage 3's SLO calculator now feeds an error-budget tracker.
// Every probe log carries the rolling compliance AND the consumed / remaining
// budget in minutes. Stage 5 will expose these same numbers as Prometheus
// metrics on :9090/metrics.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/hashir/canary-runner/internal/budget"
	"github.com/hashir/canary-runner/internal/config"
	"github.com/hashir/canary-runner/internal/probe"
	"github.com/hashir/canary-runner/internal/slo"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
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

	// Fan out one config.Target into three shapes: what the prober needs,
	// what the SLO calculator needs, and what the budget tracker needs.
	// All three downstreams share the window duration but otherwise look
	// at different fields — keeping the wiring explicit is easier to read
	// (and easier to grep for) than hiding it behind a helper.
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

	// One Calculator and one Tracker serve every endpoint. The Tracker
	// takes the Calculator as its availability provider — slo.Calculator
	// satisfies budget.AvailabilityProvider by virtue of exposing
	// Availability(endpoint) float64.
	calc := slo.NewCalculator(slo.RealClock{}, sloEndpoints)
	tracker := budget.NewTracker(budget.RealClock{}, calc, budgetEndpoints)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("canary-runner starting",
		zap.Int("targets", len(probeTargets)),
		zap.String("config_path", *configPath),
	)

	s := probe.NewScheduler(probe.NewProber(), logger)
	results := s.Run(ctx, probeTargets)

	// Main loop: Record the probe into the SLO calculator, kick the budget
	// tracker so its exhaustion timestamps stay current, then log the full
	// picture. Order matters — the Record must precede Update so Update's
	// availability read already reflects this probe.
	for r := range results {
		calc.Record(r)
		tracker.Update(r.Name)

		avail, lat := calc.Compliance(r.Name)
		bud := tracker.Status(r.Name)

		logger.Info("probe complete",
			zap.String("endpoint", r.Name),
			zap.String("url", r.URL),
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

	logger.Info("canary-runner stopped")
}
