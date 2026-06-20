// Package file реализует core-модуль `core.file` ([ADR-015]).
//
// Состояния MVP:
//   - present:   файл существует с заданным content/mode/owner/group.
//   - absent:    файл удалён.
//   - rendered:  файл = результат рендера text/template-шаблона ([ADR-010]).
//     Keeper кладёт literal template_content + CEL-rendered vars в params,
//     Soul-сторона рендерит сама через shared/tmpl (см. rendered.go).
//   - directory: каталог существует с заданным owner/group/mode (см.
//     directory.go); декларативная замена `core.exec.run install -d`.
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [ADR-015]: docs/adr/0015-core-modules-mvp.md#adr-015-core-модули-mvp-точный-список
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
)

// Name — каноническая верхушка адреса.
const Name = "core.file"

// Module — реализация sdk/module.SoulModule для core.file.
//
// Lookup{User,Group} вынесены в поля для тестабельности: тесты подменяют
// на функции, возвращающие фиксированные uid/gid без обращения к /etc/passwd.
// В проде — user.Lookup / user.LookupGroup.
type Module struct {
	// LookupUser / LookupGroup — точки подмены для unit-тестов.
	// Default — обёртки над os.* (см. New()).
	LookupUser  func(name string) (*user.User, error)
	LookupGroup func(name string) (*user.Group, error)

	// engine — text/template-движок для state rendered (stateless,
	// потокобезопасен, переиспользуется всеми Apply). Собирается один раз
	// в New(); см. rendered.go.
	engine *tmpl.Engine
}

func New() *Module {
	engine, err := tmpl.New()
	if err != nil {
		// Ошибка возможна только при программном расхождении sprig-allowlist-а
		// (баг сборки, не ввод пользователя) — паникуем при wire-up, а не
		// прячем nil-engine до первого rendered-вызова.
		panic(fmt.Sprintf("core.file: init template engine: %v", err))
	}
	return &Module{
		LookupUser:  user.Lookup,
		LookupGroup: user.LookupGroup,
		engine:      engine,
	}
}

// Validate проверяет **runtime**-форму params (то, что доставил Keeper после
// рендер-фаз), которая для state `rendered` отличается от author-формы из
// shared/coremanifest/file.yaml: author пишет `template:`+`vars:`, а Keeper
// доставляет `template_content`+`render_context` (ADR-010/ADR-012). Поэтому
// здесь нет делегации в util.ValidateAgainstManifest (как в core.exec) —
// единый источник невозможен без отдельного runtime-манифеста, что вне объёма
// пилота. soul-lint валидирует author-форму статически по file.yaml; этот метод
// — runtime-страховка перед Apply. Оба контракта согласованы для present/absent
// (там author == runtime).
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
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.file.Plan — pure-read (ADR-031 Scry):
// читает текущее состояние файла и НЕ мутирует хост (маркер для host-а,
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее состояние файла (тот
// же stat/read/perm/ownership-сравнение, что в начале Apply) и шлёт
// PlanEvent.changed — «Apply изменил бы файл?». НЕ мутирует хост: ни запись,
// ни chmod/chown. Покрывает present/absent/rendered/directory.
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

// planPresent — pure-read drift для state present: переиспользует ту же
// read-логику, что applyPresent (stat + sha256-сверка content + perm-сверка +
// ownership-сверка), но без записи/chmod/chown.
func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	content, err := util.OptStringParam(req.Params, "content")
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
	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.PlanFailed(perr.Error())
	}

	contentHash := sha256.Sum256([]byte(content))

	info, statErr := os.Stat(path)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// Файла нет — Apply создал бы его (drift).
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

// planAbsent — pure-read drift для state absent: drift = файл существует
// (Apply удалил бы его). Тот же stat-read, что applyAbsent, без os.Remove.
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
	// Defense-in-depth: запрещаем относительные пути. Resolve относительного
	// `etc/passwd` относительно cwd Soul-демона (обычно root) — типовой
	// footgun опечатки в Destiny. Тот же инвариант обязан держать soul-lint
	// статически; это runtime-страховка.
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

	mode, perr := util.ParseMode(modeStr)
	if perr != nil {
		return util.SendFailed(stream, perr.Error())
	}

	contentHash := sha256.Sum256([]byte(content))
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
		if werr := os.WriteFile(path, []byte(content), mode); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("write %s: %v", path, werr))
		}
	}
	if modeChanged && !contentChanged {
		// WriteFile уже установил mode при contentChanged; иначе chmod отдельно.
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
