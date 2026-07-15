package util

import (
	"context"
	"fmt"
	"strings"
)

// ServiceActive is an init-agnostic check for whether service name is
// running. Single source of truth for core.service (active-state idempotency
// check) and the core-beacon `core.beacon.service_down` (activity
// monitoring). These consumers used to keep separate copies and drift apart
// (OpenRC false-up bug: `rc-service status` exits 0 even for a stopped service).
//
// Correct form per init system:
//   - systemd: `systemctl is-active --quiet` → exit 0 = active;
//   - OpenRC:  `rc-service <name> status` → exit 0 + stdout contains "started"
//     (exit 0 alone is NOT enough — stopped services can also exit 0; exit 3 = stopped);
//   - SysV:    `service <name> status` → exit 0 = active.
//
// Returns an error only if the runner failed to execute the command, or the
// init system isn't supported. A non-zero exit means "not active", not an
// error — that's a valid state.
func ServiceActive(ctx context.Context, runner Runner, init InitSystem, name string) (bool, error) {
	switch init {
	case InitSystemSystemd:
		r := runner.Run(ctx, "systemctl", "is-active", "--quiet", name)
		if r.Err != nil {
			return false, fmt.Errorf("systemctl is-active: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	case InitSystemOpenRC:
		r := runner.Run(ctx, "rc-service", name, "status")
		if r.Err != nil {
			return false, fmt.Errorf("rc-service status: %v", r.Err)
		}
		return r.ExitCode == 0 && strings.Contains(r.Stdout, "started"), nil
	case InitSystemSysV:
		r := runner.Run(ctx, "service", name, "status")
		if r.Err != nil {
			return false, fmt.Errorf("service status: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	}
	return false, fmt.Errorf("ServiceActive: unsupported init %q", init)
}

// DaemonReloadMode is the centralized daemon-reload mode for core.service
// (param `daemon_reload`, ADR-015 amendment). Closed set, applied before
// mutating actions (running/restarted/enabled).
type DaemonReloadMode string

const (
	// DaemonReloadAuto gates on the systemd NeedDaemonReload flag: reload
	// only when the unit file is out of sync with the loaded definition. Default.
	DaemonReloadAuto DaemonReloadMode = "auto"
	// DaemonReloadAlways always daemon-reloads before the action.
	DaemonReloadAlways DaemonReloadMode = "always"
	// DaemonReloadNever is an explicit opt-out: reload never runs.
	DaemonReloadNever DaemonReloadMode = "never"
)

// EnsureDaemonReloaded runs `systemctl daemon-reload` before a mutating
// core.service action (start/restart/enable), if mode and the init system
// require it. Fixes a bug: without daemon-reload after a unit-file edit,
// `systemctl restart` silently restarts with the OLD definition (exit 0,
// just a warning).
//
// Semantics per init system and mode:
//   - non-systemd (openrc/sysv) — no-op (false, nil): no daemon-reload concept;
//   - mode never — no-op (false, nil): explicit operator opt-out;
//   - mode always — unconditional `systemctl daemon-reload` (true, nil);
//   - mode auto — `systemctl show <name> --property=NeedDaemonReload --value`;
//     reloads (true) on `yes`, no-op (false) otherwise. On a fresh unit
//     install the flag is `no` (systemd picks up the definition on start) —
//     no reload needed.
//
// reloaded is returned for diagnostics (output["reloaded"]) and does NOT
// affect the step's changed: reload is a side effect of applying, not an
// independent service-state change. Runs through the same Runner as the
// module's other systemctl calls (mocked in unit tests).
func EnsureDaemonReloaded(ctx context.Context, runner Runner, init InitSystem, name string, mode DaemonReloadMode) (reloaded bool, err error) {
	if init != InitSystemSystemd || mode == DaemonReloadNever {
		return false, nil
	}
	if mode == DaemonReloadAuto {
		r := runner.Run(ctx, "systemctl", "show", name, "--property=NeedDaemonReload", "--value")
		if r.Err != nil {
			return false, fmt.Errorf("systemctl show NeedDaemonReload: %v", r.Err)
		}
		if strings.TrimSpace(r.Stdout) != "yes" {
			return false, nil
		}
	}
	r := runner.Run(ctx, "systemctl", "daemon-reload")
	if r.Err != nil {
		return false, fmt.Errorf("systemctl daemon-reload: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("systemctl daemon-reload exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}
