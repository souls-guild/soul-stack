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

// applyRendered реализует state `rendered` модуля core.file ([ADR-010]).
//
// Контракт params (всё, кроме template_content/render_context, симметрично present):
//   - template_content: literal text/template-шаблон (Keeper прочитал .tmpl
//     as-is после CEL-фазы, text/template НЕ рендерил);
//   - render_context:   КОРЕНЬ text/template-контекста {vars, self, role,
//     essence} (templating.md §3.2), собранный Keeper-side per-host; передаётся
//     движку корнем, поэтому шаблон видит `.vars.*`/`.self.*`/`.role`/`.essence.*`;
//   - path:             целевой файл (обязателен, читается в Apply);
//   - mode/owner/group: опциональны, как у present.
//
// Плоский корень vars/`template`-путь Soul НЕ читает: Keeper доставляет только
// template_content + render_context (см. injectTemplateContent / setRenderContext
// на keeper-стороне, A1/ADR-012(d)).
//
// Идемпотентность: рендерим в память → SHA-256 нового content сверяем с
// существующим файлом → запись только при diff. Запись атомарна (temp+rename
// в той же директории). mode/owner применяются всегда после материализации,
// changed=true если изменился хотя бы один из content/mode/owner.
func (m *Module) applyRendered(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, path string) error {
	// Defense-in-depth: дублирует проверку в Apply (см. file.go) — на случай,
	// если applyRendered позовут вне Apply-switch (например, новой entry-point).
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
		// render_context — корень §3.2 ({vars,self,role,essence}); без него
		// шаблоны с `.self.*`/`.vars.*` падают strict-mode. Keeper обязан его
		// доставить (handoff не настроен — прод-блокер golden-path, как было с
		// template_content).
		return util.SendFailed(stream, fmt.Sprintf("render %s: отсутствует render_context (Keeper не доставил корень §3.2)", path))
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
		// ErrParse/ErrExecute (включая missingkey=error для отсутствующей
		// переменной) → штатный failed-event шага, не gRPC-ошибка.
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
		// content совпал — atomicWrite не вызывался; mode правим отдельно.
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

// planRendered — pure-read drift для state rendered (ADR-031 Scry): рендерит
// шаблон В ПАМЯТЬ (рендер чист — побочных эффектов нет) и сверяет sha256/mode/
// ownership с существующим файлом БЕЗ записи. Та же read-логика, что applyRendered,
// минус AtomicWrite/Chmod/Chown.
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
		return util.PlanFailed(fmt.Sprintf("render %s: отсутствует render_context (Keeper не доставил корень §3.2)", path))
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
