// Canary Runner entry point.
//
// Stage 3 scope: on top of Stage 2's concurrent probing, every result is now
// fed into an slo.Calculator and the rolling availability + latency
// compliance percentages are logged alongside each probe so you can watch
// them evolve at runtime. Stages 4+ will consume the same Calculator to
// drive the error-budget tracker and Prometheus exporter.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

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

	// Convert config.Target into the two downstream shapes at once — saves
	// a second loop later and keeps the mapping from YAML to runtime types
	// visible in one place.
	probeTargets := make([]probe.Target, len(cfg.Targets))
	sloEndpoints := make([]slo.EndpointConfig, len(cfg.Targets))
	for i, t := range cfg.Targets {
		probeTargets[i] = probe.Target{
			Name:     t.Name,
			URL:      t.URL,
			Method:   t.Method,
			Timeout:  time.Duration(t.TimeoutSeconds) * time.Second,
			Interval: time.Duration(t.IntervalSeconds) * time.Second,
		}
		sloEndpoints[i] = slo.EndpointConfig{
			Name:           t.Name,
			WindowDuration: time.Duration(t.SLO.WindowDays) * 24 * time.Hour,
			LatencyTarget:  time.Duration(t.SLO.LatencyTargetMS) * time.Millisecond,
		}
	}

	// The calculator uses a real wall-clock in production. Tests inject
	// their own FakeClock — see internal/slo/calculator_test.go.
	calc := slo.NewCalculator(slo.RealClock{}, sloEndpoints)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("canary-runner starting",
		zap.Int("targets", len(probeTargets)),
		zap.String("config_path", *configPath),
	)

	s := probe.NewScheduler(probe.NewProber(), logger)
	results := s.Run(ctx, probeTargets)

	// Main loop: record every result into the SLO calculator, then log
	// both the probe outcome and the resulting rolling compliance. Doing
	// the Record *before* the log means the logged percentages reflect
	// the very probe we're logging about.
	for r := range results {
		calc.Record(r)
		avail, lat := calc.Compliance(r.Name)
		logger.Info("probe complete",
			zap.String("endpoint", r.Name),
			zap.String("url", r.URL),
			zap.Bool("success", r.Success),
			zap.Int("status_code", r.StatusCode),
			zap.Int64("latency_ms", r.Latency.Milliseconds()),
			zap.Time("timestamp", r.Timestamp),
			zap.Float64("availability_slo_percent", avail),
			zap.Float64("latency_slo_percent", lat),
			zap.Error(r.Err),
		)
	}

	logger.Info("canary-runner stopped")
}
