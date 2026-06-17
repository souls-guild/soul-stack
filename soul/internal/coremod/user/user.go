// Package user реализует core-модуль `core.user` ([ADR-015]).
//
// Состояния:
//   - present: пользователь существует с заданными uid/shell/home/groups.
//   - absent:  пользователь удалён.
//
// Опциональные params present:
//   - uid (int):        явный uid (useradd -u).
//   - shell (string):   login shell (useradd -s).
//   - home (string):    домашний каталог (useradd -d).
//   - groups ([]string): supplementary-группы (useradd -G a,b).
//   - system (bool):    системный аккаунт (useradd -r). Для сервис-аккаунтов
//     stateful-сервисов (например redis).
//   - group (string):   primary-группа (useradd -g). Группа должна уже
//     существовать — caller создаёт её через core.group ДО. Отличается от
//     `groups` (supplementary, -G).
//
// Семантика present — present-or-create (MVP): существующий пользователь не
// реконсилится. Новые params (system/group, как и uid/shell/home/groups)
// действуют ТОЛЬКО при создании; для уже существующего пользователя — no-op,
// они НЕ триггерят usermod/reconcile.
//
// Backend: useradd/usermod/userdel (busybox-совместимое подмножество). На
// alpine это пакет shadow или busybox-built-ins — оба понимают эти флаги.
package user

import (
	"context"
	"fmt"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.user"

// maxUID — uid_t знаковый 32-бит на Linux; useradd отвергает значения вне
// диапазона. Верхняя граница защищает от заведомо-битого ввода до запуска
// подпроцесса.
const maxUID = 2147483647

// nameRe повторяет NAME_REGEX shadow-utils по умолчанию
// (`^[a-z_][a-z0-9_-]*\$?$`): имя начинается с буквы/`_`, может оканчиваться на
// `$` (NIS/Samba machine-account), остальное — нижний регистр/цифры/`_`/`-`.
// Это конвенция самого useradd — не строже, чтобы не резать легитимные имена.
// Длина (≤ 32) проверяется отдельно в validName.
var nameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*\$?$`)

type Module struct {
	Runner     util.Runner
	LookupUser func(name string) (*user.User, error)
}

func New() *Module {
	return &Module{
		Runner:     util.OSRunner{},
		LookupUser: user.Lookup,
	}
}

// Validate — known-state + required-param (name) делегированы в
// shared/coremanifest/user.yaml (единый источник с soul-lint, убран дубль).
// Поверх делегации — ранний type-guard опциональных params (manifest-DSL его не
// выражает) и СЕМАНТИЧЕСКИЕ проверки формата/диапазона + arg-injection guard.
// Это input-validation/safety НАШЕГО кода: отсекаем инъекции (ведущий `-` →
// argument confusion в argv useradd) и заведомо-битый ввод с понятной ошибкой,
// НЕ ужесточая реальные ограничения useradd. present/absent семантика не меняется.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)

	// name берётся StringParam в Apply; формат-проверка здесь даёт ранний отказ
	// (soul-lint / Validate-фаза), не дожидаясь запуска useradd.
	if name, err := util.StringParam(req.Params, "name"); err == nil {
		if verr := validName("name", name); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if uid, has, err := util.OptIntParam(req.Params, "uid"); err != nil {
		errs = append(errs, err.Error())
	} else if has && (uid < 0 || uid > maxUID) {
		errs = append(errs, fmt.Sprintf("param %q: out of range [0, %d], got %d", "uid", maxUID, uid))
	}

	if shell, err := util.OptStringParam(req.Params, "shell"); err != nil {
		errs = append(errs, err.Error())
	} else if shell != "" {
		if verr := validAbsPath("shell", shell); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if home, err := util.OptStringParam(req.Params, "home"); err != nil {
		errs = append(errs, err.Error())
	} else if home != "" {
		if verr := validAbsPath("home", home); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	if groups, err := util.OptStringSliceParam(req.Params, "groups"); err != nil {
		errs = append(errs, err.Error())
	} else {
		for _, g := range groups {
			if verr := validName("groups", g); verr != nil {
				errs = append(errs, verr.Error())
			}
		}
	}

	if _, err := util.OptBoolParam(req.Params, "system"); err != nil {
		errs = append(errs, err.Error())
	}

	if group, err := util.OptStringParam(req.Params, "group"); err != nil {
		errs = append(errs, err.Error())
	} else if group != "" {
		if verr := validName("group", group); verr != nil {
			errs = append(errs, verr.Error())
		}
	}

	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// validName проверяет логин/имя группы по NAME_REGEX shadow-utils + длина ≤ 32.
// Ведущий `-` отсекается regex-ом (имя обязано начинаться с буквы/`_`), что и
// есть guard от argument injection: имя `-x` не попадёт в argv как опция.
func validName(field, name string) error {
	if name == "" {
		return fmt.Errorf("param %q: must not be empty", field)
	}
	if len(name) > 32 {
		return fmt.Errorf("param %q: too long (max 32), got %d chars", field, len(name))
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("param %q: invalid name %q (must match %s)", field, name, nameRe.String())
	}
	return nil
}

// validAbsPath требует абсолютный путь без ведущего `-` (defense-in-depth от
// argument confusion). Существование файла НЕ проверяется — useradd его не
// требует, гибкость оператора не режем.
func validAbsPath(field, path string) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("param %q: must be an absolute path (start with %q), got %q", field, "/", path)
	}
	return nil
}

// PlanReadSafe объявляет, что core.user.Plan — pure-read (ADR-031 Scry):
// читает LookupUser и НЕ мутирует хост (маркер для host-а, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее наличие пользователя
// (тот же LookupUser, что в начале Apply) и шлёт PlanEvent.changed — «Apply
// изменил бы пользователя?». НЕ мутирует хост: ни useradd, ни userdel.
//
// Семантика 1:1 с Apply: present-or-create (uid/shell/home/groups/system/group
// на уже существующем НЕ триггерят reconcile в MVP — см. doc Apply), поэтому
// drift для present = «пользователя нет», для absent = «пользователь есть».
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if verr := validName("name", name); verr != nil {
		return util.PlanFailed(verr.Error())
	}
	_, lookupErr := m.LookupUser(name)
	exists := lookupErr == nil
	switch req.State {
	case "present":
		return util.SendPlanFinal(stream, !exists)
	case "absent":
		return util.SendPlanFinal(stream, exists)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Формат-проверка name единожды для обоих state: Apply может вызываться без
	// предшествующей Validate-фазы, инъекционное/битое имя не должно дойти до
	// argv useradd/userdel.
	if verr := validName("name", name); verr != nil {
		return util.SendFailed(stream, verr.Error())
	}
	switch req.State {
	case "present":
		return m.applyPresent(ctx, stream, req, name)
	case "absent":
		return m.applyAbsent(ctx, stream, name)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyPresent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], req *pluginv1.ApplyRequest, name string) error {
	uid, hasUID, err := util.OptIntParam(req.Params, "uid")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	shell, err := util.OptStringParam(req.Params, "shell")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	home, err := util.OptStringParam(req.Params, "home")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	groups, err := util.OptStringSliceParam(req.Params, "groups")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	system, err := util.OptBoolParam(req.Params, "system")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	primary, err := util.OptStringParam(req.Params, "group")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// Семантические проверки опциональных params и здесь, а не только в Validate:
	// Apply может быть вызван без предшествующей Validate-фазы, а битый/
	// инъекционный ввод не должен дойти до argv useradd. name уже проверен в Apply.
	if hasUID && (uid < 0 || uid > maxUID) {
		return util.SendFailed(stream, fmt.Sprintf("param %q: out of range [0, %d], got %d", "uid", maxUID, uid))
	}
	if primary != "" {
		if verr := validName("group", primary); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	for _, g := range groups {
		if verr := validName("groups", g); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	if shell != "" {
		if verr := validAbsPath("shell", shell); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}
	if home != "" {
		if verr := validAbsPath("home", home); verr != nil {
			return util.SendFailed(stream, verr.Error())
		}
	}

	if existing, lookupErr := m.LookupUser(name); lookupErr == nil && existing != nil {
		// Уже есть. По MVP не делаем reconcile uid/shell/home/groups/system/
		// group — это требует usermod, который меняет state «не для
		// слабонервных» (например, изменение uid каскадом на права файлов).
		// Для первой версии достаточно «present-or-create»; reconcile —
		// следующий slice. Новые params (system/group) тоже НЕ триггерят
		// reconcile для существующего — они действуют только при создании.
		return util.SendFinal(stream, false, map[string]any{
			"name":    name,
			"exists":  true,
			"created": false,
		})
	}

	args := []string{"-M"}
	if system {
		args = append(args, "-r")
	}
	if hasUID {
		args = append(args, "-u", strconv.FormatInt(uid, 10))
	}
	if primary != "" {
		args = append(args, "-g", primary)
	}
	if shell != "" {
		args = append(args, "-s", shell)
	}
	if home != "" {
		args = append(args, "-d", home)
	}
	if len(groups) > 0 {
		args = append(args, "-G", strings.Join(groups, ","))
	}
	// `--` отделяет позиционный name от опций: имя, начинающееся с `-`, иначе
	// распарсится useradd как флаг (argument injection, defense-in-depth поверх
	// validName). useradd использует getopt_long — `--` поддержан (man useradd).
	args = append(args, "--", name)
	if err := m.must(ctx, "useradd", args...); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":    name,
		"exists":  true,
		"created": true,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], name string) error {
	_, lookupErr := m.LookupUser(name)
	if lookupErr != nil {
		return util.SendFinal(stream, false, map[string]any{
			"name":   name,
			"exists": false,
		})
	}
	if err := m.must(ctx, "userdel", "--", name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":   name,
		"exists": false,
	})
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}
