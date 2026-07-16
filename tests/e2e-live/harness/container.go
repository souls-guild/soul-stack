//go:build e2e_live

package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// SoulContainer — a wrapper over testcontainers.Container for a real-soul instance.
//
// SpawnSoulContainer fills in SID/BootstrapToken/Container and registers the
// container in Stack.SoulContainers + LIFO cleanup. Exec is then used for
// container-side asserts (L3b-4, stubs in asserts.go).
type SoulContainer struct {
	// SID — the Soul's FQDN name (e.g. `soul-live-a.example.com`). Echoed in
	// the gRPC payload; the authority is the mTLS peer cert.
	SID string

	// Container — a handle to testcontainers.Container. Used for Exec
	// (container-side asserts L3b-4) and Terminate (via Stack.Cleanup).
	Container testcontainers.Container

	// BootstrapToken — the plain SoulSeed token issued by the harness before
	// spawn. Passed into soul.yml inside the container; on first start the
	// soul agent does a CSR via the Keeper.Bootstrap RPC (mTLS server-only).
	BootstrapToken string
}

// Exec runs a command inside the soul container. Used by container-side
// asserts (AssertHostPkgInstalled / AssertHostServiceActive / ...) — L3b-4.
//
// Returns (stdout+stderr, exitCode, err). tcexec.Multiplexed demultiplexes
// the docker stream (8-byte frame headers) into plain text — without it the
// reader contains raw header bytes (`\x01\x00…\x07active`), and asserts that
// do an exact stdout comparison (e.g. AssertHostServiceActive: `== "active"`)
// would falsely fail. stdout and stderr are merged into one stream (the
// caller only needs the exit code + text for diagnostics).
func (sc *SoulContainer) Exec(ctx context.Context, cmd []string) (combined string, exitCode int, err error) {
	if sc == nil || sc.Container == nil {
		return "", -1, errors.New("SoulContainer.Exec: nil container")
	}
	code, reader, err := sc.Container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", code, fmt.Errorf("exec %v: %w", cmd, err)
	}
	body, readErr := io.ReadAll(reader)
	if readErr != nil {
		return string(body), code, fmt.Errorf("exec %v: read output: %w", cmd, readErr)
	}
	return string(body), code, nil
}

// soulStartupTimeout — the window from container spawn to souls.status='connected'.
// docker build (~60s cold build) + systemd-PID-1 boot (~3-10s) + soul init
// (CSR/Vault round-trip ~1s) + soul run dial (~1s) + first connect commit ~ 90s
// upper cap; usually 30-40s.
const soulStartupTimeout = 120 * time.Second

// SpawnSoulContainer brings up one real-soul container (Debian-12 systemd-PID-1),
// mounts the soul binary from the host, drops soul.yml + CA bundle, runs
// `soul init` (CSR Bootstrap flow → leaf cert), starts `soul run` in the
// background, and waits for keeper registration (souls.status='connected').
//
// Parameters:
//   - sid — FQDN, must match the cert's CN;
//   - bootstrapToken — a plain SoulSeed token (issued by IssueBootstrapToken before spawn).
//
// Side effects:
//   - the first invocation creates a docker user-bridge `soul-stack-e2e-live-*`
//     (used for inter-soul connectivity in multi-host L3b-5; in single-host
//     L3b-2 scenarios host.docker.internal to keeper is enough);
//   - the container is registered in Stack.cleanups (LIFO), Terminate is
//     called in Stack.Cleanup before the Postgres teardown.
func SpawnSoulContainer(t *testing.T, stack *Stack, sid, bootstrapToken string) *SoulContainer {
	t.Helper()
	if stack == nil {
		t.Fatal("SpawnSoulContainer: stack is nil")
	}

	ctx, cancel := context.WithTimeout(context.Background(), soulStartupTimeout)
	defer cancel()

	// 1. Pre-flight: the soul-linux binary must be built (`make build-linux`).
	soulBinPath, err := locateLinuxSoulBinary()
	if err != nil {
		t.Fatalf("SpawnSoulContainer: %v", err)
	}

	// 2. Lazy-create a shared user-bridge for all soul containers of this Stack.
	if stack.dockerNetwork == nil {
		nw, err := tcnetwork.New(ctx)
		if err != nil {
			t.Fatalf("SpawnSoulContainer: create network: %v", err)
		}
		stack.dockerNetwork = nw
		stack.cleanups = append(stack.cleanups, func() {
			toCtx, toCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer toCancel()
			_ = nw.Remove(toCtx)
		})
	}

	// 3. Lay out host-side bind mounts: soul binary + CA + soul.yml.
	mountRoot := filepath.Join(stack.tmpDir, "soul-"+sanitizeSID(sid))
	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		t.Fatalf("SpawnSoulContainer: mkdir mountRoot: %v", err)
	}
	caPath := filepath.Join(mountRoot, "ca.pem")
	if err := os.WriteFile(caPath, stack.caBundle, 0o644); err != nil {
		t.Fatalf("SpawnSoulContainer: write ca: %v", err)
	}
	soulYAMLPath := filepath.Join(mountRoot, "soul.yml")
	if err := os.WriteFile(soulYAMLPath, []byte(buildSoulYAML(stack)), 0o644); err != nil {
		t.Fatalf("SpawnSoulContainer: write soul.yml: %v", err)
	}

	// 4. ContainerRequest: privileged systemd-PID-1, /sys/fs/cgroup from the
	//    host, soul-binary read-only mount, soul.yml + CA via /etc/soul/.
	dockerfilePath, err := findDockerfile(t)
	if err != nil {
		t.Fatalf("SpawnSoulContainer: %v", err)
	}
	req := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:       filepath.Dir(dockerfilePath),
			Dockerfile:    filepath.Base(dockerfilePath),
			PrintBuildLog: false,
			KeepImage:     true, // same Dockerfile for all L3b tests — reuse layers.
		},
		Name:       fmt.Sprintf("soul-live-%s-%d", sanitizeSID(sid), time.Now().UnixNano()),
		Hostname:   sid,
		ExtraHosts: keeperExtraHosts(),
		Networks:   []string{stack.dockerNetwork.Name},
		Files: []testcontainers.ContainerFile{
			{HostFilePath: soulBinPath, ContainerFilePath: "/usr/local/bin/soul", FileMode: 0o755},
			{HostFilePath: caPath, ContainerFilePath: "/etc/soul/ca.pem", FileMode: 0o644},
			{HostFilePath: soulYAMLPath, ContainerFilePath: "/etc/soul/soul.yml", FileMode: 0o644},
		},
		HostConfigModifier: func(hc *dockercontainer.HostConfig) {
			hc.Privileged = true
			// systemd-PID-1 requires tmpfs /run + /run/lock; CgroupnsMode=host —
			// so systemd sees the host's cgroup fs (needed for systemctl).
			hc.CgroupnsMode = "host"
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs["/run"] = "rw"
			hc.Tmpfs["/run/lock"] = "rw"
		},
		// WaitingFor: systemd readiness — written to stdout during PID-1 boot.
		// "Started" fits most units; what we need is to wait until systemd
		// actually accepts commands (we then call Exec ourselves for
		// soul init/run, see below).
		WaitingFor: wait.ForExec([]string{"systemctl", "is-system-running", "--wait"}).
			WithExitCodeMatcher(func(code int) bool {
				// is-system-running returns 0 for `running`, 1 for `degraded`
				// (fine for us: degraded on a slim Debian without units is
				// normal), 2 for `initializing` (still waiting). Accept 0 and 1.
				return code == 0 || code == 1
			}).
			WithStartupTimeout(60 * time.Second),
	}

	cont, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("SpawnSoulContainer: generic container: %v", err)
	}
	stack.containers = append(stack.containers, cont)
	stack.cleanups = append(stack.cleanups, func() {
		toCtx, toCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer toCancel()
		_ = cont.Terminate(toCtx)
	})

	sc := &SoulContainer{
		SID:            sid,
		Container:      cont,
		BootstrapToken: bootstrapToken,
	}

	// 5. soul init — a real CSR Bootstrap flow.
	initOut, initCode, err := sc.Exec(ctx, []string{
		"/usr/local/bin/soul", "init",
		"--config", "/etc/soul/soul.yml",
		"--token", bootstrapToken,
		"--sid", sid,
	})
	if err != nil || initCode != 0 {
		t.Fatalf("SpawnSoulContainer: soul init: code=%d err=%v output=%s", initCode, err, initOut)
	}

	// 6. soul run — a background daemon. testcontainers Exec doesn't support
	//    detach, so we launch it via nohup inside a shell; stdout/stderr go to
	//    /var/log/soul.log for later inspection if the connect fails.
	runOut, runCode, err := sc.Exec(ctx, []string{
		"/bin/sh", "-c",
		"nohup /usr/local/bin/soul run --config /etc/soul/soul.yml " +
			">/var/log/soul.log 2>&1 </dev/null &",
	})
	if err != nil || runCode != 0 {
		t.Fatalf("SpawnSoulContainer: soul run launch: code=%d err=%v output=%s", runCode, err, runOut)
	}

	// 7. Wait souls.status='connected'.
	if err := waitForSoulConnected(ctx, stack, sid, 60*time.Second); err != nil {
		// Dump /var/log/soul.log to the test log for diagnostics.
		dump, _, _ := sc.Exec(context.Background(),
			[]string{"/bin/sh", "-c", "cat /var/log/soul.log 2>/dev/null | tail -n 100"})
		t.Fatalf("SpawnSoulContainer: %v\nsoul.log tail:\n%s", err, dump)
	}

	return sc
}

// waitForSoulConnected polls `souls.status` for sid, returns nil on the
// first 'connected'. Terminal statuses (revoked/expired/destroyed) → an
// immediate fail, don't wait for the timeout.
func waitForSoulConnected(ctx context.Context, stack *Stack, sid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var status string
		err := stack.db.QueryRow(ctx,
			"SELECT status FROM souls WHERE sid = $1", sid).Scan(&status)
		if err != nil {
			return fmt.Errorf("query souls(%s): %w", sid, err)
		}
		switch status {
		case "connected":
			return nil
		case "revoked", "expired", "destroyed":
			return fmt.Errorf("soul %s reached terminal status %q", sid, status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("soul %s did not reach status=connected within %v", sid, timeout)
}

// defaultKeeperHost — the host the soul container dials to reach keeper by
// default. On native-Linux CI it's resolved via the ExtraHosts host-gateway
// (see SpawnSoulContainer); this is the working default, don't break it.
const defaultKeeperHost = "host.docker.internal"

// keeperEndpointHostEnv — env override for the keeper-endpoint host used by
// the soul container.
//
// Why: on WSL2 + Docker Desktop, containers live in the DD VM while the
// keeper process runs in the WSL2 distro (different network namespaces).
// From inside the container, `host.docker.internal` resolves to the DD VM
// gateway (192.168.65.254), where keeper is NOT listening → bootstrap fails
// with `connection refused`. The real WSL2 host IP (first `hostname -I`,
// e.g. 172.27.x.x) is reachable from the container. The override writes this
// IP into soul.yml::keeper.endpoints[].host + adds it to the keeper cert's
// TLS SAN as well.
//
// If the env var is unset — default to host.docker.internal (native-Linux CI
// isn't broken). Run on WSL2 as:
// `E2E_KEEPER_HOST=$(hostname -I | awk '{print $1}') go test ...`.
const keeperEndpointHostEnv = "E2E_KEEPER_HOST"

// keeperEndpointHost returns the host the soul container dials to reach
// keeper: the E2E_KEEPER_HOST env value, or default host.docker.internal.
func keeperEndpointHost() string {
	if v := strings.TrimSpace(os.Getenv(keeperEndpointHostEnv)); v != "" {
		return v
	}
	return defaultKeeperHost
}

// keeperExtraHosts returns the ExtraHosts mapping for the soul container.
//
// We always keep the default `host.docker.internal:host-gateway` — on
// native-Linux the docker-desktop alias isn't set up by default, and the
// keeper endpoint uses it by default. On a name override (not an IP) we add
// `<host>:host-gateway` so the name resolves to the gateway. An IP override
// (the WSL2 case) doesn't need ExtraHosts — the container routes to the host
// IP directly.
func keeperExtraHosts() []string {
	hosts := []string{defaultKeeperHost + ":host-gateway"}
	if override := strings.TrimSpace(os.Getenv(keeperEndpointHostEnv)); override != "" &&
		override != defaultKeeperHost && net.ParseIP(override) == nil {
		hosts = append(hosts, override+":host-gateway")
	}
	return hosts
}

// buildSoulYAML renders soul.yml to run inside the container. All paths are
// container-side; the keeper endpoint is <host>:<port>, where host comes
// from keeperEndpointHost() (default host.docker.internal, resolved via the
// ExtraHosts host-gateway; on WSL2 — the real host IP via E2E_KEEPER_HOST).
func buildSoulYAML(stack *Stack) string {
	// metrics.enabled=true → soul brings up /metrics on loopback 127.0.0.1:9091
	// (default listen). The port is NOT published externally (no port
	// mapping) — scraped only container-side via Exec(curl). Needed by the
	// FC-3 test, which reads soul_apply_task_retries_total; harmless for the
	// other tests (loopback bind).
	const tmpl = `paths:
  seed: /var/lib/soul-stack/seed
  modules: /var/lib/soul-stack/modules
keeper:
  endpoints:
    - host: %s
      bootstrap_port: %d
      event_stream_port: %d
      priority: 1
  tls:
    ca: /etc/soul/ca.pem
logging:
  level: info
  format: text
metrics:
  enabled: true
hot_reload:
  enable_signal: false
  enable_inotify: false
`
	return fmt.Sprintf(tmpl, keeperEndpointHost(), stack.bootstrapPort, stack.eventStreamPort)
}

// findDockerfile returns the absolute path to the L3b Dockerfile. Relative
// lookup: `tests/e2e-live/dockerfiles/debian-12.Dockerfile` from the test's cwd.
func findDockerfile(t *testing.T) (string, error) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("findDockerfile: getwd: %w", err)
	}
	// Walk upward: the test may live in `tests/e2e-live/` or a subpackage.
	dir := wd
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "dockerfiles", "debian-12.Dockerfile")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("findDockerfile: debian-12.Dockerfile not found (wd=%s)", wd)
}

// sanitizeSID turns an FQDN into a slug suitable for a docker container name
// (length <128, [a-z0-9_.-]).
func sanitizeSID(sid string) string {
	s := strings.ReplaceAll(sid, ".", "-")
	s = strings.ReplaceAll(s, ":", "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}
