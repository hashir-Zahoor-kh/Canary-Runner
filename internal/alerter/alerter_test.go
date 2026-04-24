package alerter

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock — same pattern as slo and budget. Duplicating the ~10 lines
// is cheaper than introducing a shared testutil package.
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

// newTestAlerter returns an Alerter writing into a caller-visible buffer
// plus the buffer itself. This is the ground-zero test fixture.
func newTestAlerter() (*Alerter, *bytes.Buffer) {
	var buf bytes.Buffer
	return NewAlerter(newFakeClock(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)), &buf), &buf
}

// goodProbe returns an Input representing a healthy probe with a full
// budget — the "baseline" from which tests mutate one or two fields.
func goodProbe(endpoint string) Input {
	return Input{
		Endpoint:                    endpoint,
		URL:                         "http://" + endpoint,
		Success:                     true,
		SLOCompliancePercent:        100,
		ErrorBudgetRemainingPercent: 100,
		ErrorBudgetRemainingMinutes: 43.2,
		ErrorBudgetExhausted:        false,
	}
}

// assertAlertTypes is a tiny helper so individual tests read as
// "expect these three alert types" rather than per-index field access.
func assertAlertTypes(t *testing.T, got []Alert, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("alert count: want %d %v, got %d %v",
			len(want), want, len(got), alertTypes(got))
	}
	for i, w := range want {
		if got[i].Alert != w {
			t.Errorf("alert[%d]: want type %q, got %q", i, w, got[i].Alert)
		}
	}
}

func alertTypes(as []Alert) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Alert
	}
	return out
}

// TestWarning_FiresOnceOnFirstCrossing covers the core edge-triggered
// behaviour of the warning alert. Going from 100% → 40% fires once;
// staying at 40% does not re-fire.
func TestWarning_FiresOnceOnFirstCrossing(t *testing.T) {
	a, _ := newTestAlerter()

	// Healthy probe — no alert.
	if fired := a.Evaluate(goodProbe("svc")); len(fired) != 0 {
		t.Fatalf("healthy probe: want 0 alerts, got %v", alertTypes(fired))
	}

	// Drop to 40% remaining: fires warning (but not critical or exhausted).
	in := goodProbe("svc")
	in.ErrorBudgetRemainingPercent = 40
	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeBudgetWarning)
	if fired[0].Level != LevelWarning {
		t.Errorf("warning level: want %q, got %q", LevelWarning, fired[0].Level)
	}

	// Same 40% again: no new alert — this is the "fire once per crossing" rule.
	if fired := a.Evaluate(in); len(fired) != 0 {
		t.Fatalf("second probe at 40%%: want 0 alerts, got %v", alertTypes(fired))
	}
}

// TestWarning_RefiresAfterRecovery: once the endpoint recovers (remaining
// climbs above 50%), a subsequent drop must fire a fresh warning.
func TestWarning_RefiresAfterRecovery(t *testing.T) {
	a, _ := newTestAlerter()

	in := goodProbe("svc")
	in.ErrorBudgetRemainingPercent = 30
	_ = a.Evaluate(in) // initial fire

	// Recovery above 50% — no alert but the Fired flag clears internally.
	in.ErrorBudgetRemainingPercent = 80
	if fired := a.Evaluate(in); len(fired) != 0 {
		t.Fatalf("recovery: want 0 alerts, got %v", alertTypes(fired))
	}

	// Drop below 50% again — a brand-new warning should fire.
	in.ErrorBudgetRemainingPercent = 45
	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeBudgetWarning)
}

// TestCritical_FiresBelowTenPercent pins down the <10% threshold.
func TestCritical_FiresBelowTenPercent(t *testing.T) {
	a, _ := newTestAlerter()

	// Crossing from 100% to 5% fires BOTH warning and critical on the same
	// probe — they are independent thresholds, both entered at once.
	in := goodProbe("svc")
	in.ErrorBudgetRemainingPercent = 5
	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeBudgetWarning, TypeBudgetCritical)
	if fired[1].Level != LevelCritical {
		t.Errorf("critical level: want %q, got %q", LevelCritical, fired[1].Level)
	}
}

// TestExhausted_FiresWhenExhaustedFlagTrue covers the exhausted alert.
// We also confirm it fires in combination with warning+critical when
// everything transitions in one step.
func TestExhausted_FiresWhenExhaustedFlagTrue(t *testing.T) {
	a, _ := newTestAlerter()

	in := goodProbe("svc")
	in.ErrorBudgetRemainingPercent = 0
	in.ErrorBudgetRemainingMinutes = 0
	in.ErrorBudgetExhausted = true

	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeBudgetWarning, TypeBudgetCritical, TypeBudgetExhausted)
	for _, want := range []string{TypeBudgetCritical, TypeBudgetExhausted} {
		for _, a := range fired {
			if a.Alert == want && a.Level != LevelCritical {
				t.Errorf("%s: want CRITICAL, got %s", want, a.Level)
			}
		}
	}
}

// TestConsecutiveFailures_FiresAtFiveInARow: the counter has to reach
// exactly 5, and does not re-fire on the 6th, 7th, ... failure.
func TestConsecutiveFailures_FiresAtFiveInARow(t *testing.T) {
	a, _ := newTestAlerter()

	in := goodProbe("svc")
	in.Success = false // flip to failing probe

	// Probes 1..4 — counter climbs, no alert.
	for i := 1; i <= 4; i++ {
		if fired := a.Evaluate(in); len(fired) != 0 {
			t.Fatalf("probe %d: want 0 alerts, got %v", i, alertTypes(fired))
		}
	}

	// Probe 5 — threshold met, alert fires with consecutive_failures=5.
	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeConsecutiveFailures)
	if fired[0].ConsecutiveFailures != 5 {
		t.Errorf("consecutive_failures field: want 5, got %d", fired[0].ConsecutiveFailures)
	}

	// Probe 6 — still failing, but the alert does not re-fire.
	if fired := a.Evaluate(in); len(fired) != 0 {
		t.Fatalf("probe 6: want 0 alerts, got %v", alertTypes(fired))
	}
}

// TestConsecutiveFailures_ResetsOnSuccess: a success zeroes the counter,
// so the next run of 5 failures fires a fresh alert.
func TestConsecutiveFailures_ResetsOnSuccess(t *testing.T) {
	a, _ := newTestAlerter()

	bad := goodProbe("svc")
	bad.Success = false
	good := goodProbe("svc")

	// 5 failures → first alert.
	for i := 0; i < 5; i++ {
		a.Evaluate(bad)
	}
	// Single success — counter resets, Fired flag clears.
	if fired := a.Evaluate(good); len(fired) != 0 {
		t.Fatalf("success after failures: want 0 alerts, got %v", alertTypes(fired))
	}
	// Next 4 failures — still below threshold, no alert.
	for i := 0; i < 4; i++ {
		if fired := a.Evaluate(bad); len(fired) != 0 {
			t.Fatalf("post-reset probe %d: want 0 alerts, got %v", i+1, alertTypes(fired))
		}
	}
	// 5th failure after reset — brand-new alert.
	fired := a.Evaluate(bad)
	assertAlertTypes(t, fired, TypeConsecutiveFailures)
}

// TestAlertPayload_HasAllFields serialises an alert and checks every
// spec-mandated JSON key is present with the right type. This is the
// only test that reads the writer output directly.
func TestAlertPayload_HasAllFields(t *testing.T) {
	a, buf := newTestAlerter()

	in := Input{
		Endpoint:                    "checkout",
		URL:                         "http://checkout.example",
		Success:                     true,
		SLOCompliancePercent:        99.1,
		ErrorBudgetRemainingPercent: 48.2,
		ErrorBudgetRemainingMinutes: 20.8,
		ErrorBudgetExhausted:        false,
	}
	fired := a.Evaluate(in)
	assertAlertTypes(t, fired, TypeBudgetWarning)

	// The Alerter writes one JSON object per line; there's exactly one
	// here, so we can decode it straight into a map to inspect keys.
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("decode emitted JSON: %v (body=%q)", err, buf.String())
	}

	// Spec-mandated keys.
	for _, key := range []string{
		"level", "timestamp", "alert", "endpoint", "url",
		"slo_compliance_percent", "error_budget_remaining_percent",
		"error_budget_remaining_minutes", "consecutive_failures", "message",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("alert JSON missing key %q (keys=%v)", key, keysOf(got))
		}
	}

	// Spot-check values we know.
	if got["level"] != LevelWarning {
		t.Errorf("level: want %q, got %v", LevelWarning, got["level"])
	}
	if got["endpoint"] != "checkout" {
		t.Errorf("endpoint: want checkout, got %v", got["endpoint"])
	}
	if got["slo_compliance_percent"].(float64) != 99.1 {
		t.Errorf("slo_compliance_percent: want 99.1, got %v", got["slo_compliance_percent"])
	}

	// Timestamp must parse as RFC3339 — otherwise the contract is broken.
	if ts, ok := got["timestamp"].(string); !ok {
		t.Errorf("timestamp not a string: %v", got["timestamp"])
	} else if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("timestamp not RFC3339 (%q): %v", ts, err)
	}

	// The emitted JSON should be exactly one line (Encoder adds \n).
	if count := strings.Count(buf.String(), "\n"); count != 1 {
		t.Errorf("want exactly 1 newline in buffer, got %d (body=%q)", count, buf.String())
	}
}

// keysOf is a small helper so error messages list the keys we DID see.
// Cheaper to debug than "missing key x".
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
