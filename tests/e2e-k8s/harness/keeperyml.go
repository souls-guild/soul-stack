//go:build e2e_k8s

package harness

import "fmt"

// keeperyml.go — renders keeper.yml for the k8s deployment in L3c. Symmetric
// to tests/e2e-live/harness/config_builder.go::buildKeeperYAML, but:
//   - listeners bind to `0.0.0.0:<port>` (no other network inside the pod);
//   - PG/Redis/Vault addresses are in-cluster service DNS, not testcontainers
//     mapped ports;
//   - TLS paths are `/etc/keeper-tls/keeper.crt|.key|/vault-ca.crt` (Secret mount);
//   - reaper.enabled=false (minimizes side effects for the L3c-2 smoke test).
//
// keeper.yml format and grammar — `shared/config/keeper.go` (KeeperConfig).
// No templating engine is used: values are substituted via `fmt.Sprintf`;
// CEL/Go-template would be over-engineering here (same as in L3b
// config_builder.go).

// keeperYAMLInputs — the dynamic render fields. All other settings
// (audit/logging/plugin_runtime) are fixed constants in the template.
//
// KID in the template is the literal `__KID__`, which the init container
// `kid-render` (see manifests/keeper/deployment.yaml) substitutes with the
// pod name. This templating mechanism lets a multi-replica Deployment run
// 3 keeper pods with different KIDs from a single ConfigMap.
type keeperYAMLInputs struct {
	VaultAddr           string // `http://vault.default.svc.cluster.local:8200`
	VaultToken          string // dev-mode root-token
	RedisAddr           string // `redis-master.default.svc.cluster.local:6379` (host:port, no scheme)
	OpenAPIPort         int
	MCPPort             int
	MetricsPort         int
	BootstrapGRPCPort   int
	EventStreamGRPCPort int

	// ReaperEnabled — true for tests that need live leader election
	// (L3c-4 failover). Default false: for L3c-2/3 the reaper is disabled
	// to minimize side effects.
	//
	// When ReaperEnabled=true, a `reaper:` block is added to the config with
	// short interval/lock_ttl (15s/15s) -- the failover test must observe a
	// new leader elected within a reasonable time (< 60s). Rules are left
	// at their defaults.
	ReaperEnabled bool
}

// renderKeeperYAML returns the keeper.yml string to write into the
// ConfigMap.
//
// TLS certificates are expected at fixed paths in the pod:
//
//	/etc/keeper-tls/keeper.crt
//	/etc/keeper-tls/keeper.key
//	/etc/keeper-tls/vault-ca.crt
//
// -- the same paths mounted by manifests/keeper/deployment.yaml::volumes.keeper-tls.
//
// `acolytes: 2` — same value as dev/keeper.dev.yml; the multi-keeper standard.
//
// `allow_unsafe_single_path_multi_keeper: false` — L3c-3 is multi-replica,
// and the "single Vault path shared by many keepers" flag conflicts with HA
// mode; the config reads Vault via kubernetes DNS, path shared -> flag off.
//
// `reaper.enabled: false` — background jobs aren't needed for the L3c smoke
// test. L3c-4 failover enables the reaper via in.ReaperEnabled.
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

// reaperBlock returns the YAML `reaper:` block gated by the ReaperEnabled
// flag.
//
// Disabled (default L3c-2/3): a single `enabled: false` key -- no rules, no
// leader election, no background SQL.
//
// Enabled (L3c-4 failover): short lock_ttl=15s -- after kill-leader-pod the
// remaining 2 keepers will see the expired lease within at most the TTL and
// take over leadership. interval=15s minimizes the idle window between
// ticks; rules are left at their defaults (see the defaults in
// keeper/internal/reaper/runner.go).
//
// This block is factored into its own function because valid YAML with a
// minimal `enabled: true` needs at least a couple of fields; mixing it into
// a single fmt.Sprintf string would be hard to read.
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
