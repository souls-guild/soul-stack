//go:build integration

// L2 trial execution (ADR-023): design "Option A" (no real Keeper, no SSH).
// soul-trial renders plan in-process using the same Keeper-side render pipeline
// as L0 (renderCase), serializes ApplyRequest to protojson, delivers the soul
// binary + ApplyRequest to an ephemeral container (testcontainers-go) and executes
// `soul apply` (push-oneshot, ADR-004) with protojson redirected from file to stdin.
// From stdout, reads NDJSON stream of TaskEvent + final RunResult. Real core
// modules run in the container; vault-ref is resolved Keeper-side (fixture-vault
// as L0) — host receives ready ApplyRequest without vault references.
//
// Build-tag integration: default `make test` does not run L2 (requires docker +
// testcontainers). Run: `go test -tags integration -run TestL2 ./internal/trial/...`.
package trial

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	render "github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// containerSoulPath / containerReqPath — fixed paths in the stand where soul binary
// and serialized ApplyRequest are delivered.
const (
	containerSoulPath = "/usr/local/bin/soul"
	containerReqPath  = "/tmp/apply-request.json"
)

// L2Stand — ephemeral stand with delivered soul binary. Closed via Close
// (container terminate). Apply plan via Apply.
type L2Stand struct {
	ctr  testcontainers.Container
	soul string // path to soul binary inside container
}

// applyOutcome — parsed result of one `soul apply`: final RunResult + per-task
// register payloads (for idempotent check changed==false and verify-expect).
// exitCode — soul process exit code.
type applyOutcome struct {
	exitCode int
	result   *keeperv1.RunResult
	events   []*keeperv1.TaskEvent
	rawErr   string // soul stderr contents (for diagnostic on failure)
}

// StartL2Stand builds soul for linux/<stand arch> (= host GOARCH, container
// matches), starts container and delivers soul. Container stays alive (sleep infinity
// in init: none mode, systemd-PID1 in init: systemd mode) — execs run on top of it
// (soul apply each time as separate exec, like oneshot push-session).
func StartL2Stand(ctx context.Context, stand Stand) (*L2Stand, error) {
	soulBin, err := buildSoulLinux(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(soulBin)

	req, err := standRequest(stand)
	if err != nil {
		return nil, err
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("trial L2: start stand (init=%s): %w", stand.init(), err)
	}

	if err := ctr.CopyFileToContainer(ctx, soulBin, containerSoulPath, 0o755); err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("trial L2: deliver soul to stand: %w", err)
	}

	return &L2Stand{ctr: ctr, soul: containerSoulPath}, nil
}

// standRequest builds ContainerRequest for selected stand init mode.
//
//   - none (default): container on `sleep infinity` from stand.Image, no PID1-init
//     (current L2 pilot behavior, unchanged);
//   - systemd: systemd-PID1-stand from tests/e2e-live/dockerfiles/debian-12.Dockerfile
//     (same tuned image as L3b real-soul harness). Privileged profile +
//     CgroupnsMode=host + tmpfs /run,/run/lock + Entrypoint /sbin/init + wait for
//     `systemctl is-system-running --wait` (exit 0=running|1=degraded) — copy of
//     canonical harness.SpawnSoulContainer.
func standRequest(stand Stand) (testcontainers.ContainerRequest, error) {
	if stand.init() == StandInitNone {
		return testcontainers.ContainerRequest{
			Image:      stand.Image,
			Entrypoint: []string{"sleep", "infinity"},
			WaitingFor: wait.ForExec([]string{"true"}),
		}, nil
	}

	dockerfile, err := systemdStandDockerfile()
	if err != nil {
		return testcontainers.ContainerRequest{}, err
	}
	return testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    filepath.Dir(dockerfile),
			Dockerfile: filepath.Base(dockerfile),
			// Image determined by Dockerfile — cache layers between cases,
			// as L3b harness (KeepImage).
			KeepImage: true,
		},
		Entrypoint: []string{"/sbin/init"},
		HostConfigModifier: func(hc *dockercontainer.HostConfig) {
			hc.Privileged = true
			// systemd-PID1: tmpfs /run + /run/lock; CgroupnsMode=host — systemd
			// sees host cgroup-fs (necessary for systemctl).
			hc.CgroupnsMode = "host"
			if hc.Tmpfs == nil {
				hc.Tmpfs = map[string]string{}
			}
			hc.Tmpfs["/run"] = "rw"
			hc.Tmpfs["/run/lock"] = "rw"
		},
		// is-system-running: 0=running, 1=degraded (slim-Debian without units —
		// normal), 2=initializing (still waiting). Accept 0 and 1.
		WaitingFor: wait.ForExec([]string{"systemctl", "is-system-running", "--wait"}).
			WithExitCodeMatcher(func(code int) bool { return code == 0 || code == 1 }).
			WithStartupTimeout(60 * time.Second),
	}, nil
}

// systemdStandDockerfile returns path to L3b-Dockerfile of systemd-PID1-stand
// (tests/e2e-live/dockerfiles/debian-12.Dockerfile), reused from this package.
// repo-root derived via same runtime.Caller as soulModuleDir.
func systemdStandDockerfile() (string, error) {
	_, self, _, ok := runtimeCaller()
	if !ok {
		return "", fmt.Errorf("trial L2: could not determine package path")
	}
	// self = .../keeper/internal/trial/l2_run.go → repo-root = ../../../..
	trialDir := filepath.Dir(self)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(trialDir)))
	dockerfile := filepath.Join(repoRoot, "tests", "e2e-live", "dockerfiles", "debian-12.Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		return "", fmt.Errorf("trial L2: systemd-stand: Dockerfile not found at %s: %w", dockerfile, err)
	}
	return dockerfile, nil
}

// Close destroys the stand.
func (s *L2Stand) Close(ctx context.Context) error {
	return s.ctr.Terminate(ctx)
}

// containerErrPath — file inside stand where soul apply stderr is redirected.
// stdout stays clean (NDJSON only) so tcexec.Multiplexed combined-reader
// doesn't mix soul diagnostics with NDJSON stream.
const containerErrPath = "/tmp/apply-stderr.log"

// Apply serializes req to protojson, delivers to stand and executes
// `soul apply` with protojson redirected from file to stdin (testcontainers Exec
// doesn't pass stdin — redirecting from file inside container is equivalent).
// soul stderr sent to file so stdout carries only NDJSON. Parses NDJSON into outcome.
func (s *L2Stand) Apply(ctx context.Context, req *keeperv1.ApplyRequest) (applyOutcome, error) {
	var out applyOutcome

	payload, err := protojson.Marshal(req)
	if err != nil {
		return out, fmt.Errorf("trial L2: marshal ApplyRequest: %w", err)
	}
	if err := s.copyBytes(ctx, payload, containerReqPath); err != nil {
		return out, err
	}

	// soul apply reads ApplyRequest from stdin; redirect from delivered file —
	// behavior identical to push-session (Keeper writes protojson to stdin SSH-exec).
	// stderr → file: stdout stays clean NDJSON for Multiplexed-reader.
	cmd := []string{"sh", "-c", fmt.Sprintf("%s apply < %s 2> %s", s.soul, containerReqPath, containerErrPath)}
	code, stdout, err := s.exec(ctx, cmd)
	if err != nil {
		return out, fmt.Errorf("trial L2: exec soul apply: %w", err)
	}
	out.exitCode = code
	out.rawErr = s.readFile(ctx, containerErrPath)

	if err := parseNDJSON(stdout, &out); err != nil {
		return out, fmt.Errorf("trial L2: parse NDJSON soul apply (stderr: %s): %w", strings.TrimSpace(out.rawErr), err)
	}
	return out, nil
}

// copyBytes writes data to host temp file and delivers to stand at dst.
func (s *L2Stand) copyBytes(ctx context.Context, data []byte, dst string) error {
	tmp, err := os.CreateTemp("", "l2-apply-*.json")
	if err != nil {
		return fmt.Errorf("trial L2: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("trial L2: write temp: %w", err)
	}
	tmp.Close()
	if err := s.ctr.CopyFileToContainer(ctx, tmp.Name(), dst, 0o644); err != nil {
		return fmt.Errorf("trial L2: deliver %s to stand: %w", dst, err)
	}
	return nil
}

// exec runs cmd in stand and returns combined-output (tcexec.Multiplexed
// removes docker-stream headers itself). Caller redirects command stderr to
// file, so combined here = clean command stdout.
func (s *L2Stand) exec(ctx context.Context, cmd []string) (int, string, error) {
	code, reader, err := s.ctr.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return 0, "", err
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return code, buf.String(), fmt.Errorf("read output: %w", err)
	}
	return code, buf.String(), nil
}

// readFile reads file from stand via cat (best-effort, for diagnostics).
func (s *L2Stand) readFile(ctx context.Context, path string) string {
	_, out, err := s.exec(ctx, []string{"cat", path})
	if err != nil {
		return ""
	}
	return out
}

// RunL2Case runs one L2-case end-to-end (design Option A): render in-process →
// ApplyRequest → stand → soul apply → verify → expect_idempotent. caseFile —
// path to L2 case.yml (alongside scenario/<name>/main.yml). Returns Result
// (LevelL2, Pass + Failures). Stand is started and destroyed inside.
func RunL2Case(ctx context.Context, c *L2Case, caseFile string) (Result, error) {
	res := Result{Case: c.Name, Level: LevelL2}

	// 1. Render in-process using same Keeper-side path as L0. L2-case carries input:
	//    (not fixtures:), so map it to hermetic Fixtures.Input; rest of L2 pilot
	//    context is empty (one host, no essence/vault).
	l0 := &Case{Name: c.Name, Fixtures: Fixtures{Input: c.Input}}
	rc, err := renderCase(ctx, l0, caseFile)
	if err != nil {
		return res, err
	}

	stand, err := StartL2Stand(ctx, c.Stand)
	if err != nil {
		return res, err
	}
	defer func() { _ = stand.Close(ctx) }()

	// 2. Plan → ApplyRequest → stand.
	req := &keeperv1.ApplyRequest{
		ApplyId: "trial-l2-" + sanitizeID(c.Name),
		Tasks:   render.ToProtoTasks(rc.tasks),
	}
	first, err := stand.Apply(ctx, req)
	if err != nil {
		return res, err
	}
	if fail := assertRunSuccess("apply", first); fail != "" {
		res.Failures = append(res.Failures, fail)
		res.Pass = false
		return res, nil
	}

	// 3. verify-block: each check — single-task ApplyRequest on same stand.
	for _, v := range c.Verify {
		fails, err := stand.runVerify(ctx, req.ApplyId, v)
		if err != nil {
			return res, err
		}
		res.Failures = append(res.Failures, fails...)
	}

	// 4. expect_idempotent: re-run same ApplyRequest → all register.changed==false
	//    (host state converged, second apply — no-op).
	if c.expectIdempotent() {
		second, err := stand.Apply(ctx, req)
		if err != nil {
			return res, err
		}
		if fail := assertRunSuccess("idempotent-apply", second); fail != "" {
			res.Failures = append(res.Failures, fail)
		}
		res.Failures = append(res.Failures, assertNoChanges(second)...)
	}

	res.Pass = len(res.Failures) == 0
	return res, nil
}

// runVerify executes one verify-step as single-task ApplyRequest and compares
// task register-output with Expect. apply_id inherited from main run for
// traceability (verify — continuation of same stand session). Module specified
// by full name `<namespace>.<module>.<state>` (e.g. core.cmd.shell) — soul
// separates state via suffix (splitModuleAddr).
func (s *L2Stand) runVerify(ctx context.Context, applyID string, v Verify) ([]string, error) {
	params, err := structpb.NewStruct(v.Apply.Params)
	if err != nil {
		return nil, fmt.Errorf("trial L2: verify %q params: %w", v.Name, err)
	}
	req := &keeperv1.ApplyRequest{
		ApplyId: applyID + "-verify-" + sanitizeID(v.Name),
		Tasks: []*keeperv1.RenderedTask{{
			Name:   v.Name,
			Module: v.Apply.Module,
			Params: params,
		}},
	}
	out, err := s.Apply(ctx, req)
	if err != nil {
		return nil, err
	}
	return compareExpect(v, out), nil
}

// Conversion of render-plan to wire-form ApplyRequest.tasks — shared
// render.ToProtoTasks (keeper/internal/render/prototask.go), same as called by
// scenario-orchestrator. Index — orchestrator-only, not in proto; Module carries
// full name with state-suffix (soul splits state from RenderedTask).

// parseNDJSON parses stdout `soul apply` (NDJSON: one line per TaskEvent, final
// RunResult). Distinguishes by presence of apply_id+status: RunResult carries
// RunStatus, TaskEvent — TaskStatus+task_idx. Simple parse: try TaskEvent; line
// without task-fields but with status — RunResult. More reliable — by exclusive
// field: RunResult has state_changes/status only; TaskEvent — task_idx.
func parseNDJSON(stdout string, out *applyOutcome) error {
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// RunResult and TaskEvent both carry apply_id+status; distinguish by task_idx
		// (TaskEvent only). protojson ignores unknown fields with DiscardUnknown=false
		// → parse strictly into correct type, choosing by key.
		if strings.Contains(line, "\"taskIdx\"") || strings.Contains(line, "\"task_idx\"") || strings.Contains(line, "\"registerData\"") || strings.Contains(line, "\"register_data\"") {
			ev := &keeperv1.TaskEvent{}
			if err := protojson.Unmarshal([]byte(line), ev); err != nil {
				return fmt.Errorf("TaskEvent %q: %w", line, err)
			}
			out.events = append(out.events, ev)
			continue
		}
		rr := &keeperv1.RunResult{}
		if err := protojson.Unmarshal([]byte(line), rr); err != nil {
			return fmt.Errorf("RunResult %q: %w", line, err)
		}
		out.result = rr
	}
	if out.result == nil {
		return fmt.Errorf("no RunResult in stdout")
	}
	return nil
}

// assertRunSuccess checks that run completed with RUN_STATUS_SUCCESS and exit 0.
func assertRunSuccess(phase string, out applyOutcome) string {
	if out.result == nil {
		return fmt.Sprintf("%s: no RunResult (exit=%d, stderr: %s)", phase, out.exitCode, strings.TrimSpace(out.rawErr))
	}
	if out.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		return fmt.Sprintf("%s: status %s, expected SUCCESS (exit=%d, stderr: %s)",
			phase, out.result.GetStatus(), out.exitCode, strings.TrimSpace(out.rawErr))
	}
	if out.exitCode != 0 {
		return fmt.Sprintf("%s: exit %d with SUCCESS status", phase, out.exitCode)
	}
	return ""
}

// assertNoChanges checks that on second run no task marked changed==true
// (idempotency). Skipped tasks with changed==false — that's expected no-op
// of re-application.
func assertNoChanges(out applyOutcome) []string {
	var fails []string
	for _, ev := range out.events {
		if registerBool(ev.GetRegisterData(), "changed") {
			fails = append(fails, fmt.Sprintf(
				"idempotent: task idx=%d (%s) on second run changed=true — plan not idempotent",
				ev.GetTaskIdx(), ev.GetStatus()))
		}
	}
	return fails
}

// compareExpect compares verify-task register-output with Expect (exit_code /
// stdout / stdout_contains). One TaskEvent per verify-step (single-task req).
func compareExpect(v Verify, out applyOutcome) []string {
	var fails []string
	if len(out.events) == 0 {
		return []string{fmt.Sprintf("verify %q: no TaskEvent (stderr: %s)", v.Name, strings.TrimSpace(out.rawErr))}
	}
	rd := out.events[len(out.events)-1].GetRegisterData()

	if v.Expect.ExitCode != nil {
		got := registerInt(rd, "exit_code")
		if got != *v.Expect.ExitCode {
			fails = append(fails, fmt.Sprintf("verify %q: exit_code=%d, expected %d", v.Name, got, *v.Expect.ExitCode))
		}
	}
	if v.Expect.Stdout != nil {
		got := strings.TrimRight(registerString(rd, "stdout"), "\n")
		want := strings.TrimRight(*v.Expect.Stdout, "\n")
		if got != want {
			fails = append(fails, fmt.Sprintf("verify %q: stdout=%q, expected %q", v.Name, got, want))
		}
	}
	if v.Expect.StdoutContains != "" {
		got := registerString(rd, "stdout")
		if !strings.Contains(got, v.Expect.StdoutContains) {
			fails = append(fails, fmt.Sprintf("verify %q: stdout does not contain %q (got %q)", v.Name, v.Expect.StdoutContains, got))
		}
	}
	return fails
}

func registerBool(s *structpb.Struct, key string) bool {
	if s == nil {
		return false
	}
	v, ok := s.GetFields()[key]
	return ok && v.GetBoolValue()
}

func registerInt(s *structpb.Struct, key string) int {
	if s == nil {
		return 0
	}
	if v, ok := s.GetFields()[key]; ok {
		return int(v.GetNumberValue())
	}
	return 0
}

func registerString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	if v, ok := s.GetFields()[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

// sanitizeID converts arbitrary case/step name to safe id-fragment for
// apply_id (letters/digits/hyphen).
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "case"
	}
	return out
}

// buildSoulLinux builds soul binary for linux/<host GOARCH> to temp file,
// returns path to it (caller must delete). Container started matches host
// platform (docker desktop), so host GOARCH = stand arch. CGO_ENABLED=0 —
// static binary without glibc dependencies (works in any base image, incl. alpine).
func buildSoulLinux(ctx context.Context) (string, error) {
	soulMod, err := soulModuleDir()
	if err != nil {
		return "", err
	}
	bin, err := os.CreateTemp("", "soul-l2-*")
	if err != nil {
		return "", fmt.Errorf("trial L2: temp soul: %w", err)
	}
	bin.Close()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin.Name(), "./cmd/soul")
	cmd.Dir = soulMod
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(bin.Name())
		return "", fmt.Errorf("trial L2: go build soul (linux/%s): %w\n%s", runtime.GOARCH, err, stderr.String())
	}
	return bin.Name(), nil
}

// soulModuleDir finds soul/ module root relative to this package
// (keeper/internal/trial). go.work layout ADR-011: soul/ — sibling of keeper/.
func soulModuleDir() (string, error) {
	_, self, _, ok := runtimeCaller()
	if !ok {
		return "", fmt.Errorf("trial L2: could not determine package path")
	}
	// self = .../keeper/internal/trial/l2_run.go → repo-root = ../../../..
	trialDir := filepath.Dir(self)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(trialDir)))
	soulMod := filepath.Join(repoRoot, "soul")
	if _, err := os.Stat(filepath.Join(soulMod, "go.mod")); err != nil {
		return "", fmt.Errorf("trial L2: soul/ module not found at %s: %w", soulMod, err)
	}
	return soulMod, nil
}

// runtimeCaller — thin wrapper over runtime.Caller (import isolation for tests).
func runtimeCaller() (uintptr, string, int, bool) {
	return runtime.Caller(0)
}
