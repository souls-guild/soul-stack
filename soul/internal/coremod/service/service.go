// Package service implements the `core.service` core module ([ADR-015]).
//
// States:
//   - running:   service is running. Optional param `enabled` (bool) also
//     manages autostart in the same step (true → enable, false → disable,
//     omitted → leave alone) — parallels Ansible's state=started enabled=yes.
//   - stopped:   service is stopped.
//   - restarted: unconditional restart (changed is always true).
//   - enabled:   autostart on boot (orthogonal to active state).
//
// Backend is chosen from the soulprint init_system fact (primary, ADR-018(b));
// the same source as CEL `soulprint.self.os.init_system`, so module and
// predicates agree on one init system. An empty/unknown fact falls back to
// runtime detection: systemd (`systemctl --version`) → openrc
// (`rc-service --version`) → sysvinit (`service --version`), see
// util.ResolveInitSystem. Idempotency via `is-active` / `is-enabled` (systemd)
// or OpenRC equivalents.
//
// The fact is injected in-process by the Soul agent via [Module.SetHostFacts]
// (util.SoulprintAware, Variant A) before Apply.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const Name = "core.service"

type Module struct {
	Runner util.Runner

	// facts is the soulprint host snapshot injected by the Soul agent before
	// Apply (SetHostFacts). Zero-value (empty init_system) falls back to
	// runtime detection (util.ResolveInitSystem). No concurrent Apply on one
	// Soul (ADR-012(a)), so no synchronization needed.
	facts util.HostFacts
}

func New() *Module { return &Module{Runner: util.OSRunner{}} }

// SetHostFacts implements util.SoulprintAware: ApplyRunner injects the
// collected soulprint host fact before calling Apply (Variant A, in-process).
func (m *Module) SetHostFacts(f util.HostFacts) { m.facts = f }

// Validate delegates known-state + required-param (name) checks to
// shared/coremanifest/service.yaml (shared source with soul-lint, dedup'd).
// Type-checks the optional `enabled` tri-bool (omitted/true/false) on top of
// delegation: this is an early type-guard the manifest-DSL can't express
// (value check, not literal) — non-bool `enabled` must be rejected at
// Validate, not silently ignored (see service_test.go).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	if _, _, err := util.TriBoolParam(req.Params, "enabled"); err != nil {
		errs = append(errs, err.Error())
	}
	// daemon_reload is a closed-set enum (auto|always|never); manifest-DSL
	// declares it for UI/linter, this runtime-guard rejects unknown values
	// (symmetric with tri-bool `enabled`).
	if _, err := daemonReloadMode(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// daemonReloadMode extracts and validates param `daemon_reload` (default
// auto). Absent/null → auto. An unknown string value errors (rejected at
// Validate). Applies only to mutating states (running/restarted/enabled); on
// stopped the param is ignored (manifest doesn't declare it there).
func daemonReloadMode(params *structpb.Struct) (util.DaemonReloadMode, error) {
	s, err := util.OptStringParam(params, "daemon_reload")
	if err != nil {
		return "", err
	}
	switch s {
	case "":
		return util.DaemonReloadAuto, nil
	case string(util.DaemonReloadAuto), string(util.DaemonReloadAlways), string(util.DaemonReloadNever):
		return util.DaemonReloadMode(s), nil
	default:
		return "", fmt.Errorf("param %q: unknown value %q (want auto|always|never)", "daemon_reload", s)
	}
}

// PlanReadSafe declares core.service.Plan as pure-read (ADR-031 Scry): reads
// is-active/is-enabled and never mutates the host (marker for default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads unit activity/autostart
// (same ServiceActive/isEnabled as the start of Apply) and reports whether
// Apply would change the service. Never mutates the host: no start/stop/
// restart, no enable/disable.
//
// restarted always reports drift=true: restart is unconditionally changed
// (Apply doesn't idempotent it, see applyRestarted), so dry-run honestly
// reports "will change".
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	init := util.ResolveInitSystem(ctx, m.Runner, m.facts.InitSystem)
	if init == util.InitSystemUnknown {
		return util.PlanFailed("no supported init system detected (systemd/openrc/sysv)")
	}

	switch req.State {
	case "running":
		return m.planRunning(ctx, stream, init, name, req)
	case "stopped":
		active, aerr := util.ServiceActive(ctx, m.Runner, init, name)
		if aerr != nil {
			return util.PlanFailed(aerr.Error())
		}
		// drift: service is active (Apply would stop it).
		return util.SendPlanFinal(stream, active)
	case "restarted":
		// restart is unconditionally changed=true (applyRestarted) — dry-run matches.
		return util.SendPlanFinal(stream, true)
	case "enabled":
		enabled, eerr := m.isEnabled(ctx, init, name)
		if eerr != nil {
			return util.PlanFailed(eerr.Error())
		}
		// drift: autostart is disabled (Apply would enable it).
		return util.SendPlanFinal(stream, !enabled)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planRunning computes pure-read drift for state running: drift = service not
// active OR (managing autostart and current enabled != want). Same
// ServiceActive/isEnabled checks as applyRunning, without start/enable/disable.
func (m *Module) planRunning(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], init util.InitSystem, name string, req *pluginv1.PlanRequest) error {
	wantEnabled, manageEnabled, err := util.TriBoolParam(req.Params, "enabled")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if !active {
		return util.SendPlanFinal(stream, true)
	}
	if manageEnabled {
		enabled, eerr := m.isEnabled(ctx, init, name)
		if eerr != nil {
			return util.PlanFailed(eerr.Error())
		}
		if enabled != wantEnabled {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// init system: soulprint fact is primary, runtime detection is fallback (BUG-B).
	init := util.ResolveInitSystem(ctx, m.Runner, m.facts.InitSystem)
	if init == util.InitSystemUnknown {
		return util.SendFailed(stream, "no supported init system detected (systemd/openrc/sysv)")
	}

	switch req.State {
	case "running":
		return m.applyRunning(ctx, stream, init, name, req)
	case "stopped":
		return m.applyStopped(ctx, stream, init, name)
	case "restarted":
		return m.applyRestarted(ctx, stream, init, name, req)
	case "enabled":
		return m.applyEnabled(ctx, stream, init, name, req)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// applyRunning ensures the service is running. Optional param `enabled`
// (tri-state, ADR-015, parallels Ansible `state=started enabled=yes`):
//
//	omitted — leave autostart alone (manage activity only);
//	true    — also enable the unit (autostart on boot);
//	false   — also disable the unit.
//
// changed=true if activity OR enabled state changed. enable/disable are
// idempotent via isEnabled: a repeat call on an already-correct unit doesn't
// mark changed.
func (m *Module) applyRunning(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	wantEnabled, manageEnabled, err := util.TriBoolParam(req.Params, "enabled")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload before start/enable: without it, systemd would start with a
	// stale unit definition after an edit. Doesn't mark the step changed (see
	// EnsureDaemonReloaded), just diagnostics in output.
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed := false
	if !active {
		if err := m.start(ctx, init, name); err != nil {
			return util.SendFailed(stream, err.Error())
		}
		changed = true
	}

	output := map[string]any{"name": name, "active": true}
	if manageEnabled {
		enabledChanged, err := m.ensureEnabled(ctx, init, name, wantEnabled)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
		changed = changed || enabledChanged
		output["enabled"] = wantEnabled
	}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, changed, output)
}

func (m *Module) applyStopped(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string) error {
	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !active {
		return util.SendFinal(stream, false, map[string]any{"name": name, "active": false})
	}
	if err := m.stop(ctx, init, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{"name": name, "active": false})
}

func (m *Module) applyRestarted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload before restart: without it, systemd restarts with the stale
	// unit. Doesn't affect changed (restarted is already unconditionally true).
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// restarted is unconditionally changed=true: the user explicitly asked for
	// a restart, e.g. after core.file.present updated a config and wants the
	// service to reread it. No idempotency here.
	if err := m.restart(ctx, init, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	output := map[string]any{"name": name, "active": true}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, true, output)
}

func (m *Module) applyEnabled(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload before enable: a new/changed unit must be picked up by
	// systemd before creating enable symlinks. Doesn't affect changed.
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed, err := m.ensureEnabled(ctx, init, name, true)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	output := map[string]any{"name": name, "enabled": true}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, changed, output)
}

// ensureEnabled idempotently brings the unit's autostart state to want (true =
// enable, false = disable): reads isEnabled first, acts only on drift. Returns
// changed. Shared by state `enabled` and param `enabled` in state `running`.
func (m *Module) ensureEnabled(ctx context.Context, init util.InitSystem, name string, want bool) (bool, error) {
	enabled, err := m.isEnabled(ctx, init, name)
	if err != nil {
		return false, err
	}
	if enabled == want {
		return false, nil
	}
	if want {
		return true, m.enable(ctx, init, name)
	}
	return true, m.disable(ctx, init, name)
}

func (m *Module) isEnabled(ctx context.Context, init util.InitSystem, name string) (bool, error) {
	switch init {
	case util.InitSystemSystemd:
		r := m.Runner.Run(ctx, "systemctl", "is-enabled", "--quiet", name)
		if r.Err != nil {
			return false, fmt.Errorf("systemctl is-enabled: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	case util.InitSystemOpenRC:
		// rc-update show default | grep -q name
		r := m.Runner.Run(ctx, "rc-update", "show", "default")
		if r.Err != nil {
			return false, fmt.Errorf("rc-update show: %v", r.Err)
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == name {
				return true, nil
			}
		}
		return false, nil
	case util.InitSystemSysV:
		// chkconfig --list name → exit 0 if present.
		r := m.Runner.Run(ctx, "chkconfig", "--list", name)
		if r.Err != nil {
			return false, fmt.Errorf("chkconfig: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	}
	return false, fmt.Errorf("isEnabled: unsupported init %q", init)
}

func (m *Module) start(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "start")
}
func (m *Module) stop(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "stop")
}
func (m *Module) restart(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "restart")
}

func (m *Module) svcAction(ctx context.Context, init util.InitSystem, name, action string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", action, name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-service", name, action)
	case util.InitSystemSysV:
		return m.must(ctx, "service", name, action)
	}
	return fmt.Errorf("svcAction: unsupported init %q", init)
}

func (m *Module) enable(ctx context.Context, init util.InitSystem, name string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", "enable", name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-update", "add", name, "default")
	case util.InitSystemSysV:
		return m.must(ctx, "chkconfig", name, "on")
	}
	return fmt.Errorf("enable: unsupported init %q", init)
}

func (m *Module) disable(ctx context.Context, init util.InitSystem, name string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", "disable", name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-update", "del", name, "default")
	case util.InitSystemSysV:
		return m.must(ctx, "chkconfig", name, "off")
	}
	return fmt.Errorf("disable: unsupported init %q", init)
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}
