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
	bootstrapAddr := s.reserveLoopback()
	eventStreamAddr := s.reserveLoopback()
	httpAddr := s.reserveLoopback()
	mcpAddr := s.reserveLoopback()
	metricsAddr := s.reserveLoopback()

	s.KeeperBootstrapGRPC = bootstrapAddr
	s.KeeperGRPCAddr = eventStreamAddr
	s.KeeperHTTPURL = "http://" + httpAddr
	s.MetricsURL = "http://" + metricsAddr

	s.assignIdentity("keeper-test", httpAddr)

	pluginsCacheDir := s.tmpDir + "/plugins"
	socketsDir := s.tmpDir + "/plugin-sockets"

	tmpl := `kid: %s

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
    issuer: %s
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
		s.issuer,
		bootstrapAddr, certPath, keyPath,
		eventStreamAddr, certPath, keyPath, caPath,
		httpAddr, mcpAddr, metricsAddr,
		s.RedisAddr,
		s.VaultAddr, s.vaultToken,
		s.issuer,
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
// `127.0.0.1:<port>` together with the listener still holding it. The caller
// owns the listener and must close it immediately before the real bind — see
// Stack.releasePortReservations.
//
// It used to close the listener at once and return only the address, with a
// comment calling the resulting race small and acceptable (NIM-469). It is
// neither. The gap between allocation and the keeper's actual bind spans a whole
// `keeper init` — schema migrations against a cold Postgres container, seconds
// on a loaded box — and 127.0.0.1's ephemeral range is exactly where every
// outgoing connection on the host also draws from: the pgx pool, the Vault and
// Redis clients, testcontainers' own traffic, and any neighbouring suite. Losing
// that race is not benign in either direction:
//
//   - the keeper fails to bind and the test dies on the /readyz deadline with
//     nothing about a port in the message;
//   - or something else is already listening there, and probeReady's 2xx check
//     accepts it, after which the test drives a foreign process for its whole
//     duration.
//
// Holding the listener until the last instant does not make the race
// theoretically impossible — only the kernel could, by binding the fd the keeper
// will use — but it collapses the window from seconds to milliseconds.
func allocLoopback(t interface {
	Fatalf(format string, args ...any)
}) (string, net.Listener) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocLoopback: %v", err)
	}
	addr := l.Addr().String()
	if !strings.Contains(addr, ":") {
		_ = l.Close()
		t.Fatalf("allocLoopback: unexpected addr %q", addr)
	}
	return addr, l
}

// releasePortReservations closes every held listener. Idempotent: called
// immediately before the keeper binds, and again from cleanup for the stacks
// that never got that far.
func (s *Stack) releasePortReservations() {
	for _, l := range s.portReservations {
		_ = l.Close()
	}
	s.portReservations = nil
}

// reserveLoopback allocates a port and records the reservation on the stack.
func (s *Stack) reserveLoopback() string {
	addr, l := allocLoopback(s.t)
	s.portReservations = append(s.portReservations, l)
	return addr
}

// assignIdentity derives this stack's JWT identity — the `kid` it signs with
// and the `iss` its keeper pins — and records it on the stack.
//
// One identity per stack, not one identity for the whole suite (NIM-469):
// `kid` and `iss` were both the literal `keeper-test-01` everywhere, so nothing
// in a keeper's config, and nothing in a failure report, said WHICH of the forty
// stacks in a run it belonged to. That is what this buys — attribution.
//
// What it does NOT buy is a different rejection, and the first version of this
// comment claimed it did. Each stack writes its own randomly generated signing
// key into its own Vault (vault.go, generateHS256Key), and Verify checks the
// signature INSIDE ParseWithClaims, before it ever compares `iss`
// (keeper/internal/jwt/verifier.go). A token that reaches another stack's keeper
// therefore fails on the signature and collapses to the generic
// `{"detail":"invalid token"}` — the same 401 an actual auth regression
// produces. `token issuer not trusted` is UNREACHABLE between two stacks of this
// harness and must not be relied on to tell them apart. assertOwnKeeper
// (probe.go) is what detects a wrong endpoint, and it needs no help from the
// issuer: any 401 at all, against a token this stack's own keeper minted seconds
// earlier, already means the answering process is not ours.
//
// The port is the source of uniqueness because the kernel already guarantees
// it: no two stacks alive at the same moment hold the same one.
func (s *Stack) assignIdentity(prefix, httpAddr string) string {
	s.issuer = prefix + "-" + portOf(httpAddr)
	return s.issuer
}

// portOf returns the port part of `host:port`. Used to key a stack's identity
// to something already unique among concurrently live stacks.
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}
