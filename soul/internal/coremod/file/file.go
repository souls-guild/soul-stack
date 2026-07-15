// Package file implements the `core.file` core module ([ADR-015]).
//
// MVP states:
//   - present:   file exists with given content (inline content OR a copy
//     from the host via src — mutually exclusive) + mode/owner/group.
//   - absent:    file removed.
//   - rendered:  file = output of rendering a text/template template ([ADR-010]).
//     Keeper puts literal template_content + CEL-rendered vars into params,
//     the Soul side renders it via shared/tmpl (see rendered.go).
//   - directory: directory exists with given owner/group/mode (see
//     directory.go); a declarative replacement for `core.exec.run install -d`.
//
// [ADR-010]: docs/adr/0010-templating.md
// [ADR-015]: docs/adr/0015-core-modules-mvp.md
package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/tmpl"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — canonical address top-level.
const Name = "core.file"

// Module — sdk/module.SoulModule implementation for core.file.
//
// Lookup{User,Group} are struct fields for testability: tests substitute
// functions that return fixed uid/gid without touching /etc/passwd.
// In production these are user.Lookup / user.LookupGroup.
type Module struct {
	// LookupUser / LookupGroup — substitution points for unit tests.
	// Defaults wrap user.* (see New()).
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)

	// engine — text/template engine for state rendered (stateless,
	// thread-safe, reused across all Apply calls). Built once in New();
	// see rendered.go.
	engine *tmpl.Engine
}

func New() *Module {
	engine, err := tmpl.New()
	if err != nil {
		// Only fails on a programming-level sprig-allowlist mismatch (a build
		// bug, not user input) — panic at wire-up instead of hiding a nil
		// engine until the first rendered call.
		panic(fmt.Sprintf("core.file: init template engine: %v", err))
	}
	return &Module{
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
		engine:      engine,
	}
}

// Validate checks the **runtime** shape of params (what Keeper delivers after
// the render phases), which for state `rendered` differs from the author
// shape in shared/coremanifest/file.yaml: authors write `template:`+`vars:`,
// but Keeper delivers `template_content`+`render_context` (ADR-010/ADR-012).
// So this doesn't delegate to util.ValidateAgainstManifest (unlike core.exec)
// — a single source isn't possible without a separate runtime manifest, out
// of scope for the pilot. soul-lint statically validates the author shape
// against file.yaml; this method is a runtime safety net before Apply. Both
// contracts agree for present/absent (there author == runtime).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case "present", "absent", "rendered", "directory":
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want present|absent|rendered|directory)", req.State))
	}
	if _, err := util.StringParam(req.Params, "path"); err != nil {
		errs = append(errs, err.Error())
	}
	if req.State == "rendered" {
		if _, err := util.StringParam(req.Params, "template_content"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if req.State == "present" {
		// content XOR src, by key presence (see presentSourceConflict).
		if msg, ok := presentSourceConflict(req.Params); !ok {
			errs = append(errs, msg)
		}
		if util.ParamPresent(req.Params, "src") {
			src, _ := util.OptStringParam(req.Params, "src")
			if !filepath.IsAbs(src) {
				errs = append(errs, fmt.Sprintf("src must be absolute: %q", src))
			}
		}
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe declares that core.file.Plan is pure-read (ADR-031 Scry):
// it reads current file state and does NOT mutate the host (marker for the
// host, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan is a pure-read dry-run (ADR-031 Scry): reads current file state (the
// same stat/read/perm/ownership comparison as the start of Apply) and sends
// PlanEvent.changed — "would Apply change the file?". Does NOT mutate the
// host: no write, no chmod/chown. Covers present/absent/rendered/directory.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	switch req.State {
	case "present":
		return m.planPresent(stream, req, path)
	case "absent":
		return m.planAbsent(stream, path)
	case "rendered":
		return m.planRendered(stream, req, path)
	case "directory":
		return m.planDirectory(stream, req, path)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent — pure-read drift check for state present: reuses the same
// read logic as applyPresent (stat + sha256 content check + perm check +
// ownership check), without writing/chmod/chown.
func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	content, err := util.OptStringParam(req.Params, "content")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	src, err := util.OptStringParam(req.Params, "src")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
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
	if msg, ok := presentSourceConflict(req.Params); !ok {
		return util.PlanFailed(msg)
	}
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.PlanFailed(perr.Error())
	}

	// Desired hash = hash of the src file (if set) or inline content. If src
	// is missing/unreadable during Plan → PlanFailed (NOT false-clean): with
	// no reference source there's nothing to compare, silently reporting
	// "matches" would be a lie.
	desired := []byte(content)
	if src != "" {
		desired, err = util.ReadRegularFile(src)
		if err != nil {
			return util.PlanFailed(err.Error())
		}
	}
	contentHash := sha256.Sum256(desired)

	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// File doesn't exist — Apply would create it (drift).
		return util.SendPlanFinal(stream, true)
	case statErr != nil:
		return util.PlanFailed(fmt.Sprintf("stat %s: %v", path, statErr))
	}

	existing, rerr := os.ReadFile(path)
	if rerr != nil {
		return util.PlanFailed(fmt.Sprintf("read %s: %v", path, rerr))
	}
	if sha256.Sum256(existing) != contentHash {
		return util.SendPlanFinal(stream, true)
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

// planAbsent — pure-read drift check for state absent: drift = file exists
// (Apply would remove it). Same stat-read as applyAbsent, without os.Remove.
func (m *Module) planAbsent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], path string) error {
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return util.SendPlanFinal(stream, false)
	}
	if statErr != nil {
		return util.PlanFailed(fmt.Sprintf("stat %s: %v", path, statErr))
	}
	return util.SendPlanFinal(stream, true)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	path, err := util.StringParam(req.Params, "path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Defense-in-depth: reject relative paths. Resolving a relative
	// `etc/passwd` against the Soul daemon's cwd (usually root) is a classic
	// Destiny-typo footgun. soul-lint enforces the same invariant statically;
	// this is the runtime safety net.
	if !filepath.IsAbs(path) {
		return util.SendFailed(stream, fmt.Sprintf("path must be absolute: %q", path))
	}
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, path)
	case "absent":
		return m.applyAbsent(stream, path)
	case "rendered":
		return m.applyRendered(stream, req, path)
	case "directory":
		return m.applyDirectory(stream, req, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	content, err := util.OptStringParam(req.Params, "content")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	src, err := util.OptStringParam(req.Params, "src")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
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
	if msg, ok := presentSourceConflict(req.Params); !ok {
		return util.SendFailed(stream, msg)
	}

	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.SendFailed(stream, perr.Error())
	}

	// Desired content: either the src file from the host (read into memory
	// once, the same buffer is hashed and written to dest — no double read,
	// no TOCTOU), or inline content (legacy branch, incl. an empty file when
	// neither is set).
	desired := []byte(content)
	if src != "" {
		desired, err = util.ReadRegularFile(src)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}
	contentHash := sha256.Sum256(desired)
	contentChanged, modeChanged, ownerChanged := false, false, false

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("read %s: %v", path, rerr))
		}
		existingHash := sha256.Sum256(existing)
		if existingHash != contentHash {
			contentChanged = true
		}
		if modeStr != "" && info.Mode().Perm() != mode {
			modeChanged = true
		}
	case errors.Is(statErr, fs.ErrNotExist):
		contentChanged = true
	default:
		return util.SendFailed(stream, fmt.Sprintf("stat %s: %v", path, statErr))
	}

	if contentChanged {
		if src != "" {
			// The src branch writes atomically (temp + rename): a config copy
			// from the host may be read by a concurrent daemon, so a partially
			// written file is unacceptable. The content branch stays on
			// os.WriteFile (see README: atomicity asymmetry within present).
			if werr := util.AtomicWrite(path, desired, mode); werr != nil {
				return util.SendFailed(stream, werr.Error())
			}
		} else if werr := os.WriteFile(path, desired, mode); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("write %s: %v", path, werr))
		}
	}
	if modeChanged && !contentChanged {
		// WriteFile/AtomicWrite already set mode when contentChanged; otherwise chmod separately.
		if cerr := os.Chmod(path, mode); cerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
		}
	}

	if owner != "" || group != "" {
		changed, oerr := util.ApplyOwnership(path, owner, group, m.LookupUser, m.LookupGroup)
		if oerr != nil {
			return util.SendFailed(stream, oerr.Error())
		}
		ownerChanged = changed
	}

	changed := contentChanged || modeChanged || ownerChanged
	return util.SendFinal(stream, changed, map[string]any{
		"path":      path,
		"sha256":    hex.EncodeToString(contentHash[:]),
		"mode":      fmt.Sprintf("%04o", mode),
		"installed": true,
	})
}

// presentSourceConflict — cross-field invariant for present: content and src
// are mutually exclusive. Checked by KEY presence (util.ParamPresent), not by
// empty string: `content: ""` + `src: /x` must be caught as a conflict, not
// masked by empty content. Neither set is valid (legacy empty file).
// Returns (error message, ok); ok=false → conflict.
func presentSourceConflict(params *structpb.Struct) (string, bool) {
	if util.ParamPresent(params, "content") && util.ParamPresent(params, "src") {
		return "content and src are mutually exclusive", false
	}
	return "", true
}

func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], path string) error {
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return util.SendFinal(stream, false, map[string]any{
			"path":      path,
			"installed": false,
		})
	}
	if statErr != nil {
		return util.SendFailed(stream, fmt.Sprintf("stat %s: %v", path, statErr))
	}
	if err := os.Remove(path); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("remove %s: %v", path, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"path":      path,
		"installed": false,
	})
}
