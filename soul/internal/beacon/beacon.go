// Package beacon — Soul-side event-driven мониторинг (ADR-030, срез S1).
//
// Состав:
//   - [Beacon]: read-only интерфейс тела проверки (`core.beacon.<name>`,
//     параллель core-модулям). Beacon наблюдает состояние хоста и НЕ мутирует
//     его — это инвариант конструкции (ADR-030).
//   - [Registry]: статический реестр встроенных core-beacon (как coremod.Default
//     для модулей). Покрывает весь канонический набор адресов
//     [beaconaddr.All] (service_down / file_changed / port_closed / disk_full /
//     process_absent / http_unhealthy); [Default] паникует при рассинхроне с
//     ним — баг сборки, не ввод. soul_beacon-плагины — S5, сейчас только
//     встроенные.
//   - [Scheduler] (scheduler.go): per-process планировщик активного набора
//     Vigil, edge-triggered эмиссия Portent.
//
// Soul-safe изоляция (ADR-012(d)): пакет не тянет Vault/cel-go — beacon-проверки
// читают только локальное состояние хоста.
package beacon

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// State — результат одной beacon-проверки: непрозрачная строка состояния хоста.
// Scheduler сравнивает её с предыдущим значением (edge-triggered): смена State
// → Portent. Семантика строки — на усмотрение конкретного beacon-а
// (`core.beacon.service_down` → "up"/"down"; `core.beacon.file_changed` → хеш
// файла либо "missing").
type State = string

// Beacon — тело одной проверки. Read-only по конструкции (ADR-030): Check
// наблюдает состояние хоста и возвращает его, но НЕ изменяет систему.
//
// Возвращает:
//   - state: текущее состояние (см. [State]);
//   - data:  факты для PortentEvent.data (путь файла, имя сервиса, хеш и т.п.);
//     может быть nil, тогда Portent несёт только base-поля scheduler-а;
//   - err:   проверка не смогла выполниться (например невалидный param). На
//     ошибке scheduler пропускает тик — baseline/last-state не трогаются, Portent
//     не эмитится (ошибка проверки ≠ смена состояния хоста).
type Beacon interface {
	Check(ctx context.Context, params *structpb.Struct) (state State, data *structpb.Struct, err error)
}

// BeaconLookup — узкий интерфейс резолва beacon-а по адресу VigilDef.check.
// Реализуют статический [Registry] (core-beacon) и [CompositeRegistry]
// (core + plugin-beacon ADR-030 V5-2). Scheduler оперирует только этим
// интерфейсом — не различает встроенные и plugin-beacon (Composite-резолв
// решает диспетчеризацию).
type BeaconLookup interface {
	Lookup(name string) (Beacon, bool)
}

// Registry — статический набор встроенных core-beacon, адресуемых по имени
// (`core.beacon.service_down` / `core.beacon.file_changed`). Иммутабелен после
// сборки [Default]; Lookup — единственная операция, нужная scheduler-у.
type Registry struct {
	beacons map[string]Beacon
}

// Default собирает реестр всех встроенных core-beacon MVP (ADR-030 S1).
//
// Покрытие канонического набора [beaconaddr.All] проверяется тут же: реестр
// обязан содержать impl ровно для каждого адреса и не больше. Рассинхрон —
// программный баг сборки (забыли зарегистрировать новый beacon или адрес
// уехал из общего источника), а не пользовательский ввод → паника при инициа-
// лизации, а не молча неполный реестр (тот самый класс, что давал S3/OpenRC
// баги до выноса в shared).
func Default() *Registry {
	beacons := map[string]Beacon{
		ServiceDownName:   NewServiceDown(),
		FileChangedName:   NewFileChanged(),
		PortClosedName:    NewPortClosed(),
		DiskFullName:      NewDiskFull(),
		ProcessAbsentName: NewProcessAbsent(),
		HTTPUnhealthyName: NewHTTPUnhealthy(),
		InotifyName:       NewInotify(),
	}
	canonical := beaconaddr.All()
	if len(beacons) != len(canonical) {
		panic(fmt.Sprintf("beacon: реестр (%d) рассинхронен с beaconaddr.All (%d)", len(beacons), len(canonical)))
	}
	for _, addr := range canonical {
		if _, ok := beacons[addr]; !ok {
			panic(fmt.Sprintf("beacon: канонический адрес %q не зарегистрирован в Default", addr))
		}
	}
	return &Registry{beacons: beacons}
}

// Lookup возвращает beacon по адресу `core.beacon.<name>` (VigilDef.check).
// Второй результат false — нет такого встроенного beacon-а (scheduler
// логирует и пропускает Vigil, не падая).
func (r *Registry) Lookup(name string) (Beacon, bool) {
	b, ok := r.beacons[name]
	return b, ok
}

// Names возвращает адреса всех зарегистрированных beacon-ов (для логов старта).
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.beacons))
	for name := range r.beacons {
		out = append(out, name)
	}
	return out
}
