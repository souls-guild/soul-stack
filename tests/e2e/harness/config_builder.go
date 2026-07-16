//go:build e2e

package harness

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// buildKeeperYAML renders keeper.yml for the test scenario: substitutes
// testcontainer addresses (PG/Redis/Vault), TLS material paths and the
// canonical signing_key_ref.
//
// No templating engine is used — keeper.yml has a fixed shape (see
// dev/keeper.dev.yml), all dynamic values go through fmt.Sprintf. CEL/Go
// templates here would be over-engineering.
//
// Side effect: after rendering the YAML the caller must push the PG DSN to
// Vault (`secret/keeper/postgres`, field `dsn`). We do it right here —
// buildKeeperYAML logically pins the `dsn_ref: vault:secret/keeper/postgres`
// contract, and keeping the matching secret write at the same point is
// simpler than splitting it out.
func (s *Stack) buildKeeperYAML(certPath, keyPath, caPath string) string {
	// Dynamic listener addresses: reserve free TCP ports up front so Stack
	// fields and the YAML consistently point at the same port numbers
	// (probeReady hits KeeperHTTPURL).
	bootstrapAddr := allocLoopback(s.t)
	eventStreamAddr := allocLoopback(s.t)
	httpAddr := allocLoopback(s.t)
	mcpAddr := allocLoopback(s.t)
	metricsAddr := allocLoopback(s.t)

	s.KeeperBootstrapGRPC = bootstrapAddr
	s.KeeperGRPCAddr = eventStreamAddr
	s.KeeperHTTPURL = "http://" + httpAddr
	s.MetricsURL = "http://" + metricsAddr

	pluginsCacheDir := s.tmpDir + "/plugins"
	socketsDir := s.tmpDir + "/plugin-sockets"

	tmpl := `kid: keeper-test-01

listen:
  grpc:
    bootstrap:
      addr: "%s"
      tls:
        cert: %s
        key:  %s
    event_stream:
      addr: "%s"
      tls:
        cert: %s
        key:  %s
        ca:   %s
  openapi: { addr: "%s" }
  mcp:     { addr: "%s" }
  metrics: { addr: "%s" }

postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 2, max: 5 }

redis:
  addr: "%s"
  password_ref: ""

vault:
  addr: "%s"
  token: "%s"
  auth:
    method: token
  pki_mount: "pki"
  pki_role: "soul-seed"

auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-test-01
    ttl_default: 24h
    ttl_bootstrap: 720h

logging:
  level: info
  format: text
  rotation: { max_size_mb: 100, max_files: 5, compress: false }

plugins:
  cache_root: %s

plugin_runtime:
  socket_dir: %s
  startup_timeout: 10s
  shutdown_grace: 10s
  allowed_capabilities:
    - run_as_root
    - network_outbound
    - network_inbound
    - vault_access
    - fs_write_root
    - exec_subprocess
  conflict_policy: warn
  enable_tls: false

hot_reload:
  enable_signal: false
  enable_inotify: false
  audit_correlation_id: true

audit:
  enabled: true
  otel_export: false
  retention_days: 365

watchman_interval: 5s
watchman_fail_threshold: 3
allow_unsafe_single_path_multi_keeper: true

acolytes: 2

reaper:
  enabled: false
`
	yaml := fmt.Sprintf(tmpl,
		bootstrapAddr, certPath, keyPath,
		eventStreamAddr, certPath, keyPath, caPath,
		httpAddr, mcpAddr, metricsAddr,
		s.RedisAddr,
		s.VaultAddr, s.vaultToken,
		pluginsCacheDir, socketsDir,
	)

	s.seedPostgresDSN()
	return yaml
}

// seedPostgresDSN writes the PG DSN into Vault KV `secret/keeper/postgres`
// (field `dsn`). keeper.yml::postgres.dsn_ref points right here.
func (s *Stack) seedPostgresDSN() {
	vc := newVaultClient(s.VaultAddr, s.vaultToken)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := vc.write(ctx, "secret/data/keeper/postgres", map[string]any{
		"data": map[string]any{"dsn": s.PGURL},
	}); err != nil {
		s.t.Fatalf("seedPostgresDSN: %v", err)
	}
}

// allocLoopback reserves a free TCP port on 127.0.0.1 and returns
// `127.0.0.1:<port>`. The listener is closed immediately — there's a small
// race (the port may be taken between alloc and the actual bind), which is
// acceptable for a test environment.
func allocLoopback(t interface {
	Fatalf(format string, args ...any)
}) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocLoopback: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()
	if !strings.Contains(addr, ":") {
		t.Fatalf("allocLoopback: unexpected addr %q", addr)
	}
	return addr
}
