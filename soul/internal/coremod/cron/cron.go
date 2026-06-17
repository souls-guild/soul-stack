// Package cron реализует core-модуль `core.cron` ([ADR-015]).
//
// Состояния:
//   - present: job-файл `/etc/cron.d/<name>` существует с заданным расписанием
//     и командой.
//   - absent:  файл удалён.
//
// MVP: только system-level `/etc/cron.d/<name>`, одно правило на файл.
// User-crontab (`crontab -u user -l/-`) — отложен до реального запроса.
//
// Платформенная поддержка: Linux distros, где cron-daemon читает /etc/cron.d/.
// На FreeBSD каталога нет — модуль не должен туда применяться (контролируется
// `where:`-предикатом в scenario, не на стороне модуля).
package cron

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.cron"

// CronDir — каталог системных cron-job (фиксирован архитектурно; на minimal-
// контейнерах его может не быть, тогда `present` сам создаст каталог).
const CronDir = "/etc/cron.d"

type Module struct {
	// Dir подменяется в unit-тестах на t.TempDir(), в проде — CronDir.
	Dir string
}

func New() *Module { return &Module{Dir: CronDir} }

// Validate — known-state + per-state required-params (name; present →
// schedule/command) делегированы в shared/coremanifest/cron.yaml (единый
// источник с soul-lint). Валидность имени job ([A-Za-z0-9_-]) — императивная
// проверка в Apply (validCronName), manifest-DSL её не выражает.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.cron.Plan — pure-read (ADR-031 Scry):
// читает существующий job-файл и НЕ мутирует ФС (маркер для host-а, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее содержимое job-файла
// `<Dir>/<name>` (тот же os.ReadFile, что в начале Apply) и шлёт PlanEvent.changed
// — «Apply изменил бы файл?». НЕ мутирует ФС: ни MkdirAll, ни WriteFile, ни Remove.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if !validCronName(name) {
		return util.PlanFailed(fmt.Sprintf("param %q: invalid cron job name %q (allowed [A-Za-z0-9_-])", "name", name))
	}
	path := filepath.Join(m.Dir, name)
	switch req.State {
	case "present":
		return m.planPresent(stream, req, path)
	case "absent":
		return m.planAbsent(stream, path)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planPresent — pure-read drift для state present: тот же сравнительный read,
// что в applyPresent, без записи. drift = файла нет ИЛИ содержимое отличается.
func (m *Module) planPresent(stream grpc.ServerStreamingServer[pluginv1.PlanEvent], req *pluginv1.PlanRequest, path string) error {
	schedule, err := util.StringParam(req.Params, "schedule")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	command, err := util.StringParam(req.Params, "command")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	cronUser, err := util.OptStringParam(req.Params, "user")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if cronUser == "" {
		cronUser = "root"
	}
	want := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)
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

// planAbsent — pure-read drift для state absent: drift = файл существует
// (Apply удалил бы его).
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
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !validCronName(name) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: invalid cron job name %q (allowed [A-Za-z0-9_-])", "name", name))
	}
	path := filepath.Join(m.Dir, name)
	switch req.State {
	case "present":
		return m.applyPresent(stream, req, name, path)
	case "absent":
		return m.applyAbsent(stream, name, path)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, name, path string) error {
	schedule, err := util.StringParam(req.Params, "schedule")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	command, err := util.StringParam(req.Params, "command")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	cronUser, err := util.OptStringParam(req.Params, "user")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if cronUser == "" {
		cronUser = "root"
	}

	content := fmt.Sprintf("%s %s %s\n", schedule, cronUser, command)
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == content {
			return util.SendFinal(stream, false, map[string]any{
				"name":      name,
				"path":      path,
				"installed": true,
			})
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// will create below
	default:
		return util.SendFailed(stream, fmt.Sprintf("read %s: %v", path, rerr))
	}

	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("mkdir %s: %v", m.Dir, err))
	}
	// 0644: cron строго требует, чтобы /etc/cron.d/<file> был owned root и не
	// был group/world-writable. WriteFile не меняет owner; на тестовых TempDir
	// owner и так uid процесса — это нормально, прод-Soul бежит из-под root.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return util.SendFailed(stream, fmt.Sprintf("write %s: %v", path, err))
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"path":      path,
		"installed": true,
	})
}

func (m *Module) applyAbsent(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name, path string) error {
	_, statErr := os.Stat(path)
	if errors.Is(statErr, fs.ErrNotExist) {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
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
		"name":      name,
		"path":      path,
		"installed": false,
	})
}

// validCronName — cron-daemon игнорирует файлы с точками/спецсимволами в имени
// (вплоть до полного skip каталога на debian-derivatives). Ограничиваем имя
// строго [A-Za-z0-9_-] — это исключает path-injection и совместимо с run-parts.
func validCronName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}
