package beacon

import (
	"log/slog"
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/beaconaddr"
	"google.golang.org/protobuf/types/known/structpb"
)

// Typed-payload mapper (V5-1, ADR-030 amendment 2026-05-26): проецирует
// data *structpb.Struct от встроенных core-beacon в типизированный
// PortentEvent.payload (oneof). Soul-side в течение 1-release deprecation
// period заполняет ОБЕ ветки (event.Data + event.Payload) — backward-compat
// для существующих where-CEL `event.data.<field>`. После 1-release —
// `data`-ветка удаляется hard-cut (S5-final, parity с push S7-decision).
//
// Маппинг плоский: переключатель по check-address из VigilDef.GetCheck() →
// конкретный builder. Неизвестный check (например plugin-beacon V5-2) →
// payload не выставляется, data-ветка ещё несёт сырой Struct.

// deprecationWarnOnce сигналит ровно один раз на процесс при первой эмиссии
// Portent-а с заполненными ОБЕИМИ ветками (data + payload) — чтобы оператор
// один раз увидел в логах факт hand-off-периода. log-spam при тысячах Portent-ов
// в час неприемлем.
var deprecationWarnOnce sync.Once

// fillTypedPayload выставляет PortentEvent.Payload (oneof) по check-address
// Vigil-а из data *structpb.Struct, возвращённого Check-ом конкретного beacon-а.
// nil-data → no-op (Payload остаётся nil). Неизвестный check → no-op (для
// plugin-beacon V5-2 ветка `custom` заполняется отдельно apply-loop-ом плагина,
// не здесь). Локальная функция в этом же пакете — приватный oneof-интерфейс
// keeperv1.isPortentEvent_Payload здесь доступен через прямое присваивание
// конкретного типа в Payload-поле.
func fillTypedPayload(ev *keeperv1.PortentEvent, check string, data *structpb.Struct) {
	if data == nil {
		return
	}
	switch check {
	case beaconaddr.FileChanged:
		ev.Payload = &keeperv1.PortentEvent_FileChanged{FileChanged: &keeperv1.FileChangedPortent{
			Path:   getString(data, "path"),
			Sha256: getString(data, "sha256"),
		}}
	case beaconaddr.ServiceDown:
		ev.Payload = &keeperv1.PortentEvent_ServiceDown{ServiceDown: &keeperv1.ServiceDownPortent{
			Service:    getString(data, "service"),
			Active:     getBool(data, "active"),
			InitSystem: getString(data, "init_system"),
		}}
	case beaconaddr.PortClosed:
		ev.Payload = &keeperv1.PortentEvent_PortClosed{PortClosed: &keeperv1.PortClosedPortent{
			Host: getString(data, "host"),
			Port: int32(getNumber(data, "port")),
		}}
	case beaconaddr.DiskFull:
		ev.Payload = &keeperv1.PortentEvent_DiskFull{DiskFull: &keeperv1.DiskFullPortent{
			Path:        getString(data, "path"),
			UsedPercent: getNumber(data, "used_percent"),
			Threshold:   getNumber(data, "threshold"),
		}}
	case beaconaddr.ProcessAbsent:
		ev.Payload = &keeperv1.PortentEvent_ProcessAbsent{ProcessAbsent: &keeperv1.ProcessAbsentPortent{
			Pattern: getString(data, "pattern"),
		}}
	case beaconaddr.HTTPUnhealthy:
		ev.Payload = &keeperv1.PortentEvent_HttpUnhealthy{HttpUnhealthy: &keeperv1.HttpUnhealthyPortent{
			Url:    getString(data, "url"),
			Status: int32(getNumber(data, "status")),
		}}
	case beaconaddr.Inotify:
		ev.Payload = &keeperv1.PortentEvent_Inotify{Inotify: buildInotifyPayload(data)}
	}
}

// buildInotifyPayload собирает InotifyPortent из data-Struct (V5-3). Список
// events приходит через `data.events: []map{type,file,at}` — projection одного
// узла в repeated typed-message. Пустой / отсутствующий events → пустой
// repeated, но Portent всё равно эмитится только при state="events"
// (scheduler-инвариант), поэтому пустой список не должен встретиться.
func buildInotifyPayload(data *structpb.Struct) *keeperv1.InotifyPortent {
	out := &keeperv1.InotifyPortent{
		Path:  getString(data, "path"),
		Count: int32(getNumber(data, "count")),
	}
	lv, ok := data.GetFields()["events"]
	if !ok || lv == nil {
		return out
	}
	list := lv.GetListValue()
	if list == nil {
		return out
	}
	for _, item := range list.GetValues() {
		s := item.GetStructValue()
		if s == nil {
			continue
		}
		out.Events = append(out.Events, &keeperv1.InotifyEvent{
			Type: getString(s, "type"),
			File: getString(s, "file"),
			At:   int64(getNumber(s, "at")),
		})
	}
	return out
}

// getString читает строковое поле Struct-а; отсутствует/не-строка → "".
func getString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

// getNumber читает числовое поле; отсутствует/не-число → 0. proto-json маршалит
// все числа во float64 (NumberValue), поэтому одна функция и для double, и для
// int (с явным cast int32(getNumber)).
func getNumber(s *structpb.Struct, key string) float64 {
	if s == nil {
		return 0
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return 0
	}
	return v.GetNumberValue()
}

// getBool читает bool-поле; отсутствует/не-bool → false.
func getBool(s *structpb.Struct, key string) bool {
	if s == nil {
		return false
	}
	v, ok := s.GetFields()[key]
	if !ok {
		return false
	}
	return v.GetBoolValue()
}

// emitDeprecationWarnOnce пишет один раз WARN-лог о dual-write data+payload.
// Вызывается из emit() после успешной постановки Portent с typed payload.
func emitDeprecationWarnOnce(logger *slog.Logger) {
	deprecationWarnOnce.Do(func() {
		logger.Warn("beacon: PortentEvent.data заполняется параллельно с typed payload — deprecated, 1-release WARN, удалится hard-cut в S5-final (V5-1 ADR-030 amendment 2026-05-26)")
	})
}
