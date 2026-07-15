package main

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestFailbackInterval_NoSpray(t *testing.T) {
	t.Parallel()
	got := failbackInterval(time.Hour, 0)
	if got != time.Hour {
		t.Errorf("failbackInterval(1h, 0) = %s, want 1h", got)
	}
}

func TestFailbackInterval_BoundedRange(t *testing.T) {
	t.Parallel()
	// 100 iterations must stay within [interval/2, interval+spray]. spray
	// doesn't stretch the interval, but the lower clamp is interval/2
	// (guards against a negative value when interval ≤ spray).
	interval := 100 * time.Millisecond
	spray := 30 * time.Millisecond
	for i := 0; i < 100; i++ {
		got := failbackInterval(interval, spray)
		if got < interval/2 || got > interval+spray {
			t.Fatalf("iter %d: got %s, want in [%s, %s]", i, got, interval/2, interval+spray)
		}
	}
}

func TestLoadFailback_Defaults(t *testing.T) {
	t.Parallel()
	fb, err := loadFailback(&config.SoulConfig{})
	if err != nil {
		t.Fatalf("loadFailback: %v", err)
	}
	if !fb.enabled {
		t.Errorf("default enabled = false, want true")
	}
	if fb.interval != time.Hour {
		t.Errorf("default interval = %s, want 1h", fb.interval)
	}
	if fb.spray != 10*time.Minute {
		t.Errorf("default spray = %s, want 10m", fb.spray)
	}
}

func TestLoadFailback_FromConfig(t *testing.T) {
	t.Parallel()
	cfg := &config.SoulConfig{
		Keeper: config.SoulKeeper{
			Failback: &config.SoulKeeperFailback{
				Enabled:  true,
				Interval: "30m",
				Spray:    "5m",
			},
		},
	}
	fb, err := loadFailback(cfg)
	if err != nil {
		t.Fatalf("loadFailback: %v", err)
	}
	if !fb.enabled || fb.interval != 30*time.Minute || fb.spray != 5*time.Minute {
		t.Errorf("loadFailback = %+v", fb)
	}
}

func TestLoadFailback_InvalidInterval(t *testing.T) {
	t.Parallel()
	cfg := &config.SoulConfig{
		Keeper: config.SoulKeeper{
			Failback: &config.SoulKeeperFailback{Interval: "bogus"},
		},
	}
	if _, err := loadFailback(cfg); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

// priority=0 (omitted in YAML) is treated as the default highest priority
// (1), not as "higher than one". orderedByPriority normalizes 0→1, so an
// endpoint with no explicit priority joins the same group as priority:1 —
// it neither breaks nor jumps ahead of priority:1 hosts.
func TestOrderedByPriority_ZeroNormalized(t *testing.T) {
	t.Parallel()
	in := []config.SoulKeeperEndpoint{
		{Host: "p2", Priority: 2},
		{Host: "p0", Priority: 0}, // omitted → normalized to 1
		{Host: "p1", Priority: 1},
	}
	out := orderedByPriority(in)
	// p0 (normalized 1) and p1 (1) come before p2 (2); SliceStable preserves
	// the original relative order of p0/p1 (p0 declared before p1).
	gotHosts := []string{out[0].Host, out[1].Host, out[2].Host}
	want := []string{"p0", "p1", "p2"}
	for i := range want {
		if gotHosts[i] != want[i] {
			t.Fatalf("orderedByPriority order = %v, want %v", gotHosts, want)
		}
	}
	// Doesn't mutate the input slice.
	if in[1].Host != "p0" || in[1].Priority != 0 {
		t.Errorf("input mutated: %+v", in)
	}
}
