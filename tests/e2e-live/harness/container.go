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

	// The artifact mirror speaks https with a CA generated per run (NIM-542). Its
	// root has to be in the container's system trust store before the first
	// `core.url` fetch, or the create dies on an unknown authority — so the file
	// ships with the container and update-ca-certificates runs below, before the
	// soul is ever started. Absent when the test opted into real upstream
	// (Config.UpstreamArtifacts): then github's chain is the one in play and this
	// CA has no business in the store.
	containerFiles := []testcontainers.ContainerFile{
		{HostFilePath: soulBinPath, ContainerFilePath: "/usr/local/bin/soul", FileMode: 0o755},
		{HostFilePath: caPath, ContainerFilePath: "/etc/soul/ca.pem", FileMode: 0o644},
		{HostFilePath: soulYAMLPath, ContainerFilePath: "/etc/soul/soul.yml", FileMode: 0o644},
	}
	if stack.mirror != nil {
		mirrorCAPath := filepath.Join(mountRoot, artifactMirrorCAFile)
		if err := os.WriteFile(mirrorCAPath, stack.mirror.caPEM, 0o644); err != nil {
			t.Fatalf("SpawnSoulContainer: write artifact mirror CA: %v", err)
		}
		containerFiles = append(containerFiles, testcontainers.ContainerFile{
			HostFilePath:      mirrorCAPath,
			ContainerFilePath: artifactMirrorCADir + "/" + artifactMirrorCAFile,
			FileMode:          0o644,
		})
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
		Files:      containerFiles,
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
		// WaitingFor: systemd readiness, judged by the state systemctl printed
		// rather than by the code it returned — see soulWaitStrategy in
		// waitstrategy.go for why the exit code cannot tell a booted system from
		// one whose bus is not up yet, and what that cost. Everything below this
		// point talks to the container with Exec, so "ready" here is the
		// precondition for all of it.
		WaitingFor: soulWaitStrategy(),
	}

	// The stand is BUILT, and testcontainers resolves registry credentials for every
	// build — including for a Dockerfile whose only base image is public. Make sure a
	// credential helper that cannot run does not take the build down with it
	// (registry_auth.go, NIM-347).
	ensureRegistryAuthUsable()

	cont, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("SpawnSoulContainer: generic container: %v", annotateRegistryAuthError(err))
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

	// 4b. Trust the artifact mirror's CA. Dropping the .crt into
	//     /usr/local/share/ca-certificates is only half of it — Debian's system
	//     bundle (/etc/ssl/certs/ca-certificates.crt, what Go's SystemCertPool
	//     reads) is regenerated by this command and by nothing else.
	//
	//     The exit code is not the check. update-ca-certificates returns 0 for
	//     "0 added, 0 removed" just as happily as for "1 added" — a file under the
	//     wrong name, in the wrong directory, or holding a PEM it will not parse is
	//     skipped silently and successfully. What that costs is an x509 error
	//     inside the container twenty minutes later, read as a product defect. So
	//     the count in the output is the check, and the exit code is a formality
	//     next to it.
	if stack.mirror != nil {
		caOut, caCode, err := sc.Exec(ctx, []string{"update-ca-certificates"})
		if err != nil || caCode != 0 {
			t.Fatalf("SpawnSoulContainer: trust artifact mirror CA: code=%d err=%v output=%s", caCode, err, caOut)
		}
		switch added := caAddedCount(caOut); {
		case added < 0:
			t.Fatalf("SpawnSoulContainer: update-ca-certificates printed no `N added` line, so there is no\n"+
				"  way to tell whether it absorbed the mirror CA — and its exit code says nothing either.\n"+
				"  Treated as a failure rather than assumed fine: the alternative is an x509 unknown-authority\n"+
				"  error inside a create, read as a product defect. Output:\n%s", caOut)
		case added == 0:
			t.Fatalf("SpawnSoulContainer: update-ca-certificates absorbed no certificate (`0 added`).\n"+
				"  The mirror CA was shipped to %s/%s but the system bundle did not take it, and the\n"+
				"  command reported success anyway — a wrong directory, a wrong extension or a PEM it\n"+
				"  will not parse are all silent and all exit 0. The first https fetch will fail as an\n"+
				"  unknown authority against the product. Output:\n%s",
				artifactMirrorCADir, artifactMirrorCAFile, caOut)
		}

		// And now prove the container can actually USE it: reach the mirror and
		// validate its chain, from inside, before soul is started. Without this the
		// same two environment faults — a host firewall on the mirror's ephemeral
		// port, or a trust store that did not take — surface as a `core.url` fetch
		// error deep inside a create, where the classifier has no way to tell them
		// from a defect in the code (NIM-542 acceptance (c)).
		//
		// The health path is deliberately not counted as a mirror hit; see
		// artifactMirrorHealthPath.
		probeURL := stack.mirror.baseURL + artifactMirrorHealthPath
		probeOut, probeCode, err := sc.Exec(ctx, []string{
			"curl", "-sS", "--fail", "--max-time", "20", probeURL,
		})
		if err != nil || probeCode != 0 {
			// "still serving" would be an assertion nobody checked. Say what was
			// read: a nil serveError means the goroutine has not reported one, which
			// is not the same as the listener being healthy.
			serveErr := "no error reported by the serve goroutine"
			if e := stack.mirror.serveError(); e != nil {
				serveErr = e.Error()
			}
			// Printing `E2E_KEEPER_HOST=""` sends the reader looking for an empty
			// variable instead of at the default they are actually running on.
			advertise := keeperEndpointHost() + " (default; " + keeperEndpointHostEnv + " unset)"
			if v := strings.TrimSpace(os.Getenv(keeperEndpointHostEnv)); v != "" {
				advertise = v + " (from " + keeperEndpointHostEnv + ")"
			}
			t.Fatalf("SpawnSoulContainer: the container cannot reach or cannot trust the artifact mirror at %s.\n"+
				"  Nothing of the service has run yet — soul is not started, so this is not the product\n"+
				"  failing to fetch. Usual causes are a host firewall on the mirror's ephemeral port and an\n"+
				"  advertise host the container cannot route to: %s.\n"+
				"  code=%d err=%v mirror-server=%s output=%s",
				probeURL, advertise, probeCode, err, serveErr, probeOut)
		}
		// The body, not just the status. `--fail` already rejects a 404, so this is
		// not about reachability — it is the runtime half of the health-path
		// collision guard: if artifactMirrorHealthPath ever names a catalogued
		// tarball, the handler answers it from a literal and returns before the file
		// server, and the product silently receives one sentence of English where it
		// expected a tarball. Docker-free, that goes red in artifactmirror_test.go;
		// here, it goes red before soul starts instead of as an unpack failure
		// twenty minutes in.
		if !strings.Contains(probeOut, artifactMirrorHealthBody) {
			t.Fatalf("SpawnSoulContainer: the artifact mirror answered %s with %q, which is not the health\n"+
				"  body. Something other than the health handler is serving that path — most likely it now\n"+
				"  collides with a catalogued artifact, in which case that artifact is being shadowed and\n"+
				"  the product will be blamed for the unpack failure.",
				probeURL, probeOut)
		}
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
