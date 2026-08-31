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
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
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

	// registered — service names RegisterService has put in the registry,
	// mapped to the example directory each was materialized from. Read only
	// by failNotRegistered, so that a "not registered" 422 which outlives the
	// warm-up window can name the omission instead of quoting a handler
	// (NIM-317).
	registered map[string]string

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

	// issuer — this stack's JWT `iss` / keeper `kid`. Unique per stack
	// (NIM-469): it used to be the constant `keeper-test-01` for every stack in
	// the suite, which made one stack's keeper indistinguishable from another's
	// on the wire. See buildKeeperYAML.
	issuer string

	// portReservations — listeners held open on the addresses written into
	// keeper.yml, released immediately before the keeper binds them. See
	// allocLoopback for why the reservation is held rather than dropped at
	// allocation time.
	portReservations []net.Listener

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

	// standContainers — the dependency containers by stand name, kept so a
	// FAILED bring-up can still be interrogated (state, exit code, logs) rather
	// than described only by the library's error string. See daemonhealth.go.
	standContainers map[string]testcontainers.Container

	// standRetriedAway — stands whose failed attempt this harness terminated
	// itself. Kept because the termination removes the container from
	// standContainers, and an absent entry otherwise reads as "never created" —
	// a claim about a container that did run, and whose logs the retry deleted.
	// See terminateStandAttempt and describeStandFailure in daemonhealth.go.
	standRetriedAway map[string]bool

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
// default `make build` output) and fails without one, BEFORE spawning
// testcontainers — the same answer missing docker already gets, and for the
// same reason: building with `-tags=e2e` is an explicit request for this tier,
// so a missing input is the harness failing, not a reason to report a pass.
//
// It skipped until NIM-533, and that skip was the tier's one way to certify
// work it never did. `go test` without `-v` prints `ok <pkg> 0.1s` for a
// package whose tests all skipped — byte-identical to one where they all
// passed — so a run that located no binary exited 0, satisfied every gate above
// it, and never reached scripts/classify-l3a-failure.py, which only runs on a
// non-zero status. The skip cost a developer without a build 0 seconds instead
// of a 5-minute timeout; a Fatalf here costs the same 0 seconds and says which
// input is missing. See TestMissingKeeperBinaryFailsTheTierInsteadOfSkipping.
//
// PRESENCE was the whole of that pre-flight until NIM-490, and presence is the
// weaker half of the question. The binary is a file left behind by the last
// `make build`, so a present one can predate the tree by weeks and the tier
// would answer confidently about code nobody is editing — the same false green
// NIM-533 closed from the other side, reached by running something rather than
// by running nothing. assertKeeperBinaryMatchesTree asks the binary which
// commit it carries: absent is a fatal missing input, wrong is a fatal refusal
// to answer at all (provenance.go).
func NewStack(t *testing.T, cfg Config) *Stack {
	t.Helper()
	if cfg.Souls <= 0 {
		cfg.Souls = 1
	}

	// Registered above the pre-flight, not below it: a stand that cannot be
	// built is a fact about the machine whichever line notices it first, and a
	// missing keeper binary is as much bring-up as a container that never
	// answers. Inside the region its refusal carries the STAND-SETUP marker;
	// one line higher it would arrive as a bare `--- FAIL: TestX` on all forty
	// tests at once — the single most misreadable shape this tier can produce.
	// Where the region ENDS is the load-bearing part; see setupdecl.go.
	infraUp := false
	defer declareStandSetupFailure(t, t.Failed(), &infraUp)

	binaryPath, err := locateKeeperBinary()
	if err != nil {
		t.Fatalf("L3a: keeper binary not found (%v); export KEEPER_BIN or run `make build`", err)
	}

	// Inside the declared region, one line below the presence check, because it
	// answers the same question that check does — "can this machine stand up a
	// valid L3a stand at all" — and a wrong binary is no more a finding about
	// the code than a missing one. NIM-490 first put it above the defer, on the
	// grounds that "your binary is from another tree" is an instruction and
	// STAND-SETUP reads as "ignore this one". That trade was the wrong way
	// round: the refusal's own text survives either placement, while outside the
	// region it arrives as forty unlabelled `--- FAIL:` lines, and forty of those
	// are read as forty findings.
	//
	// It execs the product and is still not a product entry point (setupdecl.go's
	// doors). `keeper version` reads the artifact's nameplate; it exercises no
	// behaviour this tier is testing, and there is no regression it can catch.
	// A helper that grew past that — one that called keeperBinaryPath, or drove
	// the binary to do something — would be derived as a door on the day it was
	// written, and TestDeclaredRegionsEndBeforeTheProductRuns would then demand
	// it move below `infraUp`. That is the correct answer for a helper that runs
	// the product, and the wrong one for this.
	assertKeeperBinaryMatchesTree(t, "L3a", binaryPath)

	s := &Stack{
		t:      t,
		cfg:    cfg,
		tmpDir: t.TempDir(),
	}

	// Teardown belongs to the stand from the moment the stand exists, not from
	// the moment the constructor manages to return one.
	//
	// The caller's `defer stack.Cleanup()` cannot cover this function: it is
	// registered on the value NewStack returns, and a t.Fatalf in here never
	// returns one. Everything below — IssueKeeperServerCert, RegisterSoulPreAuth,
	// InitVaultTestSecrets, runKeeperInit — is fatal-on-error and runs AFTER the
	// containers and, past `infraUp`, after the keeper subprocess. Each of those
	// paths leaked the whole stand, and on this tier a leaked stand is not merely
	// untidy: it is three containers and a process that the next thirty-nine
	// tests then share a daemon with. That is the mechanism behind "green alone,
	// red in company" (NIM-533).
	//
	// t.Cleanup rather than another s.runCleanups() at each fatal, because the
	// property has to hold for the fatal someone adds next year. It runs on
	// Goexit, so it covers every t.Fatalf in the dynamic extent — including the
	// ones in helpers this file does not know about — and Cleanup is idempotent,
	// so it costs nothing where an explicit teardown already ran.
	t.Cleanup(s.Cleanup)

	// Everything from the pre-flight to `infraUp = true` is docker, Vault and
	// the filesystem; a failure in it is a fact about the machine, not a
	// finding about the code, and it says so instead of arriving as a bare
	// `--- FAIL: TestX`. See setupdecl.go for why the region ends where it does.

	// Derived from the per-container budgets, never its own number: the stands
	// come up in sequence on this one ctx, so a flat literal here would cap them
	// all and the last in line would silently get the remainder.
	ctx, cancel := context.WithTimeout(context.Background(), standBringUpTimeout)
	defer cancel()

	// Through bringUpStand, not called directly: it is what makes a failed
	// bring-up say whether the daemon or the container was the thing that failed,
	// instead of surfacing testcontainers' `get state: … context deadline
	// exceeded` for a reader to interpret (NIM-533, daemonhealth.go).
	if err := s.bringUpStand(ctx, "postgres", s.startPostgres); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: %v", err)
	}
	if err := s.bringUpStand(ctx, "redis", s.startRedis); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: %v", err)
	}
	if err := s.bringUpStand(ctx, "vault", s.startVault); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: %v", err)
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

	// The third-party plumbing is up. Everything below runs this repo's own
	// binaries, so from here a failure is a finding and must not be labelled
	// infrastructure — every L3a test passes through this line, so a region that
	// swallowed `keeper init` would hide a regression across the whole tier at
	// once.
	infraUp = true

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

	// /readyz answered, but /readyz is anonymous — confirm it was OUR keeper
	// that answered before any test builds on the assumption (NIM-469).
	if err := s.assertOwnKeeper(); err != nil {
		s.runCleanups()
		t.Fatalf("NewStack: %v", err)
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

// terminateContainer tears one container down and says so when it fails.
//
// The three call sites used to discard the error outright (`_ = c.Terminate()`),
// which is why "do containers survive a run?" had no answer anywhere in the log
// (NIM-469): a container that refused to die left no trace at all, and every
// later test just started against a busier daemon. Cleanup still never fails a
// test — a teardown problem is not a verdict on the code under test — it simply
// stops being invisible.
func (s *Stack) terminateContainer(name string, c testcontainers.Container) {
	ctxTo, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Terminate(ctxTo); err != nil {
		s.t.Logf("[teardown] %s container did not terminate: %v — it stays on the daemon "+
			"for every test after this one", name, err)
	}
}

func (s *Stack) runCleanups() {
	// Idempotent, and needed for the stacks that never reached the keeper: a
	// bring-up that fails between reserving the ports and binding them would
	// otherwise leak the listeners for the rest of the run, holding addresses
	// the following tests still have to allocate from.
	s.releasePortReservations()
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
		// AndDeadline, not the bare WithWaitStrategy: that one is
		// WithWaitStrategyAndDeadline(60s, …) verbatim (options.go), so it would
		// re-wrap the strategy and discard the deadline the constructor set.
		testcontainers.WithWaitStrategyAndDeadline(standReadyTimeout, postgresWaitStrategy()),
	)
	// Adopt BEFORE looking at the error, not after. testcontainers returns a
	// live container alongside a failed wait strategy and says so in as many
	// words (generic.go: "At this point `c` might not be nil. Give the caller an
	// opportunity to call Destroy on the container."). Checking the error first
	// and returning is what leaked one container per failed stand, on exactly
	// the failure NIM-533 is about. The nil check is on the concrete pointer:
	// a nil *PostgresContainer forwarded into the interface parameter would be a
	// non-nil interface holding nil.
	if pgC != nil {
		s.adoptContainer("postgres", pgC)
	}
	if err != nil {
		return fmt.Errorf("postgres container: %w", err)
	}

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
		// Stated rather than inherited. Without this the module default's 10 s
		// applies — the shortest budget of the three, on checks that themselves
		// make docker-daemon round-trips.
		testcontainers.WithWaitStrategyAndDeadline(standReadyTimeout, redisWaitStrategy()),
	)
	// Adopted before the error check, for the reason spelled out in
	// startPostgres: a failed wait still leaves a container running.
	if rC != nil {
		s.adoptContainer("redis", rC)
	}
	if err != nil {
		return fmt.Errorf("redis container: %w", err)
	}

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
		ExposedPorts: []string{vaultContainerPort},
		Env: map[string]string{
			"VAULT_DEV_ROOT_TOKEN_ID":  rootToken,
			"VAULT_DEV_LISTEN_ADDRESS": "0.0.0.0:8200",
		},
		// Set directly rather than through a testcontainers option, so nothing
		// re-wraps it in the library's 60 s deadline.
		WaitingFor: vaultWaitStrategy(),
		// vault dev-mode wants IPC_LOCK / cap_add, otherwise it logs a
		// warning but still starts. Ignored in the test environment.
	}
	vc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	// Adopted before the error check, for the reason spelled out in
	// startPostgres: a failed wait still leaves a container running.
	if vc != nil {
		s.adoptContainer("vault", vc)
	}
	if err != nil {
		return fmt.Errorf("vault container: %w", err)
	}

	host, err := vc.Host(ctx)
	if err != nil {
		return fmt.Errorf("vault host: %w", err)
	}
	port, err := vc.MappedPort(ctx, vaultContainerPort)
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
	stdoutLog := &testLogWriter{t: s.t, prefix: "keeper-stdout"}
	stderrLog := &testLogWriter{t: s.t, prefix: "keeper-stderr"}
	cmd.Stdout = stdoutLog
	cmd.Stderr = stderrLog

	// Hand the reserved listener ports back to the kernel in the last instant
	// before the keeper binds them, so the window in which anything else on the
	// host can take one is milliseconds rather than the whole of `keeper init`.
	s.releasePortReservations()

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
		// Unconditional, and inside cleanup rather than after it: on the Kill
		// branch the Wait goroutine is still draining the pipes, and this is the
		// last moment at which the test is guaranteed to still exist.
		stdoutLog.stop()
		stderrLog.stop()
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
// if missing (the pre-flight in NewStack has already fatalled on it).
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
// The stop/done guard is not decoration (NIM-469). Cleanup gives the keeper 15s
// to exit on SIGINT, then Kills it and returns immediately — but `cmd.Wait()`
// runs in a goroutine that is still draining these pipes, so a keeper that takes
// its time dying writes into a test that has already finished. Go answers that
// with `panic: Log in goroutine after TestX has completed`, and the TestX it
// names is whichever test happens to be running by then: a bystander. One slow
// shutdown therefore kills an unrelated, innocent test — precisely the "green
// alone, red in company, a different test each time" shape this ticket is about.
type testLogWriter struct {
	// t is held through an interface so the stop guard is testable without a
	// finished *testing.T — the very state that cannot be staged on purpose.
	t      testLogger
	prefix string

	mu   sync.Mutex
	done bool
}

// testLogger is the one method testLogWriter needs from *testing.T.
type testLogger interface {
	Logf(format string, args ...any)
}

// stop detaches the writer from t. It must be called while the test is still
// alive — that is, from cleanup, before cleanup returns.
func (w *testLogWriter) stop() {
	w.mu.Lock()
	w.done = true
	w.mu.Unlock()
}

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		if w.done {
			// The test is gone, so t is off limits — but the output is not
			// dropped: a keeper too slow to die is usually a keeper with
			// something to say about why.
			fmt.Fprintf(os.Stderr, "[%s after test] %s\n", w.prefix, line)
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
// baseline incarnation.state (JSONB). Membership is NOT set here: the roster
// resolves via incarnation_membership (NIM-124); bind hosts separately with
// AddMember after this seed — never before it, the FK on incarnation(name)
// forbids it (NIM-210). To bootstrap a new incarnation THROUGH its create
// scenario use [Stack.CreateIncarnationOnRoster], which owns that whole order.
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
		INSERT INTO incarnation (name, service, service_version, state, status)
		VALUES ($1, $2, $3, $4::jsonb, 'ready')
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
	if status == http.StatusUnprocessableEntity && strings.Contains(string(resp), "not registered") {
		s.failNotRegistered(t, "CreateIncarnation "+name, service, resp)
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
//
// It is NOT the way to bootstrap an incarnation whose create scenario needs a
// roster it does not build itself: the run resolves its roster at start, and
// members cannot be bound before this call inserts the row (FK, migration 099)
// nor after it (the run has already started). That is a closed loop — use
// [Stack.CreateIncarnationOnRoster]. This call is for a create run that builds
// its own roster, i.e. the two no_hosts bypass classes of run.go §3: an
// all-keeper scenario, or one carrying a refresh emitter
// (create_roster_guard_test.go — the only path that reaches the pre-flight
// assert gate, NIM-235).
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
	if status == http.StatusUnprocessableEntity && strings.Contains(string(resp), "not registered") {
		s.failNotRegistered(t, "CreateIncarnationWithApply "+name, stripServiceRef(serviceRef), resp)
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
	if status == http.StatusUnprocessableEntity && strings.Contains(string(resp), "not registered") {
		s.failNotRegistered(t, fmt.Sprintf("RunScenario %s/%s", incarnationName, scenarioName), "", resp)
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

// RunScenarioRaw — low-level POST .../scenarios/{scenario}: returns
// (responseBody, statusCode) without checking the status, the run-path twin of
// [Stack.CreateIncarnationRaw]. For negative tests where the code IS the
// subject — e.g. the pre-flight assert gate answering 422 assert-failed before
// the run starts (NIM-270). Use [Stack.RunScenario] for the happy path.
//
// No transient-422 polling here: the caller is asserting on 422 itself, so
// swallowing one would hide the very thing under test. Register the service and
// let a prior RunScenario/CreateIncarnation warm the snapshot first.
func (s *Stack) RunScenarioRaw(t *testing.T, incarnationName, scenarioName string, input map[string]any) ([]byte, int) {
	t.Helper()
	c := s.opClient(t)
	body := map[string]any{}
	if input != nil {
		body["input"] = input
	}
	path := fmt.Sprintf("/v1/incarnations/%s/scenarios/%s", incarnationName, scenarioName)
	resp, status, err := c.post(context.Background(), path, body)
	if err != nil {
		t.Fatalf("RunScenarioRaw %s/%s: http: %v", incarnationName, scenarioName, err)
	}
	return resp, status
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
// Why separate from WaitApplySuccess: apply_runs.status=success is a PER-HOST
// task terminal, and a run is more than its host tasks. A `core.state.<verb>`
// capture ([ADR-0084]) is a keeper-side step committing its field at its own
// step, which for a capture standing after the host work is AFTER those hosts
// report success. On smoke-nginx (2 tasks) the window is microscopic and
// AssertIncarnationState right after WaitApplySuccess passes; on a service
// with dozens of tasks (redis::create — 3 destinies) the window is wider,
// and reading state catches an empty `{}`. We wait specifically for
// status='ready' — the run's own terminal, and the only point that guarantees
// every capture is already in the DB. Mirrors the L3b harness (tests/e2e-live).
//
// [ADR-0084]: docs/adr/0084-explicit-state-capture.md
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

// failNotRegistered reports a "service is not registered" 422 that outlived
// the registry warm-up poll, and it reports it as the test's own omission.
//
// Why this exists (NIM-317). ADR-029 made the service registry a precondition
// for CreateIncarnation, and four L3a tests were never taught: they went
// straight from NewStack to CreateIncarnation. The failure they produced was
// the handler's own sentence — "service service-hello-world is not registered
// (manage via service.* API, ADR-029)" — which reads like a keeper defect and
// says nothing about the missing harness call. Worse, the poll loop above
// spends 15 s first, so the test looks slow-and-broken rather than
// mis-written. Three of those four also named a service that no longer exists
// (the examples dropped the `service-` prefix), and the handler cannot tell
// "you forgot to register" from "you registered under a different name".
//
// This is the NIM-238 shape applied to a setup step: the point is not to
// document the required call harder, it is to make its absence name itself.
// Whoever writes the next L3a test gets the fix in the failure text.
// service may be empty when the caller does not carry a service ref
// (RunScenario knows only the incarnation).
func (s *Stack) failNotRegistered(t *testing.T, op, service string, body []byte) {
	t.Helper()
	t.Fatal(notRegisteredMessage(op, service, s.cfg.ExamplePath, s.registered, body))
}

// notRegisteredMessage is split out of [Stack.failNotRegistered] so the wording
// can be asserted without a Stack (and therefore without containers) — see
// not_registered_test.go. A diagnostic whose whole job is to be read is worth a
// test: it is the only thing standing between the next author and the 422 that
// sent four tests unnoticed for a release.
func notRegisteredMessage(op, service, examplePath string, registered map[string]string, body []byte) string {
	var b strings.Builder
	if service == "" {
		fmt.Fprintf(&b, "%s: the incarnation's service is not in the registry after the warm-up window.\n", op)
	} else {
		fmt.Fprintf(&b, "%s: service %q is not in the registry after the warm-up window.\n", op, service)
	}

	if len(registered) == 0 {
		name := service
		if name == "" {
			name = "<the name you register the example under>"
		}
		fmt.Fprintf(&b, "This Stack registered NOTHING — ADR-029 requires the service to exist before\n"+
			"an incarnation can be created on it. Add, before the first CreateIncarnation:\n"+
			"    stack.RegisterService(t, %q, %q)\n", name, examplePath)
	} else {
		fmt.Fprintf(&b, "This Stack registered %v, so the name does not match what the test asked for.\n"+
			"A service is named at registration and nowhere else (NIM-726 took `name:` out of\n"+
			"service.yml), so the name RegisterService was given is the only one there is, and\n"+
			"the service ref passed to CreateIncarnation/RunScenario must use that same name.\n",
			slices.Sorted(maps.Keys(registered)))
	}

	fmt.Fprintf(&b, "keeper answered: %s", string(body))
	return b.String()
}

// DB returns the pool for the test (read-only for asserts). The caller must
// not Close it: the pool is managed by Cleanup.
func (s *Stack) DB() *pgxpool.Pool {
	return s.db
}
