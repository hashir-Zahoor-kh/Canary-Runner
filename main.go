// Canary Runner entry point.
//
// Stage 1 scope: execute a single hard-coded probe against a demo URL and
// print the result as structured JSON via zap. Later stages will replace the
// hard-coded target with config.yaml loading, schedule multiple probes, and
// expose Prometheus metrics.
package main

import (
	"context"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/hashir/canary-runner/internal/probe"
)

func main() {
	// zap.NewProduction emits JSON-formatted logs with ISO8601 timestamps and
	// a "level" field — exactly what we want for machine-consumable output,
	// and what containerised deployments (Stage 7) expect on stdout.
	logger, err := zap.NewProduction()
	if err != nil {
		// Logger construction only fails for pathological reasons (e.g. bad
		// config). We write to stderr directly because we don't have a logger.
		_, _ = os.Stderr.WriteString("failed to construct logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	// Sync flushes any buffered log entries. The error is usually a benign
	// "invalid argument" on stdout — we deliberately ignore it here, which is
	// the idiomatic pattern for zap.
	defer func() { _ = logger.Sync() }()

	// Root context. In Stage 2 we'll wire this to SIGTERM/SIGINT so probes
	// stop cleanly on shutdown; for now it just bounds the demo probe.
	ctx := context.Background()

	p := probe.NewProber()
	target := probe.Target{
		Name:    "demo",
		URL:     "https://example.com",
		Method:  "GET",
		Timeout: 5 * time.Second,
	}

	res := p.Do(ctx, target)

	// zap's structured-field API: each field becomes a separate JSON key,
	// which is much easier to grep/parse than a templated log line.
	logger.Info("probe complete",
		zap.String("endpoint", res.Name),
		zap.String("url", res.URL),
		zap.Bool("success", res.Success),
		zap.Int("status_code", res.StatusCode),
		zap.Duration("latency", res.Latency),
		zap.Int64("latency_ms", res.Latency.Milliseconds()),
		zap.Time("timestamp", res.Timestamp),
		zap.Error(res.Err), // zap.Error renders nil as no field at all
	)
}
