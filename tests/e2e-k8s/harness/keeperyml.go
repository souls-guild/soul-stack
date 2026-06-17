//go:build e2e_k8s

package harness

import "fmt"

// keeperyml.go — рендер keeper.yml для k8s-deployment-а в L3c. Симметрично
// tests/e2e-live/harness/config_builder.go::buildKeeperYAML, но:
//   - listener-ы биндят на `0.0.0.0:<port>` (внутри pod-а нет other-network);
//   - адреса PG/Redis/Vault — in-cluster service-DNS, не testcontainers
//     mapped-port-ы;
//   - TLS-пути — `/etc/keeper-tls/keeper.crt|.key|/vault-ca.crt` (Secret mount);
//   - reaper.enabled=false (минимизация side-effects для L3c-2 smoke).
//
// Формат и грамматика keeper.yml — `shared/config/keeper.go` (KeeperConfig).
// Не используется templating-движок: значения подставляются `fmt.Sprintf`,
// CEL/Go-template здесь были бы over-engineering (как в L3b config_builder.go).

// keeperYAMLInputs — динамические поля рендера. Все остальные настройки
// (audit/logging/plugin_runtime) фиксированы константами в шаблоне.
//
// KID в шаблоне — литерал `__KID__`, который init-container `kid-render`
// (см. manifests/keeper/deployment.yaml) подставляет на pod-name. Этот
// шаблонный механизм позволяет multi-replica Deployment-у разворачивать
// 3 keeper-pod с разными KID из одного ConfigMap.
type keeperYAMLInputs struct {
	VaultAddr           string // `http://vault.default.svc.cluster.local:8200`
	VaultToken          string // dev-mode root-token
	RedisAddr           string // `redis-master.default.svc.cluster.local:6379` (host:port без схемы)
	OpenAPIPort         int
	MCPPort             int
	MetricsPort         int
	BootstrapGRPCPort   int
	EventStreamGRPCPort int

	// ReaperEnabled — true для тестов, которым нужен live leader-election
	// (L3c-4 failover). Дефолт false: для L3c-2/3 reaper выключен ради
	// минимизации side-effects.
	//
	// При ReaperEnabled=true в конфиг добавляется блок `reaper:` с короткими
	// interval/lock_ttl (15s/15s) — failover-тест должен видеть выбор нового
	// лидера за разумное время (< 60s). Правила оставлены дефолтными.
	ReaperEnabled bool
}

// renderKeeperYAML возвращает строку keeper.yml для записи в ConfigMap.
//
// TLS-сертификаты ожидаются по фиксированным путям в pod-е:
//
//	/etc/keeper-tls/keeper.crt
//	/etc/keeper-tls/keeper.key
//	/etc/keeper-tls/vault-ca.crt
//
// — то же что mount-ит manifests/keeper/deployment.yaml::volumes.keeper-tls.
//
// `acolytes: 2` — same value как dev/keeper.dev.yml; стандарт multi-keeper.
//
// `allow_unsafe_single_path_multi_keeper: false` — L3c-3 multi-replica, флаг
// «один путь к Vault на много keeper-ов» противоречит HA-режиму; конфигурация
// читает Vault через kubernetes-DNS, путь общий → флаг отключён.
//
// `reaper.enabled: false` — для L3c smoke не нужны background job-ы.
// L3c-4 failover включает reaper через in.ReaperEnabled.
func renderKeeperYAML(in keeperYAMLInputs) string {
	const tmpl = `kid: __KID__

listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:%d"
      tls:
        cert: /etc/keeper-tls/keeper.crt
        key:  /etc/keeper-tls/keeper.key
    event_stream:
      addr: "0.0.0.0:%d"
      tls:
        cert: /etc/keeper-tls/keeper.crt
        key:  /etc/keeper-tls/keeper.key
        ca:   /etc/keeper-tls/vault-ca.crt
  openapi: { addr: "0.0.0.0:%d" }
  mcp:     { addr: "0.0.0.0:%d" }
  metrics: { addr: "0.0.0.0:%d" }

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
    issuer: keeper-l3c
    ttl_default: 24h
    ttl_bootstrap: 720h

logging:
  level: info
  format: text
  rotation: { max_size_mb: 100, max_files: 5, compress: false }

plugins:
  cache_root: /var/lib/soul-stack/plugins

plugin_runtime:
  socket_dir: /var/lib/soul-stack/plugin-sockets
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
allow_unsafe_single_path_multi_keeper: false

acolytes: 2
%s`
	return fmt.Sprintf(tmpl,
		in.BootstrapGRPCPort,
		in.EventStreamGRPCPort,
		in.OpenAPIPort,
		in.MCPPort,
		in.MetricsPort,
		in.RedisAddr,
		in.VaultAddr,
		in.VaultToken,
		reaperBlock(in.ReaperEnabled),
	)
}

// reaperBlock возвращает YAML-блок `reaper:` под флагом ReaperEnabled.
//
// Disabled (default L3c-2/3): один key `enabled: false` — без правил, без
// leader-election, без background SQL.
//
// Enabled (L3c-4 failover): короткий lock_ttl=15s — после kill-leader-pod
// оставшиеся 2 keeper увидят истёкший lease максимум за TTL и переберут
// leadership. interval=15s минимизирует idle-окно между tick-ами; правила
// оставлены дефолтными (см. defaults в keeper/internal/reaper/runner.go).
//
// Этот блок выделен отдельной функцией, потому что валидный YAML с минимум
// `enabled: true` требует хотя бы пары полей; смешивать в одной строке
// fmt.Sprintf трудно читать.
func reaperBlock(enabled bool) string {
	if !enabled {
		return `
reaper:
  enabled: false
`
	}
	return `
reaper:
  enabled: true
  interval: 15s
  lock_ttl: 15s
  dry_run: false
  batch_size: 500
  rules:
    purge_audit_old: { enabled: true, max_age: 365d, action: delete }
`
}
