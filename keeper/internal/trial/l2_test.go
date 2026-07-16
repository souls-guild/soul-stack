//go:build integration

// L2 integration test of Trial (ADR-023, design «Variant A»). Runs pilot case
// node-exporter end-to-end on docker stand: render in-process → ApplyRequest →
// soul apply in container → verify → expect_idempotent. Not included in default
// `make test` (build-tag integration). Run:
//
//	cd keeper && go test -tags integration -run TestL2 ./internal/trial/...
//
// Requires docker. Without docker and without SOUL_STACK_INTEGRATION_REQUIRE_DOCKER — skip;
// with flag — fatal (pattern of other keeper integration tests).
package trial

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// l2PilotCase — path to pilot case relative to this package (keeper/internal/trial).
// Layout ADR-023: <destiny>/_trial/scenario/<name>/tests/<case>/case.yml.
const l2PilotCase = "../../../examples/destiny/node-exporter/_trial/scenario/verify-l2/tests/run-and-probe"

// l2ReloadCases — daemon-reload L2 cases (systemd-PID1 stand). auto-reload —
// main guard (NeedDaemonReload-flag-flip reliable on debian-12); always-reload —
// deterministic duplicate (reload→new definition regardless of flag).
var l2ReloadCases = []string{
	"../../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload",
	"../../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/always-reload",
}

// dockerAvailable — best-effort probe of docker availability via short run attempt:
// real check done by StartL2Stand. Here only rough filter by presence of docker socket,
// to give meaningful skip without full fail.
func dockerAvailable() bool {
	for _, p := range []string{
		os.Getenv("DOCKER_HOST"),
		"/var/run/docker.sock",
		filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock"),
		filepath.Join(os.Getenv("HOME"), ".colima/default/docker.sock"),
	} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(strings.TrimPrefix(p, "unix://")); err == nil {
			return true
		}
	}
	return false
}

func TestL2_NodeExporterPilot(t *testing.T) {
	if !dockerAvailable() && !requireDocker() {
		t.Skip("L2: docker unavailable, SOUL_STACK_INTEGRATION_REQUIRE_DOCKER not set — skip")
	}

	// Absolute path: template resolver (securejoin) rejects root with '..'
	// (serviceRootFor derives svcRoot from caseFile; relative path gives '..').
	caseAbs, err := filepath.Abs(l2PilotCase)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	c, file, err := LoadL2Case(caseAbs)
	if err != nil {
		t.Fatalf("LoadL2Case: %v", err)
	}

	// go build soul (linux) + pull image + apply×2 + verify×3 — generous timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	res, err := RunL2Case(ctx, c, file)
	if err != nil {
		// docker truly unavailable (StartL2Stand failed on stand start):
		// without REQUIRE_DOCKER — skip, otherwise fail.
		if isDockerSetupErr(err) && !requireDocker() {
			t.Skipf("L2: stand failed to start (docker?): %v", err)
		}
		t.Fatalf("RunL2Case: %v", err)
	}

	if res.Level != LevelL2 {
		t.Errorf("Level = %v, expected LevelL2", res.Level)
	}
	if !res.Pass {
		t.Fatalf("L2 pilot case did not pass:\n  - %s", joinFailures(res.Failures))
	}
}

// TestL2_ServiceDaemonReload runs daemon-reload L2 cases on systemd-PID1
// stand (init: systemd). Proves fix of util.EnsureDaemonReloaded: after
// unit file rewrite core.service.restarted itself does daemon-reload and
// applies NEW definition (ExecStart=…2000) without manual reload.
//
// Skip if docker absent OR privileged/cgroup denied (rootless/sandbox):
// isDockerSetupErr recognizes privileged/cgroup/permission/timeout as
// setup error → t.Skip, not t.Fatal (no false red on environment without privileged).
func TestL2_ServiceDaemonReload(t *testing.T) {
	if !dockerAvailable() && !requireDocker() {
		t.Skip("L2: docker unavailable, SOUL_STACK_INTEGRATION_REQUIRE_DOCKER not set — skip")
	}

	for _, casePath := range l2ReloadCases {
		casePath := casePath
		t.Run(filepath.Base(casePath), func(t *testing.T) {
			caseAbs, err := filepath.Abs(casePath)
			if err != nil {
				t.Fatalf("filepath.Abs: %v", err)
			}
			c, file, err := LoadL2Case(caseAbs)
			if err != nil {
				t.Fatalf("LoadL2Case: %v", err)
			}

			// systemd-build (~60s cold) + boot + apply×N + verify — generous timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()

			res, err := RunL2Case(ctx, c, file)
			if err != nil {
				if isDockerSetupErr(err) && !requireDocker() {
					t.Skipf("L2: systemd stand failed to start (docker/privileged?): %v", err)
				}
				t.Fatalf("RunL2Case: %v", err)
			}
			if !res.Pass {
				t.Fatalf("daemon-reload L2 case %q did not pass:\n  - %s", c.Name, joinFailures(res.Failures))
			}
		})
	}
}

func isDockerSetupErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"starting stand", "docker", "cannot connect", "dial unix", "rootless",
		// systemd stand requires --privileged + CgroupnsMode=host: on rootless/
		// sandbox environment docker denies these options. We consider this
		// setup error (skip, not fatal): case must not give false red where
		// privileged unavailable. WaitingFor via systemctl on non-systemd host
		// will hit startup timeout → DeadlineExceeded (below).
		"privileged", "cgroup", "permission denied", "operation not permitted",
		"is-system-running", "starting container", "/sbin/init",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func joinFailures(fs []string) string {
	return strings.Join(fs, "\n  - ")
}
