package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Host-side policy for interactive console (PTY) sessions — the `console:` block
// of soul.yml (docs/soul/console.md). Soul is the last word on whether an
// interactive shell may run on its own host, so these knobs must survive a
// round-trip and reject typos loudly.

// hasErrorDiag reports whether any diagnostic is an error (warnings are fine).
func hasErrorDiag(ds []diag.Diagnostic) bool {
	for _, d := range ds {
		if d.Level == diag.LevelError {
			return true
		}
	}
	return false
}

// soulBaseWithConsole assembles a minimally valid soul.yml with an arbitrary
// console: block body.
func soulBaseWithConsole(consoleBlock string) []byte {
	return []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
` + consoleBlock)
}

// An absent block means "consoles work, with the built-in defaults" — the feature
// is part of the product, not an opt-in.
func TestSoulConsole_OmittedBlockIsEnabled(t *testing.T) {
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", soulBaseWithConsole(""), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("a soul.yml without a console: block must be valid")
	}
	if cfg.Console != nil {
		t.Fatalf("omitted console: block must decode as nil, got %+v", cfg.Console)
	}
	if !cfg.Console.ConsoleEnabled() {
		t.Error("a nil console block must resolve to enabled")
	}
}

// A block that only tunes one knob must NOT silently disable the feature: that is
// exactly the footgun a plain bool `enabled` would create.
func TestSoulConsole_PartialBlockStaysEnabled(t *testing.T) {
	src := soulBaseWithConsole(`console:
  max_sessions: 1
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("console.max_sessions alone must be valid")
	}
	if !cfg.Console.ConsoleEnabled() {
		t.Error("omitting console.enabled must leave consoles enabled")
	}
	if cfg.Console.MaxSessions != 1 {
		t.Errorf("max_sessions = %d, want 1", cfg.Console.MaxSessions)
	}
}

// The operator switch the block exists for: forbid interactive shells outright.
func TestSoulConsole_ExplicitlyDisabled(t *testing.T) {
	src := soulBaseWithConsole(`console:
  enabled: false
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("console.enabled: false must be valid")
	}
	if cfg.Console.ConsoleEnabled() {
		t.Error("console.enabled: false must resolve to disabled")
	}
}

func TestSoulConsole_AllFieldsRoundTrip(t *testing.T) {
	src := soulBaseWithConsole(`console:
  enabled: true
  max_sessions: 4
  rate_limit_kbps: 512
  kill_grace: 5s
  shell: /bin/dash
`)
	cfg, _, diags, err := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadSoulFromBytes: %v", err)
	}
	if hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("a fully populated console: block must be valid")
	}
	c := cfg.Console
	if !c.ConsoleEnabled() || c.MaxSessions != 4 || c.RateLimitKBps != 512 || c.KillGrace != "5s" || c.Shell != "/bin/dash" {
		t.Errorf("round-trip mismatch: %+v", c)
	}
}

// A relative shell would be resolved through PATH, letting a shadowed binary
// become the console. Rejected at config load, not just at session open.
func TestSoulConsole_RelativeShellRejected(t *testing.T) {
	src := soulBaseWithConsole(`console:
  shell: bash
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCode(diags, "console_shell_not_absolute") {
		dump(t, diags)
		t.Fatal("a relative console.shell must be rejected")
	}
}

func TestSoulConsole_NegativeValuesRejected(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"max_sessions", "console:\n  max_sessions: -1\n"},
		{"rate_limit_kbps", "console:\n  rate_limit_kbps: -1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, diags, _ := LoadSoulFromBytes("soul.yml", soulBaseWithConsole(tc.block), ValidateOptions{})
			if !hasCode(diags, "value_out_of_range") {
				dump(t, diags)
				t.Fatalf("negative console.%s must be rejected", tc.name)
			}
		})
	}
}

func TestSoulConsole_BadKillGraceRejected(t *testing.T) {
	src := soulBaseWithConsole(`console:
  kill_grace: "2 seconds"
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasErrorDiag(diags) {
		dump(t, diags)
		t.Fatal("an unparseable console.kill_grace must be rejected")
	}
}
