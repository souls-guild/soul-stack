// Package mount implements the core-module `core.mount` ([ADR-015]).
//
// States:
//   - present:   entry in /etc/fstab + mounted.
//   - absent:    unmounted + removed from /etc/fstab.
//   - mounted:   mounted "as is" (no fstab edit) — runtime-mount.
//   - unmounted: unmounted only (fstab entry stays, no autoremove).
//
// Idempotency: parse /etc/fstab, look up the entry by mount point. If it
// exists and source/fstype/opts match, fstab is left untouched. Current mount
// status via `findmnt --target <path>` (util-linux/busybox).
//
// Writing fstab is preserve-by-default (util.AtomicWritePreserving, the
// core.line pilot pattern, [ADR-015]): fstab is edited in place, its
// mode/owner are preserved, the module never resets them to 0644/process
// owner. The module doesn't accept owner/group as params for fstab — it
// always preserves the current ones.
package mount

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.mount"

// FstabPath — path to the canonical fstab; overridden in unit tests.
const FstabPath = "/etc/fstab"

// Module — sdk/module.SoulModule implementation for core.mount.
//
// LookupUser / LookupGroup are struct fields for testability (symmetric with
// core.line / core.repo); passed into util.AtomicWritePreserving. Since fstab
// doesn't accept owner/group as params, the lookup functions' override branch
// is never exercised — preserve restores ownership directly from uid/gid.
type Module struct {
	Runner      util.Runner
	FstabPath   string
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		Runner:      util.OSRunner{},
		FstabPath:   FstabPath,
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// Validate — known-state + per-state required-params (path; present/mounted →
// source/fstype) are delegated to shared/coremanifest/mount.yaml (single
// source shared with soul-lint). No cross-field invariants beyond per-state required.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares core.mount.Plan as pure-read (ADR-031 Scry): it reads
// findmnt and fstab, does NOT mutate the host (marker for the host, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): reads the current mount status
// (`findmnt --target`) + fstab line (the same read Apply does first) and sends
// PlanEvent.changed — "would Apply change the host?". Does NOT mutate: no
// mount/umount, no fstab write.
//
// `findmnt --target <path>` is a read-only call (just reads
// /proc/self/mountinfo), the same one Apply uses for idempotency. fstab is
// read via readFstabLines.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		return m.planPresent(ctx, stream, req, path)
	case "absent":
		return m.planAbsent(ctx, stream, path)
	case "mounted":
		return util.SendPlanFinal(stream, !m.isMounted(ctx, path))
	case "unmounted":
		return util.SendPlanFinal(stream, m.isMounted(ctx, path))
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent — pure-read drift for state present: drift = fstab entry is
// missing/differs OR path is not mounted. Same comparison read as applyPresent
// (upsertFstab + isMounted), minus the write and mount.
func (m *Module) planPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}
	want := fstabEntry{source: source, target: path, fstype: fstype, opts: opts, dump: "0", pass: "0"}
	wantLine := want.String()

	lines, rerr := readFstabLines(m.FstabPath)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	fstabMatch := false
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if !ok {
			continue
		}
		if parsed.target == want.target && line == wantLine {
			fstabMatch = true
			break
		}
	}
	if !fstabMatch {
		return util.SendPlanFinal(stream, true)
	}
	if !m.isMounted(ctx, path) {
		return util.SendPlanFinal(stream, true)
	}
	return util.SendPlanFinal(stream, false)
}

// planAbsent — pure-read drift for state absent: drift = path is mounted
// OR fstab contains an entry with this target.
func (m *Module) planAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], path string) error {
	if m.isMounted(ctx, path) {
		return util.SendPlanFinal(stream, true)
	}
	lines, rerr := readFstabLines(m.FstabPath)
	if rerr != nil {
		return util.PlanFailed(rerr.Error())
	}
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if ok && parsed.target == path {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(ctx, stream, req, path)
	case "absent":
		return m.applyAbsent(ctx, stream, path)
	case "mounted":
		return m.applyMounted(ctx, stream, req, path)
	case "unmounted":
		return m.applyUnmounted(ctx, stream, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}

	wantEntry := fstabEntry{source: source, target: path, fstype: fstype, opts: opts, dump: "0", pass: "0"}
	fstabChanged, err := m.upsertFstab(wantEntry)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mountChanged, err := m.ensureMounted(ctx, path, source, fstype, opts)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, fstabChanged || mountChanged, map[string]any{
		"path":     path,
		"source":   source,
		"fstype":   fstype,
		"mounted":  true,
		"in_fstab": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	unmountChanged, err := m.ensureUnmounted(ctx, path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	fstabChanged, err := m.removeFstab(path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, fstabChanged || unmountChanged, map[string]any{
		"path":     path,
		"mounted":  false,
		"in_fstab": false,
	})
}

func (m *Module) applyMounted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	source, _ := util.StringParam(req.Params, "source")
	fstype, _ := util.StringParam(req.Params, "fstype")
	opts, err := util.OptStringParam(req.Params, "opts")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if opts == "" {
		opts = "defaults"
	}
	changed, err := m.ensureMounted(ctx, path, source, fstype, opts)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mounted": true,
	})
}

func (m *Module) applyUnmounted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	changed, err := m.ensureUnmounted(ctx, path)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mounted": false,
	})
}

// ensureMounted: if findmnt sees path already mounted — no-op; otherwise runs
// `mount -t <fstype> -o <opts> <source> <path>`. Creates the mount point if
// needed (standard behavior).
func (m *Module) ensureMounted(ctx context.Context, path, source, fstype, opts string) (bool, error) {
	if m.isMounted(ctx, path) {
		return false, nil
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", path, err)
	}
	// `--` separates positional source/path from options (security review L1):
	// a source/path starting with `-` would otherwise be parsed by mount as an option.
	args := []string{"-t", fstype, "-o", opts, "--", source, path}
	r := m.Runner.Run(ctx, "mount", args...)
	if r.Err != nil {
		return false, fmt.Errorf("mount: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("mount %s: exit %d: %s", path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}

func (m *Module) ensureUnmounted(ctx context.Context, path string) (bool, error) {
	if !m.isMounted(ctx, path) {
		return false, nil
	}
	r := m.Runner.Run(ctx, "umount", "--", path)
	if r.Err != nil {
		return false, fmt.Errorf("umount: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("umount %s: exit %d: %s", path, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}

func (m *Module) isMounted(ctx context.Context, path string) bool {
	r := m.Runner.Run(ctx, "findmnt", "--target", path)
	return r.Err == nil && r.ExitCode == 0
}

// upsertFstab — reads FstabPath, looks for a line with the same target. If it
// matches exactly, fstab is left untouched. If it differs, replaces it. If
// missing, appends it. Returns changed.
func (m *Module) upsertFstab(want fstabEntry) (bool, error) {
	lines, err := readFstabLines(m.FstabPath)
	if err != nil {
		return false, err
	}
	wantLine := want.String()
	for i, line := range lines {
		parsed, ok := parseFstabLine(line)
		if !ok {
			continue
		}
		if parsed.target == want.target {
			if line == wantLine {
				return false, nil
			}
			lines[i] = wantLine
			return true, m.writeFstab(lines)
		}
	}
	lines = append(lines, wantLine)
	return true, m.writeFstab(lines)
}

func (m *Module) removeFstab(target string) (bool, error) {
	lines, err := readFstabLines(m.FstabPath)
	if err != nil {
		return false, err
	}
	out := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		parsed, ok := parseFstabLine(line)
		if ok && parsed.target == target {
			changed = true
			continue
		}
		out = append(out, line)
	}
	if !changed {
		return false, nil
	}
	return true, m.writeFstab(out)
}

type fstabEntry struct {
	source, target, fstype, opts, dump, pass string
}

func (e fstabEntry) String() string {
	dump, pass := e.dump, e.pass
	if dump == "" {
		dump = "0"
	}
	if pass == "" {
		pass = "0"
	}
	return fmt.Sprintf("%s %s %s %s %s %s", e.source, e.target, e.fstype, e.opts, dump, pass)
}

// parseFstabLine — fstab format: SOURCE TARGET FSTYPE OPTS DUMP PASS.
// Returns (entry, true) for data lines, (zero, false) for blank/comment lines.
func parseFstabLine(line string) (fstabEntry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return fstabEntry{}, false
	}
	f := strings.Fields(trimmed)
	if len(f) < 4 {
		return fstabEntry{}, false
	}
	e := fstabEntry{source: f[0], target: f[1], fstype: f[2], opts: f[3]}
	if len(f) >= 5 {
		e.dump = f[4]
	}
	if len(f) >= 6 {
		e.pass = f[5]
	}
	return e, true
}

func readFstabLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %v", path, err)
	}
	// strings.Split on "\n" leaves a trailing "" at the end; trim it.
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

// writeFstab atomically rewrites fstab with preserve-by-default: the existing
// file's mode and owner are preserved (core.line pilot pattern). The write is
// only called when content actually changed (upsert/remove returned
// changed=true) — an idempotent no-op leaves fstab untouched. owner/group are
// never passed (always ""), so preserve restores the original uid/gid; for a
// new fstab (didn't exist before) the default mode 0644 applies.
func (m *Module) writeFstab(lines []string) error {
	content := strings.Join(lines, "\n") + "\n"
	return util.AtomicWritePreserving(
		m.FstabPath, []byte(content),
		"", "", "", m.LookupUser, m.LookupGroup,
	)
}
