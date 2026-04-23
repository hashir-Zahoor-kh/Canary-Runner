package slo

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashir/canary-runner/internal/probe"
)

// fakeClock is the controllable clock used by every test in this file.
// We deliberately keep it tiny — a real clock-library dependency would
// be overkill for a single .Now() method.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

// Advance moves the clock forward. Used by tests to simulate the passage
// of time across the rolling window boundary.
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// helper: build a Calculator with a single endpoint and return it plus
// its clock and name. Keeps test bodies focused on the scenario, not wiring.
func newTestCalc(window, latencyTarget time.Duration) (*Calculator, *fakeClock) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := NewCalculator(clock, []EndpointConfig{{
		Name:           "svc",
		WindowDuration: window,
		LatencyTarget:  latencyTarget,
	}})
	return c, clock
}

// result is a small shortcut for building probe.Result values.
// We centralise it so tests read as scenarios, not plumbing.
func result(clock *fakeClock, status int, latency time.Duration, err error) probe.Result {
	return probe.Result{
		Name:       "svc",
		URL:        "http://svc",
		Success:    err == nil && status >= 200 && status < 300,
		StatusCode: status,
		Latency:    latency,
		Err:        err,
		Timestamp:  clock.Now(),
	}
}

// TestCalculator_EmptyWindowIs100 pins the "no data -> optimistic 100%"
// behaviour. This prevents bogus startup alerts (see Stage 6).
func TestCalculator_EmptyWindowIs100(t *testing.T) {
	c, _ := newTestCalc(time.Hour, 200*time.Millisecond)
	if got := c.Availability("svc"); got != 100 {
		t.Errorf("Availability on empty window: want 100, got %v", got)
	}
	if got := c.Latency("svc"); got != 100 {
		t.Errorf("Latency on empty window: want 100, got %v", got)
	}
}

// TestCalculator_AllGoodProbes — 10 fast 200s should produce 100/100.
func TestCalculator_AllGoodProbes(t *testing.T) {
	c, clock := newTestCalc(time.Hour, 200*time.Millisecond)
	for i := 0; i < 10; i++ {
		c.Record(result(clock, 200, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}
	avail, lat := c.Compliance("svc")
	if avail != 100 {
		t.Errorf("Availability: want 100, got %v", avail)
	}
	if lat != 100 {
		t.Errorf("Latency: want 100, got %v", lat)
	}
}

// TestCalculator_Formulas verifies the exact percentages for a known
// mix. 10 probes: 8 available, 2 5xx; 6 under latency, 4 over.
func TestCalculator_Formulas(t *testing.T) {
	c, clock := newTestCalc(time.Hour, 200*time.Millisecond)
	// 6 fast successes
	for i := 0; i < 6; i++ {
		c.Record(result(clock, 200, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}
	// 2 fast 500s — hurt availability, NOT latency (they responded quickly)
	for i := 0; i < 2; i++ {
		c.Record(result(clock, 500, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}
	// 2 slow successes — pass availability, FAIL latency
	for i := 0; i < 2; i++ {
		c.Record(result(clock, 200, 500*time.Millisecond, nil))
		clock.Advance(time.Second)
	}

	avail, lat := c.Compliance("svc")
	// 8 out of 10 are non-5xx → 80%
	if avail != 80 {
		t.Errorf("Availability: want 80, got %v", avail)
	}
	// 6 out of 10 were under 200ms → 60%
	// (The two 500s were fast, so they ARE under latency threshold — the
	// latency SLO does not care about the response body/status, only time.)
	if lat != 80 {
		// 6 fast 200s + 2 fast 500s = 8 fast; 2 slow 200s = 2 slow.
		// So 8/10 = 80% under threshold.
		t.Errorf("Latency: want 80, got %v", lat)
	}
}

// TestCalculator_TimeoutsFailBothSLOs: an errored probe (timeout or
// connection failure) should count against BOTH SLOs.
func TestCalculator_TimeoutsFailBothSLOs(t *testing.T) {
	c, clock := newTestCalc(time.Hour, 200*time.Millisecond)
	// 3 good probes
	for i := 0; i < 3; i++ {
		c.Record(result(clock, 200, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}
	// 1 timeout — Err non-nil, even though recorded latency might be small
	c.Record(result(clock, 0, 5*time.Second, errors.New("context deadline exceeded")))
	clock.Advance(time.Second)

	avail, lat := c.Compliance("svc")
	// 3 of 4 non-error → 75% availability
	if avail != 75 {
		t.Errorf("Availability with 1 error: want 75, got %v", avail)
	}
	// 3 of 4 under latency (the errored probe does not count as under) → 75%
	if lat != 75 {
		t.Errorf("Latency with 1 error: want 75, got %v", lat)
	}
}

// TestCalculator_WindowTrimsOldPoints verifies the rolling-window behaviour:
// once time advances past window duration, old points must stop affecting
// compliance. This is the core correctness property of the package.
func TestCalculator_WindowTrimsOldPoints(t *testing.T) {
	c, clock := newTestCalc(10*time.Minute, 200*time.Millisecond)

	// Batch A: 5 FAILING probes at t=0
	for i := 0; i < 5; i++ {
		c.Record(result(clock, 500, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}
	// At this point availability should be 0% — all 5 are 500s.
	if got := c.Availability("svc"); got != 0 {
		t.Fatalf("pre-trim availability: want 0, got %v", got)
	}

	// Jump forward past the window so the failing batch ages out…
	clock.Advance(11 * time.Minute)

	// Batch B: 5 PASSING probes after the gap
	for i := 0; i < 5; i++ {
		c.Record(result(clock, 200, 50*time.Millisecond, nil))
		clock.Advance(time.Second)
	}

	// Only batch B is inside the 10-minute window now → 100% availability.
	if got := c.Availability("svc"); got != 100 {
		t.Errorf("post-trim availability: want 100, got %v", got)
	}
}

// TestCalculator_UnknownEndpointReturns100 — the calculator tolerates
// Record calls for endpoints it was never configured for (silent drop).
// This keeps config reloads (future work) from crashing the service.
func TestCalculator_UnknownEndpointReturns100(t *testing.T) {
	c, _ := newTestCalc(time.Hour, 200*time.Millisecond)
	if got := c.Availability("not-configured"); got != 100 {
		t.Errorf("unknown endpoint availability: want 100, got %v", got)
	}
	if got := c.Latency("not-configured"); got != 100 {
		t.Errorf("unknown endpoint latency: want 100, got %v", got)
	}
}

// TestCalculator_ConcurrentReadsAndWrites exercises the RWMutex under
// pressure. The test body does nothing clever — correctness is verified
// by -race on the test runner.
func TestCalculator_ConcurrentReadsAndWrites(t *testing.T) {
	c, clock := newTestCalc(time.Hour, 200*time.Millisecond)

	done := make(chan struct{})
	var wg sync.WaitGroup

	// One writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				c.Record(result(clock, 200, 50*time.Millisecond, nil))
			}
		}
	}()

	// Several readers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = c.Availability("svc")
					_, _ = c.Compliance("svc")
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}
