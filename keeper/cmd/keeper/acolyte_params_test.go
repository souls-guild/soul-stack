package main

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Acolyte-param resolvers (ADR-027): empty/zero config -> defaults matching
// the previous hardcoded values; explicit values pass through.

func TestAcolyteLease_DefaultOnEmpty(t *testing.T) {
	if got := acolyteLease(&config.KeeperConfig{}); got != config.DefaultAcolyteLease {
		t.Fatalf("empty acolyte_lease = %v, want default %v", got, config.DefaultAcolyteLease)
	}
}

func TestAcolyteLease_ParsedValue(t *testing.T) {
	if got := acolyteLease(&config.KeeperConfig{AcolyteLease: "45s"}); got != 45*time.Second {
		t.Fatalf("acolyte_lease 45s = %v, want 45s", got)
	}
}

func TestAcolyteLease_DefaultOnGarbageOrNonPositive(t *testing.T) {
	for _, raw := range []string{"not-a-duration", "0s", "-5s"} {
		if got := acolyteLease(&config.KeeperConfig{AcolyteLease: raw}); got != config.DefaultAcolyteLease {
			t.Errorf("acolyte_lease %q = %v, want default %v", raw, got, config.DefaultAcolyteLease)
		}
	}
}

func TestAcolyteBatch_DefaultOnZero(t *testing.T) {
	if got := acolyteBatch(&config.KeeperConfig{}); got != config.DefaultAcolyteBatch {
		t.Fatalf("empty acolyte_batch = %d, want default %d", got, config.DefaultAcolyteBatch)
	}
	if got := acolyteBatch(&config.KeeperConfig{AcolyteBatch: -3}); got != config.DefaultAcolyteBatch {
		t.Fatalf("negative acolyte_batch = %d, want default %d", got, config.DefaultAcolyteBatch)
	}
}

func TestAcolyteBatch_ParsedValue(t *testing.T) {
	if got := acolyteBatch(&config.KeeperConfig{AcolyteBatch: 32}); got != 32 {
		t.Fatalf("acolyte_batch 32 = %d, want 32", got)
	}
}

func TestAcolytePollInterval_DefaultOnEmpty(t *testing.T) {
	if got := acolytePollInterval(&config.KeeperConfig{}); got != config.DefaultAcolytePollInterval {
		t.Fatalf("empty acolyte_poll_interval = %v, want default %v", got, config.DefaultAcolytePollInterval)
	}
}

func TestAcolytePollInterval_ParsedValue(t *testing.T) {
	if got := acolytePollInterval(&config.KeeperConfig{AcolytePollInterval: "500ms"}); got != 500*time.Millisecond {
		t.Fatalf("acolyte_poll_interval 500ms = %v, want 500ms", got)
	}
}

func TestAcolytePollInterval_DefaultOnGarbage(t *testing.T) {
	if got := acolytePollInterval(&config.KeeperConfig{AcolytePollInterval: "5x"}); got != config.DefaultAcolytePollInterval {
		t.Fatalf("garbage acolyte_poll_interval = %v, want default %v", got, config.DefaultAcolytePollInterval)
	}
}

func TestAcolyteDrainGrace_DefaultOnEmpty(t *testing.T) {
	if got := acolyteDrainGrace(&config.KeeperConfig{}); got != config.DefaultAcolyteDrainGrace {
		t.Fatalf("empty acolyte_drain_grace = %v, want default %v", got, config.DefaultAcolyteDrainGrace)
	}
}

func TestAcolyteDrainGrace_ParsedValue(t *testing.T) {
	if got := acolyteDrainGrace(&config.KeeperConfig{AcolyteDrainGrace: "12s"}); got != 12*time.Second {
		t.Fatalf("acolyte_drain_grace 12s = %v, want 12s", got)
	}
}

func TestAcolyteDrainGrace_DefaultOnGarbageOrNonPositive(t *testing.T) {
	for _, raw := range []string{"nope", "0s", "-1s"} {
		if got := acolyteDrainGrace(&config.KeeperConfig{AcolyteDrainGrace: raw}); got != config.DefaultAcolyteDrainGrace {
			t.Errorf("acolyte_drain_grace %q = %v, want default %v", raw, got, config.DefaultAcolyteDrainGrace)
		}
	}
}
