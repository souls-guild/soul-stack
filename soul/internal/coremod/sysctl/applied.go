package sysctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const stateApplied = "applied"

// reloadMode извлекает и валидирует param `reload` (default auto). Переиспользует
// closed-set словарь util.DaemonReloadMode (auto|always|never) — тот же набор,
// что `daemon_reload` у core.service ([ADR-015] amend), но семантика «применения»
// у sysctl своя: reload = `sysctl -p <file>` (точечно по drop-in), а не
// systemctl daemon-reload. Отсутствие/null → auto. Неизвестное строковое
// значение → ошибка (отвергается на Validate, симметрично core.service).
func reloadMode(params *structpb.Struct) (util.DaemonReloadMode, error) {
	s, err := util.OptStringParam(params, "reload")
	if err != nil {
		return "", err
	}
	switch s {
	case "":
		return util.DaemonReloadAuto, nil
	case string(util.DaemonReloadAuto), string(util.DaemonReloadAlways), string(util.DaemonReloadNever):
		return util.DaemonReloadMode(s), nil
	default:
		return "", fmt.Errorf("param %q: unknown value %q (want auto|always|never)", "reload", s)
	}
}

// applyApplied — state `applied`: bulk-набор `settings` материализуется ОДНИМ
// детерминированным drop-in `/etc/sysctl.d/<filename>.conf` (sorted keys), reload
// через `sysctl -p <file>` точечно по drop-in (НЕ весь --system). Reload gating
// (см. shouldReload):
//
//   - never → reload не делается ВООБЩЕ (явный opt-out, даже при file-change);
//   - always → reload безусловно;
//   - auto → reload только при file-change (как daemon_reload:auto на смену unit);
//   - сам reload `changed` НЕ помечает (changed=факт записи drop-in).
//
// `ignore_failures` → `sysctl -e -p <file>` (-e/--ignore глушит read-only/
// несуществующие ключи в контейнерах; явный opt-in оператора).
func (m *Module) applyApplied(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	settings, err := util.OptStringMapParam(req.Params, "settings")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if settings == nil {
		return util.SendFailed(stream, `param "settings": missing`)
	}
	fname, err := util.StringParam(req.Params, "filename")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, err := reloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	ignoreFailures, err := util.OptBoolParam(req.Params, "ignore_failures")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	path := dropInPath(m.Dir, fname)

	// Пустой набор (len==0): ранний no-op. Не пишем пустой drop-in и не reload-им
	// (general-purpose edge: bulk-задача без параметров — нечего применять; пустой
	// файл /etc/sysctl.d/<f>.conf — мусор, а reload на нём бессмыслен). changed=false:
	// состояние «нет параметров» уже выполнено отсутствием записи. Симметрично с
	// idempotent-веткой ensureDropIn (нет изменения → нет reload).
	if len(settings) == 0 {
		return util.SendFinal(stream, false, map[string]any{
			"path":     path,
			"settings": 0,
		})
	}

	want := renderDropIn(settings)

	changed, err := m.ensureDropIn(path, want)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if shouldReload(mode, changed) {
		if err := m.reloadDropIn(ctx, path, ignoreFailures); err != nil {
			return util.SendFailed(stream, err.Error())
		}
	}

	return util.SendFinal(stream, changed, map[string]any{
		"path":     path,
		"settings": len(settings),
	})
}

// shouldReload — gating reload по mode (never opt-out → false всегда; always →
// true; auto → только при file-change).
func shouldReload(mode util.DaemonReloadMode, changed bool) bool {
	switch mode {
	case util.DaemonReloadNever:
		return false
	case util.DaemonReloadAlways:
		return true
	default: // auto
		return changed
	}
}

// planApplied — pure-read drift state `applied` (ADR-031 Scry): сравнивает
// желаемый детерминированный контент drop-in с существующим файлом БЕЗ записи и
// reload. drift = файла нет / контент отличается.
func (m *Module) planApplied(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	settings, err := util.OptStringMapParam(req.Params, "settings")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if settings == nil {
		return util.PlanFailed(`param "settings": missing`)
	}
	fname, err := util.StringParam(req.Params, "filename")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if _, err := reloadMode(req.Params); err != nil {
		return util.PlanFailed(err.Error())
	}

	// Пустой набор → no-op (drift=false), симметрично applyApplied: нечего применять,
	// пустой drop-in не пишется, значит и дрейфа нет.
	if len(settings) == 0 {
		return util.SendPlanFinal(stream, false)
	}

	path := dropInPath(m.Dir, fname)
	want := renderDropIn(settings)

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

// ensureDropIn пишет drop-in атомарно только при drift (контент отличается /
// файла нет). Preserve-by-default mode существующего файла; новый файл — 0644
// (sysctl.d-стандарт). changed=true только при реальной записи.
func (m *Module) ensureDropIn(path, want string) (bool, error) {
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == want {
			return false, nil
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// fall through — создаём.
	default:
		return false, fmt.Errorf("read %s: %v", path, rerr)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", m.Dir, err)
	}
	if err := util.AtomicWritePreserving(path, []byte(want), "", "", "", user.Lookup, user.LookupGroup); err != nil {
		return false, err
	}
	return true, nil
}

// reloadDropIn применяет drop-in точечно через `sysctl -p <file>` (НЕ весь
// --system). `ignore_failures` добавляет `-e` (глушит read-only/несуществующие
// ключи). argv без shell.
func (m *Module) reloadDropIn(ctx context.Context, path string, ignoreFailures bool) error {
	args := make([]string, 0, 3)
	if ignoreFailures {
		args = append(args, "-e")
	}
	args = append(args, "-p", path)
	r := m.Runner.Run(ctx, "sysctl", args...)
	if r.Err != nil {
		return fmt.Errorf("sysctl -p: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("sysctl -p exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// dropInPath собирает путь drop-in в /etc/sysctl.d. `filename` для bulk-state
// обязателен (required в манифесте); суффикс `.conf` добавляется автоматически,
// как у present-filename. filepath.Join держит запись внутри m.Dir.
func dropInPath(dir, fname string) string {
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	return filepath.Join(dir, fname)
}

// renderDropIn строит детерминированный контент drop-in из map: ключи
// сортируются (стабильный порядок между прогонами → нет ложного change/повторного
// reload), формат строк `key = value` (sysctl.d-синтаксис). Финальный перевод
// строки — POSIX-конвенция текстового конфига.
func renderDropIn(settings map[string]string) string {
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(" = ")
		b.WriteString(settings[k])
		b.WriteByte('\n')
	}
	return b.String()
}
