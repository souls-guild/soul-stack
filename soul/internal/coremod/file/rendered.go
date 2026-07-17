package file

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// applyRendered implements state `rendered` of module core.file ([ADR-010]).
//
// Params contract (everything except template_content/render_context is
// symmetric with present):
//   - template_content: literal text/template template (Keeper read the .tmpl
//     file as-is after the CEL phase, text/template did NOT render it yet);
//   - render_context:   the ROOT of the text/template context {vars, self, role,
//     essence} (templating.md §3.2), assembled Keeper-side per-host; passed to
//     the engine as the root, so the template sees `.vars.*`/`.self.*`/`.role`/`.essence.*`;
//   - path:             target file (required, read in Apply);
//   - mode/owner/group: optional, same as present.
//
// Soul does NOT read the flat vars root or a `template` path: Keeper only
// delivers template_content + render_context (see injectTemplateContent /
// setRenderContext on the keeper side, A1/ADR-012(d)).
//
// Idempotency: render into memory → compare the new content's SHA-256 against
// the existing file → write only on diff. Write is atomic (temp+rename in the
// same directory). mode/owner are always applied after materialization;
// changed=true if any of content/mode/owner changed.
func (m *Module) applyRendered(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	// Defense-in-depth: duplicates the check in Apply (see file.go) — in case
	// applyRendered is called outside the Apply switch (e.g. a new entry point).
	if !filepath.IsAbs(path) {
		return util.SendFailed(stream, fmt.Sprintf("path must be absolute: %q", path))
	}
	templateContent, err := util.StringParam(req.Params, "template_content")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	renderContext, err := util.OptStructMapParam(req.Params, "render_context")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if renderContext == nil {
		// render_context is the §3.2 root ({vars,self,role,essence}); without
		// it, templates using `.self.*`/`.vars.*` fail under strict-mode.
		// Keeper must deliver it (missing handoff is a golden-path prod
		// blocker, same as template_content was).
		return util.SendFailed(stream, fmt.Sprintf("render %s: render_context is missing (Keeper did not deliver the root §3.2)", path))
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

	mode, err := util.ParseMode(modeStr)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	rendered, err := m.engine.Render(templateContent, renderContext)
	if err != nil {
		// ErrParse/ErrExecute (including missingkey=error for a missing
		// variable) → a normal step failed-event, not a gRPC error.
		return util.SendFailed(stream, fmt.Sprintf("render %s: %v", path, err))
	}
	renderedBytes := []byte(rendered)
	contentHash := sha256.Sum256(renderedBytes)

	contentChanged, modeChanged := false, false

	info, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		existing, rerr := os.ReadFile(path)
		if rerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("read %s: %v", path, rerr))
		}
		if sha256.Sum256(existing) != contentHash {
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
		if werr := util.AtomicWrite(path, renderedBytes, mode); werr != nil {
			return util.SendFailed(stream, werr.Error())
		}
	} else if modeChanged {
		// content matched — atomicWrite wasn't called; fix mode separately.
		if cerr := os.Chmod(path, mode); cerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("chmod %s: %v", path, cerr))
		}
	}

	ownerChanged := false
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

// planRendered is the pure-read drift check for state rendered (ADR-031
// Scry): renders the template IN MEMORY (rendering is pure — no side
// effects) and compares sha256/mode/ownership against the existing file
// WITHOUT writing. Same read logic as applyRendered, minus AtomicWrite/Chmod/Chown.
func (m *Module) planRendered(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	templateContent, err := util.StringParam(req.Params, "template_content")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	renderContext, err := util.OptStructMapParam(req.Params, "render_context")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if renderContext == nil {
		return util.PlanFailed(fmt.Sprintf("render %s: render_context is missing (Keeper did not deliver the root §3.2)", path))
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
	mode, err := util.ParseMode(modeStr)
	if err != nil {
		return util.PlanFailed(err.Error())
	}

	rendered, err := m.engine.Render(templateContent, renderContext)
	if err != nil {
		return util.PlanFailed(fmt.Sprintf("render %s: %v", path, err))
	}
	contentHash := sha256.Sum256([]byte(rendered))

	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
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
