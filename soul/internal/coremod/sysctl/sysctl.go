// Package sysctl реализует core-модуль `core.sysctl` ([ADR-015]).
//
// Состояние:
//   - present: ключ kernel-параметра `name` имеет значение `value`, persist
//     запись в `/etc/sysctl.d/<filename>.conf`. Применяется через `sysctl -w`
//     (runtime) + запись в файл (persist after reboot).
//
// Idempotency: текущее значение читается через `sysctl -n <name>`. Если уже
// совпадает И persist-файл содержит ту же запись — no-op. Иначе обновляем оба.
//
// `filename` опционален (default — `<name>` с заменой '.' на '-', чтобы файл
// был валидным sysctl.d-конфигом).
package sysctl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

const Name = "core.sysctl"

// SysctlDir — каталог persist-конфигов sysctl. Подменяется в unit-тестах.
const SysctlDir = "/etc/sysctl.d"

type Module struct {
	Runner util.Runner
	Dir    string
}

func New() *Module {
	return &Module{
		Runner: util.OSRunner{},
		Dir:    SysctlDir,
	}
}

// Validate — known-state + required-params (name/value) делегированы в
// shared/coremanifest/sysctl.yaml (единый источник с soul-lint). Тип
// опционального filename проверяет Apply-getter; cross-field-инвариантов нет.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// PlanReadSafe объявляет, что core.sysctl.Plan — pure-read (ADR-031 Scry):
// читает `sysctl -n` (read-only) + persist-файл, НЕ мутирует (маркер для host-а,
// default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает текущее значение runtime
// (`sysctl -n <name>`, read-only) + содержимое persist-файла и шлёт
// PlanEvent.changed — «Apply изменил бы хост?». НЕ мутирует: ни `sysctl -w`,
// ни запись persist-файла.
//
// `sysctl -n` — read-only вызов того же бэкенда, который Apply использует для
// idempotency перед записью.
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	if req.State != "present" {
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	value, err := util.StringParam(req.Params, "value")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	fname, err := util.OptStringParam(req.Params, "filename")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if fname == "" {
		fname = strings.ReplaceAll(name, ".", "-") + ".conf"
	}
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	path := filepath.Join(m.Dir, fname)

	// runtime read через `sysctl -n` — то же, что Apply использует до `-w`.
	r := m.Runner.Run(ctx, "sysctl", "-n", name)
	if r.Err != nil {
		return util.PlanFailed(fmt.Sprintf("sysctl -n: %v", r.Err))
	}
	want := normalizeSysctlValue(value)
	runtimeDrift := true
	if r.ExitCode == 0 {
		runtimeDrift = normalizeSysctlValue(r.Stdout) != want
	}
	if runtimeDrift {
		return util.SendPlanFinal(stream, true)
	}

	// persist-файл сравнение.
	wantLine := name + " = " + value + "\n"
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		return util.SendPlanFinal(stream, string(existing) != wantLine)
	case errors.Is(rerr, fs.ErrNotExist):
		return util.SendPlanFinal(stream, true)
	default:
		return util.PlanFailed(fmt.Sprintf("read %s: %v", path, rerr))
	}
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	if req.State != "present" {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	value, err := util.StringParam(req.Params, "value")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	fname, err := util.OptStringParam(req.Params, "filename")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if fname == "" {
		fname = strings.ReplaceAll(name, ".", "-") + ".conf"
	}
	if !strings.HasSuffix(fname, ".conf") {
		fname += ".conf"
	}
	path := filepath.Join(m.Dir, fname)

	runtimeChanged, err := m.ensureRuntime(ctx, name, value)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	persistChanged, err := m.ensurePersist(path, name, value)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, runtimeChanged || persistChanged, map[string]any{
		"name":  name,
		"value": value,
		"path":  path,
	})
}

func (m *Module) ensureRuntime(ctx context.Context, name, value string) (bool, error) {
	r := m.Runner.Run(ctx, "sysctl", "-n", name)
	if r.Err != nil {
		return false, fmt.Errorf("sysctl -n: %v", r.Err)
	}
	if r.ExitCode == 0 {
		// sysctl -n может вернуть «1\t0» (для tcp_keepalive multi-values); нормализуем
		// по полям через Fields, чтобы не зависеть от tab vs space.
		current := normalizeSysctlValue(r.Stdout)
		want := normalizeSysctlValue(value)
		if current == want {
			return false, nil
		}
	}
	w := m.Runner.Run(ctx, "sysctl", "-w", name+"="+value)
	if w.Err != nil {
		return false, fmt.Errorf("sysctl -w: %v", w.Err)
	}
	if w.ExitCode != 0 {
		return false, fmt.Errorf("sysctl -w exited %d: %s", w.ExitCode, strings.TrimSpace(w.Stderr))
	}
	return true, nil
}

func (m *Module) ensurePersist(path, name, value string) (bool, error) {
	wantLine := name + " = " + value + "\n"
	existing, rerr := os.ReadFile(path)
	switch {
	case rerr == nil:
		if string(existing) == wantLine {
			return false, nil
		}
	case errors.Is(rerr, fs.ErrNotExist):
		// fall through
	default:
		return false, fmt.Errorf("read %s: %v", path, rerr)
	}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %v", m.Dir, err)
	}
	if err := os.WriteFile(path, []byte(wantLine), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %v", path, err)
	}
	return true, nil
}

// normalizeSysctlValue — sysctl показывает значения с tab-разделителями для
// multi-value ключей; пользователь же может задать пробелы. Сводим к одному
// пробелу + trim, чтобы сравнение было содержательным.
func normalizeSysctlValue(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
