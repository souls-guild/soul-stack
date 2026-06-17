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
