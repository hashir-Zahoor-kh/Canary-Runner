// Package probe implements the HTTP probing primitive used by Canary Runner.
// A Probe sends a single HTTP request against a configured target and produces
// a Result describing what happened: whether it succeeded, how long it took,
// and what HTTP status (if any) came back.
package probe

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Target is the immutable configuration describing one endpoint to monitor.
// It lives separately from Result so that a single Target can be probed many
// times — each probe produces its own Result, but the Target does not change.
type Target struct {
	Name     string        // Human-readable name, e.g. "checkout-api"
	URL      string        // Full URL, including scheme
	Method   string        // HTTP verb, e.g. "GET"
	Timeout  time.Duration // Per-request timeout; the probe is cancelled if exceeded
	Interval time.Duration // How often the Scheduler re-runs this probe
}

// Result captures the outcome of a single probe execution.
// Success is deliberately defined as "any 2xx response returned before the
// timeout elapsed". 3xx, 4xx and 5xx are all treated as failures for the
// purposes of the availability SLO in later stages.
type Result struct {
	Name       string        // Copied from Target.Name so consumers don't need the Target
	URL        string        // Copied from Target.URL for the same reason
	Method     string        // HTTP method actually sent (after defaults), used for metric labels
	Success    bool          // True if a 2xx response was received before the timeout
	StatusCode int           // HTTP status code, or 0 if the request never completed
	Latency    time.Duration // Wall-clock time from request start to response-headers received
	Err        error         // Non-nil if the request failed (timeout, DNS, connection refused, …)
	Timestamp  time.Time     // When the probe finished — used later for rolling-window SLO math
}

// Prober executes probes against Targets. It is a struct (rather than a free
// function) so we can inject a custom *http.Client in tests and avoid leaking
// the default Go transport's connection pool between test cases.
type Prober struct {
	client *http.Client
}

// NewProber constructs a Prober with a dedicated http.Client.
// We disable the client-level Timeout and instead rely on a per-request
// context.WithTimeout in Do(). That way the timeout is tied to the request's
// lifecycle (and is cancelled cleanly on shutdown) rather than being a global
// knob on the client.
func NewProber() *Prober {
	return &Prober{
		// Transport is the zero value (http.DefaultTransport is reused under the
		// hood). For a learning project that is the right tradeoff: it gives us
		// connection pooling for free without hand-rolling a Transport.
		client: &http.Client{},
	}
}

// NewProberWithClient lets tests inject a pre-configured *http.Client — for
// example one pointed at an httptest.Server. Exported so tests in other
// packages could use it too, though today only probe_test.go needs it.
func NewProberWithClient(c *http.Client) *Prober {
	return &Prober{client: c}
}

// Do executes a single probe against the target and returns its Result.
//
// The returned Result always has Name, URL, Timestamp and Latency populated,
// even on failure — downstream code (SLO calculator, metrics exporter) relies
// on every probe contributing one data point to the rolling window, so we
// never return a nil Result or short-circuit on errors.
func (p *Prober) Do(ctx context.Context, t Target) Result {
	// Default the HTTP method if the caller didn't set one. We do this here
	// rather than in the config layer so the probe package is self-contained
	// and testable without going through config loading.
	method := t.Method
	if method == "" {
		method = http.MethodGet
	}

	// Derive a per-request context that will be cancelled after t.Timeout.
	// The parent ctx is usually the program's root context, so this timeout
	// also respects SIGTERM / SIGINT cancellation in later stages.
	reqCtx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()

	// Build the Result up-front so that every early-return path produces a
	// well-formed record. We overwrite fields as we learn more. Method is
	// set to the already-defaulted value so consumers never see an empty
	// string, regardless of what the caller passed on the Target.
	result := Result{
		Name:   t.Name,
		URL:    t.URL,
		Method: method,
	}

	// NewRequestWithContext fails on malformed URLs or invalid methods.
	// We treat that as a probe failure (rather than panicking) because bad
	// config should not crash the service — it should just show up as a
	// perpetual failure on the dashboard, which is exactly what operators want.
	req, err := http.NewRequestWithContext(reqCtx, method, t.URL, nil)
	if err != nil {
		result.Err = fmt.Errorf("build request: %w", err)
		result.Timestamp = time.Now()
		return result
	}

	// Record start *after* the request is built so we measure network time,
	// not request-construction overhead.
	start := time.Now()
	resp, err := p.client.Do(req)
	result.Latency = time.Since(start)
	result.Timestamp = time.Now()

	if err != nil {
		// Connection refused, DNS failure, TLS handshake error, context
		// deadline exceeded — all land here. We keep the raw error so the
		// logger can surface the specific cause to operators.
		result.Err = err
		return result
	}
	// resp.Body must always be closed to release the underlying TCP connection
	// back to the pool; skipping this is a classic Go leak.
	defer resp.Body.Close()

	result.StatusCode = resp.StatusCode
	// Only 2xx counts as success. This is the one place the "what is a
	// successful probe" policy lives — keep it centralised.
	result.Success = resp.StatusCode >= 200 && resp.StatusCode < 300
	return result
}
