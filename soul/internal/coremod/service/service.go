// Package service реализует core-модуль `core.service` ([ADR-015]).
//
// Состояния:
//   - running:    сервис запущен. Опциональный param `enabled` (bool) одним
//     шагом управляет автозапуском (true → enable, false → disable, опущено →
//     не трогать) — параллель Ansible service state=started enabled=yes.
//   - stopped:    сервис остановлен.
//   - restarted:  безусловный restart (changed всегда true).
//   - enabled:    автозапуск при загрузке системы (orthogonal to active state).
//
// Backend выбирается из soulprint-факта init_system (primary, ADR-018(b)) —
// он же источник CEL `soulprint.self.os.init_system`, поэтому модуль и предикаты
// видят одну init-систему. При пустом/unknown факте — fallback на runtime-детект
// systemd (`systemctl --version`) → openrc (`rc-service --version`) → sysvinit
// (`service --version`), см. util.ResolveInitSystem. Логика идемпотентности —
// через `is-active` / `is-enabled` (systemd) или эквиваленты OpenRC.
//
// Факт инжектится Soul-агентом in-process через [Module.SetHostFacts]
// (util.SoulprintAware, Вариант A) перед Apply.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const Name = "core.service"

type Module struct {
	Runner util.Runner

	// facts — soulprint-снимок хоста, инжектится Soul-агентом перед Apply
	// (SetHostFacts). Zero-value (init_system пуст) → Apply откатывается на
	// runtime-детект (util.ResolveInitSystem). Конкурентных Apply на одном Soul
	// нет (ADR-012(a)), отдельной синхронизации поля не требуется.
	facts util.HostFacts
}

func New() *Module { return &Module{Runner: util.OSRunner{}} }

// SetHostFacts реализует util.SoulprintAware: ApplyRunner инжектит собранный
// soulprint-факт хоста перед вызовом Apply (Вариант A, in-process).
func (m *Module) SetHostFacts(f util.HostFacts) { m.facts = f }

// Validate — known-state + required-param (name) делегированы в
// shared/coremanifest/service.yaml (единый источник с soul-lint, убран дубль).
// Тип-проверка опционального `enabled` (tri-bool: опущено/true/false) оставлена
// поверх делегации: это ранний type-guard, который manifest-DSL не выражает
// (проверка значения, не литерала), и контракт «non-bool enabled отвергается на
// Validate, не молча» (см. service_test.go).
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	errs := util.ValidateAgainstManifest(Name, req)
	if _, _, err := util.TriBoolParam(req.Params, "enabled"); err != nil {
		errs = append(errs, err.Error())
	}
	// daemon_reload — closed-set enum (auto|always|never). Проверка значения,
	// не литерала: manifest-DSL объявляет enum для UI/линтера, runtime-guard
	// «неизвестное значение → ошибка валидации, не молча» делаем тут поверх
	// делегации (симметрия с tri-bool `enabled`).
	if _, err := daemonReloadMode(req.Params); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// daemonReloadMode извлекает и валидирует param `daemon_reload` (default auto).
// Отсутствие/null → auto. Неизвестное строковое значение → ошибка (отвергается
// на Validate). Применяется только к мутирующим states (running/restarted/
// enabled); на stopped param игнорируется (manifest его там не объявляет).
func daemonReloadMode(params *structpb.Struct) (util.DaemonReloadMode, error) {
	s, err := util.OptStringParam(params, "daemon_reload")
	if err != nil {
		return "", err
	}
	switch s {
	case "":
		return util.DaemonReloadAuto, nil
	case string(util.DaemonReloadAuto), string(util.DaemonReloadAlways), string(util.DaemonReloadNever):
		return util.DaemonReloadMode(s), nil
	default:
		return "", fmt.Errorf("param %q: unknown value %q (want auto|always|never)", "daemon_reload", s)
	}
}

// PlanReadSafe объявляет, что core.service.Plan — pure-read (ADR-031 Scry):
// читает is-active/is-enabled и НЕ мутирует хост (маркер для host-а, default-deny).
func (m *Module) PlanReadSafe() {}

// Plan — pure-read dry-run (ADR-031 Scry): читает активность/autostart юнита
// (тот же ServiceActive/isEnabled, что в начале Apply) и шлёт PlanEvent.changed
// — «Apply изменил бы сервис?». НЕ мутирует хост: ни start/stop/restart, ни
// enable/disable.
//
// restarted всегда drift=true: restart безусловно changed (Apply его не
// идемпотентит — см. applyRestarted), поэтому dry-run честно сообщает «изменит».
func (m *Module) Plan(req *pluginv1.PlanRequest, stream grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	init := util.ResolveInitSystem(ctx, m.Runner, m.facts.InitSystem)
	if init == util.InitSystemUnknown {
		return util.PlanFailed("no supported init system detected (systemd/openrc/sysv)")
	}

	switch req.State {
	case "running":
		return m.planRunning(ctx, stream, init, name, req)
	case "stopped":
		active, aerr := util.ServiceActive(ctx, m.Runner, init, name)
		if aerr != nil {
			return util.PlanFailed(aerr.Error())
		}
		// drift: сервис активен (Apply остановил бы его).
		return util.SendPlanFinal(stream, active)
	case "restarted":
		// restart безусловно changed=true (applyRestarted) — dry-run сообщает то же.
		return util.SendPlanFinal(stream, true)
	case "enabled":
		enabled, eerr := m.isEnabled(ctx, init, name)
		if eerr != nil {
			return util.PlanFailed(eerr.Error())
		}
		// drift: autostart выключен (Apply включил бы его).
		return util.SendPlanFinal(stream, !enabled)
	default:
		return util.PlanFailed(fmt.Sprintf("unknown state %q", req.State))
	}
}

// planRunning — pure-read drift для state running: drift = сервис не активен ИЛИ
// (управляем autostart-ом и текущий enabled != want). Те же ServiceActive/
// isEnabled, что applyRunning, без start/enable/disable.
func (m *Module) planRunning(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.PlanEvent], init util.InitSystem, name string, req *pluginv1.PlanRequest) error {
	wantEnabled, manageEnabled, err := util.TriBoolParam(req.Params, "enabled")
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.PlanFailed(err.Error())
	}
	if !active {
		return util.SendPlanFinal(stream, true)
	}
	if manageEnabled {
		enabled, eerr := m.isEnabled(ctx, init, name)
		if eerr != nil {
			return util.PlanFailed(eerr.Error())
		}
		if enabled != wantEnabled {
			return util.SendPlanFinal(stream, true)
		}
	}
	return util.SendPlanFinal(stream, false)
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	name, err := util.StringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// init-система: soulprint-факт primary, runtime-детект fallback (BUG-B).
	init := util.ResolveInitSystem(ctx, m.Runner, m.facts.InitSystem)
	if init == util.InitSystemUnknown {
		return util.SendFailed(stream, "no supported init system detected (systemd/openrc/sysv)")
	}

	switch req.State {
	case "running":
		return m.applyRunning(ctx, stream, init, name, req)
	case "stopped":
		return m.applyStopped(ctx, stream, init, name)
	case "restarted":
		return m.applyRestarted(ctx, stream, init, name, req)
	case "enabled":
		return m.applyEnabled(ctx, stream, init, name, req)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// applyRunning гарантирует, что сервис запущен. Опциональный param `enabled`
// (tri-state, ADR-015 параллель Ansible `state=started enabled=yes`):
//
//	опущено   — autostart не трогаем (управляем только активностью);
//	true      — дополнительно enable юнита (autostart при загрузке);
//	false     — дополнительно disable юнита.
//
// changed=true, если изменилась активность ИЛИ enabled-состояние. enable/disable
// идемпотентны через isEnabled: повторный вызов на уже-в-нужном-состоянии юните
// не помечает changed.
func (m *Module) applyRunning(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	wantEnabled, manageEnabled, err := util.TriBoolParam(req.Params, "enabled")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload ДО start/enable: после правки unit-файла без reload systemd
	// стартовал бы со старым определением. reload не помечает шаг changed (см.
	// EnsureDaemonReloaded), только диагностика в output.
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed := false
	if !active {
		if err := m.start(ctx, init, name); err != nil {
			return util.SendFailed(stream, err.Error())
		}
		changed = true
	}

	output := map[string]any{"name": name, "active": true}
	if manageEnabled {
		enabledChanged, err := m.ensureEnabled(ctx, init, name, wantEnabled)
		if err != nil {
			return util.SendFailed(stream, err.Error())
		}
		changed = changed || enabledChanged
		output["enabled"] = wantEnabled
	}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, changed, output)
}

func (m *Module) applyStopped(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string) error {
	active, err := util.ServiceActive(ctx, m.Runner, init, name)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !active {
		return util.SendFinal(stream, false, map[string]any{"name": name, "active": false})
	}
	if err := m.stop(ctx, init, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	return util.SendFinal(stream, true, map[string]any{"name": name, "active": false})
}

func (m *Module) applyRestarted(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload ДО restart: без него systemd рестартует со старым unit-ом.
	// reload не влияет на changed (restarted и так безусловно changed=true).
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// restarted — безусловно changed=true: пользователь явно попросил рестарт,
	// например после core.file.present обновил конфиг и хочет, чтобы service
	// перечитал. Идемпотентности тут быть не должно.
	if err := m.restart(ctx, init, name); err != nil {
		return util.SendFailed(stream, err.Error())
	}
	output := map[string]any{"name": name, "active": true}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, true, output)
}

func (m *Module) applyEnabled(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], init util.InitSystem, name string, req *pluginv1.ApplyRequest) error {
	mode, err := daemonReloadMode(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// daemon-reload ДО enable: новый/изменённый unit должен быть подхвачен
	// systemd до создания enable-симлинков. reload не влияет на changed.
	reloaded, err := util.EnsureDaemonReloaded(ctx, m.Runner, init, name, mode)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	changed, err := m.ensureEnabled(ctx, init, name, true)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	output := map[string]any{"name": name, "enabled": true}
	if reloaded {
		output["reloaded"] = true
	}
	return util.SendFinal(stream, changed, output)
}

// ensureEnabled приводит autostart-состояние юнита к want (true = enable,
// false = disable) идемпотентно: сперва читает isEnabled, действие выполняет
// только при расхождении. Возвращает changed. Общая логика для state `enabled`
// и для param `enabled` в state `running`.
func (m *Module) ensureEnabled(ctx context.Context, init util.InitSystem, name string, want bool) (bool, error) {
	enabled, err := m.isEnabled(ctx, init, name)
	if err != nil {
		return false, err
	}
	if enabled == want {
		return false, nil
	}
	if want {
		return true, m.enable(ctx, init, name)
	}
	return true, m.disable(ctx, init, name)
}

func (m *Module) isEnabled(ctx context.Context, init util.InitSystem, name string) (bool, error) {
	switch init {
	case util.InitSystemSystemd:
		r := m.Runner.Run(ctx, "systemctl", "is-enabled", "--quiet", name)
		if r.Err != nil {
			return false, fmt.Errorf("systemctl is-enabled: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	case util.InitSystemOpenRC:
		// rc-update show default | grep -q name
		r := m.Runner.Run(ctx, "rc-update", "show", "default")
		if r.Err != nil {
			return false, fmt.Errorf("rc-update show: %v", r.Err)
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 && fields[0] == name {
				return true, nil
			}
		}
		return false, nil
	case util.InitSystemSysV:
		// chkconfig --list name → exit 0 если есть.
		r := m.Runner.Run(ctx, "chkconfig", "--list", name)
		if r.Err != nil {
			return false, fmt.Errorf("chkconfig: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	}
	return false, fmt.Errorf("isEnabled: unsupported init %q", init)
}

func (m *Module) start(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "start")
}
func (m *Module) stop(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "stop")
}
func (m *Module) restart(ctx context.Context, init util.InitSystem, name string) error {
	return m.svcAction(ctx, init, name, "restart")
}

func (m *Module) svcAction(ctx context.Context, init util.InitSystem, name, action string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", action, name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-service", name, action)
	case util.InitSystemSysV:
		return m.must(ctx, "service", name, action)
	}
	return fmt.Errorf("svcAction: unsupported init %q", init)
}

func (m *Module) enable(ctx context.Context, init util.InitSystem, name string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", "enable", name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-update", "add", name, "default")
	case util.InitSystemSysV:
		return m.must(ctx, "chkconfig", name, "on")
	}
	return fmt.Errorf("enable: unsupported init %q", init)
}

func (m *Module) disable(ctx context.Context, init util.InitSystem, name string) error {
	switch init {
	case util.InitSystemSystemd:
		return m.must(ctx, "systemctl", "disable", name)
	case util.InitSystemOpenRC:
		return m.must(ctx, "rc-update", "del", name, "default")
	case util.InitSystemSysV:
		return m.must(ctx, "chkconfig", name, "off")
	}
	return fmt.Errorf("disable: unsupported init %q", init)
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
