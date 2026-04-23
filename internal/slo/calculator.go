// Package slo computes SLO compliance percentages over a rolling time window.
//
// Two SLOs are tracked per endpoint:
//
//  1. Availability — percent of probes that returned a non-5xx response
//     before the per-probe timeout elapsed.
//  2. Latency     — percent of probes that completed under latency_target_ms.
//
// Both are expressed as float64 percentages (0–100). On an endpoint with no
// recorded data points the calculator reports 100% — this avoids scaring
// operators with "0% compliance!" alerts in the first seconds after startup.
package slo

import (
	"sync"
	"time"

	"github.com/hashir/canary-runner/internal/probe"
)

// Clock abstracts time.Now so tests can advance time deterministically.
// Production code uses RealClock; tests use a *FakeClock they control.
type Clock interface {
	Now() time.Time
}

// RealClock is the production clock. Zero value is ready to use.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// EndpointConfig tells the calculator how to judge one endpoint's SLOs.
// It is a slimmed-down view of config.Target — only the fields the slo
// package actually needs — which keeps this package independent of YAML.
type EndpointConfig struct {
	Name           string        // Matches probe.Result.Name for dispatch
	WindowDuration time.Duration // e.g. 30 * 24h for a 30-day SLO window
	LatencyTarget  time.Duration // A probe exceeding this counts as a latency-SLO miss
}

// dataPoint is the pre-classified outcome of a single probe. Pre-classifying
// at Record time means queries are O(n) integer counting instead of re-doing
// the "was this a 5xx?" logic every time Prometheus scrapes /metrics.
type dataPoint struct {
	at           time.Time
	available    bool // err==nil && status<500
	underLatency bool // err==nil && latency<LatencyTarget
}

// endpointState holds one endpoint's config plus its rolling window of
// data points. Points are appended in timestamp order (the scheduler
// emits them roughly in order) and trimmed on Record.
type endpointState struct {
	cfg    EndpointConfig
	points []dataPoint
}

// Calculator is the rolling-window SLO engine. One instance serves all
// endpoints; it is safe for concurrent use from the result-consumer
// goroutine (Record) and any number of metric-reader goroutines (the
// Availability / Latency accessors).
type Calculator struct {
	mu     sync.RWMutex
	clock  Clock
	states map[string]*endpointState
}

// NewCalculator builds a Calculator that tracks the given endpoints.
// Passing endpoints up-front (rather than auto-registering on first
// Record) guarantees that queries for a configured endpoint always find
// a state object — even before its first probe has completed.
func NewCalculator(clock Clock, endpoints []EndpointConfig) *Calculator {
	states := make(map[string]*endpointState, len(endpoints))
	for _, e := range endpoints {
		// Each endpoint gets its own state; we copy cfg by value so later
		// mutations of the caller's slice can't affect us.
		states[e.Name] = &endpointState{cfg: e}
	}
	return &Calculator{clock: clock, states: states}
}

// Record incorporates a probe result into its endpoint's rolling window.
// Results for unknown endpoints are silently dropped; that's the safe
// default if a probe lingers past a (future) config reload.
func (c *Calculator) Record(r probe.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.states[r.Name]
	if !ok {
		return
	}

	// Classify once, count many times. "Available" is the Canary Runner
	// availability definition (non-5xx, no error) — broader than
	// probe.Result.Success so a 301 or a 404 doesn't hurt the availability
	// SLO (the service is up, it just doesn't have what the probe asked for).
	point := dataPoint{
		at:           r.Timestamp,
		available:    r.Err == nil && r.StatusCode < 500,
		underLatency: r.Err == nil && r.Latency < st.cfg.LatencyTarget,
	}
	st.points = append(st.points, point)

	// Trim eagerly on write rather than letting the slice grow unbounded
	// between scrapes. trim is cheap (O(n) in an already-tiny n).
	c.trim(st)
}

// trim drops points older than the rolling window. Assumes c.mu is held
// by the caller in write mode, which Record guarantees.
func (c *Calculator) trim(st *endpointState) {
	cutoff := c.clock.Now().Add(-st.cfg.WindowDuration)
	// Points are appended in chronological order, so every stale point is
	// at the front of the slice. We find the first fresh index and reslice.
	firstFresh := 0
	for firstFresh < len(st.points) && st.points[firstFresh].at.Before(cutoff) {
		firstFresh++
	}
	if firstFresh > 0 {
		// Reslice instead of copying. The backing array will be reclaimed
		// when the slice eventually grows enough to require a new one.
		st.points = st.points[firstFresh:]
	}
}

// compute is the shared accumulator behind Availability and Latency.
// pred picks which dimension (available vs underLatency) to count.
// Taking a predicate keeps the two public methods one line each and
// prevents drift between their window-filtering logic.
func (c *Calculator) compute(endpoint string, pred func(dataPoint) bool) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st, ok := c.states[endpoint]
	if !ok {
		// Unknown endpoint: be optimistic. Alerting code will still notice
		// if the endpoint stops producing data points at all.
		return 100
	}

	// Recompute cutoff on every call (instead of relying on trim) so
	// readers see a *correct* window even if no Record has happened
	// between the last trim and this query.
	cutoff := c.clock.Now().Add(-st.cfg.WindowDuration)
	total, hit := 0, 0
	for _, p := range st.points {
		if p.at.Before(cutoff) {
			continue
		}
		total++
		if pred(p) {
			hit++
		}
	}
	if total == 0 {
		return 100
	}
	return float64(hit) / float64(total) * 100
}

// Availability returns the availability SLO compliance percentage for
// endpoint: 100.0 * (non-5xx probes) / (total probes) over the window.
func (c *Calculator) Availability(endpoint string) float64 {
	return c.compute(endpoint, func(p dataPoint) bool { return p.available })
}

// Latency returns the latency SLO compliance percentage for endpoint:
// 100.0 * (probes under LatencyTarget) / (total probes) over the window.
func (c *Calculator) Latency(endpoint string) float64 {
	return c.compute(endpoint, func(p dataPoint) bool { return p.underLatency })
}

// Compliance returns both SLOs in a single call. Useful for the metrics
// exporter in Stage 5 so it doesn't acquire the lock twice per scrape.
func (c *Calculator) Compliance(endpoint string) (availability, latency float64) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st, ok := c.states[endpoint]
	if !ok {
		return 100, 100
	}

	cutoff := c.clock.Now().Add(-st.cfg.WindowDuration)
	total, okAvail, okLat := 0, 0, 0
	for _, p := range st.points {
		if p.at.Before(cutoff) {
			continue
		}
		total++
		if p.available {
			okAvail++
		}
		if p.underLatency {
			okLat++
		}
	}
	if total == 0 {
		return 100, 100
	}
	return float64(okAvail) / float64(total) * 100,
		float64(okLat) / float64(total) * 100
}
