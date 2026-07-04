//go:build e2e_live

package harness

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// buildKeeperYAML рендерит keeper.yml для test-сценария: подставляет адреса
// testcontainer-ов (PG/Redis/Vault), пути к TLS-материалу и канонический
// signing_key_ref.
//
// Не используется templating-движок — keeper.yml фиксирован по форме (см.
// dev/keeper.dev.yml), все динамические значения — fmt.Sprintf. CEL/Go-template
// здесь были бы over-engineering.
//
// Side effect: после рендера YAML caller обязан запушить PG DSN в Vault
// (`secret/keeper/postgres`, поле `dsn`). Делаем это здесь же — buildKeeperYAML
// логически фиксирует контракт `dsn_ref: vault:secret/keeper/postgres`, и
// держать соответствующий secret-write в этой же точке проще, чем разводить.
//
// L3b-отличие от L3a: gRPC-listener-ы (bootstrap + event_stream) биндятся на
// `0.0.0.0:<port>`, чтобы соул-контейнер мог достучаться к keeper-у через
// `host.docker.internal:<port>` (на Linux — через ExtraHosts host-gateway).
// HTTP/MCP/metrics остаются на 127.0.0.1 — к ним из контейнера не ходим, а
// держать всё на 0.0.0.0 без auth — лишняя поверхность.
func (s *Stack) buildKeeperYAML(certPath, keyPath, caPath string) string {
	// Динамические TCP-порты: выделяем заранее, чтобы Stack-поля и YAML
	// согласованно указывали на одни и те же номера. probeReady и
	// host-side-asserts ходят на 127.0.0.1:<port> (loopback), а
	// keeper-yml слушает на 0.0.0.0:<port> для gRPC-listener-ов.
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

// buildPluginsSection рендерит блок `plugins:` (+ опциональный `sigil:`).
// Непустой cfg.SoulModules добавляет каталог `soul_modules[]` (ADR-065(b),
// flow-форма записей как в docs/keeper/plugins.md) и включает Sigil:
// без sigil.signing_key_ref keeper не регистрирует plugin.allow/revoke/list
// (setupSigil), и допускать материализованный слот было бы нечем. Ключ в
// Vault сеет NewStack (SeedSigilSigningKey) ДО keeper run.
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

// seedPostgresDSN кладёт PG DSN в Vault KV `secret/keeper/postgres` (поле `dsn`).
// keeper.yml::postgres.dsn_ref ссылается именно сюда.
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

// allocLoopback резервирует свободный TCP-порт на 127.0.0.1 и возвращает
// `127.0.0.1:<port>`. Listener закрывается сразу — есть малый race (порт
// может быть занят между alloc и фактическим bind), для test-окружения
// приемлемо.
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

// allocPort резервирует свободный TCP-порт (через bind на 127.0.0.1:0) и
// возвращает только номер порта. Используется для listener-ов, которые
// YAML биндит на `0.0.0.0:<port>` (gRPC bootstrap+event_stream — доступны и
// host-side-probe-у, и контейнеру через host.docker.internal).
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
