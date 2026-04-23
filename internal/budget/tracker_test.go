package budget

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fakeClock is the same pattern we used in the slo package — a controllable
// clock scoped to the test file. Deliberately duplicated (not imported from
// the slo tests) so each package's tests stay self-contained.
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

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

// stubProvider returns a caller-controlled availability per endpoint.
// This is the whole reason AvailabilityProvider is an interface — tests
// can drive the tracker through any compliance scenario without running
// a real SLO calculator or probes.
type stubProvider struct {
	mu   sync.Mutex
	vals map[string]float64
}

func newStubProvider() *stubProvider { return &stubProvider{vals: map[string]float64{}} }

func (s *stubProvider) set(endpoint string, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[endpoint] = v
}

func (s *stubProvider) Availability(endpoint string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vals[endpoint]
	if !ok {
		return 100 // same "unknown means healthy" policy as slo.Calculator
	}
	return v
}

// approxEqual compares floats with a small tolerance because the budget
// math multiplies percentages and durations and we'd otherwise drown in
// IEEE-754 noise.
func approxEqual(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}

// TestTracker_TotalBudgetFormula is the headline example from the spec:
// 99.9% availability over 30 days should produce a 43.2-minute budget.
// If this test ever fails, something has broken the core formula and
// every downstream metric is wrong.
func TestTracker_TotalBudgetFormula(t *testing.T) {
	provider := newStubProvider()
	provider.set("svc", 100)

	tr := NewTracker(newFakeClock(time.Now()), provider, []EndpointConfig{{
		Name:                      "svc",
		AvailabilityTargetPercent: 99.9,
		WindowDuration:            30 * 24 * time.Hour,
	}})

	st := tr.Status("svc")
	if !approxEqual(st.TotalBudgetMinutes, 43.2, 1e-9) {
		t.Errorf("TotalBudgetMinutes: want 43.2, got %v", st.TotalBudgetMinutes)
	}
	if !approxEqual(st.ConsumedMinutes, 0, 1e-9) {
		t.Errorf("ConsumedMinutes (100%% avail): want 0, got %v", st.ConsumedMinutes)
	}
	if !approxEqual(st.RemainingMinutes, 43.2, 1e-9) {
		t.Errorf("RemainingMinutes: want 43.2, got %v", st.RemainingMinutes)
	}
	if !approxEqual(st.RemainingPercent, 100, 1e-9) {
		t.Errorf("RemainingPercent: want 100, got %v", st.RemainingPercent)
	}
	if st.Exhausted {
		t.Errorf("Exhausted: want false, got true")
	}
}

// TestTracker_PartialConsumption verifies the consumed/remaining math
// at a non-trivial compliance level. 99.95% availability is exactly
// halfway through a 99.9% budget — that's a clean sanity check.
func TestTracker_PartialConsumption(t *testing.T) {
	provider := newStubProvider()
	provider.set("svc", 99.95) // half the allowed error rate of 0.1%

	tr := NewTracker(newFakeClock(time.Now()), provider, []EndpointConfig{{
		Name:                      "svc",
		AvailabilityTargetPercent: 99.9,
		WindowDuration:            30 * 24 * time.Hour,
	}})

	st := tr.Status("svc")
	// Consumed should be (100 - 99.95) / 100 * 43200 = 21.6 minutes
	if !approxEqual(st.ConsumedMinutes, 21.6, 1e-9) {
		t.Errorf("ConsumedMinutes: want 21.6, got %v", st.ConsumedMinutes)
	}
	// Remaining = 43.2 - 21.6 = 21.6 → exactly 50% left
	if !approxEqual(st.RemainingMinutes, 21.6, 1e-9) {
		t.Errorf("RemainingMinutes: want 21.6, got %v", st.RemainingMinutes)
	}
	if !approxEqual(st.RemainingPercent, 50, 1e-6) {
		t.Errorf("RemainingPercent: want 50, got %v", st.RemainingPercent)
	}
	if st.Exhausted {
		t.Errorf("Exhausted at 50%%: want false, got true")
	}
}

// TestTracker_ExhaustionAndTimestamp: when the budget crosses zero,
// Update should record the current clock time, and subsequent Update
// calls at later times should NOT overwrite it (outage started at T0,
// not at T0+5min).
func TestTracker_ExhaustionAndTimestamp(t *testing.T) {
	provider := newStubProvider()
	clock := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	tr := NewTracker(clock, provider, []EndpointConfig{{
		Name:                      "svc",
		AvailabilityTargetPercent: 99.9,
		WindowDuration:            30 * 24 * time.Hour,
	}})

	// Start healthy — status should be not exhausted and zero timestamp.
	provider.set("svc", 100)
	tr.Update("svc")
	if st := tr.Status("svc"); st.Exhausted || !st.ExhaustedSince.IsZero() {
		t.Fatalf("healthy start: want !Exhausted and zero timestamp, got %+v", st)
	}

	// Force exhaustion: 99% availability ≫ 99.9% target.
	provider.set("svc", 99.0)
	firstObserved := clock.Now()
	tr.Update("svc")

	st := tr.Status("svc")
	if !st.Exhausted {
		t.Fatalf("after drop to 99%%: want Exhausted=true, got %+v", st)
	}
	if !st.ExhaustedSince.Equal(firstObserved) {
		t.Errorf("ExhaustedSince: want %v, got %v", firstObserved, st.ExhaustedSince)
	}
	// Remaining should be clearly negative (blown through the budget).
	if st.RemainingMinutes >= 0 {
		t.Errorf("RemainingMinutes at 99%% availability: want negative, got %v", st.RemainingMinutes)
	}

	// Advance time and Update again while still exhausted — the stored
	// timestamp must NOT move. That's the "since when" invariant.
	clock.Advance(5 * time.Minute)
	tr.Update("svc")
	st2 := tr.Status("svc")
	if !st2.ExhaustedSince.Equal(firstObserved) {
		t.Errorf("ExhaustedSince moved while still exhausted: want %v, got %v",
			firstObserved, st2.ExhaustedSince)
	}
}

// TestTracker_RecoveryClearsTimestamp: once compliance recovers, the
// ExhaustedSince timestamp must be cleared so the next exhaustion is
// reported with a fresh "since when".
func TestTracker_RecoveryClearsTimestamp(t *testing.T) {
	provider := newStubProvider()
	clock := newFakeClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	tr := NewTracker(clock, provider, []EndpointConfig{{
		Name:                      "svc",
		AvailabilityTargetPercent: 99.9,
		WindowDuration:            30 * 24 * time.Hour,
	}})

	// Exhaust first
	provider.set("svc", 99.0)
	tr.Update("svc")
	if st := tr.Status("svc"); !st.Exhausted {
		t.Fatalf("expected exhausted after 99%%, got %+v", st)
	}

	// Recover
	clock.Advance(10 * time.Minute)
	provider.set("svc", 100)
	tr.Update("svc")
	st := tr.Status("svc")
	if st.Exhausted {
		t.Fatalf("after recovery: want !Exhausted, got %+v", st)
	}
	if !st.ExhaustedSince.IsZero() {
		t.Errorf("after recovery: want zero ExhaustedSince, got %v", st.ExhaustedSince)
	}

	// Re-exhaust to confirm the new timestamp is the current clock
	// (not the old stale one from before recovery).
	clock.Advance(5 * time.Minute)
	expected := clock.Now()
	provider.set("svc", 99.0)
	tr.Update("svc")
	st = tr.Status("svc")
	if !st.ExhaustedSince.Equal(expected) {
		t.Errorf("second exhaustion ExhaustedSince: want %v, got %v",
			expected, st.ExhaustedSince)
	}
}

// TestTracker_UnknownEndpoint: queries for endpoints not in the config
// should return a zero Status, not crash.
func TestTracker_UnknownEndpoint(t *testing.T) {
	tr := NewTracker(newFakeClock(time.Now()), newStubProvider(), nil)
	st := tr.Status("missing")
	if st.Endpoint != "missing" {
		t.Errorf("Endpoint: want 'missing', got %q", st.Endpoint)
	}
	if st.Exhausted || !st.ExhaustedSince.IsZero() {
		t.Errorf("unknown endpoint: want empty Status, got %+v", st)
	}

	// Update on an unknown endpoint must be a silent no-op, not a panic.
	tr.Update("missing")
}

// TestTracker_ConcurrentAccess exercises the mutex under a mix of
// Update and Status callers. The assertion is really "-race passed".
func TestTracker_ConcurrentAccess(t *testing.T) {
	provider := newStubProvider()
	provider.set("svc", 99.95)

	tr := NewTracker(newFakeClock(time.Now()), provider, []EndpointConfig{{
		Name:                      "svc",
		AvailabilityTargetPercent: 99.9,
		WindowDuration:            30 * 24 * time.Hour,
	}})

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Two writers flipping availability through exhausted/recovered…
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(which int) {
			defer wg.Done()
			v := 99.95
			for {
				select {
				case <-done:
					return
				default:
					if which%2 == 0 {
						v = 99.95
					} else {
						v = 99.0
					}
					provider.set("svc", v)
					tr.Update("svc")
				}
			}
		}(i)
	}

	// …and four readers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = tr.Status("svc")
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}
