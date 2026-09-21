//go:build e2e_live

package harness

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// buildKeeperYAML renders keeper.yml for the test scenario: substitutes
// testcontainer addresses (PG/Redis/Vault), TLS material paths, and the
// canonical signing_key_ref.
//
// No templating engine is used — keeper.yml is fixed in shape (see
// dev/keeper.dev.yml), all dynamic values go through fmt.Sprintf. CEL/Go
// text/template would be over-engineering here.
//
// Side effect: after rendering the YAML the caller must push the PG DSN into
// Vault (`secret/keeper/postgres`, field `dsn`). We do this right here —
// buildKeeperYAML logically fixes the `dsn_ref: vault:secret/keeper/postgres`
// contract, and it's simpler to keep the matching secret write at the same
// point than to split it out.
//
// L3b difference from L3a: the gRPC listeners (bootstrap + event_stream)
// bind on `0.0.0.0:<port>` so the soul container can reach keeper via
// `host.docker.internal:<port>` (on Linux — via the ExtraHosts host-gateway).
// HTTP/MCP/metrics stay on 127.0.0.1 — we don't reach them from the
// container, and binding everything on 0.0.0.0 without auth would be extra
// surface.
func (s *Stack) buildKeeperYAML(certPath, keyPath, caPath string) string {
	// Dynamic TCP ports: allocate up front so Stack fields and YAML
	// consistently point at the same numbers. probeReady and host-side asserts
	// hit 127.0.0.1:<port> (loopback), while keeper.yml listens on
	// 0.0.0.0:<port> for the gRPC listeners.
	bootstrapPort := allocPort(s.t)
	eventStreamPort := allocPort(s.t)
	httpAddr := allocLoopback(s.t)
	mcpAddr := allocLoopback(s.t)
	metricsAddr := allocLoopback(s.t)

	bootstrapListen := fmt.Sprintf("0.0.0.0:%d", bootstrapPort)
	eventStreamListen := fmt.Sprintf("0.0.0.0:%d", eventStreamPort)

	s.bootstrapPort = bootstrapPort
	s.eventStreamPort = eventStreamPort
	s.KeeperBootstrapGRPC = fmt.Sprintf("127.0.0.1:%d", bootstrapPort)
	s.KeeperGRPCAddr = fmt.Sprintf("127.0.0.1:%d", eventStreamPort)
	s.KeeperHTTPURL = "http://" + httpAddr
	s.MetricsURL = "http://" + metricsAddr

	pluginsCacheDir := s.tmpDir + "/plugins"
	socketsDir := s.tmpDir + "/plugin-sockets"
	s.PluginCacheRoot = pluginsCacheDir

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

%s
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
		bootstrapListen, certPath, keyPath,
		eventStreamListen, certPath, keyPath, caPath,
		httpAddr, mcpAddr, metricsAddr,
		s.RedisAddr,
		s.VaultAddr, s.vaultToken,
		s.buildPluginsSection(pluginsCacheDir), socketsDir,
	)

	s.seedPostgresDSN()
	return yaml
}

// buildPluginsSection renders the `plugins:` block (+ optional `sigil:`).
// A non-empty cfg.SoulModules adds a `soul_modules[]` catalog (ADR-065(b),
// flow-form entries as in docs/keeper/plugins.md) and enables Sigil: without
// sigil.signing_key_ref keeper does not register plugin.allow/revoke/list
// (setupSigil), and there would be nothing to admit a materialized slot
// with. The Vault key is seeded by NewStack (SeedSigilSigningKey) BEFORE the
// keeper run.
func (s *Stack) buildPluginsSection(cacheDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plugins:\n  cache_root: %s\n", cacheDir)
	if len(s.cfg.SoulModules) == 0 {
		return b.String()
	}
	b.WriteString("  soul_modules:\n")
	for _, m := range s.cfg.SoulModules {
		fmt.Fprintf(&b, "    - { name: %s, source: %q, ref: %q }\n", m.Name, m.Source, m.Ref)
	}
	b.WriteString("\nsigil:\n  signing_key_ref: " + sigilSigningKeyRef + "\n")
	return b.String()
}

// seedPostgresDSN writes the PG DSN into Vault KV `secret/keeper/postgres`
// (field `dsn`). keeper.yml::postgres.dsn_ref points exactly here.
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
// race (the port could get taken between alloc and the actual bind), which
// is acceptable for a test environment.
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

// allocPort reserves a free TCP port (via bind on 127.0.0.1:0) and returns
// just the port number. Used for listeners that the YAML binds on
// `0.0.0.0:<port>` (gRPC bootstrap+event_stream — reachable both by the
// host-side probe and by the container via host.docker.internal).
func allocPort(t interface {
	Fatalf(format string, args ...any)
}) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocPort: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if port == 0 {
		t.Fatalf("allocPort: unexpected port 0")
	}
	return port
}
