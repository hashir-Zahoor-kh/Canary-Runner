// Package alerter emits structured JSON alerts — one line per alert — to
// stdout when an endpoint crosses one of four well-defined thresholds:
//
//  1. error_budget_warning       (WARNING)  — <50% remaining
//  2. error_budget_critical      (CRITICAL) — <10% remaining
//  3. error_budget_exhausted     (CRITICAL) — consumed >= total
//  4. consecutive_failures       (CRITICAL) — 5 probes in a row failed
//
// Each alert fires exactly once per *crossing*: while the endpoint stays
// in the alerting region the alert is not re-emitted, but when the
// endpoint recovers and later re-enters the region the alert fires again.
// This is the standard "edge-triggered" alerting model — exactly what
// pager-fatigue-conscious SREs ask for.
package alerter

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Alert levels. Two tiers matches the spec; we surface them as constants
// so callers (and test assertions) don't have to duplicate the strings.
const (
	LevelWarning  = "WARNING"
	LevelCritical = "CRITICAL"
)

// Alert type identifiers used in the "alert" JSON field.
const (
	TypeBudgetWarning        = "error_budget_warning"
	TypeBudgetCritical       = "error_budget_critical"
	TypeBudgetExhausted      = "error_budget_exhausted"
	TypeConsecutiveFailures  = "consecutive_failures"
)

// ConsecutiveFailureThreshold is the run of failed probes that fires the
// consecutive_failures alert. Exported so tests and operators can see
// the magic number without re-reading the source.
const ConsecutiveFailureThreshold = 5

// BudgetWarningThresholdPercent is the "remaining %" boundary for the
// warning alert. BudgetCriticalThresholdPercent is the same for critical.
const (
	BudgetWarningThresholdPercent  = 50.0
	BudgetCriticalThresholdPercent = 10.0
)

// Clock is redeclared here — same reason as budget.Clock — so the
// alerter package has no dependency on other internal packages just to
// borrow a two-line interface.
type Clock interface {
	Now() time.Time
}

// RealClock is the production clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// Alert is the exact wire format of one alert line. Field ordering in
// the struct matches the spec so operators grepping logs by position
// aren't surprised.
type Alert struct {
	Level                       string  `json:"level"`
	Timestamp                   string  `json:"timestamp"`
	Alert                       string  `json:"alert"`
	Endpoint                    string  `json:"endpoint"`
	URL                         string  `json:"url"`
	SLOCompliancePercent        float64 `json:"slo_compliance_percent"`
	ErrorBudgetRemainingPercent float64 `json:"error_budget_remaining_percent"`
	ErrorBudgetRemainingMinutes float64 `json:"error_budget_remaining_minutes"`
	ConsecutiveFailures         int     `json:"consecutive_failures"`
	Message                     string  `json:"message"`
}

// Input carries everything the Alerter needs about one probe. Keeping
// it a flat struct (rather than threading probe.Result + slo + budget
// straight through) keeps the alerter package from depending on all the
// other internal packages at the type level.
type Input struct {
	Endpoint                    string
	URL                         string
	Success                     bool
	SLOCompliancePercent        float64
	ErrorBudgetRemainingPercent float64
	ErrorBudgetRemainingMinutes float64
	ErrorBudgetExhausted        bool
}

// endpointState holds the per-endpoint bookkeeping. The four *Fired
// booleans are the heart of the "fire-once-per-crossing" behaviour:
// they flip true when the alert fires and flip false when the condition
// clears, so the next time the endpoint enters the region we fire again.
type endpointState struct {
	consecutiveFailures      int
	warningFired             bool
	criticalFired            bool
	exhaustedFired           bool
	consecutiveFailuresFired bool
}

// Alerter evaluates probe outcomes against the four alert thresholds
// and writes any fired alerts (as one JSON line each) to its writer.
// Safe for concurrent use — a single Alerter is shared across all
// endpoints in main.go.
type Alerter struct {
	mu     sync.Mutex
	clock  Clock
	writer io.Writer
	states map[string]*endpointState
}

// NewAlerter constructs an Alerter. writer is where alert JSON lines
// are written; passing nil means os.Stdout (which is the production
// wiring). Tests pass a *bytes.Buffer and then assert on its contents.
func NewAlerter(clock Clock, writer io.Writer) *Alerter {
	if writer == nil {
		writer = os.Stdout
	}
	return &Alerter{
		clock:  clock,
		writer: writer,
		states: make(map[string]*endpointState),
	}
}

// Evaluate processes one probe outcome, updates the per-endpoint state,
// and emits every newly-fired alert as a JSON line. The slice of fired
// alerts is also returned so callers (and tests) can inspect them
// without re-parsing the writer's output.
func (a *Alerter) Evaluate(in Input) []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()

	st := a.states[in.Endpoint]
	if st == nil {
		// First time we've seen this endpoint — lazy-init the state so
		// callers don't have to register endpoints up-front.
		st = &endpointState{}
		a.states[in.Endpoint] = st
	}

	// Update the consecutive-failures counter. A success not only zeroes
	// the counter but also clears the Fired flag so the NEXT run of 5
	// failures fires a fresh alert (edge-triggered semantics).
	if in.Success {
		st.consecutiveFailures = 0
		st.consecutiveFailuresFired = false
	} else {
		st.consecutiveFailures++
	}

	now := a.clock.Now()
	var fired []Alert

	// Order: we evaluate from least to most severe so a single probe
	// that crosses multiple thresholds (e.g. 60% → 0% in one step)
	// produces a readable progression in the log.

	// --- Alert 1: error_budget_warning — <50% remaining ---
	if in.ErrorBudgetRemainingPercent < BudgetWarningThresholdPercent {
		if !st.warningFired {
			fired = append(fired, a.make(in, st, TypeBudgetWarning, LevelWarning,
				"error budget below 50% remaining", now))
			st.warningFired = true
		}
	} else {
		// Above the threshold — clear the flag so the next descent fires.
		st.warningFired = false
	}

	// --- Alert 2: error_budget_critical — <10% remaining ---
	if in.ErrorBudgetRemainingPercent < BudgetCriticalThresholdPercent {
		if !st.criticalFired {
			fired = append(fired, a.make(in, st, TypeBudgetCritical, LevelCritical,
				"error budget below 10% remaining", now))
			st.criticalFired = true
		}
	} else {
		st.criticalFired = false
	}

	// --- Alert 3: error_budget_exhausted — fully consumed ---
	if in.ErrorBudgetExhausted {
		if !st.exhaustedFired {
			fired = append(fired, a.make(in, st, TypeBudgetExhausted, LevelCritical,
				"error budget fully consumed", now))
			st.exhaustedFired = true
		}
	} else {
		st.exhaustedFired = false
	}

	// --- Alert 4: consecutive_failures — 5 in a row ---
	// Note this fires at the moment the counter hits the threshold. A
	// 6th, 7th, 8th failure do NOT re-fire because Fired is sticky
	// until a success resets the counter (see the success branch above).
	if st.consecutiveFailures >= ConsecutiveFailureThreshold {
		if !st.consecutiveFailuresFired {
			fired = append(fired, a.make(in, st, TypeConsecutiveFailures, LevelCritical,
				"5 consecutive probe failures", now))
			st.consecutiveFailuresFired = true
		}
	}

	// Emit. json.Encoder.Encode appends a newline, giving us clean JSON
	// lines. We deliberately ignore write errors: stdout is the one
	// place we cannot usefully log about a failure to write to stdout.
	enc := json.NewEncoder(a.writer)
	for _, alert := range fired {
		_ = enc.Encode(alert)
	}

	return fired
}

// make builds one Alert struct from the current Input and endpointState,
// filling in every wire-format field in one place so Evaluate stays focused
// on the condition logic.
func (a *Alerter) make(in Input, st *endpointState, alertType, level, message string, now time.Time) Alert {
	return Alert{
		Level:                       level,
		Timestamp:                   now.UTC().Format(time.RFC3339),
		Alert:                       alertType,
		Endpoint:                    in.Endpoint,
		URL:                         in.URL,
		SLOCompliancePercent:        in.SLOCompliancePercent,
		ErrorBudgetRemainingPercent: in.ErrorBudgetRemainingPercent,
		ErrorBudgetRemainingMinutes: in.ErrorBudgetRemainingMinutes,
		ConsecutiveFailures:         st.consecutiveFailures,
		Message:                     message,
	}
}
