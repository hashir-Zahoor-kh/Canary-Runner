package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestScheduler_ProbesAllTargets spins up three fake servers, runs the
// scheduler briefly, and confirms we receive results for every target.
// This is the core "multi-target" correctness test.
func TestScheduler_ProbesAllTargets(t *testing.T) {
	const numTargets = 3

	// Each server counts its own hits so we can cross-check per-target
	// traffic. atomic.Int64 because the handler runs on the httptest
	// goroutine while the test reads from the main goroutine.
	var hits [numTargets]atomic.Int64
	servers := make([]*httptest.Server, numTargets)
	for i := range servers {
		i := i // capture for the closure below
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(servers[i].Close)
	}

	// Short interval so the test runs fast. 25ms is comfortably above the
	// scheduler jitter you'd see on a loaded CI machine.
	targets := make([]Target, numTargets)
	for i := range targets {
		targets[i] = Target{
			Name:     "srv",
			URL:      servers[i].URL,
			Method:   http.MethodGet,
			Timeout:  time.Second,
			Interval: 25 * time.Millisecond,
		}
		targets[i].Name = "srv-" + string(rune('A'+i))
	}

	s := NewScheduler(NewProber(), zap.NewNop())

	// Bound the whole test with a short context — if the scheduler never
	// shuts down we'd hang forever without this.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	results := s.Run(ctx, targets)

	perTarget := make(map[string]int)
	for r := range results {
		perTarget[r.Name]++
	}

	for _, tgt := range targets {
		if perTarget[tgt.Name] == 0 {
			t.Errorf("expected >=1 result for %q, got 0", tgt.Name)
		}
	}
}

// TestScheduler_GracefulShutdown verifies that cancelling the context
// causes the scheduler's goroutines to exit and the results channel to
// close. A bug here would manifest as a leaked goroutine on shutdown —
// exactly the kind of thing the race detector and a hanging test catch.
func TestScheduler_GracefulShutdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := NewScheduler(NewProber(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())

	results := s.Run(ctx, []Target{{
		Name:     "one",
		URL:      srv.URL,
		Method:   http.MethodGet,
		Timeout:  time.Second,
		Interval: 20 * time.Millisecond,
	}})

	// Wait for at least one result to prove the goroutine is actually running.
	select {
	case <-results:
	case <-time.After(time.Second):
		t.Fatal("no result received before timeout; scheduler didn't start")
	}

	// Cancel and then drain. The channel must close within a reasonable
	// time; we put a hard deadline on it so a shutdown bug fails fast.
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-results:
			if !ok {
				return // channel closed — shutdown complete.
			}
			// still draining in-flight results; keep looping
		case <-deadline:
			t.Fatal("results channel did not close after cancel")
		}
	}
}

// TestScheduler_FailingTargetDoesNotStopOthers confirms that an endpoint
// returning 500 does not prevent the scheduler from reporting *about* it
// (nor does it affect sibling targets). Regressions here would mean one
// bad endpoint could go dark on the dashboard instead of being loudly red.
func TestScheduler_FailingTargetDoesNotStopOthers(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(okSrv.Close)
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(badSrv.Close)

	targets := []Target{
		{Name: "ok", URL: okSrv.URL, Method: http.MethodGet, Timeout: time.Second, Interval: 20 * time.Millisecond},
		{Name: "bad", URL: badSrv.URL, Method: http.MethodGet, Timeout: time.Second, Interval: 20 * time.Millisecond},
	}

	s := NewScheduler(NewProber(), zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()

	var okHits, badHits int
	for r := range s.Run(ctx, targets) {
		switch r.Name {
		case "ok":
			if !r.Success {
				t.Errorf("ok target produced a failure: %+v", r)
			}
			okHits++
		case "bad":
			if r.Success {
				t.Errorf("bad target produced a success: %+v", r)
			}
			if r.StatusCode != http.StatusInternalServerError {
				t.Errorf("bad target: want 500, got %d", r.StatusCode)
			}
			badHits++
		}
	}

	if okHits == 0 {
		t.Error("expected results from ok target, got none")
	}
	if badHits == 0 {
		t.Error("expected results from bad target, got none")
	}
}
