package probe

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Scheduler owns the goroutines that fire probes on a recurring interval.
// It does not store state between runs — each call to Run() starts a fresh
// set of goroutines and closes the result channel when they've all exited.
type Scheduler struct {
	prober *Prober
	logger *zap.Logger
}

// NewScheduler builds a Scheduler that will use the given Prober to execute
// each probe and the given logger for warnings (e.g. a dropped result
// because the consumer stopped reading).
func NewScheduler(prober *Prober, logger *zap.Logger) *Scheduler {
	return &Scheduler{prober: prober, logger: logger}
}

// Run launches one goroutine per target. Each goroutine:
//   - fires an initial probe immediately (so operators see data right away),
//   - then re-fires on time.Ticker every target.Interval,
//   - stops when ctx is cancelled.
//
// Run itself returns immediately. The caller iterates over the returned
// channel; the channel is closed once *all* per-target goroutines have
// exited, which is the idiomatic Go pattern for "collection is complete".
func (s *Scheduler) Run(ctx context.Context, targets []Target) <-chan Result {
	// Buffered with one slot per target so a slow consumer does not stall
	// the tickers for other targets. In Stage 3+ the consumer (SLO
	// calculator) will be fast; this buffer mostly covers the tiny window
	// between a probe completing and the consumer picking it up.
	results := make(chan Result, len(targets))

	// WaitGroup coordinates the "close the channel when everyone's done"
	// pattern below. We Add here (not inside the goroutine) to avoid a
	// classic race where Wait sees count==0 before a goroutine has started.
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		// Capture t by value — without this, the goroutine would share
		// the loop variable and every iteration would probe the last target.
		go s.probeLoop(ctx, t, results, &wg)
	}

	// A separate goroutine waits for all probers, then closes the channel.
	// Closing must happen exactly once and only after every sender is done,
	// otherwise a still-running goroutine could panic sending on a closed
	// channel. WaitGroup makes this trivial.
	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

// probeLoop runs one target forever (until ctx is cancelled). Split out
// from Run() so each goroutine has a small, obvious lifecycle.
func (s *Scheduler) probeLoop(ctx context.Context, t Target, out chan<- Result, wg *sync.WaitGroup) {
	defer wg.Done()

	// Fire immediately before starting the ticker so there's no "dead zone"
	// between startup and the first probe equal to Interval. Without this,
	// a target with a 60-second interval would show no data on the dashboard
	// for the first minute after the service started — bad UX.
	s.emit(ctx, t, out)

	ticker := time.NewTicker(t.Interval)
	// Stop() releases the ticker's goroutine; skipping this leaks it until
	// the process exits.
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Context cancelled — shutdown. Return cleanly so the WaitGroup
			// can decrement and the closer goroutine can close the channel.
			return
		case <-ticker.C:
			s.emit(ctx, t, out)
		}
	}
}

// emit runs a single probe and forwards its result on `out`, respecting
// context cancellation so shutdown doesn't block on a full channel.
func (s *Scheduler) emit(ctx context.Context, t Target, out chan<- Result) {
	res := s.prober.Do(ctx, t)
	select {
	case out <- res:
		// Delivered — done.
	case <-ctx.Done():
		// Shutdown in progress and the consumer is no longer draining.
		// Log at debug level so this isn't noisy on normal termination,
		// but is still greppable if a consumer stalls under load.
		s.logger.Debug("dropped probe result during shutdown",
			zap.String("endpoint", res.Name),
			zap.Bool("success", res.Success),
		)
	}
}
