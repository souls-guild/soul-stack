package sysctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const stateApplied = "applied"

// reloadMode extracts and validates the `reload` param (default auto). Reuses
// the closed-set util.DaemonReloadMode vocabulary (auto|always|never) — the
// same set as `daemon_reload` on core.service ([ADR-015] amend), but sysctl's
// "apply" semantics differ: reload = `sysctl -p <file>` (scoped to the
// drop-in), not systemctl daemon-reload. Missing/null → auto. An unknown
// string value → error (rejected at Validate, symmetric with core.service).
func reloadMode(params *structpb.Struct) (util.DaemonReloadMode, error) {
	s, err := util.OptStringParam(params, "reload")
	if err != nil {
		return "", err
	}
	switch s {
	case "":
		return util.DaemonReloadAuto, nil
	case string(util.DaemonReloadAuto), string(util.DaemonReloadAlways), string(util.DaemonReloadNever):
		return util.DaemonReloadMode(s), nil
	default:
		return "", fmt.Errorf("param %q: unknown value %q (want auto|always|never)", "reload", s)
	}
}

// applyApplied handles state `applied`: the bulk `settings` set is
// materialized as ONE deterministic drop-in `/etc/sysctl.d/<filename>.conf`
// (sorted keys), reloaded via `sysctl -p <file>` scoped to the drop-in (NOT
// the whole --system). Reload gating (see shouldReload):
//
//   - never → reload NEVER runs (explicit opt-out, even on file-change);
//   - always → reload unconditionally;
//   - auto → reload only on file-change (like daemon_reload:auto on a unit change);
//   - the reload itself does NOT mark `changed` (changed = drop-in was written).
//
// `ignore_failures` → `sysctl -e -p <file>` (-e/--ignore silences read-only/
// nonexistent keys in containers; explicit operator opt-in).
func (m *Module) applyApplied(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	settings, err := util.OptStringMapParam(req.Params, "settings")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if settings == nil {
		return util.SendFailed(stream, `param "settings": missing`)
	}
	fname, err := util.StringParam(req.Params, "filename")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, err := reloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	ignoreFailures, err := util.OptBoolParam(req.Params, "ignore_failures")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	path := dropInPath(m.Dir, fname)

	// Empty set (len==0): early no-op. Don't write an empty drop-in and don't
	// reload (general-purpose edge case: a bulk task with no params has
	// nothing to apply; an empty /etc/sysctl.d/<f>.conf would be junk, and
	// reloading it is pointless). changed=false: the "no params" state is
	// already satisfied by not writing anything. Symmetric with ensureDropIn's
	// idempotent branch (no change → no reload).
	if len(settings) == 0 {
		return util.SendFinal(stream, false, map[string]any{
			"path":     path,
			"settings": 0,
		})
	}

	want := renderDropIn(settings)

	changed, err := m.ensureDropIn(path, want)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if shouldReload(mode, changed) {
		if err := m.reloadDropIn(ctx, path, ignoreFailures); err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	return util.SendFinal(stream, changed, map[string]any{
		"path":     path,
		"settings": len(settings),
	})
}

// shouldReload gates reload by mode (never opt-out → always false; always →
// true; auto → only on file-change).
func shouldReload(mode util.DaemonReloadMode, changed bool) bool {
	switch mode {
	case util.DaemonReloadNever:
		return false
	case util.DaemonReloadAlways:
		return true
	default: // auto
		return changed
	}
}

// planApplied is the pure-read drift check for state `applied` (ADR-031
// Scry): compares the desired deterministic drop-in content against the
// existing file WITHOUT writing or reloading. drift = file missing / content differs.
func (m *Module) planApplied(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	settings, err := util.OptStringMapParam(req.Params, "settings")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if settings == nil {
		return util.PlanFailed(`param "settings": missing`)
	}
	fname, err := util.StringParam(req.Params, "filename")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if _, err := reloadMode(req.Params); err != nil {
		return util.PlanFailed(err.Error())
	}

	// Empty set → no-op (drift=false), symmetric with applyApplied: nothing to
	// apply, no drop-in gets written, so there's no drift either.
	if len(settings) == 0 {
		return util.SendPlanFinal(stream, false)
	}

	path := dropInPath(m.Dir, fname)
	want := renderDropIn(settings)

	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, string(existing) != want)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read %s: %v", path, rerr))
	}
}

// ensureDropIn writes the drop-in atomically only on drift (content differs /
// file missing). Preserve-by-default mode for an existing file; a new file
// gets 0644 (sysctl.d convention). changed=true only on an actual write.
func (m *Module) ensureDropIn(path, want string) (bool, error) {
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == want {
			return false, nil
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// fall through — create it.
	default:
		return false, fmt.Errorf("read %s: %v", path, rerr)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", m.Dir, err)
	}
	if err := util.AtomicWritePreserving(path, []byte(want), "", "", "", user.Lookup, user.LookupGroup); err != nil {
		return false, err
	}
	return true, nil
}

// reloadDropIn applies the drop-in scoped via `sysctl -p <file>` (NOT the
// whole --system). `ignore_failures` adds `-e` (silences read-only/nonexistent
// keys). argv, no shell.
func (m *Module) reloadDropIn(ctx context.Context, path string, ignoreFailures bool) error {
	args := make([]string, 0, 3)
	if ignoreFailures {
		args = append(args, "-e")
	}
	args = append(args, "-p", path)
	r := m.Runner.Run(ctx, "sysctl", args...)
	if r.Err != nil {
		return fmt.Errorf("sysctl -p: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("sysctl -p exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// dropInPath builds the drop-in path under /etc/sysctl.d. `filename` is
// required for the bulk state (required in the manifest); the `.conf` suffix
// is appended automatically, same as present's filename. filepath.Join keeps
// the write inside m.Dir.
func dropInPath(dir, fname string) string {
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	return filepath.Join(dir, fname)
}

// renderDropIn builds deterministic drop-in content from a map: keys are
// sorted (stable order across runs → no false change/repeat reload), lines
// formatted as `key = value` (sysctl.d syntax). Trailing newline follows the
// POSIX text-config convention.
func renderDropIn(settings map[string]string) string {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(settings[k])
		b.WriteByte('\n')
	}
	return b.String()
}
