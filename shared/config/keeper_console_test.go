package config

import (
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
