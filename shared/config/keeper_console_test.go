package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// The `console:` block is operator policy for the interactive console plane
// (NIM-143, docs/keeper/console.md). These pin that it survives the parse +
// schema + semantic phases — a config that is silently dropped or, worse,
// rejected as unknown would take the whole daemon down on upgrade.

const consoleBaseConfig = `
kid: keeper-1
listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"
      tls:
        cert: /etc/keeper/tls/server.crt
        key: /etc/keeper/tls/server.key
    event_stream:
      addr: "0.0.0.0:9443"
      tls:
        cert: /etc/keeper/tls/server.crt
        key: /etc/keeper/tls/server.key
        ca: /etc/keeper/tls/ca.crt
  openapi: { addr: "0.0.0.0:8080" }
  mcp: { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
redis:
  addr: "127.0.0.1:6379"
vault:
  addr: "https://vault.internal:8200"
  auth: { method: approle, role_id: keeper-prod, secret_id_file: /etc/keeper/vault-secret-id }
logging:
  level: info
`

func loadKeeperOrFail(t *testing.T, yaml string) *KeeperConfig {
	t.Helper()
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", []byte(yaml), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v (diags: %v)", err, diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected error diagnostics: %+v", diags)
	}
	return cfg
}

func TestKeeperConsole_BlockIsParsed(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
console:
  max_sessions_per_archon: 12
  max_sessions_global: 99
  idle_timeout: 15m
`)
	if cfg.Console == nil {
		t.Fatal("console block was dropped by the parser")
	}
	if cfg.Console.MaxSessionsPerArchon != 12 {
		t.Fatalf("max_sessions_per_archon = %d, want 12", cfg.Console.MaxSessionsPerArchon)
	}
	if cfg.Console.MaxSessionsGlobal != 99 {
		t.Fatalf("max_sessions_global = %d, want 99", cfg.Console.MaxSessionsGlobal)
	}
	if cfg.Console.IdleTimeout != "15m" {
		t.Fatalf("idle_timeout = %q, want 15m", cfg.Console.IdleTimeout)
	}
}

// The block is optional — consoles work out of the box on the defaults.
func TestKeeperConsole_AbsentBlockIsFine(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig)
	if cfg.Console != nil {
		t.Fatalf("console = %+v, want nil when the block is absent", cfg.Console)
	}
}

// A partial block must keep the keys it does carry; the rest resolve to
// defaults downstream.
func TestKeeperConsole_PartialBlock(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
console:
  idle_timeout: 0s
`)
	if cfg.Console == nil {
		t.Fatal("console block was dropped")
	}
	if cfg.Console.IdleTimeout != "0s" {
		t.Fatalf("idle_timeout = %q, want 0s (an explicit disable)", cfg.Console.IdleTimeout)
	}
	if cfg.Console.MaxSessionsPerArchon != 0 {
		t.Fatalf("max_sessions_per_archon = %d, want 0 so the default applies", cfg.Console.MaxSessionsPerArchon)
	}
}

// --- recording (ADR-0074(g), NIM-145) ----------------------------------------

// The recording sub-block parses and carries what policy IS allowed to decide.
func TestKeeperConsoleRecording_BlockIsParsed(t *testing.T) {
	cfg := loadKeeperOrFail(t, consoleBaseConfig+`
console:
  recording:
    max_session_bytes: 1048576
    retention: 720h
`)
	if cfg.Console == nil || cfg.Console.Recording == nil {
		t.Fatal("console.recording block was dropped")
	}
	if cfg.Console.Recording.MaxSessionBytes != 1048576 {
		t.Fatalf("max_session_bytes = %d, want 1048576", cfg.Console.Recording.MaxSessionBytes)
	}
	if cfg.Console.Recording.Retention != "720h" {
		t.Fatalf("retention = %q, want 720h", cfg.Console.Recording.Retention)
	}
}

// The absence of an on/off switch is the point, so it is pinned rather than
// assumed. Recording is mandatory (ADR-0074(g)): policy may decide WHERE
// recordings go and HOW LONG they are kept, not WHETHER a session is recorded,
// because an operator who can choose an unrecorded shell makes the control
// decorative. Adding such a key is an ADR amendment, and this test is where
// that conversation starts.
func TestKeeperConsoleRecording_HasNoEnableSwitch(t *testing.T) {
	forbidden := []string{"enabled", "disabled", "enable", "disable", "off", "on", "optional", "mode"}

	rt := reflect.TypeOf(KeeperConsoleRecording{})
	for i := 0; i < rt.NumField(); i++ {
		tag := rt.Field(i).Tag.Get("yaml")
		key, _, _ := strings.Cut(tag, ",")
		for _, bad := range forbidden {
			if strings.EqualFold(key, bad) {
				t.Fatalf("console.recording.%s exists — recording is not configurable (ADR-0074(g)); "+
					"turning it into a switch needs an ADR amendment, not a config field", key)
			}
		}
	}
}

// A malformed retention is caught in the semantic phase, like every other
// duration in this config.
func TestKeeperConsoleRecording_RejectsAMalformedRetention(t *testing.T) {
	_, _, diags, err := LoadKeeperFromBytes("keeper.yml",
		[]byte(consoleBaseConfig+"\nconsole:\n  recording:\n    retention: \"forever\"\n"),
		ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadKeeperFromBytes: %v", err)
	}
	if !diag.HasErrors(diags) {
		t.Fatalf("a malformed retention was accepted: %+v", diags)
	}
}

// --- errand_shell_gate (ADR-0074 amendment, NIM-197) --------------------------

// The stage of the console gate over the Errand path. A closed enum: a typo must
// fail the load rather than resolve to the permissive stage behind the
// operator's back.

func TestKeeperConsole_ErrandShellGate_Accepted(t *testing.T) {
	for _, mode := range ErrandShellGateModes {
		cfg := loadKeeperOrFail(t, consoleBaseConfig+`
console:
  errand_shell_gate: `+mode+`
`)
		if cfg.Console == nil || cfg.Console.ErrandShellGate != mode {
			t.Fatalf("errand_shell_gate = %+v, want %q", cfg.Console, mode)
		}
	}
}

func TestKeeperConsole_ErrandShellGate_RejectsUnknownValue(t *testing.T) {
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(consoleBaseConfig+`
console:
  errand_shell_gate: off
`), ValidateOptions{})
	if !diag.HasErrors(diags) {
		t.Fatal("an unknown errand_shell_gate value was accepted; a security gate must not fall back to permissive")
	}
}
