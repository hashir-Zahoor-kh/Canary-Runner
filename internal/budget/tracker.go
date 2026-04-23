// Package budget turns an endpoint's availability SLO into a concrete error
// budget expressed in minutes — the SRE way of talking about how much
// unplanned unavailability a service is "allowed" over a time window
// before it is formally out of compliance.
//
// Given a 99.9% availability target over a 30-day window, the total budget
// is (100 - 99.9) / 100 * 30 * 24 * 60 = 43.2 minutes. Every minute of
// unavailability observed inside the window eats into that budget.
package budget

import (
	"sync"
	"time"
)

// AvailabilityProvider is the narrow view of an availability source that
// the Tracker actually uses. slo.Calculator satisfies it, but tests can
// supply a stub without needing the real calculator.
type AvailabilityProvider interface {
	Availability(endpoint string) float64
}

// Clock mirrors slo.Clock — we redeclare it locally so the budget package
// doesn't depend on the slo package just to pick up a two-line interface.
// This also keeps test doubles trivial to write per-package.
type Clock interface {
	Now() time.Time
}

// RealClock is the production clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// EndpointConfig describes the availability SLO for one endpoint.
type EndpointConfig struct {
	Name                      string
	AvailabilityTargetPercent float64       // e.g. 99.9
	WindowDuration            time.Duration // e.g. 30 * 24h
}

// Status is a snapshot of the error-budget situation for one endpoint.
// All minute counts are float64 because the computed values are rarely
// whole minutes and we'd rather not hide precision from the metrics layer.
type Status struct {
	Endpoint           string
	TotalBudgetMinutes float64 // minutes of downtime the SLO permits over the window
	ConsumedMinutes    float64 // observed downtime, based on current availability
	RemainingMinutes   float64 // Total - Consumed; negative if budget is blown
	RemainingPercent   float64 // Remaining / Total * 100 (capped-at-zero is the UI layer's job)
	Exhausted          bool    // true when ConsumedMinutes >= TotalBudgetMinutes
	ExhaustedSince     time.Time
	// ExhaustedSince holds the clock time at which the most recent
	// exhaustion was first observed. It is the zero value when Exhausted
	// is false. A recovery (budget no longer exhausted) clears it, so
	// "duration of current outage" = clock.Now() - ExhaustedSince.
}

// Tracker computes error-budget status per endpoint and remembers the
// moment each endpoint first tipped into the "exhausted" state.
//
// The tracker itself is stateless about availability — it delegates to an
// AvailabilityProvider (normally slo.Calculator) on every call. The only
// state it *does* keep is the map of exhaustion timestamps, which must be
// remembered across calls because exhaustion is a transition, not a value.
type Tracker struct {
	mu             sync.Mutex
	clock          Clock
	provider       AvailabilityProvider
	cfgs           map[string]EndpointConfig
	exhaustedSince map[string]time.Time
}

// NewTracker constructs a Tracker bound to a specific availability
// provider and endpoint list. Endpoints not in this list will get a
// zero-valued Status from Status() — the same defensive stance the SLO
// calculator takes for unknown endpoints.
func NewTracker(clock Clock, provider AvailabilityProvider, endpoints []EndpointConfig) *Tracker {
	cfgs := make(map[string]EndpointConfig, len(endpoints))
	for _, e := range endpoints {
		cfgs[e.Name] = e
	}
	return &Tracker{
		clock:          clock,
		provider:       provider,
		cfgs:           cfgs,
		exhaustedSince: make(map[string]time.Time, len(endpoints)),
	}
}

// Update recomputes the budget for endpoint and mutates the exhaustion
// timestamp if the endpoint has just become exhausted or recovered.
// Call this after each probe so the ExhaustedSince field in Status()
// is always up to date.
func (t *Tracker) Update(endpoint string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg, ok := t.cfgs[endpoint]
	if !ok {
		return
	}

	total, consumed := budgetMinutes(cfg, t.provider.Availability(endpoint))
	if consumed >= total {
		// Currently exhausted. Record the first observation if we haven't
		// already. Repeatedly-exhausted observations keep the original
		// timestamp — that's the "when did the *current* outage start"
		// interpretation.
		if t.exhaustedSince[endpoint].IsZero() {
			t.exhaustedSince[endpoint] = t.clock.Now()
		}
	} else {
		// Recovered. Wipe the timestamp so the next exhaustion is reported
		// fresh; otherwise a recovered service would keep reporting an
		// ancient "exhausted since" from months ago.
		delete(t.exhaustedSince, endpoint)
	}
}

// Status returns the current error-budget state for endpoint. It is safe
// to call concurrently with Update.
func (t *Tracker) Status(endpoint string) Status {
	t.mu.Lock()
	defer t.mu.Unlock()

	cfg, ok := t.cfgs[endpoint]
	if !ok {
		// Unknown endpoint: return an empty Status rather than zero-division
		// the caller to death. Named field here so future additions to
		// Status don't silently default to something misleading.
		return Status{Endpoint: endpoint}
	}

	total, consumed := budgetMinutes(cfg, t.provider.Availability(endpoint))
	remaining := total - consumed

	// total is zero only when the SLO target is literally 100% — a
	// degenerate but possible config. Guard against divide-by-zero so
	// metrics don't become NaN.
	var remainingPct float64
	if total > 0 {
		remainingPct = remaining / total * 100
	} else {
		remainingPct = 100
	}

	return Status{
		Endpoint:           endpoint,
		TotalBudgetMinutes: total,
		ConsumedMinutes:    consumed,
		RemainingMinutes:   remaining,
		RemainingPercent:   remainingPct,
		Exhausted:          consumed >= total,
		ExhaustedSince:     t.exhaustedSince[endpoint],
	}
}

// budgetMinutes is the pure-math core. Extracted so both Update and
// Status call the same formulas (no drift between them) and so the
// tests can reason about the formula independently of the locking.
//
// The formulas:
//
//	total    = (100 - target%) / 100 * windowMinutes
//	consumed = (100 - availability%) / 100 * windowMinutes
//
// Consumed can legitimately exceed total when the service is performing
// worse than its SLO allows. We do NOT clamp — the metrics layer and the
// alerter need the real number to drive "budget exhausted" thresholds.
func budgetMinutes(cfg EndpointConfig, availability float64) (total, consumed float64) {
	windowMinutes := cfg.WindowDuration.Minutes()
	total = (100.0 - cfg.AvailabilityTargetPercent) / 100.0 * windowMinutes
	consumed = (100.0 - availability) / 100.0 * windowMinutes
	return total, consumed
}
