//go:build e2e

package harness

// Multi-keeper crash harness (GA recovery proof). Extends the single-keeper
// Stack to N keeper subprocesses on top of SHARED PG / Redis / Vault: each
// with its own KID + its own listener ports + an enabled VoyageWorker pool
// and the reclaim_voyages reaper rule. Presence of keeper instances — via the
// shared Redis Conclave (as in a prod HA cluster).
//
// Goal: kill the keeper PROCESS that owns a Voyage mid-run (a real SIGKILL,
// not a SQL emulation) and prove end-to-end recovery — another live keeper
// picks it up (reclaim_voyages -> re-claim) and drives the run to a terminal.
//
// Single-keeper tests (NewStack) are NOT touched by this file: everything is additive.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// keeperProc — one live keeper subprocess of the multi-keeper cluster.
type keeperProc struct {
	kid      string
	cmd      *exec.Cmd
	httpURL  string
	grpcAddr string
	// killed — flag that the process has already been killed by the test
	// (cleanup then doesn't send a signal to a dead process again).
	killed bool
}

// MultiKeeperConfig — parameters for the multi-keeper crash stand.
type MultiKeeperConfig struct {
	// Keepers — number of keeper subprocesses (>=2 for a crash scenario).
	Keepers int
	// Souls — number of pre-auth soul stubs (shared across the whole cluster).
	Souls int
	// VoyageLeaseTTL — TTL of the PG claim lease on a voyages row. Short (3-5s)
	// so that after SIGKILLing the owner, the stale claim quickly falls under
	// reclaim_voyages. Empty -> default 4s.
	VoyageLeaseTTL time.Duration

	// ReconcileOrphanStaleAfter — `stale_after` of the reconcile_orphan_applying
	// reaper rule (ADR-027 amend (m)). An applying row is considered orphaned
	// if applying_since < NOW()-stale_after AND the owner's presence is dead.
	// The rule's default is 90s; for the standalone-orphan crash test we set a
	// short threshold (2-5s) so the rule fires shortly after the killed
	// owner's Conclave presence expires, instead of waiting the by-design
	// 90s. Empty -> 3s (a short test threshold, NOT the rule's prod default).
	ReconcileOrphanStaleAfter time.Duration
}

// NewMultiKeeperStack brings up shared PG/Redis/Vault + N keeper subprocesses
// and blocks until each is ready (/readyz). Returns a Stack whose
// Stack.KeeperHTTPURL / KeeperGRPCAddr point at keeper[0] (the primary — the
// entry point for the Operator API and soul-stub EventStreams); reclaim,
// however, is executed by ANY live cluster keeper via the shared
// Reaper leader.
func NewMultiKeeperStack(t *testing.T, cfg MultiKeeperConfig) *Stack {
	t.Helper()
	if cfg.Keepers < 2 {
		t.Fatalf("NewMultiKeeperStack: need >=2 keepers for a crash scenario, got %d", cfg.Keepers)
	}
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}
	if cfg.VoyageLeaseTTL <= 0 {
		cfg.VoyageLeaseTTL = 4 * time.Second
	}
	if cfg.ReconcileOrphanStaleAfter <= 0 {
		cfg.ReconcileOrphanStaleAfter = 3 * time.Second
	}

	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("multi-keeper: keeper binary not found (%v); export KEEPER_BIN or run `make build`", err)
	}

	s := &Stack{
		t:      t,
		cfg:    Config{Souls: cfg.Souls},
		tmpDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Shared infra (as in NewStack).
	if err := s.startPostgres(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: postgres: %v", err)
	}
	if err := s.startRedis(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: redis: %v", err)
	}
	if err := s.startVault(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: vault: %v", err)
	}
	InitVaultTestSecrets(t, s)

	// A shared server cert for all keepers (one CA — the soul-stub verifies
	// any keeper's server cert against it). SAN includes 127.0.0.1, so one
	// cert works for all loopback listeners.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	s.caBundle = caPEM
	tlsDir := filepath.Join(s.tmpDir, "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("multi-keeper: mkdir tls: %v", err)
	}
	certPath := filepath.Join(tlsDir, "keeper.crt")
	keyPath := filepath.Join(tlsDir, "keeper.key")
	caPath := filepath.Join(tlsDir, "vault-ca.crt")
	mustWrite(t, certPath, keeperCertPEM, 0o644)
	mustWrite(t, keyPath, keeperKeyPEM, 0o600)
	mustWrite(t, caPath, caPEM, 0o644)

	// PG pool for direct SQL (assert claimed_by_kid / status).
	pool, err := pgxpool.New(ctx, s.PGURL)
	if err != nil {
		s.runCleanups()
		t.Fatalf("multi-keeper: pgxpool.New: %v", err)
	}
	s.db = pool
	s.cleanups = append(s.cleanups, func() { pool.Close() })

	// PG DSN into Vault (keeper.yml::postgres.dsn_ref points here).
	s.seedPostgresDSN()

	// Render per-keeper YAML + allocate ports. The first keeper (i==0) is the
	// soul-holder primary: its HTTP/gRPC addresses are set on the Stack
	// (anchors for opClient and ConnectSoulStub), and it does NOT run a
	// VoyageWorker pool (voyage.workers: 0) — so it never becomes a Voyage
	// owner. The remaining keepers (i>=1) are VoyageWorkers with no connected
	// souls.
	//
	// Why separate: soul stubs connect their stream to the primary; killing
	// the Voyage owner (always i>=1) does NOT bring down the soul streams.
	// Apply steps of the Voyage owner's per-incarnation scenario run are
	// routed to the soul-holder via cluster routing (Redis applybus,
	// cluster_routing=true). This way a Voyage-owner crash leaves the fleet
	// alive, and the reclaim-keeper finishes the run against the still
	// connected souls.
	for i := 0; i < cfg.Keepers; i++ {
		kid := fmt.Sprintf("keeper-mk-%02d", i)
		voyageWorkers := 2
		if i == 0 {
			voyageWorkers = 0 // the soul-holder never claims a Voyage
		}
		yamlPath, httpURL, grpcAddr := s.buildMultiKeeperYAML(kid, certPath, keyPath, caPath, cfg.VoyageLeaseTTL, voyageWorkers, cfg.ReconcileOrphanStaleAfter)

		if i == 0 {
			// Bootstrap the first Archon — once, on the primary config
			// (touches only PG/Vault, not the listeners). JWT is shared
			// across the whole cluster.
			credPath := s.runKeeperInit(yamlPath)
			jwtBytes, rerr := os.ReadFile(credPath)
			if rerr != nil {
				s.runCleanups()
				t.Fatalf("multi-keeper: read credential-out: %v", rerr)
			}
			s.JWT = strings.TrimSpace(string(jwtBytes))
			s.KeeperHTTPURL = httpURL
			s.KeeperGRPCAddr = grpcAddr
		}

		kp, serr := s.spawnKeeperProc(kid, yamlPath, httpURL, grpcAddr)
		if serr != nil {
			s.runCleanups()
			t.Fatalf("multi-keeper: spawn keeper %s: %v", kid, serr)
		}
		s.keepers = append(s.keepers, kp)
	}

	// Pre-auth soul stubs (shared; ConnectSoulStub opens a stream to the primary).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-mk-%d.example.com", i)
		cert, key := RegisterSoulPreAuth(t, s, sid)
		s.souls = append(s.souls, soulIdentity{SID: sid, Cert: cert, Key: key})
	}

	return s
}

// buildMultiKeeperYAML renders keeper.yml for one keeper of the multi-keeper
// cluster: a unique KID + its own listener ports, shared PG/Redis/Vault, an
// enabled VoyageWorker pool (short lease) and the reclaim_voyages reaper
// rule. Writes the YAML to tmpDir/<kid>.yml. Returns (yamlPath, httpURL,
// grpcEventStreamAddr).
func (s *Stack) buildMultiKeeperYAML(kid, certPath, keyPath, caPath string, leaseTTL time.Duration, voyageWorkers int, reconcileStaleAfter time.Duration) (string, string, string) {
	bootstrapAddr := allocLoopback(s.t)
	eventStreamAddr := allocLoopback(s.t)
	httpAddr := allocLoopback(s.t)
	mcpAddr := allocLoopback(s.t)
	metricsAddr := allocLoopback(s.t)

	pluginsCacheDir := filepath.Join(s.tmpDir, kid, "plugins")
	socketsDir := filepath.Join(s.tmpDir, kid, "plugin-sockets")

	// Renew ~1/3 TTL; the reaper interval and reclaim-stale are short so
	// reclaim fires within seconds of the killed owner's lease expiring.
	leaseRenew := leaseTTL / 3
	if leaseRenew < time.Second {
		leaseRenew = time.Second
	}

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
    issuer: keeper-mk
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

voyage:
  workers: %d
  lease_ttl: %s
  lease_renew_interval: %s
  poll_interval: 1s

reaper:
  enabled: true
  interval: 500ms
  dry_run: false
  batch_size: 1000
  lock_ttl: 2s
  rules:
    reclaim_voyages:
      enabled: true
      stale_after: 1s
    reconcile_orphan_applying:
      enabled: true
      stale_after: %s
`
	yaml := fmt.Sprintf(tmpl,
		kid,
		bootstrapAddr, certPath, keyPath,
		eventStreamAddr, certPath, keyPath, caPath,
		httpAddr, mcpAddr, metricsAddr,
		s.RedisAddr,
		s.VaultAddr, s.vaultToken,
		pluginsCacheDir, socketsDir,
		voyageWorkers, durationYAML(leaseTTL), durationYAML(leaseRenew),
		durationYAML(reconcileStaleAfter),
	)

	yamlPath := filepath.Join(s.tmpDir, kid+".yml")
	mustWrite(s.t, yamlPath, []byte(yaml), 0o600)

	return yamlPath, "http://" + httpAddr, eventStreamAddr
}

// spawnKeeperProc launches `keeper run` for one KID and blocks until ready
// (/readyz). The cleanup handler sends SIGINT (if the process is still alive).
func (s *Stack) spawnKeeperProc(kid, yamlPath, httpURL, grpcAddr string) (*keeperProc, error) {
	binaryPath := keeperBinaryPath(s.t)
	serviceCacheDir := filepath.Join(s.tmpDir, kid, "service-cache")
	destinyCacheDir := filepath.Join(s.tmpDir, kid, "destiny-cache")
	pluginWorkDir := filepath.Join(s.tmpDir, kid, "plugin-src")

	cmd := exec.Command(binaryPath, "run", "--config", yamlPath)
	cmd.Env = append(os.Environ(),
		"SOUL_STACK_ALLOW_FILE_REPOS=1",
		"KEEPER_SERVICE_CACHE_DIR="+serviceCacheDir,
		"KEEPER_DESTINY_CACHE_DIR="+destinyCacheDir,
		"KEEPER_PLUGIN_WORK_DIR="+pluginWorkDir,
	)
	cmd.Stdout = &testLogWriter{t: s.t, prefix: kid + "-stdout"}
	cmd.Stderr = &testLogWriter{t: s.t, prefix: kid + "-stderr"}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	kp := &keeperProc{kid: kid, cmd: cmd, httpURL: httpURL, grpcAddr: grpcAddr}

	s.cleanups = append(s.cleanups, func() {
		if kp.killed || cmd.Process == nil {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if probeReady(httpURL + "/readyz") {
			return kp, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, errors.New("/readyz did not become healthy in 60s")
}

// KillKeeperByKID sends SIGKILL to the keeper process with the given KID (a
// real process kill — not a SQL emulation). Fatal if the KID is unknown or
// already killed. After the kill it blocks until the process actually exits
// (cmd.Wait), so ports are freed and the test doesn't hit a zombie listener.
func (s *Stack) KillKeeperByKID(t *testing.T, kid string) {
	t.Helper()
	for _, kp := range s.keepers {
		if kp.kid != kid {
			continue
		}
		if kp.killed {
			t.Fatalf("KillKeeperByKID(%s): process already killed", kid)
		}
		if kp.cmd.Process == nil {
			t.Fatalf("KillKeeperByKID(%s): process not started", kid)
		}
		if err := kp.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("KillKeeperByKID(%s): SIGKILL: %v", kid, err)
		}
		kp.killed = true
		_ = kp.cmd.Wait() // reap the zombie; exit code is irrelevant (SIGKILL)
		t.Logf("multi-keeper: SIGKILL sent and confirmed for %s", kid)
		return
	}
	t.Fatalf("KillKeeperByKID(%s): KID not found among %d keepers", kid, len(s.keepers))
}

// LiveKeeperKIDs returns the KIDs of still-live (not killed) keepers.
func (s *Stack) LiveKeeperKIDs() []string {
	var out []string
	for _, kp := range s.keepers {
		if !kp.killed {
			out = append(out, kp.kid)
		}
	}
	return out
}

// AllKeeperGRPCAddrs returns the EventStream gRPC addresses of all cluster
// keepers in spawn order (mk-00 first). Mirrors soul.yml::keeper.endpoints —
// the soul-stub uses the list for reconnect fallback when the stream-holder
// keeper dies.
func (s *Stack) AllKeeperGRPCAddrs() []string {
	out := make([]string, 0, len(s.keepers))
	for _, kp := range s.keepers {
		out = append(out, kp.grpcAddr)
	}
	return out
}

// LiveKeeperGRPCAddrs returns the EventStream gRPC addresses of still-live
// keepers (excluding those killed with SIGKILL). For the stub's reconnect
// fallback after the holder crashes.
func (s *Stack) LiveKeeperGRPCAddrs() []string {
	out := make([]string, 0, len(s.keepers))
	for _, kp := range s.keepers {
		if !kp.killed {
			out = append(out, kp.grpcAddr)
		}
	}
	return out
}

// KeeperKIDForGRPCAddr resolves an EventStream gRPC address -> keeper KID
// (the reverse mapping of AllKeeperGRPCAddrs). Needed by the test to figure
// out, from the stub's stream-holder keeper address, which KID to kill.
// Empty string if the address is unknown.
func (s *Stack) KeeperKIDForGRPCAddr(addr string) string {
	for _, kp := range s.keepers {
		if kp.grpcAddr == addr {
			return kp.kid
		}
	}
	return ""
}

// mustWrite writes a file, fatal on error.
func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// durationYAML formats a time.Duration as a Go duration string for
// keeper.yml (the config parser accepts the "4s"/"1s" time.ParseDuration
// format).
func durationYAML(d time.Duration) string {
	return d.String()
}
