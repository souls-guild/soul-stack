// Package pkg implements the `core.pkg` core module ([ADR-015]).
//
// States:
//   - installed: package installed (optionally at a specific version).
//   - absent:    package removed.
//   - latest:    package installed and upgraded to the newest repository version.
//
// Backends: apt (Debian/Ubuntu), dnf (RHEL >= 8), yum (RHEL <= 7), apk (Alpine).
// apt calls are non-interactive and conffile-safe (see aptGet/aptInstall): the
// Soul agent's stdin is empty, so any debconf/dpkg prompt would hit EOF and
// fail the task. rpm-based backends (.rpmnew/.rpmsave instead of prompting)
// and apk are non-interactive by default.
// Backend is picked from the soulprint pkg_mgr fact (primary, ADR-018(b)) —
// the same source as CEL `soulprint.self.os.pkg_mgr`; on empty/unknown fact,
// falls back to runtime detection (`command -v` / `which`), see
// util.ResolvePkgMgr. The fact is injected by the Soul agent in-process via
// [Module.SetHostFacts] (util.SoulprintAware, Variant A) before Apply.
package pkg

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name is the canonical address prefix (core.<this>.<state>).
const Name = "core.pkg"

// Module implements sdk/module.SoulModule. Runner is swapped out in tests;
// production uses util.OSRunner{}.
//
// The same instance is reused for all install steps in a run (see
// coremod.Default), so refreshing the repository index (apt-get update /
// apk update) happens once per process lifetime — see indexMu/indexDone
// below. Option (b): cheap across many pkg tasks, no pointless update
// before every install.
type Module struct {
	Runner util.Runner

	// facts is the soulprint host snapshot, injected by the Soul agent
	// before Apply (SetHostFacts). Zero-value (empty pkg_mgr) → Apply falls
	// back to runtime detection (util.ResolvePkgMgr). No concurrent Apply on
	// one Soul (ADR-012(a)), so the field needs no extra synchronization.
	facts util.HostFacts

	indexMu   sync.Mutex
	indexDone bool // repository index already refreshed successfully this process
}

// New builds a Module with the production Runner. Used when wiring up the
// soul binary's registry.
func New() *Module { return &Module{Runner: util.OSRunner{}} }

// SetHostFacts implements util.SoulprintAware: ApplyRunner injects the
// collected soulprint host fact before calling Apply (Variant A, in-process).
func (m *Module) SetHostFacts(f util.HostFacts) { m.facts = f }

// Validate delegates known-state + required-param checks to
// shared/coremanifest/pkg.yaml (single source of truth shared with
// soul-lint). core.pkg has no cross-field invariants; value type checks
// happen in the Apply getters.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.pkg.Plan as pure-read (ADR-031 Scry): it reads
// the current package state and does NOT mutate the host. Marker for the
// host's default-deny gate: without it, Plan would never run on dry_run.
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads current package state
// (the same queryInstalled Apply uses at the start) and sends
// PlanEvent.changed — "would Apply change the package?". Does NOT mutate the
// host: no install/remove, no refreshIndex (apt-get update / apk update is a
// write to the repository index).
//
// installed/absent are supported — drift there is fully determined by
// queryInstalled (the same read Apply does before mutating). latest is NOT
// supported by Plan: "is there a newer version in the repo?" requires
// reading the index, which Apply doesn't do before mutating (it refreshes
// the index — a write), so a pure-read answer can't be derived from
// existing read logic. Returns an explicit failed PlanEvent (not
// false-clean); latest drift is Slice B's job.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	version, err := util.OptStringParam(req.Params, "version")
	if err != nil {
		return util.PlanFailed(err.Error())
	}

	mgr := util.ResolvePkgMgr(ctx, m.Runner, m.facts.PkgMgr)
	if mgr == util.PkgMgrUnknown {
		return util.PlanFailed("no supported package manager detected (apt/dnf/yum/apk)")
	}

	installed, curVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.PlanFailed(err.Error())
	}

	switch req.State {
	case "installed":
		// drift: package missing OR a version is pinned and differs from current.
		changed := !installed || (version != "" && curVer != version)
		return util.SendPlanFinal(stream, changed)
	case "absent":
		// drift: package is installed (Apply would remove it).
		return util.SendPlanFinal(stream, installed)
	case "latest":
		return util.PlanFailed("Plan(dry_run) for state latest is not supported: checking \"is there a newer version\" requires reading the repository index (Slice B)")
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// Apply is the main path. Idempotent: before running install, checks that
// the package is absent (for installed) / present (for absent).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	version, err := util.OptStringParam(req.Params, "version")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// pkg-mgr: soulprint fact is primary, runtime detection is fallback (BUG-B).
	mgr := util.ResolvePkgMgr(ctx, m.Runner, m.facts.PkgMgr)
	if mgr == util.PkgMgrUnknown {
		return util.SendFailed(stream, "no supported package manager detected (apt/dnf/yum/apk)")
	}

	switch req.State {
	case "installed":
		return m.applyInstalled(ctx, stream, mgr, name, version)
	case "absent":
		return m.applyAbsent(ctx, stream, mgr, name)
	case "latest":
		return m.applyLatest(ctx, stream, mgr, name)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyInstalled(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name, version string) error {
	installed, curVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if installed && (version == "" || curVer == version) {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
			"installed": true,
			"version":   curVer,
		})
	}
	if err := m.runInstall(ctx, mgr, name, version); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	_, newVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"installed": true,
		"version":   newVer,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name string) error {
	installed, _, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !installed {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
			"installed": false,
		})
	}
	if err := m.runRemove(ctx, mgr, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"installed": false,
	})
}

func (m *Module) applyLatest(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name string) error {
	beforeInstalled, beforeVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if err := m.runLatest(ctx, mgr, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	_, afterVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed := !beforeInstalled || beforeVer != afterVer
	return util.SendFinal(stream, changed, map[string]any{
		"name":      name,
		"installed": true,
		"version":   afterVer,
	})
}

// queryInstalled returns (installed, version, err). Version is best-effort:
// if the pkg mgr doesn't report it compactly, returns an empty string (this
// doesn't change the meaning of the installed flag).
func (m *Module) queryInstalled(ctx context.Context, mgr util.PkgMgr, name string) (bool, string, error) {
	switch mgr {
	case util.PkgMgrApt:
		// dpkg-query -W -f='${Status} ${Version}' name → "install ok installed 1.2.3"
		// exit 0 + the Status field starts with "install ok installed".
		r := m.Runner.Run(ctx, "dpkg-query", "-W", "-f=${Status} ${Version}", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("dpkg-query: %v", r.Err)
		}
		if r.ExitCode != 0 {
			return false, "", nil
		}
		return parseDpkgStatus(r.Stdout)
	case util.PkgMgrDnf, util.PkgMgrYum:
		// rpm -q --qf '%{VERSION}' name → version (exit 0) or "package … is not installed" (exit 1).
		r := m.Runner.Run(ctx, "rpm", "-q", "--qf", "%{VERSION}", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("rpm: %v", r.Err)
		}
		if r.ExitCode != 0 {
			return false, "", nil
		}
		return true, r.Stdout, nil
	case util.PkgMgrApk:
		// apk info -e name → package name (exit 0) or empty (also exit 0!).
		// So we use `apk info -e name` and check stdout instead.
		r := m.Runner.Run(ctx, "apk", "info", "-e", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("apk: %v", r.Err)
		}
		if r.ExitCode != 0 || r.Stdout == "" {
			return false, "", nil
		}
		// Version comes from a separate `apk info -ev` call (--exact
		// --verbose): for an installed package it prints exactly
		// `<name>-<version>` on one line (e.g. `nginx-1.26.3-r0`). MINOR-C:
		// without `-e` apk prints the package description ("nginx: HTTP and
		// reverse proxy server"), which would pollute the register's version
		// field with text instead of a number. parseApkVersion strips the
		// `<name>-` prefix → clean version number (`1.26.3-r0`).
		v := m.Runner.Run(ctx, "apk", "info", "-ev", name)
		return true, parseApkVersion(firstLine(v.Stdout), name), nil
	}
	return false, "", fmt.Errorf("queryInstalled: unsupported pkg mgr %q", mgr)
}

func (m *Module) runInstall(ctx context.Context, mgr util.PkgMgr, name, version string) error {
	if err := m.refreshIndex(ctx, mgr); err != nil {
		return err
	}
	target := name
	switch mgr {
	case util.PkgMgrApt:
		if version != "" {
			target = name + "=" + version
		}
		return m.aptInstall(ctx, target)
	case util.PkgMgrDnf:
		if version != "" {
			target = name + "-" + version
		}
		return m.must(ctx, "dnf", "install", "-y", target)
	case util.PkgMgrYum:
		if version != "" {
			target = name + "-" + version
		}
		return m.must(ctx, "yum", "install", "-y", target)
	case util.PkgMgrApk:
		if version != "" {
			target = name + "=" + version
		}
		return m.must(ctx, "apk", "add", "--no-cache", target)
	}
	return fmt.Errorf("runInstall: unsupported pkg mgr %q", mgr)
}

// refreshIndex updates the local repository index before install. On a
// fresh VM/container (cloud-create) the apt/apk index is empty or stale,
// and install without update hits "Unable to locate package". Modeled on
// Ansible's `apt: update_cache` (and the apk equivalent).
//
// Refresh runs once per process lifetime (indexDone): one Module instance
// serves all pkg tasks in a run, so running update before every install
// would be pointlessly expensive. The mutex guards against concurrent
// Apply steps; the flag is only set after a successful update, so a first
// failure doesn't poison the attempt for later steps.
//
// dnf/yum are NOT refreshed: yum/dnf auto-update metadata based on
// expiration (metadata_expire), and install pulls a fresh index itself
// when needed — an explicit update here is redundant and only slows the
// step down.
func (m *Module) refreshIndex(ctx context.Context, mgr util.PkgMgr) error {
	switch mgr {
	case util.PkgMgrApt, util.PkgMgrApk:
	default:
		return nil
	}

	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	if m.indexDone {
		return nil
	}

	var err error
	switch mgr {
	case util.PkgMgrApt:
		err = m.aptGet(ctx, "update")
	case util.PkgMgrApk:
		err = m.must(ctx, "apk", "update")
	}
	if err != nil {
		return err
	}
	m.indexDone = true
	return nil
}

func (m *Module) runRemove(ctx context.Context, mgr util.PkgMgr, name string) error {
	switch mgr {
	case util.PkgMgrApt:
		return m.aptGet(ctx, "remove", "-y", name)
	case util.PkgMgrDnf:
		return m.must(ctx, "dnf", "remove", "-y", name)
	case util.PkgMgrYum:
		return m.must(ctx, "yum", "remove", "-y", name)
	case util.PkgMgrApk:
		return m.must(ctx, "apk", "del", name)
	}
	return fmt.Errorf("runRemove: unsupported pkg mgr %q", mgr)
}

func (m *Module) runLatest(ctx context.Context, mgr util.PkgMgr, name string) error {
	if err := m.refreshIndex(ctx, mgr); err != nil {
		return err
	}
	switch mgr {
	case util.PkgMgrApt:
		// apt-get install --only-upgrade=yes name + install-if-missing semantics
		// would need two commands; instead we do install-without-version — apt
		// installs fresh or upgrades the existing package.
		return m.aptInstall(ctx, name)
	case util.PkgMgrDnf:
		return m.must(ctx, "dnf", "install", "-y", name)
	case util.PkgMgrYum:
		// yum update may or may not install if missing (version-dependent
		// behavior); install is more reliable.
		return m.must(ctx, "yum", "install", "-y", name)
	case util.PkgMgrApk:
		return m.must(ctx, "apk", "add", "--upgrade", name)
	}
	return fmt.Errorf("runLatest: unsupported pkg mgr %q", mgr)
}

// aptGet runs apt-get in non-interactive mode. Any apt/dpkg call on a
// managed host must be batch-safe: the Soul agent's stdin is empty, so an
// interactive debconf/dpkg prompt (service selection, conffile
// keep/replace, etc.) would hit "EOF on stdin" and fail the task.
// `DEBIAN_FRONTEND=noninteractive` puts debconf into non-interactive mode
// (no prompts, defaults are used).
//
// env is passed through the `env KEY=VAL apt-get …` wrapper, NOT via
// RunOptions.Env: the latter is a full replace of cmd.Env (see
// util.OSRunner.RunOpts), which would wipe PATH/HOME and break apt. The
// `env` wrapper adds one variable on top of the inherited environment
// without losing anything.
func (m *Module) aptGet(ctx context.Context, args ...string) error {
	full := append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get"}, args...)
	return m.must(ctx, "env", full...)
}

// aptInstall runs apt-get install for a specific target (name or
// `name=version`), non-interactively and conffile-safe.
//
// Dpkg::Options force-confdef + force-confold are key to re-apply
// robustness: our scenarios render a package's conffile (e.g.
// redis-sentinel → /etc/redis/sentinel.conf) BEFORE installing it, so on a
// subsequent apply dpkg sees an "operator-modified conffile" and would
// interactively ask "keep or replace?". force-confold = keep the file
// already on disk (our rendered one); force-confdef = take the maintainer
// default for any other conffile without asking. Without these flags, a
// conffile conflict fails install deterministically on every re-apply
// (files survive destroy).
func (m *Module) aptInstall(ctx context.Context, target string) error {
	return m.aptGet(ctx, "install", "-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		target)
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, oneLine(r.Stderr))
	}
	return nil
}

// parseDpkgStatus — "install ok installed 1.2.3-1ubuntu1" → installed=true, ver=...
// Any other Status (deinstall ok config-files, hold, etc) is treated as not installed.
func parseDpkgStatus(stdout string) (bool, string, error) {
	stdout = oneLine(stdout)
	const prefix = "install ok installed"
	if len(stdout) < len(prefix) || stdout[:len(prefix)] != prefix {
		return false, "", nil
	}
	rest := stdout[len(prefix):]
	for len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return true, rest, nil
}

// parseApkVersion extracts the clean version number from an
// `apk info -ev <name>` line (form `<name>-<version>`, e.g.
// `nginx-1.26.3-r0` → `1.26.3-r0`). Strips the `<name>-` prefix: the
// package name is known exactly (it was passed to apk), and apk names can
// contain hyphens (`py3-pip`), so splitting on hyphen is unreliable — we
// cut exactly the known prefix.
//
// Defensive: if the string doesn't start with `<name>-` (empty output,
// unexpected format), returns it as-is rather than losing the best-effort
// value (register version fields aren't critical, ADR-015 "best-effort").
func parseApkVersion(line, name string) string {
	prefix := name + "-"
	if rest, ok := strings.CutPrefix(line, prefix); ok {
		return rest
	}
	return line
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func oneLine(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, s[i])
	}
	// trim trailing spaces
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}
