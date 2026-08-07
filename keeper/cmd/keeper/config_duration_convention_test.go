package main

import (
	"testing"
	"time"
)

// The `duration` convention (docs/keeper/config.md -> "Type conventions") is Go
// duration plus a `<N>d` suffix for days, and the semantic phase validates every
// field below with it (shared/config/semantic.go -> checkDuration). The tests
// here pin the other half of that contract: the code that CONSUMES the field
// must read it with the same parser. When a consumer used stdlib
// time.ParseDuration instead, `30d` passed validation and then either failed the
// command outright (auth.jwt.*) or collapsed to a default with no error at all
// (console.*) — NIM-419.

// TestShippedKeeperExample_TTLBootstrapResolves is the guard for the reported
// bug: `keeper init` on the reference config we ship. The fixture helper already
// fails the test on any error diagnostic, so this asserts the whole chain —
// examples/keeper/keeper.yml parses, validates, AND resolves to the value it
// literally spells.
func TestShippedKeeperExample_TTLBootstrapResolves(t *testing.T) {
	t.Parallel()
	store, _ := keeperFixtureStore(t)
	cfg := store.Get()
	if cfg == nil || cfg.Auth == nil || cfg.Auth.JWT == nil {
		t.Fatal("shipped example has no auth.jwt block")
	}

	got, err := parseTTL(cfg.Auth.JWT.TTLBootstrap, "auth.jwt.ttl_bootstrap", 720*time.Hour)
	if err != nil {
		t.Fatalf("parseTTL on the shipped example: %v\n"+
			"examples/keeper/keeper.yml must bootstrap with `keeper init`", err)
	}
	if got != 720*time.Hour {
		t.Errorf("ttl_bootstrap = %v, want 720h", got)
	}
}

// TestShippedKeeperExample_TTLDefaultResolves covers the second parseTTL field.
// It is read by `keeper run`, so a narrow parser here stops the daemon from
// starting rather than stopping one subcommand.
func TestShippedKeeperExample_TTLDefaultResolves(t *testing.T) {
	t.Parallel()
	store, _ := keeperFixtureStore(t)
	cfg := store.Get()
	if cfg == nil || cfg.Auth == nil || cfg.Auth.JWT == nil {
		t.Fatal("shipped example has no auth.jwt block")
	}
	if _, err := parseTTL(cfg.Auth.JWT.TTLDefault, "auth.jwt.ttl_default", 24*time.Hour); err != nil {
		t.Fatalf("parseTTL(ttl_default) on the shipped example: %v", err)
	}
}

// TestConsoleDurations_AcceptDayForm is the silent half of NIM-419. Neither
// resolver can report an error: an unparsable value becomes 0, which the caller
// reads as "take the package default". So a `retention: 30d` that the config
// phase accepted was answered with the 90d default, and nothing said so.
func TestConsoleDurations_AcceptDayForm(t *testing.T) {
	t.Parallel()
	store, _ := keeperFixtureStoreWith(t, `
console:
  idle_timeout: 1d
  recording:
    retention: 30d
`)
	cfg := store.Get()
	if cfg == nil || cfg.Console == nil || cfg.Console.Recording == nil {
		t.Fatal("console block did not load")
	}

	if got := consoleRecordingRetention(cfg); got != 30*24*time.Hour {
		t.Errorf("consoleRecordingRetention(30d) = %v, want 720h; "+
			"0 means the value was dropped and the store default applies", got)
	}
	if got := parseConsoleIdleTimeout(cfg.Console.IdleTimeout); got != 24*time.Hour {
		t.Errorf("parseConsoleIdleTimeout(1d) = %v, want 24h; "+
			"0 means the value was dropped and the package default applies", got)
	}
}

// TestConsoleDurations_HourFormStillWorks — config.ParseDuration delegates to
// the stdlib parser when there is no `d` suffix, so the existing spellings are
// untouched.
func TestConsoleDurations_HourFormStillWorks(t *testing.T) {
	t.Parallel()
	store, _ := keeperFixtureStoreWith(t, `
console:
  idle_timeout: 30m
  recording:
    retention: 2160h
`)
	cfg := store.Get()
	if got := consoleRecordingRetention(cfg); got != 2160*time.Hour {
		t.Errorf("retention(2160h) = %v, want 2160h", got)
	}
	if got := parseConsoleIdleTimeout(cfg.Console.IdleTimeout); got != 30*time.Minute {
		t.Errorf("idle_timeout(30m) = %v, want 30m", got)
	}
}
