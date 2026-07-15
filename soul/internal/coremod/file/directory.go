package file

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyDirectory implements state `directory`: declarative directory
// creation (replaces `core.exec.run install -d`). Idempotency:
//   - directory missing → create (MkdirAll if parents, else Mkdir) → changed=true;
//   - directory exists, owner/group/mode match → no-op (changed=false);
//   - directory exists but owner/group/mode drifted → fix (chmod/chown),
//     changed=true (parity with applyPresent);
//   - path exists but is NOT a directory (file/symlink) → error, no overwrite.
//
// recurse (recursively setting permissions on contents) is deliberately NOT
// implemented in the MVP — only the directory itself is managed.
func (m *Module) applyDirectory(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
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

// planDirectory is pure-read drift for state `directory` (ADR-031 Scry): the
// same stat + perm/ownership comparison as the start of applyDirectory, but
// without Mkdir/chmod/chown. A type conflict (path isn't a directory) is a
// plan error (util.PlanFailed), NOT a false-clean.
func (m *Module) planDirectory(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
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

// mkdir creates a directory: with parents, MkdirAll (intermediate
// directories, like `mkdir -p`), else Mkdir (missing parent → error). mode is
// applied adjusted by umask; the caller sets exact permissions via a separate
// chmod when mode is explicit.
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
