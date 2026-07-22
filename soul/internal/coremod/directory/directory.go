// Package directory implements the `core.directory` core module ([ADR-015]).
//
// States:
//   - present: directory exists with the given owner/group/mode (create +
//     drift-fix; parents = mkdir -p). Moved 1:1 from the former
//     core.file.directory state.
//   - absent:  directory removed. An empty directory is always removed; a
//     non-empty one only with explicit recursive:true (default false → error).
//     This deliberately never silently deletes a non-empty directory.
//
// Safety guards on absent: refuses protected system paths (`/`, `/etc`, …),
// refuses a symlink at path (never traverses it), type-conflict on a
// non-directory.
//
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
package directory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — canonical address top-level.
const Name = "core.directory"

// Module — sdk/module.SoulModule implementation for core.directory.
//
// Lookup{User,Group} are struct fields for testability: tests substitute
// functions that return fixed uid/gid without touching /etc/passwd.
// In production these are user.Lookup / user.LookupGroup.
type Module struct {
	// LookupUser / LookupGroup — substitution points for unit tests.
	// Defaults wrap user.* (see New()).
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)
}

func New() *Module {
	return &Module{
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
	}
}

// protectedRoots — cleaned absolute paths that absent refuses to remove. The
// path itself is denied; a child (e.g. /etc/foo) is allowed. Fail-closed guard
// against a templated empty variable collapsing to a system root.
var protectedRoots = map[string]struct{}{
	"/": {}, "/etc": {}, "/usr": {}, "/var": {}, "/home": {}, "/root": {},
	"/boot": {}, "/bin": {}, "/sbin": {}, "/lib": {}, "/lib64": {},
	"/proc": {}, "/sys": {}, "/dev": {}, "/opt": {}, "/mnt": {},
}

// Validate delegates known-state + required-param (path) checks to
// shared/coremanifest/directory.yaml (shared source with soul-lint). Author
// shape == runtime shape for directory (no rendered-style divergence), so one
// manifest is the source of truth.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares that core.directory.Plan is pure-read (ADR-031 Scry):
// it reads current directory state and does NOT mutate the host.
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads current directory state and
// reports whether Apply would change it, without mutating the host.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		return m.planPresent(stream, req, path)
	case "absent":
		return m.planAbsent(stream, req, path)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Defense-in-depth: reject relative paths (a Destiny-typo footgun resolved
	// against the Soul daemon's cwd). soul-lint enforces the same statically.
	if !filepath.IsAbs(path) {
		return util.SendFailed(stream, fmt.Sprintf("path must be absolute: %q", path))
	}
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, path)
	case "absent":
		return m.applyAbsent(stream, req, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// applyPresent implements state `present`: declarative directory creation
// (replaces `core.exec.run install -d`). Idempotency:
//   - directory missing → create (MkdirAll if parents, else Mkdir) → changed=true;
//   - directory exists, owner/group/mode match → no-op (changed=false);
//   - directory exists but owner/group/mode drifted → fix (chmod/chown), changed=true;
//   - path exists but is NOT a directory (file/symlink) → error, no overwrite.
//
// recurse (recursively setting permissions on contents) is deliberately NOT
// implemented in the MVP — only the directory itself is managed.
func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	parents, err := util.OptBoolParam(req.Params, "parents")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.SendFailed(stream, perr.Error())
	}

	created, modeChanged, ownerChanged := false, false, false

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if !info.IsDir() {
			// Path is occupied by a file/symlink — don't overwrite (parity
			// with `mkdir`, which fails on an existing file).
			return util.SendFailed(stream, fmt.Sprintf("path %s exists and is not a directory", path))
		}
		if modeStr != "" && info.Mode().Perm() != mode {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
			}
			modeChanged = true
		}
	case errors.Is(statErr, fs.ErrNotExist):
		mkErr := mkdir(path, mode, parents)
		if mkErr != nil {
			return util.SendFailed(stream, mkErr.Error())
		}
		created = true
		// MkdirAll/Mkdir apply mode adjusted by umask, so an explicit mode
		// needs a separate chmod to set exact permissions.
		if modeStr != "" {
			if cerr := os.Chmod(path, mode); cerr != nil {
				return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
			}
		}
	default:
		return util.SendFailed(stream, fmt.Sprintf("stat %s: %v", path, statErr))
	}

	if owner != "" || group != "" {
		changed, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
		ownerChanged = changed
	}

	changed := created || modeChanged || ownerChanged
	return util.SendFinal(stream, changed, map[string]any{
		"path":    path,
		"mode":    fmt.Sprintf("%04o", mode),
		"created": created,
	})
}

// planPresent is pure-read drift for state `present` (ADR-031 Scry): the same
// stat + perm/ownership comparison as the start of applyPresent, but without
// Mkdir/chmod/chown. A type conflict (path isn't a directory) is a plan error
// (util.PlanFailed), NOT a false-clean.
func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	modeStr, err := util.OptStringParam(req.Params, "mode")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	owner, err := util.OptStringParam(req.Params, "owner")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	group, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.PlanFailed(perr.Error())
	}

	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// Directory missing — Apply would create it (drift).
		return util.SendPlanFinal(stream, true)
	case statErr != nil:
		return util.PlanFailed(fmt.Sprintf("stat %s: %v", path, statErr))
	}

	if !info.IsDir() {
		return util.PlanFailed(fmt.Sprintf("path %s exists and is not a directory", path))
	}
	if modeStr != "" && info.Mode().Perm() != mode {
		return util.SendPlanFinal(stream, true)
	}
	if owner != "" || group != "" {
		drift, _, _, oerr := util.OwnershipDrift(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return util.PlanFailed(oerr.Error())
		}
		if drift {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

// applyAbsent implements state `absent`: remove a directory. An empty directory
// is always removed (os.Remove); a non-empty one only with recursive:true
// (os.RemoveAll), otherwise a fatal directory_not_empty error. Guards: refuses
// a protected system root, a symlink at path (never traversed), and a
// type-conflict on a non-directory. Idempotent: a missing path → changed=false.
func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	recursive, err := util.OptBoolParam(req.Params, "recursive")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	clean := filepath.Clean(path)
	if _, deny := protectedRoots[clean]; deny {
		return util.SendFailed(stream, fmt.Sprintf("refusing to remove protected system path %s", clean))
	}

	info, statErr := os.Lstat(clean)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return util.SendFinal(stream, false, map[string]any{"path": clean, "removed": false})
	case statErr != nil:
		return util.SendFailed(stream, fmt.Sprintf("lstat %s: %v", clean, statErr))
	}
	// Lstat does not follow the link: a symlink at path is never traversed.
	if info.Mode()&os.ModeSymlink != 0 {
		return util.SendFailed(stream, fmt.Sprintf("path %s is a symlink, not a directory", clean))
	}
	if !info.IsDir() {
		return util.SendFailed(stream, fmt.Sprintf("path %s exists and is not a directory", clean))
	}

	empty, eerr := dirIsEmpty(clean)
	if eerr != nil {
		return util.SendFailed(stream, eerr.Error())
	}
	switch {
	case empty:
		if rerr := os.Remove(clean); rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("remove %s: %v", clean, rerr))
		}
	case recursive:
		// os.RemoveAll removes in-tree symlinks as links (never follows them).
		if rerr := os.RemoveAll(clean); rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("remove -r %s: %v", clean, rerr))
		}
	default:
		return util.SendFailed(stream, fmt.Sprintf("directory %s is not empty (set recursive: true to remove its contents)", clean))
	}
	return util.SendFinal(stream, true, map[string]any{"path": clean, "removed": true})
}

// planAbsent is pure-read drift for state `absent` (ADR-031 Scry): mirrors
// applyAbsent's decision without deleting. missing → changed=false; empty or
// (non-empty && recursive) → changed=true; non-empty && !recursive →
// PlanFailed(directory_not_empty) (NOT false-clean — Apply would fail);
// symlink/file/protected-root → PlanFailed.
func (m *Module) planAbsent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	recursive, err := util.OptBoolParam(req.Params, "recursive")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	clean := filepath.Clean(path)
	if _, deny := protectedRoots[clean]; deny {
		return util.PlanFailed(fmt.Sprintf("refusing to remove protected system path %s", clean))
	}

	info, statErr := os.Lstat(clean)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, false)
	case statErr != nil:
		return util.PlanFailed(fmt.Sprintf("lstat %s: %v", clean, statErr))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return util.PlanFailed(fmt.Sprintf("path %s is a symlink, not a directory", clean))
	}
	if !info.IsDir() {
		return util.PlanFailed(fmt.Sprintf("path %s exists and is not a directory", clean))
	}

	empty, eerr := dirIsEmpty(clean)
	if eerr != nil {
		return util.PlanFailed(eerr.Error())
	}
	if empty || recursive {
		return util.SendPlanFinal(stream, true)
	}
	return util.PlanFailed(fmt.Sprintf("directory %s is not empty (set recursive: true to remove its contents)", clean))
}

// dirIsEmpty reports whether the directory at path has no entries, reading at
// most one name (no full listing).
func dirIsEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open %s: %v", path, err)
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("readdir %s: %v", path, err)
	}
	return false, nil
}

// mkdir creates a directory: with parents, MkdirAll (intermediate directories,
// like `mkdir -p`), else Mkdir (missing parent → error). mode is applied
// adjusted by umask; the caller sets exact permissions via a separate chmod
// when mode is explicit.
func mkdir(path string, mode fs.FileMode, parents bool) error {
	if parents {
		if err := os.MkdirAll(path, mode); err != nil {
			return fmt.Errorf("mkdir -p %s: %v", path, err)
		}
		return nil
	}
	if err := os.Mkdir(path, mode); err != nil {
		return fmt.Errorf("mkdir %s: %v", path, err)
	}
	return nil
}
