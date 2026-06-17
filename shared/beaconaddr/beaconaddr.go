// Package beaconaddr — единый источник канонических адресов встроенных
// core-beacon ([ADR-030], срез S1).
//
// Адрес beacon-а (`core.beacon.<name>`, VigilDef.check) нужен ОБЕИМ сторонам:
//   - Keeper (`keeper/internal/oracle`) — closed enum валидации VigilDef.check
//     (неизвестный check → ошибка валидации, а не молча неисполнимый Vigil);
//   - Soul (`soul/internal/beacon`) — статический реестр тел проверок, по этим
//     же адресам адресуемых.
//
// ADR-011 запрещает keeper→soul import, поэтому раньше списки дублировались и
// разъезжались (S3-баг: keeper-enum отстал на 4 адреса → ложный 422 на валидном
// Vigil). Канонический список здесь, в нейтральном `shared/` (его импортируют и
// keeper, и soul, но НЕ друг друга), убирает этот дубль источника истины.
//
// Beacon — Vigil-check (read-only наблюдатель), а НЕ apply-модуль, поэтому
// адреса живут отдельным пакетом, а не в shared/coremanifest (реестр
// input-манифестов apply-модулей).
//
// Plugin-kind `soul_beacon` (community-проверки, S5) ещё не введён — до того
// набор закрыт этим списком.
package beaconaddr

// Адреса встроенных core-beacon MVP (`core.beacon.<name>`, VigilDef.check).
// При добавлении нового core-beacon — добавить константу сюда и в [All];
// инвариант-тест keeper-enum == soul-registry == этот список ловит рассинхрон.
const (
	ServiceDown   = "core.beacon.service_down"
	FileChanged   = "core.beacon.file_changed"
	PortClosed    = "core.beacon.port_closed"
	DiskFull      = "core.beacon.disk_full"
	ProcessAbsent = "core.beacon.process_absent"
	HTTPUnhealthy = "core.beacon.http_unhealthy"
	// Inotify — Linux-only kernel inotify syscall (V5-3, ADR-030 amendment
	// 2026-05-26). На non-Linux платформах beacon отдаёт явную ошибку
	// "platform not supported"; адрес-константа доступна везде ради единого
	// source-of-truth keeper-enum / soul-registry.
	Inotify = "core.beacon.inotify"
)

// All возвращает все канонические адреса core-beacon MVP. Свежий срез на каждый
// вызов — caller не может молча мутировать общий список.
func All() []string {
	return []string{
		ServiceDown,
		FileChanged,
		PortClosed,
		DiskFull,
		ProcessAbsent,
		HTTPUnhealthy,
		Inotify,
	}
}
