package main

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// TestResolveCertRotatorConfig_DayDurations -- guard for a major fix: the
// threshold/jitter of the rotate_due_certs rule are written by convention as
// `<N>d` (like all reaper rules) and must be parsed via config.ParseDuration,
// not stdlib time.ParseDuration (the latter doesn't know the `d` suffix ->
// Threshold=0 -> the rule SILENTLY fails to rotate while enabled:true, a
// silent security failure).
func TestResolveCertRotatorConfig_DayDurations(t *testing.T) {
	seven := 7
	cfg := &config.KeeperConfig{
		Vault: config.KeeperVault{PKIMount: "pki/soulstack", PKIRole: "service-tls"},
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:             true,
					RotateThreshold:     "30d",
					RotateJitter:        "7d",
					MaxRotationsPerTick: &seven,
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)

	if out.Threshold != 30*24*time.Hour {
		t.Errorf("Threshold = %v, want 720h (30d)", out.Threshold)
	}
	if out.JitterWindow != 7*24*time.Hour {
		t.Errorf("JitterWindow = %v, want 168h (7d)", out.JitterWindow)
	}
	if out.MaxRotationsPerTick != 7 {
		t.Errorf("MaxRotationsPerTick = %d, want 7", out.MaxRotationsPerTick)
	}
	if out.DefaultPKIMount != "pki/soulstack" || out.DefaultPKIRole != "service-tls" {
		t.Errorf("PKI mount/role = %q/%q, want pki/soulstack/service-tls", out.DefaultPKIMount, out.DefaultPKIRole)
	}
}

// TestResolveCertRotatorConfig_HourDurations -- the stdlib-compatible form
// `720h` keeps working (config.ParseDuration delegates to time.ParseDuration
// when there's no `d` suffix).
func TestResolveCertRotatorConfig_HourDurations(t *testing.T) {
	cfg := &config.KeeperConfig{
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:         true,
					RotateThreshold: "720h",
					RotateJitter:    "168h",
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)

	if out.Threshold != 720*time.Hour {
		t.Errorf("Threshold = %v, want 720h", out.Threshold)
	}
	if out.JitterWindow != 168*time.Hour {
		t.Errorf("JitterWindow = %v, want 168h", out.JitterWindow)
	}
}

// TestResolveCertRotatorConfig_InvalidThreshold -- a malformed threshold
// format must not silently collapse Threshold to 0 without a trace: invalid
// input leaves Threshold=0 (the rule doesn't rotate), but the fact must be
// logged as a warning (see runDurationRule). Here we only check that invalid
// input doesn't crash the resolve and doesn't produce a garbage threshold.
func TestResolveCertRotatorConfig_InvalidThreshold(t *testing.T) {
	cfg := &config.KeeperConfig{
		Reaper: &config.KeeperReaper{
			Rules: map[string]config.ReaperRule{
				"rotate_due_certs": {
					Enabled:         true,
					RotateThreshold: "not-a-duration",
				},
			},
		},
	}

	out := resolveCertRotatorConfig(cfg, nil)
	if out.Threshold != 0 {
		t.Errorf("Threshold = %v on invalid input, want 0 (rule stays inert)", out.Threshold)
	}
}
