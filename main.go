// Canary Runner entry point.
//
// Stage 2 scope: load a YAML config, launch one probing goroutine per target,
// log every Result as structured JSON, and shut down cleanly on SIGTERM/SIGINT.
// Stages 3+ will insert the SLO calculator and metrics exporter between the
// scheduler and the logger.
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
)

func main() {
	// -config is a CLI flag so the Dockerfile in Stage 7 can mount an
	// arbitrary config path. Default matches the project-root file so
	// `go run .` works without arguments.
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	flag.Parse()

	// Structured JSON logger. Constructed before config loading so we can
	// log the "couldn't load config" error with the same machinery as
	// everything else.
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

	// Convert config.Target (user-facing, with seconds-as-ints) into
	// probe.Target (runtime, with time.Duration). Doing the conversion here
	// keeps the two packages decoupled — neither imports the other.
	targets := make([]probe.Target, len(cfg.Targets))
	for i, t := range cfg.Targets {
		targets[i] = probe.Target{
			Name:     t.Name,
			URL:      t.URL,
			Method:   t.Method,
			Timeout:  time.Duration(t.TimeoutSeconds) * time.Second,
			Interval: time.Duration(t.IntervalSeconds) * time.Second,
		}
	}

	// signal.NotifyContext gives us a context that is automatically
	// cancelled when the process receives SIGINT (Ctrl-C) or SIGTERM (the
	// standard "please stop" signal from Kubernetes or docker stop). Every
	// probe goroutine derives from this context, so cancellation fans out
	// cleanly to the whole service.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("canary-runner starting",
		zap.Int("targets", len(targets)),
		zap.String("config_path", *configPath),
	)

	s := probe.NewScheduler(probe.NewProber(), logger)
	results := s.Run(ctx, targets)

	// Main loop: log every result. Range-over-channel exits when the
	// scheduler has closed it (which happens after every probe goroutine
	// has returned), giving us a natural "shutdown is done" signal.
	for r := range results {
		logger.Info("probe complete",
			zap.String("endpoint", r.Name),
			zap.String("url", r.URL),
			zap.Bool("success", r.Success),
			zap.Int("status_code", r.StatusCode),
			zap.Int64("latency_ms", r.Latency.Milliseconds()),
			zap.Time("timestamp", r.Timestamp),
			zap.Error(r.Err),
		)
	}

	logger.Info("canary-runner stopped")
}
