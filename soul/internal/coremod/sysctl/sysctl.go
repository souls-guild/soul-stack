// Package sysctl implements the `core.sysctl` core module ([ADR-015]).
//
// States:
//   - present: a single kernel-param key `name` is set to `value`, persisted
//     to `/etc/sysctl.d/<filename>.conf`. Applied via `sysctl -w` (runtime) +
//     a file write (persist after reboot). Idempotent: reads the current
//     value via `sysctl -n <name>`; no-op if it already matches AND the
//     persist file has the same entry, otherwise updates both. `filename` is
//     optional (defaults to `<name>` with '.' replaced by '-').
//   - applied: a bulk `settings` map (key→value) is written as ONE
//     deterministic drop-in `/etc/sysctl.d/<filename>.conf` (sorted keys);
//     reload (`sysctl -p <file>`) only on file change. See applied.go.
//     Deliberate exception to the core.file boundary: the module owns
//     drop-in + reload + idempotency itself, [ADR-015] amend.
package sysctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.sysctl"

// SysctlDir is the sysctl persist-config directory. Overridden in unit tests.
const SysctlDir = "/etc/sysctl.d"

type Module struct {
	Runner util.Runner
	Dir    string
}

func New() *Module {
	return &Module{
		Runner: util.OSRunner{},
		Dir:    SysctlDir,
	}
}

// Validate delegates known-state + required-params (present: name/value;
// applied: settings/filename) to shared/coremanifest/sysctl.yaml (shared
// source with soul-lint). Cross-field guard on top of delegation: reload is a
// closed-set enum (auto|always|never) for state `applied`, symmetric with
// daemon_reload in core.service.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	if req.State == stateApplied {
		if _, err := reloadMode(req.Params); err != nil {
			errs = append(errs, err.Error())
		}
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.sysctl.Plan as pure-read (ADR-031 Scry): reads
// `sysctl -n` (read-only) + the persist file, never mutates (marker for
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads the current host state and
// reports whether Apply would change the host. Never mutates. Covers
// present/applied.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	switch req.State {
	case "present":
		return m.planPresent(req, stream)
	case stateApplied:
		return m.planApplied(req, stream)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent is the pure-read dry-run for state `present`: reads the current
// runtime value (`sysctl -n <name>`, read-only) plus the persist file's
// content. Never mutates: no `sysctl -w`, no persist-file write. `sysctl -n`
// is the same read-only call Apply uses for idempotency before writing.
func (m *Module) planPresent(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	value, err := util.StringParam(req.Params, "value")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	fname, err := util.OptStringParam(req.Params, "filename")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if fname == "" {
		fname = strings.ReplaceAll(name, ".", "-") + ".conf"
	}
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	path := filepath.Join(m.Dir, fname)

	// runtime read via `sysctl -n` — same call Apply uses before `-w`.
	r := m.Runner.Run(ctx, "sysctl", "-n", name)
	if r.Err != nil {
		return util.PlanFailed(fmt.Sprintf("sysctl -n: %v", r.Err))
	}
	want := normalizeSysctlValue(value)
	runtimeDrift := true
	if r.ExitCode == 0 {
		runtimeDrift = normalizeSysctlValue(r.Stdout) != want
	}
	if runtimeDrift {
		return util.SendPlanFinal(stream, true)
	}

	// persist-file comparison.
	wantLine := name + " = " + value + "\n"
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, string(existing) != wantLine)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read %s: %v", path, rerr))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case "present":
		return m.applyPresent(req, stream)
	case stateApplied:
		return m.applyApplied(req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	value, err := util.StringParam(req.Params, "value")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	fname, err := util.OptStringParam(req.Params, "filename")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if fname == "" {
		fname = strings.ReplaceAll(name, ".", "-") + ".conf"
	}
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	path := filepath.Join(m.Dir, fname)

	runtimeChanged, err := m.ensureRuntime(ctx, name, value)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	persistChanged, err := m.ensurePersist(path, name, value)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, runtimeChanged || persistChanged, map[string]any{
		"name":  name,
		"value": value,
		"path":  path,
	})
}

func (m *Module) ensureRuntime(ctx context.Context, name, value string) (bool, error) {
	r := m.Runner.Run(ctx, "sysctl", "-n", name)
	if r.Err != nil {
		return false, fmt.Errorf("sysctl -n: %v", r.Err)
	}
	if r.ExitCode == 0 {
		// sysctl -n can return "1\t0" (multi-value keys like tcp_keepalive);
		// normalize via Fields to not depend on tab vs space.
		current := normalizeSysctlValue(r.Stdout)
		want := normalizeSysctlValue(value)
		if current == want {
			return false, nil
		}
	}
	w := m.Runner.Run(ctx, "sysctl", "-w", name+"="+value)
	if w.Err != nil {
		return false, fmt.Errorf("sysctl -w: %v", w.Err)
	}
	if w.ExitCode != 0 {
		return false, fmt.Errorf("sysctl -w exited %d: %s", w.ExitCode, strings.TrimSpace(w.Stderr))
	}
	return true, nil
}

func (m *Module) ensurePersist(path, name, value string) (bool, error) {
	wantLine := name + " = " + value + "\n"
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == wantLine {
			return false, nil
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// fall through
	default:
		return false, fmt.Errorf("read %s: %v", path, rerr)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", m.Dir, err)
	}
	if err := os.WriteFile(path, []byte(wantLine), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %v", path, err)
	}
	return true, nil
}

// normalizeSysctlValue — sysctl shows tab-separated values for multi-value
// keys, but the user may write spaces. Collapse to a single space + trim so
// comparisons are meaningful.
func normalizeSysctlValue(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
