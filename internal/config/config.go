// Package config loads and validates Canary Runner's YAML configuration.
//
// The file describes which HTTP endpoints to monitor and, for each, the SLO
// the operator cares about. Every field except `name` and `url` has a
// sensible default so a real-world config can be very terse.
package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Default values applied to any field the user leaves unset. Centralising
// them here means there is exactly one place to change "what does Canary
// Runner assume if I forget this field?" — and test coverage pins them down.
const (
	DefaultMethod                     = "GET"
	DefaultTimeoutSeconds             = 5
	DefaultIntervalSeconds            = 60
	DefaultAvailabilityTargetPercent  = 99.9
	DefaultLatencyTargetMS            = 200
	DefaultLatencySLOPercent          = 95.0
	DefaultWindowDays                 = 30
)

// Config is the top-level YAML document: a list of targets to probe.
// We keep this a struct (instead of a bare []Target) so we can grow the
// schema later — e.g. a global `listen_address` field for the metrics
// endpoint — without breaking existing config files.
type Config struct {
	Targets []Target `yaml:"targets"`
}

// Target describes a single endpoint to probe, plus the SLO Canary Runner
// should judge it against. The yaml tags map the Go field names to the
// snake_case keys used in config.yaml.
type Target struct {
	Name            string `yaml:"name"`
	URL             string `yaml:"url"`
	Method          string `yaml:"method"`
	TimeoutSeconds  int    `yaml:"timeout_seconds"`
	IntervalSeconds int    `yaml:"interval_seconds"`
	SLO             SLO    `yaml:"slo"`
}

// SLO captures the two SLOs Canary Runner tracks per endpoint: availability
// and latency. Both are expressed as percentages (e.g. 99.9 = "99.9% of
// probes should succeed"). WindowDays is the rolling-window length Stage 3
// will use for calculating compliance and Stage 4 for sizing the error budget.
type SLO struct {
	AvailabilityTargetPercent float64 `yaml:"availability_target_percent"`
	LatencyTargetMS           int     `yaml:"latency_target_ms"`
	LatencySLOPercent         float64 `yaml:"latency_slo_percent"`
	WindowDays                int     `yaml:"window_days"`
}

// Load reads the YAML file at path, applies defaults, and validates the
// result. Any error — missing file, bad YAML, or invalid target — is
// returned unchanged so main.go can log and exit.
func Load(path string) (*Config, error) {
	// os.ReadFile returns the whole file as a []byte in one call. For config
	// files (kilobytes at most) this is fine; we'd stream for anything larger.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	// yaml.Unmarshal populates exported fields that match yaml tags. Unknown
	// keys are ignored by default, which is good for forward compatibility
	// (an older binary sees a newer config and just skips unknown fields).
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	// Apply defaults in-place. Doing this *after* parsing means explicit
	// zero values in YAML (e.g. `timeout_seconds: 0`) also get normalised,
	// which is almost always what the user meant.
	for i := range cfg.Targets {
		applyDefaults(&cfg.Targets[i])
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults mutates t to fill in any unset fields. We detect "unset" by
// comparing against the zero value — this is deliberate; YAML that
// explicitly sets `interval_seconds: 0` would be nonsensical anyway.
func applyDefaults(t *Target) {
	if t.Method == "" {
		t.Method = DefaultMethod
	}
	if t.TimeoutSeconds == 0 {
		t.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if t.IntervalSeconds == 0 {
		t.IntervalSeconds = DefaultIntervalSeconds
	}
	if t.SLO.AvailabilityTargetPercent == 0 {
		t.SLO.AvailabilityTargetPercent = DefaultAvailabilityTargetPercent
	}
	if t.SLO.LatencyTargetMS == 0 {
		t.SLO.LatencyTargetMS = DefaultLatencyTargetMS
	}
	if t.SLO.LatencySLOPercent == 0 {
		t.SLO.LatencySLOPercent = DefaultLatencySLOPercent
	}
	if t.SLO.WindowDays == 0 {
		t.SLO.WindowDays = DefaultWindowDays
	}
}

// validate enforces the fields that do *not* have defaults — name and URL —
// plus a couple of sanity checks that would otherwise produce confusing
// runtime behaviour (e.g. a percentage of 150).
func (c *Config) validate() error {
	if len(c.Targets) == 0 {
		return errors.New("config: at least one target is required")
	}
	// Duplicate names would make metrics-label collisions in Stage 5, so we
	// catch them here where the error message can point at the YAML file.
	seen := make(map[string]bool, len(c.Targets))
	for i, t := range c.Targets {
		if t.Name == "" {
			return fmt.Errorf("config: targets[%d]: name is required", i)
		}
		if t.URL == "" {
			return fmt.Errorf("config: target %q: url is required", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("config: duplicate target name %q", t.Name)
		}
		seen[t.Name] = true

		if t.SLO.AvailabilityTargetPercent <= 0 || t.SLO.AvailabilityTargetPercent > 100 {
			return fmt.Errorf("config: target %q: availability_target_percent must be in (0,100]", t.Name)
		}
		if t.SLO.LatencySLOPercent <= 0 || t.SLO.LatencySLOPercent > 100 {
			return fmt.Errorf("config: target %q: latency_slo_percent must be in (0,100]", t.Name)
		}
	}
	return nil
}
