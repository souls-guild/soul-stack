package config

import (
	"testing"
	"time"
)

// TestResolvedMaxAwaitTimeout — resolves the await_timeout cap (ADR-061):
// empty/invalid → default; a valid value → parsed.
func TestResolvedMaxAwaitTimeout(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"empty default", "", DefaultMaxAwaitTimeout},
		{"explicit 10m", "10m", 10 * time.Minute},
		{"days suffix", "1d", 24 * time.Hour},
		{"invalid falls back", "garbage", DefaultMaxAwaitTimeout},
		{"zero falls back", "0s", DefaultMaxAwaitTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &KeeperConfig{MaxAwaitTimeout: tc.raw}
			if got := c.ResolvedMaxAwaitTimeout(); got != tc.want {
				t.Fatalf("ResolvedMaxAwaitTimeout(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
	// nil receiver — default (must not panic).
	var nilCfg *KeeperConfig
	if got := nilCfg.ResolvedMaxAwaitTimeout(); got != DefaultMaxAwaitTimeout {
		t.Fatalf("nil receiver = %v, want default", got)
	}
}

// TestKeeperSemantic_MaxAwaitTimeoutInvalid — an invalid duration in
// keeper.yml::max_await_timeout is caught by the semantic phase (ADR-061).
func TestKeeperSemantic_MaxAwaitTimeoutInvalid(t *testing.T) {
	src := keeperBaseRequired + "max_await_timeout: not-a-duration\n"
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.max_await_timeout") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for malformed max_await_timeout")
	}
}

// TestKeeperSemantic_MaxAwaitTimeoutValid_OK — a valid duration passes.
func TestKeeperSemantic_MaxAwaitTimeoutValid_OK(t *testing.T) {
	src := keeperBaseRequired + "max_await_timeout: 1h\n"
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("valid max_await_timeout must not trigger duration_invalid")
	}
}
