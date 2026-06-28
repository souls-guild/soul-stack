// Package pkg реализует core-модуль `core.pkg` ([ADR-015]).
//
// Состояния:
//   - installed: пакет установлен (с опциональной версией).
//   - absent:    пакет удалён.
//   - latest:    пакет установлен и подтянут до новейшей версии репозитория.
//
// Backend-ы: apt (Debian/Ubuntu), dnf (RHEL ≥ 8), yum (RHEL ≤ 7), apk (Alpine).
// apt-вызовы неинтерактивны и conffile-safe (см. aptGet/aptInstall): stdin
// Soul-агента пуст, любой debconf/dpkg-промпт уронил бы задачу на EOF. rpm-based
// (.rpmnew/.rpmsave вместо промпта) и apk неинтерактивны по умолчанию.
// Backend выбирается из soulprint-факта pkg_mgr (primary, ADR-018(b)) — тот же
// источник, что и CEL `soulprint.self.os.pkg_mgr`; при пустом/unknown факте —
// fallback на runtime-детект (`command -v` / `which`), см. util.ResolvePkgMgr.
// Факт инжектится Soul-агентом in-process через [Module.SetHostFacts]
// (util.SoulprintAware, Вариант A) перед Apply.
package pkg

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// Name — каноническая верхушка адреса (core.<this>.<state>).
const Name = "core.pkg"

// Module — реализация sdk/module.SoulModule. Runner подменяется в тестах;
// в проде используется util.OSRunner{}.
//
// Один и тот же инстанс переиспользуется на все install-шаги прогона (см.
// coremod.Default), поэтому refresh индекса репозиториев (apt-get update /
// apk update) делается один раз за жизнь процесса — поля indexMu/indexDone
// ниже. Это вариант (б): дёшево при многих pkg-задачах, без бессмысленного
// update перед каждым install.
type Module struct {
	Runner util.Runner

	// facts — soulprint-снимок хоста, инжектится Soul-агентом перед Apply
	// (SetHostFacts). Zero-value (pkg_mgr пуст) → Apply откатывается на
	// runtime-детект (util.ResolvePkgMgr). Конкурентных Apply на одном Soul нет
	// (ADR-012(a)), отдельной синхронизации поля не требуется.
	facts util.HostFacts

	indexMu   sync.Mutex
	indexDone bool // индекс репозиториев уже успешно обновлён в этом процессе
}

// New собирает Module с production-Runner. Используется при wire-up в
// registry бинаря soul.
func New() *Module { return &Module{Runner: util.OSRunner{}} }

// SetHostFacts реализует util.SoulprintAware: ApplyRunner инжектит собранный
// soulprint-факт хоста перед вызовом Apply (Вариант A, in-process).
func (m *Module) SetHostFacts(f util.HostFacts) { m.facts = f }

// Validate — known-state + required-param проверки делегированы в
// shared/coremanifest/pkg.yaml (единый источник с soul-lint). Cross-field-
// инвариантов у core.pkg нет. Тип-проверка значений — в Apply-getters.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.pkg.Plan — pure-read (ADR-031 Scry):
// читает текущее состояние пакета и НЕ мутирует хост. Маркер для host-а
// (default-deny): без него Plan на dry_run не вызывался бы.
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее состояние пакета
// (тот же queryInstalled, что в начале Apply) и шлёт PlanEvent.changed —
// «Apply изменил бы пакет?». НЕ мутирует хост: ни install/remove, ни
// refreshIndex (apt-get update / apk update — это запись в индекс репозиториев).
//
// Поддержаны installed/absent — там drift полностью определяется чтением
// queryInstalled (тот же read, что Apply делает до мутации). latest НЕ
// поддержан Plan-ом: «есть ли новее в репозитории» требует read-а индекса,
// которого Apply до мутации не делает (он refresh-ит индекс — запись), поэтому
// pure-read-ответ из существующей read-логики не выводится. Возвращаем явный
// failed-PlanEvent (не false-clean), drift latest — задача Slice B.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	version, err := util.OptStringParam(req.Params, "version")
	if err != nil {
		return util.PlanFailed(err.Error())
	}

	mgr := util.ResolvePkgMgr(ctx, m.Runner, m.facts.PkgMgr)
	if mgr == util.PkgMgrUnknown {
		return util.PlanFailed("no supported package manager detected (apt/dnf/yum/apk)")
	}

	installed, curVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.PlanFailed(err.Error())
	}

	switch req.State {
	case "installed":
		// drift: пакета нет ИЛИ закреплена версия и она расходится с текущей.
		changed := !installed || (version != "" && curVer != version)
		return util.SendPlanFinal(stream, changed)
	case "absent":
		// drift: пакет установлен (Apply удалил бы его).
		return util.SendPlanFinal(stream, installed)
	case "latest":
		return util.PlanFailed("Plan(dry_run) для state latest не поддержан: проверка «есть ли новее» требует чтения индекса репозитория (Slice B)")
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// Apply — основной путь. Идемпотентен: до install-команды проверяет, что
// пакета нет (для installed) / есть (для absent).
func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	version, err := util.OptStringParam(req.Params, "version")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	// pkg-mgr: soulprint-факт primary, runtime-детект fallback (BUG-B).
	mgr := util.ResolvePkgMgr(ctx, m.Runner, m.facts.PkgMgr)
	if mgr == util.PkgMgrUnknown {
		return util.SendFailed(stream, "no supported package manager detected (apt/dnf/yum/apk)")
	}

	switch req.State {
	case "installed":
		return m.applyInstalled(ctx, stream, mgr, name, version)
	case "absent":
		return m.applyAbsent(ctx, stream, mgr, name)
	case "latest":
		return m.applyLatest(ctx, stream, mgr, name)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

func (m *Module) applyInstalled(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name, version string) error {
	installed, curVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if installed && (version == "" || curVer == version) {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
			"installed": true,
			"version":   curVer,
		})
	}
	if err := m.runInstall(ctx, mgr, name, version); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	_, newVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"installed": true,
		"version":   newVer,
	})
}

func (m *Module) applyAbsent(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name string) error {
	installed, _, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !installed {
		return util.SendFinal(stream, false, map[string]any{
			"name":      name,
			"installed": false,
		})
	}
	if err := m.runRemove(ctx, mgr, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{
		"name":      name,
		"installed": false,
	})
}

func (m *Module) applyLatest(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], mgr util.PkgMgr, name string) error {
	beforeInstalled, beforeVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if err := m.runLatest(ctx, mgr, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	_, afterVer, err := m.queryInstalled(ctx, mgr, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed := !beforeInstalled || beforeVer != afterVer
	return util.SendFinal(stream, changed, map[string]any{
		"name":      name,
		"installed": true,
		"version":   afterVer,
	})
}

// queryInstalled возвращает (installed, version, err). Версия — best-effort:
// если pkg-mgr её не отдаёт компактно, возвращаем пустую строку (это не
// меняет смысл флага installed).
func (m *Module) queryInstalled(ctx context.Context, mgr util.PkgMgr, name string) (bool, string, error) {
	switch mgr {
	case util.PkgMgrApt:
		// dpkg-query -W -f='${Status} ${Version}' name → "install ok installed 1.2.3"
		// exit 0 + поле Status начинается с "install ok installed".
		r := m.Runner.Run(ctx, "dpkg-query", "-W", "-f=${Status} ${Version}", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("dpkg-query: %v", r.Err)
		}
		if r.ExitCode != 0 {
			return false, "", nil
		}
		return parseDpkgStatus(r.Stdout)
	case util.PkgMgrDnf, util.PkgMgrYum:
		// rpm -q --qf '%{VERSION}' name → версия (exit 0) или «package … is not installed» (exit 1).
		r := m.Runner.Run(ctx, "rpm", "-q", "--qf", "%{VERSION}", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("rpm: %v", r.Err)
		}
		if r.ExitCode != 0 {
			return false, "", nil
		}
		return true, r.Stdout, nil
	case util.PkgMgrApk:
		// apk info -e name → имя пакета (exit 0) или пусто (exit 0 тоже!).
		// Поэтому используем `apk info -e name` и смотрим stdout.
		r := m.Runner.Run(ctx, "apk", "info", "-e", name)
		if r.Err != nil {
			return false, "", fmt.Errorf("apk: %v", r.Err)
		}
		if r.ExitCode != 0 || r.Stdout == "" {
			return false, "", nil
		}
		// Версия — отдельным вызовом `apk info -ev` (--exact --verbose): для
		// установленного пакета выводит ровно `<name>-<version>` одной строкой
		// (например `nginx-1.26.3-r0`). MINOR-C: без `-e` apk печатает описание
		// пакета («nginx: HTTP and reverse proxy server»), и version-поле
		// register-а засорялось бы текстом вместо номера. parseApkVersion срезает
		// `<name>-` префикс → чистый номер версии (`1.26.3-r0`).
		v := m.Runner.Run(ctx, "apk", "info", "-ev", name)
		return true, parseApkVersion(firstLine(v.Stdout), name), nil
	}
	return false, "", fmt.Errorf("queryInstalled: unsupported pkg mgr %q", mgr)
}

func (m *Module) runInstall(ctx context.Context, mgr util.PkgMgr, name, version string) error {
	if err := m.refreshIndex(ctx, mgr); err != nil {
		return err
	}
	target := name
	switch mgr {
	case util.PkgMgrApt:
		if version != "" {
			target = name + "=" + version
		}
		return m.aptInstall(ctx, target)
	case util.PkgMgrDnf:
		if version != "" {
			target = name + "-" + version
		}
		return m.must(ctx, "dnf", "install", "-y", target)
	case util.PkgMgrYum:
		if version != "" {
			target = name + "-" + version
		}
		return m.must(ctx, "yum", "install", "-y", target)
	case util.PkgMgrApk:
		if version != "" {
			target = name + "=" + version
		}
		return m.must(ctx, "apk", "add", "--no-cache", target)
	}
	return fmt.Errorf("runInstall: unsupported pkg mgr %q", mgr)
}

// refreshIndex обновляет локальный индекс репозиториев перед install. На свежей
// VM/контейнере (cloud-create) индекс apt/apk пустой или устаревший, и install
// без update упирается в «Unable to locate package». Идея — Ansible
// `apt: update_cache` (и аналог для apk).
//
// Refresh выполняется один раз за жизнь процесса (indexDone): один инстанс
// Module обслуживает все pkg-задачи прогона, гонять update перед каждым install
// бессмысленно дорого. Mutex защищает от конкурентных Apply-шагов; флаг ставится
// только после успешного update, поэтому первый фейл не «съедает» попытку для
// последующих шагов.
//
// dnf/yum НЕ refresh-атся: yum/dnf авто-обновляют metadata по expiration
// (metadata_expire), а install сам подтягивает свежий индекс при необходимости —
// явный update тут лишний и только замедляет шаг.
func (m *Module) refreshIndex(ctx context.Context, mgr util.PkgMgr) error {
	switch mgr {
	case util.PkgMgrApt, util.PkgMgrApk:
	default:
		return nil
	}

	m.indexMu.Lock()
	defer m.indexMu.Unlock()
	if m.indexDone {
		return nil
	}

	var err error
	switch mgr {
	case util.PkgMgrApt:
		err = m.aptGet(ctx, "update")
	case util.PkgMgrApk:
		err = m.must(ctx, "apk", "update")
	}
	if err != nil {
		return err
	}
	m.indexDone = true
	return nil
}

func (m *Module) runRemove(ctx context.Context, mgr util.PkgMgr, name string) error {
	switch mgr {
	case util.PkgMgrApt:
		return m.aptGet(ctx, "remove", "-y", name)
	case util.PkgMgrDnf:
		return m.must(ctx, "dnf", "remove", "-y", name)
	case util.PkgMgrYum:
		return m.must(ctx, "yum", "remove", "-y", name)
	case util.PkgMgrApk:
		return m.must(ctx, "apk", "del", name)
	}
	return fmt.Errorf("runRemove: unsupported pkg mgr %q", mgr)
}

func (m *Module) runLatest(ctx context.Context, mgr util.PkgMgr, name string) error {
	if err := m.refreshIndex(ctx, mgr); err != nil {
		return err
	}
	switch mgr {
	case util.PkgMgrApt:
		// apt-get install --only-upgrade=yes name + install-if-missing семантика
		// требует двух команд; делаем install-без-версии — apt установит свежую
		// либо обновит существующую.
		return m.aptInstall(ctx, name)
	case util.PkgMgrDnf:
		return m.must(ctx, "dnf", "install", "-y", name)
	case util.PkgMgrYum:
		// yum update создаёт пакет, если его нет (поведение зависит от версии);
		// надёжнее install.
		return m.must(ctx, "yum", "install", "-y", name)
	case util.PkgMgrApk:
		return m.must(ctx, "apk", "add", "--upgrade", name)
	}
	return fmt.Errorf("runLatest: unsupported pkg mgr %q", mgr)
}

// aptGet запускает apt-get в неинтерактивном режиме. Любой apt/dpkg-вызов на
// управляемом хосте обязан быть batch-safe: stdin Soul-агента пуст, поэтому
// интерактивный debconf/dpkg-промпт (выбор сервиса, conffile keep/replace и т.п.)
// упёрся бы в «EOF on stdin» и уронил задачу. `DEBIAN_FRONTEND=noninteractive`
// переводит debconf в noninteractive-режим (промптов нет, берутся дефолты).
//
// env передаётся через обёртку `env KEY=VAL apt-get …`, а НЕ через RunOptions.Env:
// последнее — full-replace cmd.Env (см. util.OSRunner.RunOpts), что снесло бы
// PATH/HOME и сломало запуск apt. Обёртка `env` добавляет одну переменную поверх
// унаследованного окружения, ничего не теряя.
func (m *Module) aptGet(ctx context.Context, args ...string) error {
	full := append([]string{"DEBIAN_FRONTEND=noninteractive", "apt-get"}, args...)
	return m.must(ctx, "env", full...)
}

// aptInstall — apt-get install конкретного target-а (имя или `name=version`),
// неинтерактивно и conffile-safe.
//
// Dpkg::Options force-confdef + force-confold — ключ к re-apply-робастности:
// наши сценарии рендерят conffile пакета (напр. redis-sentinel →
// /etc/redis/sentinel.conf) ДО его установки, и при последующем apply dpkg
// видит «изменённый оператором conffile» и в интерактиве спросил бы «keep or
// replace?». force-confold = сохранить уже лежащий (наш отрендеренный) файл;
// force-confdef = для остальных conffile взять дефолт мейнтейнера без вопроса.
// Без этих флагов conffile-конфликт роняет install детерминированно при каждом
// re-apply (файлы переживают destroy).
func (m *Module) aptInstall(ctx context.Context, target string) error {
	return m.aptGet(ctx, "install", "-y",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		target)
}

func (m *Module) must(ctx context.Context, name string, args ...string) error {
	r := m.Runner.Run(ctx, name, args...)
	if r.Err != nil {
		return fmt.Errorf("%s: %v", name, r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", name, r.ExitCode, oneLine(r.Stderr))
	}
	return nil
}

// parseDpkgStatus — «install ok installed 1.2.3-1ubuntu1» → installed=true, ver=...
// Любой другой Status (deinstall ok config-files, hold, etc) считаем как not installed.
func parseDpkgStatus(stdout string) (bool, string, error) {
	stdout = oneLine(stdout)
	const prefix = "install ok installed"
	if len(stdout) < len(prefix) || stdout[:len(prefix)] != prefix {
		return false, "", nil
	}
	rest := stdout[len(prefix):]
	for len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return true, rest, nil
}

// parseApkVersion извлекает чистый номер версии из строки `apk info -ev <name>`
// (форма `<name>-<version>`, например `nginx-1.26.3-r0` → `1.26.3-r0`). Срезает
// префикс `<name>-`: имя пакета известно точно (его передали в apk), а имена apk
// дефис содержать могут (`py3-pip`), поэтому split по дефису ненадёжен — режем
// именно известный префикс.
//
// Defensive: если строка не начинается с `<name>-` (пустой вывод, неожиданный
// формат) — возвращаем как есть, не теряя best-effort-значение (version поля
// register-а не критичны, ADR-015 «best-effort»).
func parseApkVersion(line, name string) string {
	prefix := name + "-"
	if rest, ok := strings.CutPrefix(line, prefix); ok {
		return rest
	}
	return line
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

func oneLine(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			out = append(out, ' ')
			continue
		}
		out = append(out, s[i])
	}
	// trim trailing spaces
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	return string(out)
}
