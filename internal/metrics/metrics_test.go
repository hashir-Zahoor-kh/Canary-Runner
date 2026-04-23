package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/hashir/canary-runner/internal/probe"
)

// successResult is a compact builder for a green probe Result. Having
// one helper keeps individual tests focused on the assertion, not on
// filling in every Result field.
func successResult(name string, latency time.Duration) probe.Result {
	return probe.Result{
		Name:       name,
		URL:        "http://" + name,
		Method:     http.MethodGet,
		Success:    true,
		StatusCode: 200,
		Latency:    latency,
	}
}

func failureResult(name string, latency time.Duration, status int) probe.Result {
	return probe.Result{
		Name:       name,
		URL:        "http://" + name,
		Method:     http.MethodGet,
		Success:    false,
		StatusCode: status,
		Latency:    latency,
	}
}

// TestExporter_AllMetricsRegistered ensures every metric family listed
// in the Stage 5 spec actually shows up in the registry. If someone
// ever removes a collector, this test is the tripwire.
func TestExporter_AllMetricsRegistered(t *testing.T) {
	e := NewExporter()

	// Observe one value per family so they all appear in Gather().
	// Unpopulated *Vec families don't emit metric family entries until a
	// child gauge/counter has been touched at least once.
	e.RecordResult(successResult("svc", 75*time.Millisecond))
	e.UpdateSLO("svc", 100, 100)
	e.UpdateBudget("svc", 0, 100)

	mfs, err := e.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		got[mf.GetName()] = true
	}
	want := []string{
		"canary_probe_success",
		"canary_probe_latency_seconds",
		"canary_slo_compliance_percent",
		"canary_error_budget_remaining_percent",
		"canary_error_budget_consumed_minutes",
		"canary_probes_total",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("metric %q not registered / not collected", name)
		}
	}
}

// TestExporter_RecordResult_Success pins down the happy-path outputs:
// success gauge = 1, failure counter untouched, success counter bumped.
func TestExporter_RecordResult_Success(t *testing.T) {
	e := NewExporter()
	e.RecordResult(successResult("svc", 100*time.Millisecond))

	if got := testutil.ToFloat64(e.probeSuccess.WithLabelValues("svc", "GET")); got != 1 {
		t.Errorf("probe_success: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(e.probesTotal.WithLabelValues("svc", "success")); got != 1 {
		t.Errorf("probes_total{result=success}: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(e.probesTotal.WithLabelValues("svc", "failure")); got != 0 {
		t.Errorf("probes_total{result=failure}: want 0, got %v", got)
	}
}

// TestExporter_RecordResult_Failure is the symmetric case: success
// gauge = 0, failure counter bumped instead of success counter.
func TestExporter_RecordResult_Failure(t *testing.T) {
	e := NewExporter()
	e.RecordResult(failureResult("svc", 200*time.Millisecond, 500))

	if got := testutil.ToFloat64(e.probeSuccess.WithLabelValues("svc", "GET")); got != 0 {
		t.Errorf("probe_success: want 0, got %v", got)
	}
	if got := testutil.ToFloat64(e.probesTotal.WithLabelValues("svc", "failure")); got != 1 {
		t.Errorf("probes_total{result=failure}: want 1, got %v", got)
	}
}

// TestExporter_LatencyHistogramObservationCount verifies observations
// land in the histogram — we count via the raw Gather() path because
// testutil.ToFloat64 doesn't work on histograms.
func TestExporter_LatencyHistogramObservationCount(t *testing.T) {
	e := NewExporter()
	e.RecordResult(successResult("svc", 75*time.Millisecond))
	e.RecordResult(successResult("svc", 300*time.Millisecond))
	e.RecordResult(failureResult("svc", 5*time.Second, 500))

	mfs, err := e.registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var count uint64
	for _, mf := range mfs {
		if mf.GetName() != "canary_probe_latency_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			count += m.GetHistogram().GetSampleCount()
		}
	}
	if count != 3 {
		t.Errorf("histogram sample count: want 3, got %d", count)
	}
}

// TestExporter_UpdateSLO verifies both gauge values land on the right
// slo_type label. The two SLOs are conceptually independent, so we
// explicitly assert each one.
func TestExporter_UpdateSLO(t *testing.T) {
	e := NewExporter()
	e.UpdateSLO("svc", 99.5, 97.2)

	if got := testutil.ToFloat64(e.sloCompliance.WithLabelValues("svc", "availability")); got != 99.5 {
		t.Errorf("availability gauge: want 99.5, got %v", got)
	}
	if got := testutil.ToFloat64(e.sloCompliance.WithLabelValues("svc", "latency")); got != 97.2 {
		t.Errorf("latency gauge: want 97.2, got %v", got)
	}
}

// TestExporter_UpdateBudget is the analogue for the budget gauges.
func TestExporter_UpdateBudget(t *testing.T) {
	e := NewExporter()
	e.UpdateBudget("svc", 21.6, 50)

	if got := testutil.ToFloat64(e.budgetConsumed.WithLabelValues("svc")); got != 21.6 {
		t.Errorf("consumed_minutes gauge: want 21.6, got %v", got)
	}
	if got := testutil.ToFloat64(e.budgetRemaining.WithLabelValues("svc")); got != 50 {
		t.Errorf("remaining_percent gauge: want 50, got %v", got)
	}
}

// TestExporter_HealthEndpoint exercises the /health JSON shape.
// We deliberately pre-date startTime so UptimeSeconds is predictable.
func TestExporter_HealthEndpoint(t *testing.T) {
	e := NewExporter()
	start := time.Now().Add(-42 * time.Second)
	server := e.Server(":0", 3, start)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	server.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}

	var h Health
	if err := json.NewDecoder(rr.Body).Decode(&h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Status != "healthy" {
		t.Errorf("status field: want healthy, got %q", h.Status)
	}
	if h.TargetsMonitored != 3 {
		t.Errorf("targets_monitored: want 3, got %d", h.TargetsMonitored)
	}
	// Uptime is computed from the real clock, so we allow a small window.
	if h.UptimeSeconds < 42 || h.UptimeSeconds > 60 {
		t.Errorf("uptime_seconds: want ~42, got %d", h.UptimeSeconds)
	}
}

// TestExporter_MetricsEndpointServesPromFormat ensures the /metrics
// handler returns the OpenMetrics/Prom text format, including the
// names of all six collectors. This is what a real Prometheus server
// would see during a scrape.
func TestExporter_MetricsEndpointServesPromFormat(t *testing.T) {
	e := NewExporter()
	e.RecordResult(successResult("svc", 80*time.Millisecond))
	e.UpdateSLO("svc", 100, 100)
	e.UpdateBudget("svc", 0, 100)

	server := e.Server(":0", 1, time.Now())

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	server.Handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, name := range []string{
		"canary_probe_success",
		"canary_probe_latency_seconds",
		"canary_slo_compliance_percent",
		"canary_error_budget_remaining_percent",
		"canary_error_budget_consumed_minutes",
		"canary_probes_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("metrics body missing %q", name)
		}
	}
	// Spot-check a label too — catches the common typo of "endpoint"
	// vs "target" etc.
	if !strings.Contains(body, `endpoint="svc"`) {
		t.Errorf("metrics body missing endpoint=\"svc\" label: %s", body)
	}
}
