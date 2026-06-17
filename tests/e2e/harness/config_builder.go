//go:build e2e

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
func (s *Stack) buildKeeperYAML(certPath, keyPath, caPath string) string {
	// Динамические адреса listener-ов: выделяем свободные TCP-порты заранее,
	// чтобы Stack-поля и YAML согласованно указывали на одни и те же номера
	// (probeReady ходит на KeeperHTTPURL).
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
