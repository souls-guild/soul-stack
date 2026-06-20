package util

import (
	"context"
	"fmt"
	"strings"
)

// ServiceActive — init-агностичная проверка, запущен ли сервис name. Единый
// источник истины для core.service (idempotency-проверка active-состояния) и
// core-beacon `core.beacon.service_down` (наблюдение активности). Раньше эти
// потребители держали свои копии и разъезжались (OpenRC false-up баг:
// `rc-service status` даёт exit 0 и для остановленного сервиса).
//
// Корректная форма по init-системам:
//   - systemd: `systemctl is-active --quiet` → exit 0 = active;
//   - OpenRC:  `rc-service <name> status` → exit 0 + stdout содержит "started"
//     (одного exit 0 НЕ достаточно — он бывает и у stopped, exit 3 = stopped);
//   - SysV:    `service <name> status` → exit 0 = active.
//
// Ошибка — только если runner не смог выполнить команду либо init-система не из
// поддерживаемого набора. Не-нулевой exit интерпретируется как «не активен», а
// не как ошибка (это валидное состояние).
func ServiceActive(ctx context.Context, runner Runner, init InitSystem, name string) (bool, error) {
	switch init {
	case InitSystemSystemd:
		r := runner.Run(ctx, "systemctl", "is-active", "--quiet", name)
		if r.Err != nil {
			return false, fmt.Errorf("systemctl is-active: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	case InitSystemOpenRC:
		r := runner.Run(ctx, "rc-service", name, "status")
		if r.Err != nil {
			return false, fmt.Errorf("rc-service status: %v", r.Err)
		}
		return r.ExitCode == 0 && strings.Contains(r.Stdout, "started"), nil
	case InitSystemSysV:
		r := runner.Run(ctx, "service", name, "status")
		if r.Err != nil {
			return false, fmt.Errorf("service status: %v", r.Err)
		}
		return r.ExitCode == 0, nil
	}
	return false, fmt.Errorf("ServiceActive: unsupported init %q", init)
}

// DaemonReloadMode — режим централизованного daemon-reload в core.service
// (param `daemon_reload`, ADR-015 amendment). Closed-set: применяется перед
// мутирующими actions (running/restarted/enabled).
type DaemonReloadMode string

const (
	// DaemonReloadAuto — gated по systemd-флагу NeedDaemonReload: reload только
	// при рассинхроне unit-файла с загруженным определением. Дефолт.
	DaemonReloadAuto DaemonReloadMode = "auto"
	// DaemonReloadAlways — безусловный daemon-reload перед action.
	DaemonReloadAlways DaemonReloadMode = "always"
	// DaemonReloadNever — явный opt-out: reload не делается вообще.
	DaemonReloadNever DaemonReloadMode = "never"
)

// EnsureDaemonReloaded выполняет `systemctl daemon-reload` перед мутирующим
// action core.service (start/restart/enable), если того требует режим mode и
// init-система. Закрывает баг: после изменения unit-файла без daemon-reload
// `systemctl restart` тихо рестартует со СТАРЫМ определением (exit 0, лишь
// warning).
//
// Семантика по init-системам и режимам:
//   - non-systemd (openrc/sysv) — no-op (false, nil): у них нет daemon-reload;
//   - mode never — no-op (false, nil): явный opt-out оператора;
//   - mode always — безусловный `systemctl daemon-reload` (true, nil);
//   - mode auto — `systemctl show <name> --property=NeedDaemonReload --value`;
//     при `yes` → reload (true), иначе no-op (false). На первом install нового
//     unit флаг = `no` (systemd подхватит определение на start) — reload не нужен.
//
// reloaded возвращается для диагностики (output["reloaded"]) и НЕ влияет на
// changed шага: reload — побочное условие применения, не самостоятельное
// изменение состояния сервиса. Исполняется через тот же Runner, что прочие
// systemctl-вызовы модуля (мокается в unit-тестах).
func EnsureDaemonReloaded(ctx context.Context, runner Runner, init InitSystem, name string, mode DaemonReloadMode) (reloaded bool, err error) {
	if init != InitSystemSystemd || mode == DaemonReloadNever {
		return false, nil
	}
	if mode == DaemonReloadAuto {
		r := runner.Run(ctx, "systemctl", "show", name, "--property=NeedDaemonReload", "--value")
		if r.Err != nil {
			return false, fmt.Errorf("systemctl show NeedDaemonReload: %v", r.Err)
		}
		if strings.TrimSpace(r.Stdout) != "yes" {
			return false, nil
		}
	}
	r := runner.Run(ctx, "systemctl", "daemon-reload")
	if r.Err != nil {
		return false, fmt.Errorf("systemctl daemon-reload: %v", r.Err)
	}
	if r.ExitCode != 0 {
		return false, fmt.Errorf("systemctl daemon-reload exited %d: %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return true, nil
}
