package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDo_Success verifies the golden path: a 200 response is reported as
// Success=true with the correct status code and a non-zero latency.
// httptest.NewServer spins up a real HTTP listener on a random localhost port
// so we exercise the real net/http stack without touching the network.
func TestDo_Success(t *testing.T) {
	// Handler returns 200 OK for every request.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close) // t.Cleanup runs even if the test panics — cleaner than defer in long tests.

	p := NewProber()
	res := p.Do(context.Background(), Target{
		Name:    "ok-server",
		URL:     srv.URL,
		Method:  http.MethodGet,
		Timeout: 2 * time.Second,
	})

	if !res.Success {
		t.Fatalf("want Success=true, got false (err=%v, status=%d)", res.Err, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want status 200, got %d", res.StatusCode)
	}
	if res.Latency <= 0 {
		t.Fatalf("want positive latency, got %v", res.Latency)
	}
	if res.Err != nil {
		t.Fatalf("want nil err, got %v", res.Err)
	}
}

// TestDo_Non2xxIsFailure pins down the rule that anything outside 200–299
// counts as a failed probe. This is the core of the availability SLO later,
// so regressing it would silently break everything downstream.
func TestDo_Non2xxIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	p := NewProber()
	res := p.Do(context.Background(), Target{
		Name:    "broken-server",
		URL:     srv.URL,
		Timeout: 2 * time.Second,
	})

	if res.Success {
		t.Fatalf("500 response should not be Success; got %+v", res)
	}
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("want status 500, got %d", res.StatusCode)
	}
	// Err should be nil because the *HTTP exchange* completed fine — only the
	// application-level status said no. Keeping these separate lets the
	// alerter distinguish "couldn't even connect" from "server said 500".
	if res.Err != nil {
		t.Fatalf("want nil err on HTTP 500, got %v", res.Err)
	}
}

// TestDo_TimeoutIsFailure checks that a slow server trips the per-request
// timeout and produces a failed Result (rather than hanging forever).
func TestDo_TimeoutIsFailure(t *testing.T) {
	// Handler sleeps longer than the probe's timeout. We deliberately make
	// the sleep a few multiples of the timeout to keep the test insensitive
	// to scheduler jitter on slow CI machines.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p := NewProber()
	res := p.Do(context.Background(), Target{
		Name:    "slow-server",
		URL:     srv.URL,
		Timeout: 20 * time.Millisecond, // forces deadline before the 200ms sleep completes
	})

	if res.Success {
		t.Fatalf("timed-out probe should not be Success; got %+v", res)
	}
	if res.Err == nil {
		t.Fatalf("want non-nil err on timeout, got nil")
	}
}

// TestDo_BadURLIsFailure ensures malformed config does not crash the probe;
// it just shows up as a Result with Err set. Operators prefer a loud red
// dashboard row over a crashed monitoring process.
func TestDo_BadURLIsFailure(t *testing.T) {
	p := NewProber()
	res := p.Do(context.Background(), Target{
		Name:    "bad-url",
		URL:     "http://%zz", // not a parseable URL
		Timeout: time.Second,
	})

	if res.Success {
		t.Fatalf("bad URL should not be Success; got %+v", res)
	}
	if res.Err == nil {
		t.Fatalf("want non-nil err on bad URL, got nil")
	}
}

// TestDo_DefaultsMethodToGET pins the convenience behaviour that an empty
// Method field is treated as GET. This is documented on the Target struct
// and relied on by the config loader in Stage 2.
func TestDo_DefaultsMethodToGET(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	p := NewProber()
	_ = p.Do(context.Background(), Target{
		Name:    "default-method",
		URL:     srv.URL,
		Method:  "", // intentionally empty
		Timeout: time.Second,
	})

	if gotMethod != http.MethodGet {
		t.Fatalf("empty Method should default to GET, server saw %q", gotMethod)
	}
}
