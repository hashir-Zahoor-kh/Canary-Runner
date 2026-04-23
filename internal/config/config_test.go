package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempConfig drops a YAML string into a fresh temp file and returns its
// path. t.TempDir() is cleaned up automatically at the end of the test, so
// we don't need to remove the file ourselves.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper() // makes test failures point at the caller, not this helper
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return p
}

// TestLoad_AppliesDefaults locks down the "minimal config" case: with only
// name + url supplied, every other field should end up at its documented
// default. This test is the canonical place to notice if we silently change
// a default — it will fail loudly.
func TestLoad_AppliesDefaults(t *testing.T) {
	path := writeTempConfig(t, `
targets:
  - name: "only-required"
    url: "http://localhost:1"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(cfg.Targets))
	}
	got := cfg.Targets[0]

	// Use table-driven field checks so a single assertion failure still
	// reports every default that's wrong — easier to debug.
	checks := []struct {
		field string
		want  any
		got   any
	}{
		{"Method", DefaultMethod, got.Method},
		{"TimeoutSeconds", DefaultTimeoutSeconds, got.TimeoutSeconds},
		{"IntervalSeconds", DefaultIntervalSeconds, got.IntervalSeconds},
		{"SLO.AvailabilityTargetPercent", DefaultAvailabilityTargetPercent, got.SLO.AvailabilityTargetPercent},
		{"SLO.LatencyTargetMS", DefaultLatencyTargetMS, got.SLO.LatencyTargetMS},
		{"SLO.LatencySLOPercent", DefaultLatencySLOPercent, got.SLO.LatencySLOPercent},
		{"SLO.WindowDays", DefaultWindowDays, got.SLO.WindowDays},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: want %v, got %v", c.field, c.want, c.got)
		}
	}
}

// TestLoad_ExplicitValuesWin ensures user-supplied values beat the defaults.
// This is the test that catches the easy bug of "applyDefaults overwrites
// everything unconditionally".
func TestLoad_ExplicitValuesWin(t *testing.T) {
	path := writeTempConfig(t, `
targets:
  - name: "full"
    url: "http://localhost:2"
    method: "HEAD"
    timeout_seconds: 7
    interval_seconds: 15
    slo:
      availability_target_percent: 99.5
      latency_target_ms: 500
      latency_slo_percent: 90.0
      window_days: 7
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Targets[0]

	if got.Method != "HEAD" {
		t.Errorf("Method: want HEAD, got %q", got.Method)
	}
	if got.TimeoutSeconds != 7 {
		t.Errorf("TimeoutSeconds: want 7, got %d", got.TimeoutSeconds)
	}
	if got.IntervalSeconds != 15 {
		t.Errorf("IntervalSeconds: want 15, got %d", got.IntervalSeconds)
	}
	if got.SLO.AvailabilityTargetPercent != 99.5 {
		t.Errorf("AvailabilityTargetPercent: want 99.5, got %v", got.SLO.AvailabilityTargetPercent)
	}
	if got.SLO.WindowDays != 7 {
		t.Errorf("WindowDays: want 7, got %d", got.SLO.WindowDays)
	}
}

// TestLoad_ValidationErrors is a table-driven negative test: every row is a
// config that *should* fail. Keeping them in one place documents the full
// surface of validation rules.
func TestLoad_ValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string // substring we expect to appear in the error
	}{
		{
			name:    "no targets",
			body:    `targets: []`,
			wantSub: "at least one target",
		},
		{
			name: "missing name",
			body: `
targets:
  - url: "http://localhost:1"
`,
			wantSub: "name is required",
		},
		{
			name: "missing url",
			body: `
targets:
  - name: "x"
`,
			wantSub: "url is required",
		},
		{
			name: "duplicate names",
			body: `
targets:
  - name: "dup"
    url: "http://localhost:1"
  - name: "dup"
    url: "http://localhost:2"
`,
			wantSub: "duplicate target name",
		},
		{
			name: "availability out of range",
			body: `
targets:
  - name: "bad"
    url: "http://localhost:1"
    slo:
      availability_target_percent: 150
`,
			wantSub: "availability_target_percent",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %q", tc.wantSub, err.Error())
			}
		})
	}
}

// TestLoad_MissingFile verifies a nice error (not a panic) when the config
// path doesn't exist. In production this usually means the operator passed
// -config to a path that wasn't mounted into the container.
func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("want error on missing file, got nil")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("want error to mention 'read config', got %q", err.Error())
	}
}
