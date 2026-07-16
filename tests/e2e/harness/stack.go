//go:build e2e

// Package harness — reusable test helpers for L3a E2E testing (ADR-039).
//
// Stack — the unit of test isolation: one test = one Stack = its own PG /
// Redis / Vault via testcontainers + its own Keeper process (a sub-process
// of the real binary) + N soul-stubs opening a bidi stream to the Keeper.
// NewStack blocks until the infra is fully ready (PG healthy + keeper run
// responds on /readyz + all soul-stubs registered).
//
// Architectural invariants (see ADR-039 Amendment 2026-05-26):
//   - the harness does NOT import `keeper/internal/*` (Go internal rules);
//   - all DB operations are direct SQL via pgx;
//   - all Vault operations are direct HTTP API (see vault.go);
//   - the Keeper process is a sub-process of the real binary, not an
//     in-process import.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Config — parameters for constructing a Stack.
//
// ExamplePath — relative path to the service directory in the repo (e.g.
// "examples/service/smoke-nginx"). The harness reads it, takes a snapshot,
// and sets up a per-test bare git repo in $TMP (see git.go).
//
// Souls — number of soul-stubs opening a stream to the Keeper. For each
// stub the harness generates its own SID (e.g. "soul-test-0.example.com")
// and a minimal soulprint, unless fixtures/souls.yaml specifies otherwise.
type Config struct {
	ExamplePath string
	Souls       int
}

// Stack — isolated E2E stand for one test.
type Stack struct {
	t *testing.T

	cfg Config

	// Resolved endpoints (filled in by NewStack after spawn).
	PGURL               string
	RedisAddr           string
	VaultAddr           string
	KeeperHTTPURL       string
	KeeperGRPCAddr      string
	KeeperBootstrapGRPC string
	// MetricsURL — the keeper's Prometheus endpoint (separate listener,
	// ADR-024). Used by AssertMetricGE.
	MetricsURL string

	// JWT — the first Archon's credential, read from the credential file
	// `keeper init --credential-out=...`.
	JWT string

	// Internal state.
	vaultToken string
	tmpDir     string

	db *pgxpool.Pool

	keeperCmd *exec.Cmd

	// keepers — multi-cluster keeper sub-processes (NewMultiKeeperStack).
	// Empty for a single-keeper Stack (keeperCmd above). See multikeeper.go.
	keepers []*keeperProc

	// souls — pre-auth-registered soul-stubs (SID + mTLS client cert),
	// filled in by NewStack. caBundle — root CA of the keeper server cert,
	// shared by all (ConnectSoulStub verifies the server cert against it).
	// Used by ConnectSoulStub to open a live EventStream to the Keeper.
	souls    []soulIdentity
	caBundle []byte

	containers []testcontainers.Container

	// Cleanup-shutdown order: LIFO via cleanups (like defers); NewStack
	// accumulates teardown handlers as dependencies come up, Cleanup runs
	// them in reverse order.
	cleanups []func()
}

// NewStack brings up an isolated stand and blocks until it is ready.
//
// Pilot phase (before v3): t.Skip without spawning. Now (v3) — a real infra
// spawn.
//
// Pre-flight: the harness requires a keeper binary (env `KEEPER_BIN` or the
// default `make build` output); without it the test is Skipped BEFORE
// spawning testcontainers (otherwise a developer without a build gets a
// 5-minute timeout). Symmetrically — without docker, testcontainers returns
// a spawn error and the test fails explicitly: the developer explicitly
// requested E2E, so missing docker is a fail, not a skip.
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}

	// Pre-flight: keeper binary. Skip effectively means "E2E is impossible
	// in this environment".
	if _, err := locateKeeperBinary(); err != nil {
		t.Skipf("L3a: keeper binary not found (%v); export KEEPER_BIN or run `make build`", err)
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

	// Vault test-secrets: PKI + JWT signing-key. Mirrors provision.sh.
	InitVaultTestSecrets(t, s)

	// Outgoing-TLS material for keeper-server listeners.
	keeperCertPEM, keeperKeyPEM, caPEM := IssueKeeperServerCert(t, s)
	// Save the CA for ConnectSoulStub (soul-stub verifies the server cert against it).
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

	// keeper.yml — rendered into tmpDir.
	keeperYAML := s.buildKeeperYAML(certPath, keyPath, caPath)
	keeperYAMLPath := filepath.Join(s.tmpDir, "keeper.yml")
	if err := os.WriteFile(keeperYAMLPath, []byte(keeperYAML), 0o600); err != nil {
		t.Fatalf("NewStack: write keeper.yml: %v", err)
	}

	// PG connection pool — for direct SQL after bootstrap.
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

	// Pre-auth registration of soul-stubs in the DB. Save each SID's mTLS
	// client cert — ConnectSoulStub will use it to open a live EventStream
	// to the Keeper (needed for dispatch routing: Errand/Apply go to the
	// local Outbound only with a live stream + acquired Redis SID lease).
	for i := 0; i < cfg.Souls; i++ {
		sid := fmt.Sprintf("soul-test-%d.example.com", i)
		cert, key := RegisterSoulPreAuth(t, s, sid)
		s.souls = append(s.souls, soulIdentity{SID: sid, Cert: cert, Key: key})
	}

	return s
}

// soulIdentity — a pre-auth-registered soul-stub: SID + mTLS client cert.
type soulIdentity struct {
	SID  string
	Cert []byte
	Key  []byte
}

// SoulSID returns the SID of the i-th pre-auth soul (0-based). Fatal if out
// of range (the test requested more Souls than Config.Souls created).
func (s *Stack) SoulSID(i int) string {
	if i < 0 || i >= len(s.souls) {
		s.t.Fatalf("SoulSID(%d): out of range (%d souls created)", i, len(s.souls))
	}
	return s.souls[i].SID
}

// Cleanup tears down the whole stand. Safe to call more than once.
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
		// vault dev-mode wants IPC_LOCK / cap_add, otherwise it logs a
		// warning but still starts. Ignored in the test environment.
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

// runKeeperInit calls `keeper init` with the canonical flags and returns
// the path to the credential file (the first Archon's JWT).
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
	// The artifact loader caches service/destiny git snapshots in a
	// directory that defaults to `/var/lib/soul-stack-keeper/...` (not
	// writable in the test env). Redirect to tmpDir via env overrides
	// (KEEPER_SERVICE_CACHE_DIR / KEEPER_DESTINY_CACHE_DIR /
	// KEEPER_PLUGIN_WORK_DIR — see cmd/keeper/main.go). Without this,
	// incarnation-create fails with 500 "mkdir /var/lib/...: permission
	// denied" while materializing the service snapshot from a file:// repo.
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

// keeperBinaryPath — path to the keeper binary for exec calls. Fatal-fails
// if missing (pre-flight in NewStack already Skipped earlier).
func keeperBinaryPath(t *testing.T) string {
	t.Helper()
	path, err := locateKeeperBinary()
	if err != nil {
		t.Fatalf("keeperBinaryPath: %v", err)
	}
	return path
}

// locateKeeperBinary returns the path to the keeper binary without a
// testing.TB dependency. Source: env KEEPER_BIN (priority), otherwise
// `$REPO/keeper/bin/keeper` (Makefile target `make build`).
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
	// tests/e2e/<test>.go → repo-root = wd/../..
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	candidate := filepath.Join(repoRoot, "keeper", "bin", "keeper")
	if _, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("default %s: %w", candidate, err)
	}
	return candidate, nil
}

// testLogWriter forwards the keeper process's stdout/stderr to t.Log.
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

// SeedIncarnationReady inserts an incarnation row directly into Postgres
// with status=ready and a given baseline state, bypassing the `create`
// scenario.
//
// Needed for e2e mutating scenarios of services where `create` is not
// available in the L3a fixture (cloud-spawn / declared-role / probe on a
// not-yet-running host — e.g. redis-cluster): such a scenario requires a
// pre-existing ready incarnation, but its create cannot be run. A direct
// seed provides the needed entry point.
//
// serviceVersion — the service's git ref (usually "main"); state — the
// baseline incarnation.state (JSONB). covens are NOT set (declared env
// tags aren't needed: the roster resolves via
// `incarnation.name in souls.coven[]`, see AddSoulToCoven).
// created_by_aid = NULL (seed without an operator; FK ON DELETE SET NULL
// allows this). state_schema_version is not set explicitly, defaulting
// from DDL (DEFAULT 1) — the mutating scenario reads state by field, not
// by version.
func (s *Stack) SeedIncarnationReady(t *testing.T, name, service, serviceVersion string, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("SeedIncarnationReady(%s): marshal state: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4::jsonb, 'ready')
	`, name, service, serviceVersion, string(stateJSON)); err != nil {
		t.Fatalf("SeedIncarnationReady(%s): %v", name, err)
	}
}

// CreateIncarnation creates an incarnation via the Keeper's Operator API.
//
// serviceRef — `<service>@<ref>` per the spec contract; the harness strips
// the `@<ref>` suffix (POST /v1/incarnations only accepts a bare
// service-name, the version is resolved via the service registry,
// ADR-029). spec — the request's `input` body.
//
// 202 -> returns the incarnation name. Any other status -> t.Fatal with the
// response body (4xx diagnosis without guessing).
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

	// service-registry propagation: RegisterService commits a DB row and
	// PUBLISHes `service:invalidate`, but serviceregistry.Holder updates
	// its snapshot in a background goroutine (near-instant pub/sub + 10s
	// TTL fallback). Between the 201 from RegisterService and the warm
	// snapshot there is a short window where incarnation-create sees
	// "service is not registered". We poll ONLY this transient 422 (by the
	// "not registered" detail marker); any other status or a 422 of a
	// different nature (required-input) is an immediate fatal, no masking.
	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), "/v1/incarnations", body)
		if err != nil {
			t.Fatalf("CreateIncarnation %s: http: %v", name, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
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

// CreateIncarnationWithApply — like CreateIncarnation, but also returns the
// apply_id of the auto-started `create` scenario (incarnation.go starts it
// immediately, moving the incarnation to `applying`). Use instead of a
// separate RunScenario(create) right after Create: a second, parallel
// create run is rejected ("incarnation already in status applying"), and
// waiting on its apply_id would hang. Returns (incarnationName, applyID).
//
// create_scenario=`create` — the Phase-2 contract (2026-06-29): choosing a
// starting scenario is mandatory when the service has a non-empty create
// set; the scenario must carry `create: true`. The bare path (no run) is
// CreateIncarnation.
func (s *Stack) CreateIncarnationWithApply(t *testing.T, name, serviceRef string, spec map[string]any) (string, string) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":            name,
		"service":         stripServiceRef(serviceRef),
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
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if status != http.StatusAccepted {
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
		t.Fatalf("CreateIncarnationWithApply %s: empty apply_id in 202 body=%s (create-scenario not started?)", name, string(resp))
	}
	return out.Incarnation, out.ApplyID
}

// CreateIncarnationRaw — low-level POST /v1/incarnations: returns
// (responseBody, statusCode) without checking the status. For negative
// tests (e.g. 422 sync-validation of required-input — fix 6ce69ce), where
// the response code itself is the subject of the assert. Use
// CreateIncarnation for the happy path.
func (s *Stack) CreateIncarnationRaw(t *testing.T, name, serviceRef string, spec map[string]any) ([]byte, int) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{
		"name":    name,
		"service": stripServiceRef(serviceRef),
	}
	if spec != nil {
		body["input"] = spec
	}
	resp, status, err := c.post(context.Background(), "/v1/incarnations", body)
	if err != nil {
		t.Fatalf("CreateIncarnationRaw %s: http: %v", name, err)
	}
	return resp, status
}

// RunScenario runs a scenario on an existing incarnation.
//
// 202 -> returns apply_id from the response body. Any other status -> t.Fatal.
func (s *Stack) RunScenario(t *testing.T, incarnationName string, scenarioName string, input map[string]any) string {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incarnationName, scenarioName)
	// The same transient 422 "service ... not registered" as in
	// CreateIncarnation: serviceregistry.Holder refreshes its snapshot
	// asynchronously (pub/sub + 10s TTL). A direct incarnation seed
	// (SeedIncarnationReady) bypasses CreateIncarnation's polling, so the
	// first RunScenario can hit the cold snapshot window. We poll ONLY
	// this marker; any other 422 (input/required) is an immediate fatal,
	// no masking.
	var resp []byte
	var status int
	var err error
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, status, err = c.post(context.Background(), path, body)
		if err != nil {
			t.Fatalf("RunScenario %s/%s: http: %v", incarnationName, scenarioName, err)
		}
		if status == http.StatusUnprocessableEntity &&
			strings.Contains(string(resp), "not registered") &&
			time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
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

// WaitApplySuccess blocks until apply_runs.status becomes success for all
// rows of the run. PK apply_runs = (apply_id, sid) -> one run produces N
// rows (one per Soul host). Success condition: all rows are success; any
// row in failed/cancelled/orphaned/no_match -> fatal before success is
// reached.
//
// pre-running statuses (planned/claimed/dispatched/running) count as
// "in progress", waiting continues. Terminal != success -> immediate
// t.Fatal with a dump of the status matrix (no hoping it "resolves itself").
func (s *Stack) WaitApplySuccess(t *testing.T, applyID string, timeoutSec int) {
	t.Helper()
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		rows, err := s.db.Query(context.Background(),
			"SELECT sid, status FROM apply_runs WHERE apply_id = $1", applyID)
		if err != nil {
			t.Fatalf("WaitApplySuccess %s: query: %v", applyID, err)
		}
		statuses := map[string]string{}
		for rows.Next() {
			var sid, st string
			if err := rows.Scan(&sid, &st); err != nil {
				rows.Close()
				t.Fatalf("WaitApplySuccess %s: scan: %v", applyID, err)
			}
			statuses[sid] = st
		}
		rows.Close()
		if len(statuses) == 0 {
			time.Sleep(250 * time.Millisecond)
			continue
		}
		allSuccess := true
		for sid, st := range statuses {
			switch st {
			case "success":
				continue
			case "failed", "cancelled", "orphaned", "no_match":
				t.Fatalf("WaitApplySuccess %s: sid=%s reached terminal %q (statuses=%v)", applyID, sid, st, statuses)
			default:
				allSuccess = false
			}
		}
		if allSuccess {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("WaitApplySuccess %s: success not reached within %ds", applyID, timeoutSec)
}

// WaitIncarnationReady blocks until incarnation.status becomes `ready`.
//
// Why separate from WaitApplySuccess: apply_runs.status=success (per-host
// task barrier) is set EARLIER than the state_changes commit into
// incarnation.state — commitSuccess (run.go section 8) writes
// state+status='ready' in one PG transaction AFTER the barrier over all
// hosts. On smoke-nginx (2 tasks) the window is microscopic and
// AssertIncarnationState right after WaitApplySuccess passes; on a service
// with dozens of tasks (redis::create — 3 destinies) the window is wider,
// and reading state catches an empty `{}`. We wait specifically for
// status='ready' — the only point that guarantees state_changes is already
// in the DB. Mirrors the L3b harness (tests/e2e-live).
//
// Terminal != ready (error_locked / migration_failed / destroyed) ->
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
	t.Fatalf("WaitIncarnationReady %s: status=ready not reached within %ds (last status=%q)",
		incarnationName, timeoutSec, last)
}

// stripServiceRef strips `@<ref>` (if present). The Operator API creates an
// incarnation by bare service-name; the ref is resolved via the service
// registry (ADR-029). The harness spec passes `smoke-nginx@main` — for
// compatibility with `examples/service/<name>` (the package name matches
// the "service-name").
func stripServiceRef(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return ref[:i]
	}
	return ref
}

// DB returns the pool for the test (read-only for asserts). The caller must
// not Close it: the pool is managed by Cleanup.
func (s *Stack) DB() *pgxpool.Pool {
	return s.db
}
