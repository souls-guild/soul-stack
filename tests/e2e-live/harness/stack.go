//go:build e2e_live

// Package harness - reusable test-helpers for L3b E2E testing
// (real-soul-in-container, ADR-039).
//
// L3b differs from L3a in that, instead of the soul-stub-helper package, it
// spins up a real soul binary in a Linux container (Debian-12 systemd-PID-1)
// and goes through the real CSR-Bootstrap flow. See tests/e2e-live/README.md.
//
// Stack - the unit of test isolation: one test = one Stack = its own PG / Redis /
// Vault + its own Keeper process + N soul containers. NewStack blocks until the
// infra is fully ready. soul-container spawn arrives in the L3b-2 slice.
//
// Architectural invariants (ADR-039 Amendment 2026-05-26):
//   - harness does NOT import `keeper/internal/*` (Go internal rules);
//   - all DB operations are direct SQL via pgx;
//   - all Vault operations are direct HTTP API (see vault.go);
//   - the Keeper process is a sub-process of the real binary, not an in-process import;
//   - soul is a real binary in a privileged container, cross-compiled via `make build-linux`.
package harness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Config - parameters for constructing a Stack.
//
// ExamplePath - relative path to the service directory in the repo (e.g.
// "examples/service/smoke-nginx-live"). The harness reads it, takes a snapshot,
// and sets up a per-test bare git repo in $TMP (L3b-3).
//
// Souls - number of soul containers spawned via SpawnSoulContainer
// (L3b-2+). 0 means a keeper-only stand (no soul containers and no pre-flight
// requirement for the soul-linux binary): light tests of keeper-side surfaces
// (plugin channel NIM-32 S1 etc).
//
// SoulModules - the `plugins.soul_modules[]` catalog of keeper.yml (ADR-065(b)):
// SoulModule plugins that keeper git-resolves into cache_root at startup
// (plugingit, slot `<ns>-<name>/current/`). A non-empty list automatically
// enables Sigil (config_builder writes the sigil block, NewStack seeds an
// ed25519 signing key into Vault) - without a Signer the allow flow doesn't come up.
type Config struct {
	ExamplePath string
	ServiceName string
	Souls       int
	SoulModules []SoulModuleEntry
}

// SoulModuleEntry - one entry of the `plugins.soul_modules[]` catalog
// (`{name, source, ref}`, mirrors config.PluginCatalogEntry - the harness does
// not import shared/config, the public contract is tested as a black box).
type SoulModuleEntry struct {
	Name   string
	Source string
	Ref    string
}

// Stack - the isolated E2E stand for a single test.
type Stack struct {
	t *testing.T

	cfg Config

	// Resolved endpoints (filled in by NewStack after spawning).
	PGURL               string
	RedisAddr           string
	VaultAddr           string
	KeeperHTTPURL       string
	KeeperGRPCAddr      string
	KeeperBootstrapGRPC string
	// MetricsURL - keeper's Prometheus endpoint (separate listener, ADR-024).
	MetricsURL string

	// JWT - the first Archon's credential, read from the credential file
	// produced by `keeper init --credential-out=...`.
	JWT string

	// SoulContainers - populated by SpawnSoulContainer in L3b-2+; nil/empty on L3b-1.
	SoulContainers []*SoulContainer

	// PluginCacheRoot - `plugins.cache_root` from keeper.yml (filled in by
	// buildKeeperYAML): the plugingit resolver materializes the plugin catalog
	// slots `<ns>-<name>/current/` here (ADR-065(b)/(g)). Plugin-channel tests
	// assert the slot at this path.
	PluginCacheRoot string

	// caBundle - PEM bundle of the Vault PKI root CA issued by IssueKeeperServerCert.
	// The soul container mounts it as `/etc/soul/ca.pem` to verify the
	// keeper-server cert during the server-only TLS handshake (`soul init`).
	caBundle []byte

	// Ports of the keeper gRPC listeners. YAML binds them on `0.0.0.0:<port>`
	// (reachable by the host-side probe + the container via host.docker.internal),
	// while the Stack fields KeeperBootstrapGRPC/KeeperGRPCAddr hold
	// `127.0.0.1:<port>` (host-side).
	bootstrapPort   int
	eventStreamPort int

	// dockerNetwork - user-defined bridge for soul containers. nil on L3a-style
	// runs (cfg.Souls=0); created on the first SpawnSoulContainer call.
	dockerNetwork *testcontainers.DockerNetwork

	// Internal state.
	vaultToken string
	tmpDir     string

	db *pgxpool.Pool

	keeperCmd *exec.Cmd

	containers []testcontainers.Container

	// Cleanup shutdown order: LIFO via cleanups (like defers); NewStack
	// accumulates teardown handlers as dependencies come up, Cleanup drains
	// them in reverse order.
	cleanups []func()
}

// NewStack brings up an isolated stand and blocks until it's ready.
//
// L3b-1: brings up PG/Redis/Vault/keeper the same way as L3a (a copy of the harness).
// soul-container spawn is deferred to the L3b-2 slice - the cfg.Souls parameter
// on L3b-1 is logged as a warning, but no containers come up.
//
// Pre-flight (L3b-specific):
//   - keeper binary (env `KEEPER_BIN` or default `./keeper/bin/keeper`);
//     `make build` builds a native binary (host-arch), L3b-1 runs keeper ON
//     THE HOST (not in a container) - a native keeper works fine.
//   - soul-linux binary (env `SOUL_BIN_LINUX` or default
//     `./soul/bin/soul-linux-amd64`) - needed in L3b-2+ to mount into the
//     container. L3b-1 for now only checks its presence (skips the test if not built).
//
// Missing either binary - t.Skip with a hint about `make build` / `make build-linux`.
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()
	if cfg.Souls < 0 {
		cfg.Souls = 0
	}

	// Pre-flight: keeper binary (native, host-arch).
	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("L3b: keeper binary not found (%v); export KEEPER_BIN or run `make build`", err)
	}
	// Pre-flight: linux-soul binary (for L3b-2+). A keeper-only stand (Souls=0)
	// doesn't mount it - not required.
	if cfg.Souls > 0 {
		if _, err := locateLinuxSoulBinary(); err != nil {
			t.Skipf("L3b: soul-linux-amd64 not found (%v); export SOUL_BIN_LINUX or run `make build-linux`", err)
		}
	}

	s := &Stack{
		t:      t,
		cfg:    cfg,
		tmpDir: t.TempDir(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := s.startPostgres(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: postgres: %v", err)
	}
	if err := s.startRedis(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: redis: %v", err)
	}
	if err := s.startVault(ctx); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: vault: %v", err)
	}

	// Vault test-secrets: PKI + JWT signing-key. Symmetric with provision.sh.
	InitVaultTestSecrets(t, s)

	// Sigil signing key - only when the SoulModules catalog is non-empty:
	// keeper with a sigil block in the config fails at startup without a Vault
	// secret (buildSigilSigner cfg-fallback), and without a sigil block the
	// plugin.allow routes aren't registered.
	if len(cfg.SoulModules) > 0 {
		SeedSigilSigningKey(t, s)
	}

	// Outgoing TLS material for the keeper-server listeners.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	s.caBundle = caPEM
	tlsDir := filepath.Join(s.tmpDir, "tls")
	if err := os.MkdirAll(tlsDir, 0o755); err != nil {
		t.Fatalf("NewStack: mkdir tls: %v", err)
	}
	certPath := filepath.Join(tlsDir, "keeper.crt")
	keyPath := filepath.Join(tlsDir, "keeper.key")
	caPath := filepath.Join(tlsDir, "vault-ca.crt")
	if err := os.WriteFile(certPath, keeperCertPEM, 0o644); err != nil {
		t.Fatalf("NewStack: write keeper.crt: %v", err)
	}
	if err := os.WriteFile(keyPath, keeperKeyPEM, 0o600); err != nil {
		t.Fatalf("NewStack: write keeper.key: %v", err)
	}
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatalf("NewStack: write vault-ca.crt: %v", err)
	}

	// keeper.yml - rendered into tmpDir.
	keeperYAML := s.buildKeeperYAML(certPath, keyPath, caPath)
	keeperYAMLPath := filepath.Join(s.tmpDir, "keeper.yml")
	if err := os.WriteFile(keeperYAMLPath, []byte(keeperYAML), 0o600); err != nil {
		t.Fatalf("NewStack: write keeper.yml: %v", err)
	}

	// PG connection pool - for direct SQL after bootstrap.
	pool, err := pgxpool.New(ctx, s.PGURL)
	if err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: pgxpool.New: %v", err)
	}
	s.db = pool
	s.cleanups = append(s.cleanups, func() { pool.Close() })

	// Bootstrap: keeper init --credential-out=...
	credPath := s.runKeeperInit(keeperYAMLPath)
	jwtBytes, err := os.ReadFile(credPath)
	if err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: read credential-out %s: %v", credPath, err)
	}
	s.JWT = strings.TrimSpace(string(jwtBytes))

	// keeper run — sub-process.
	if err := s.startKeeperRun(keeperYAMLPath); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: keeper run: %v", err)
	}

	// Service registration (L3b-3): materializes cfg.ExamplePath into a per-test
	// git repo and registers cfg.ServiceName@main via POST /v1/services. Without
	// this, CreateIncarnation responds 422 "service is not registered" (ADR-029).
	// Not tied to soul-spawn order - the soul isn't needed for registration.
	s.registerExampleService(t)

	// soul-container spawn (L3b-2): one privileged Debian-12 systemd-PID-1
	// container per requested Soul. Names are deterministic -
	// `soul-live-<idx>.example.com` (PKI role soul-seed allows example.com).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-live-%c.example.com", 'a'+i)
		token := IssueBootstrapToken(t, s, sid)
		container := SpawnSoulContainer(t, s, sid, token)
		s.SoulContainers = append(s.SoulContainers, container)
	}

	return s
}

// Cleanup tears down the whole stand. Safe to call repeatedly.
func (s *Stack) Cleanup() {
	if s == nil {
		return
	}
	s.runCleanups()
}

func (s *Stack) runCleanups() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		func(fn func()) {
			defer func() {
				if r := recover(); r != nil {
					s.t.Logf("cleanup panic: %v", r)
				}
			}()
			fn()
		}(s.cleanups[i])
	}
	s.cleanups = nil
}

// startPostgres brings up a PG container via testcontainers-go/modules/postgres.
func (s *Stack) startPostgres(ctx context.Context) error {
	pgC, err := tcpostgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:16-alpine"),
		tcpostgres.WithDatabase("keeper"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return fmt.Errorf("postgres container: %w", err)
	}
	s.containers = append(s.containers, pgC)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = pgC.Terminate(ctxTo)
	})

	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("postgres dsn: %w", err)
	}
	s.PGURL = dsn
	return nil
}

func (s *Stack) startRedis(ctx context.Context) error {
	rC, err := tcredis.RunContainer(ctx,
		testcontainers.WithImage("redis:7-alpine"),
	)
	if err != nil {
		return fmt.Errorf("redis container: %w", err)
	}
	s.containers = append(s.containers, rC)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = rC.Terminate(ctxTo)
	})

	addr, err := rC.ConnectionString(ctx)
	if err != nil {
		return fmt.Errorf("redis addr: %w", err)
	}
	// ConnectionString returns `redis://host:port`. keeper.yml::redis.addr
	// needs host:port without the scheme.
	addr = strings.TrimPrefix(addr, "redis://")
	s.RedisAddr = addr
	return nil
}

func (s *Stack) startVault(ctx context.Context) error {
	const rootToken = "root-test-token"
	req := testcontainers.ContainerRequest{
		Image:        "hashicorp/vault:1.15",
		ExposedPorts: []string{"8200/tcp"},
		Env: map[string]string{
			"VAULT_DEV_ROOT_TOKEN_ID":  rootToken,
			"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
		},
		WaitingFor: wait.ForLog("Root Token:").WithStartupTimeout(45 * time.Second),
	}
	vc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return fmt.Errorf("vault container: %w", err)
	}
	s.containers = append(s.containers, vc)
	s.cleanups = append(s.cleanups, func() {
		ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = vc.Terminate(ctxTo)
	})

	host, err := vc.Host(ctx)
	if err != nil {
		return fmt.Errorf("vault host: %w", err)
	}
	port, err := vc.MappedPort(ctx, "8200")
	if err != nil {
		return fmt.Errorf("vault port: %w", err)
	}
	s.VaultAddr = fmt.Sprintf("http://%s:%s", host, port.Port())
	s.vaultToken = rootToken
	return nil
}

// runKeeperInit calls `keeper init` with the canonical flags and returns the
// path to the credential file (JWT of the first Archon).
func (s *Stack) runKeeperInit(keeperYAMLPath string) string {
	s.t.Helper()
	binaryPath := keeperBinaryPath(s.t)
	credentialPath := filepath.Join(s.tmpDir, "archon-test.credential")

	cmd := exec.Command(binaryPath, "init",
		"--archon=archon-test",
		"--config", keeperYAMLPath,
		"--credential-out", credentialPath,
	)
	cmd.Env = append(os.Environ(), "SOUL_STACK_ALLOW_FILE_REPOS=1")

	output, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("keeper init failed: %v\nOUTPUT:\n%s", err, output)
	}
	return credentialPath
}

// startKeeperRun spawns `keeper run` as a sub-process. Blocks until the
// HTTP listener starts responding (polling /readyz).
func (s *Stack) startKeeperRun(keeperYAMLPath string) error {
	binaryPath := keeperBinaryPath(s.t)
	// Service/destiny git snapshots of the artifact loader are cached in a
	// directory that defaults to `/var/lib/soul-stack-keeper/...` (not writable:
	// keeper in L3b runs ON THE HOST under a regular user). Redirect to tmpDir
	// via env override (KEEPER_SERVICE_CACHE_DIR / KEEPER_DESTINY_CACHE_DIR /
	// KEEPER_PLUGIN_WORK_DIR - see cmd/keeper/main.go). Without this, loading
	// the service on CreateIncarnation fails with 500 "mkdir /var/lib/...:
	// permission denied" when materializing the service snapshot from a
	// file:// repo (parity with L3a).
	serviceCacheDir := filepath.Join(s.tmpDir, "service-cache")
	destinyCacheDir := filepath.Join(s.tmpDir, "destiny-cache")
	pluginWorkDir := filepath.Join(s.tmpDir, "plugin-src")
	cmd := exec.Command(binaryPath, "run", "--config", keeperYAMLPath)
	cmd.Env = append(os.Environ(),
		"SOUL_STACK_ALLOW_FILE_REPOS=1",
		"KEEPER_SERVICE_CACHE_DIR="+serviceCacheDir,
		"KEEPER_DESTINY_CACHE_DIR="+destinyCacheDir,
		"KEEPER_PLUGIN_WORK_DIR="+pluginWorkDir,
	)
	cmd.Stdout = &testLogWriter{t: s.t, prefix: "keeper-stdout"}
	cmd.Stderr = &testLogWriter{t: s.t, prefix: "keeper-stderr"}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start keeper run: %w", err)
	}
	s.keeperCmd = cmd
	s.cleanups = append(s.cleanups, func() {
		if cmd.Process == nil {
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

	// Wait /readyz.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if probeReady(s.KeeperHTTPURL + "/readyz") {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("keeper run: /readyz did not become healthy in 60s")
}

// keeperBinaryPath - path to the keeper binary for exec calls. Fatal-fails if
// missing (pre-flight in NewStack already Skipped earlier).
func keeperBinaryPath(t *testing.T) string {
	t.Helper()
	path, err := locateKeeperBinary()
	if err != nil {
		t.Fatalf("keeperBinaryPath: %v", err)
	}
	return path
}

// locateKeeperBinary returns the keeper binary path without a testing.TB dependency.
// Source: env KEEPER_BIN (priority), else `$REPO/keeper/bin/keeper`
// (Makefile target `make build`).
func locateKeeperBinary() (string, error) {
	if v := os.Getenv("KEEPER_BIN"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("KEEPER_BIN=%s: %w", v, err)
		}
		return v, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	// tests/e2e-live/<test>.go -> repo-root = wd/../..
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "keeper", "bin", "keeper")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("default %s: %w", candidate, err)
	}
	return candidate, nil
}

// locateLinuxSoulBinary - path to the cross-compiled soul-linux-amd64 for
// mounting into the soul container (L3b-2+). Source: env SOUL_BIN_LINUX
// (priority), else `$REPO/soul/bin/soul-linux-amd64` (Makefile target `make build-linux`).
//
// On L3b-1 the function is only called in pre-flight (Skip if missing); the
// actual container mount is in the L3b-2 slice.
func locateLinuxSoulBinary() (string, error) {
	if v := os.Getenv("SOUL_BIN_LINUX"); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("SOUL_BIN_LINUX=%s: %w", v, err)
		}
		return v, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "soul", "bin", "soul-linux-amd64")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("default %s: %w", candidate, err)
	}
	return candidate, nil
}

// testLogWriter forwards the keeper process stdout/stderr to t.Log.
type testLogWriter struct {
	t      *testing.T
	prefix string
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		w.t.Logf("[%s] %s", w.prefix, line)
	}
	return len(p), nil
}

// CreateIncarnation creates an incarnation via the Keeper Operator API.
//
// serviceRef - `<service>@<ref>`; the harness strips the `@<ref>` suffix
// (ADR-029: POST /v1/incarnations only accepts a bare service name).
//
// 202 -> returns the incarnation name. Any other status - t.Fatal with the
// response body.
//
// Retry on 422 "service is not registered": the service registry resolves from
// an in-memory Holder snapshot (serviceregistry.Holder), refreshed via a TTL
// poll (10s) + Redis pub/sub invalidation. registerExampleService publishes
// the service right before the test flow, and the first CreateIncarnation may
// land in the window before the snapshot has picked up the new entry (POST
// returned 201, but the snapshot is still stale). This is a
// registration<->snapshot-refresh race, not a missing service - a short retry
// closes it hermetically without touching the public contract. Once the
// service is visible, the first request goes through immediately.
func (s *Stack) CreateIncarnation(t *testing.T, name string, serviceRef string, spec map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	service := stripServiceRef(serviceRef)
	body := map[string]any{
		"name":    name,
		"service": service,
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnation %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnation %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnation %s: decode: %v (body=%s)", name, err, string(resp))
	}
	return out.Incarnation
}

// CreateIncarnationWithApply - like CreateIncarnation, but also returns the
// apply_id of the auto-started `create` scenario. POST /v1/incarnations
// immediately starts the create run and moves the incarnation to `applying`
// (incarnation.go). So a separate RunScenario(create) right after Create is
// rejected by the lock gate ("incarnation already in status applying" -
// run.go), and waiting for its apply_id hangs in WaitApplySuccess until
// timeout. Use this method instead of the CreateIncarnation +
// RunScenario(create) pair. Symmetric with the L3a harness
// (tests/e2e/harness/stack.go::CreateIncarnationWithApply).
//
// create_scenario=`create` - Phase-2 contract (2026-06-29): choosing the
// starting scenario is mandatory when the service has a non-empty create set;
// the scenario must carry `create: true`. Bare path (no run) - CreateIncarnation.
//
// Returns (incarnationName, applyID of the auto-create run).
func (s *Stack) CreateIncarnationWithApply(t *testing.T, name string, serviceRef string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	service := stripServiceRef(serviceRef)
	body := map[string]any{
		"name":            name,
		"service":         service,
		"create_scenario": "create",
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnationWithApply %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnationWithApply %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnationWithApply %s: decode: %v (body=%s)", name, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("CreateIncarnationWithApply %s: empty apply_id in 202 body=%s (create scenario not started?)", name, string(resp))
	}
	return out.Incarnation, out.ApplyID
}

// CreateIncarnationWithApplyScenario - like CreateIncarnationWithApply, but
// the create scenario is chosen explicitly (services with multiple `create:
// true`, e.g. redis create/create_from_souls/migrate_cluster, otherwise
// POST -> 422).
func (s *Stack) CreateIncarnationWithApplyScenario(t *testing.T, name, serviceRef, createScenario string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":            name,
		"service":         stripServiceRef(serviceRef),
		"create_scenario": createScenario,
	}
	if spec != nil {
		body["input"] = spec
	}

	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnationWithApplyScenario %s: http: %v", name, err)
		}
		if status == http.StatusAccepted {
			break
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "is not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		t.Fatalf("CreateIncarnationWithApplyScenario %s: status %d, body=%s", name, status, string(resp))
	}
	var out struct {
		ApplyID     string `json:"apply_id"`
		Incarnation string `json:"incarnation"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("CreateIncarnationWithApplyScenario %s: decode: %v (body=%s)", name, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("CreateIncarnationWithApplyScenario %s: empty apply_id (create scenario %q not started?) body=%s", name, createScenario, string(resp))
	}
	return out.Incarnation, out.ApplyID
}

// RunScenario runs a scenario on an existing incarnation.
//
// 202 -> returns apply_id. Any other status - t.Fatal.
func (s *Stack) RunScenario(t *testing.T, incarnationName string, scenarioName string, input map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incarnationName, scenarioName)
	resp, status, err := c.post(context.Background(), path, body)
	if err != nil {
		t.Fatalf("RunScenario %s/%s: http: %v", incarnationName, scenarioName, err)
	}
	if status != http.StatusAccepted {
		t.Fatalf("RunScenario %s/%s: status %d, body=%s", incarnationName, scenarioName, status, string(resp))
	}
	var out struct {
		ApplyID string `json:"apply_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("RunScenario %s/%s: decode: %v (body=%s)", incarnationName, scenarioName, err, string(resp))
	}
	if out.ApplyID == "" {
		t.Fatalf("RunScenario %s/%s: empty apply_id in 202 body=%s", incarnationName, scenarioName, string(resp))
	}
	return out.ApplyID
}

// WaitApplySuccess blocks until the SUCCESSFUL terminal state of the run: all
// apply_runs rows (PK = apply_id+sid, N rows per run) are success AND the
// apply bracket incarnation.applying_apply_id has been cleared for this applyID.
//
// The bracket is mandatory (NIM-46): the keeper row (sid="keeper") reaches
// success STRICTLY BEFORE the soul rows are planned, so "all visible rows
// success" without a cleared bracket is a false "done" (the NIM-45 race). The
// fix is [applySettled].
//
// Terminal != success - an immediate t.Fatal with a dump of the status matrix.
func (s *Stack) WaitApplySuccess(t *testing.T, applyID string, timeoutSec int) {
	t.Helper()
	const q = `
SELECT ar.sid, ar.status, i.applying_apply_id
FROM apply_runs ar
LEFT JOIN incarnation i ON i.name = ar.incarnation_name
WHERE ar.apply_id = $1`
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var lastSnap []ApplyRunRow
	var lastInFlight bool
	for time.Now().Before(deadline) {
		rows, err := s.db.Query(context.Background(), q, applyID)
		if err != nil {
			t.Fatalf("WaitApplySuccess %s: query: %v", applyID, err)
		}
		var snap []ApplyRunRow
		inFlight := false
		for rows.Next() {
			var sid, st string
			var applyingID *string
			if err := rows.Scan(&sid, &st, &applyingID); err != nil {
				rows.Close()
				t.Fatalf("WaitApplySuccess %s: scan: %v", applyID, err)
			}
			snap = append(snap, ApplyRunRow{SID: sid, Status: st})
			if applyingID != nil && *applyingID == applyID {
				inFlight = true
			}
		}
		rows.Close()
		lastSnap, lastInFlight = snap, inFlight
		done, failSID, failStatus := applySettled(snap, inFlight)
		if failSID != "" {
			t.Fatalf("WaitApplySuccess %s: sid=%s reached terminal %q (rows=%v)", applyID, failSID, failStatus, snap)
		}
		if done {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("WaitApplySuccess %s: success not reached in %ds (applyInFlight=%v, rows=%v)", applyID, timeoutSec, lastInFlight, lastSnap)
}

// WaitIncarnationReady blocks until incarnation.status transitions to `ready`.
//
// Why separate from WaitApplySuccess: `apply_runs.status=success` (per-host
// task terminal) is set EARLIER than the state_changes commit into
// incarnation.state. commitSuccess (run.go §8) writes state + status='ready'
// in one PG transaction AFTER the barrier for all hosts - i.e. there's a
// window between "apply_runs success" and "state committed". On L3a
// (soul-stub responds instantly) the window is microscopic; on L3b (real soul
// + gRPC round-trip) the test can read incarnation.state as empty `{}` ->
// AssertIncarnationState flakes. We wait specifically for status='ready' -
// the only point that guarantees state_changes are already in the DB.
//
// Terminal != ready (error_locked / migration_failed / destroy_failed) - an
// immediate t.Fatal with the current status.
func (s *Stack) WaitIncarnationReady(t *testing.T, incarnationName string, timeoutSec int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	var last string
	for time.Now().Before(deadline) {
		var status string
		err := s.db.QueryRow(context.Background(),
			"SELECT status FROM incarnation WHERE name = $1", incarnationName).Scan(&status)
		if err != nil {
			t.Fatalf("WaitIncarnationReady %s: query: %v", incarnationName, err)
		}
		last = status
		switch status {
		case "ready":
			return
		case "error_locked", "migration_failed", "destroy_failed", "destroyed":
			t.Fatalf("WaitIncarnationReady %s: reached terminal status %q instead of ready", incarnationName, status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationReady %s: status=ready not reached in %ds (last status=%q)",
		incarnationName, timeoutSec, last)
}

// WaitIncarnationStatus blocks until incarnation.status transitions to wantStatus.
//
// Mirrors WaitIncarnationReady for NON-ready outcomes (split-brain guard,
// failed_when fail-stop): a run that SHOULD fail leaves the incarnation in
// `error_locked` (run.go §7 - state_changes aren't committed at the
// terminal-failed barrier).
//
// * Race with seeded-ready. SeedIncarnationReady puts the incarnation directly
// into `ready`; RunScenario returns apply_id asynchronously, BEFORE lockRun
// transitions `ready -> applying -> (terminal)`. A naive poller would catch
// the initial `ready` and mistake it for "wrong terminal reached". So we wait
// in two stages: first observe `applying` (the run started and cleared the
// initial status), and only AFTER that treat terminal != wantStatus as a
// flow-control regression. If wantStatus == applying, the first stage is the result.
func (s *Stack) WaitIncarnationStatus(t *testing.T, incarnationName, wantStatus string, timeoutSec int) {
	t.Helper()
	terminal := map[string]bool{
		"ready": true, "error_locked": true, "migration_failed": true,
		"destroy_failed": true, "destroyed": true,
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	started := false // the run cleared the initial status (observed applying or an immediate terminal)
	var last string
	for time.Now().Before(deadline) {
		var status string
		err := s.db.QueryRow(context.Background(),
			"SELECT status FROM incarnation WHERE name = $1", incarnationName).Scan(&status)
		if err != nil {
			t.Fatalf("WaitIncarnationStatus %s: query: %v", incarnationName, err)
		}
		last = status
		if status == wantStatus {
			return
		}
		if status == "applying" {
			started = true
		}
		// A terminal mismatch is treated as a regression ONLY after the run
		// started: before that it's still the seeded initial status (usually
		// ready), not the run's outcome.
		if started && terminal[status] {
			t.Fatalf("WaitIncarnationStatus %s: reached terminal %q, expected %q (flow-control outcome diverged)",
				incarnationName, status, wantStatus)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitIncarnationStatus %s: status=%q not reached in %ds (last=%q)",
		incarnationName, wantStatus, timeoutSec, last)
}

// DeleteIncarnation tears down an incarnation via the Operator API (DELETE
// /v1/incarnations/<name>?allow_destroy=<bool>). allow_destroy=true ->
// force-DELETE without a teardown scenario (synchronous ON DELETE CASCADE in
// the same request, incarnation_typed.go §DestroyTyped). Asserts 2xx (route -> 202).
func (s *Stack) DeleteIncarnation(t *testing.T, name string, allowDestroy bool) {
	t.Helper()
	c := s.opClient(t)
	path := fmt.Sprintf("/v1/incarnations/%s?allow_destroy=%t", name, allowDestroy)
	resp, status, err := c.del(context.Background(), path)
	if err != nil {
		t.Fatalf("DeleteIncarnation %s: http: %v", name, err)
	}
	if status < 200 || status >= 300 {
		t.Fatalf("DeleteIncarnation %s (allow_destroy=%t): status %d, body=%s", name, allowDestroy, status, string(resp))
	}
}

// AssertIncarnationAbsent - teardown assert for the destroy test: GET
// incarnation -> 404 AND the ON DELETE CASCADE ran (apply_runs / state_history
// for the incarnation = 0 LIVE rows; the pre-delete snapshot is preserved in
// *_archive, migrations 018/006). GET is polled: force-DELETE is synchronous,
// but the teardown path removes the row asynchronously after 202.
func (s *Stack) AssertIncarnationAbsent(t *testing.T, name string) {
	t.Helper()
	c := s.opClient(t)

	deadline := time.Now().Add(15 * time.Second)
	var lastStatus int
	var lastBody []byte
	for {
		body, status, err := c.get(context.Background(), "/v1/incarnations/"+name)
		if err != nil {
			t.Fatalf("AssertIncarnationAbsent %s: GET: %v", name, err)
		}
		lastStatus, lastBody = status, body
		if status == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("AssertIncarnationAbsent %s: GET status=%d, expected 404 (incarnation not destroyed)\nbody=%s", name, lastStatus, string(lastBody))
		}
		time.Sleep(300 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tbl := range []string{"apply_runs", "state_history"} {
		var n int
		// Table name is a literal from a closed list (not user input), format is safe.
		q := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE incarnation_name = $1", tbl)
		if err := s.db.QueryRow(ctx, q, name).Scan(&n); err != nil {
			t.Fatalf("AssertIncarnationAbsent %s: count %s: %v", name, tbl, err)
		}
		if n != 0 {
			t.Fatalf("AssertIncarnationAbsent %s: cascade did not run - %s still has %d rows", name, tbl, n)
		}
	}
}

// stripServiceRef strips `@<ref>` (if present). The Operator API creates
// incarnations by bare service name (ADR-029).
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// DB returns the pool for the test (read-only for asserts). Does not Close for
// the caller: the pool is managed by Cleanup.
func (s *Stack) DB() *pgxpool.Pool {
	return s.db
}
