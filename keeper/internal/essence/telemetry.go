package essence

import (
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// telemetryIntervalCeilSec — верхний sanity-потолок интервала (bound на
// Redis-TTL, ADR-072/NIM-87). Нижняя граница — config.TelemetryIntervalFloor.
const telemetryIntervalCeilSec = 3600

// essence-ключи override-а телеметрии (плоские, как node_exporter_*).
const (
	essenceKeyTelemetryInterval   = "telemetry_interval"
	essenceKeyTelemetryCollectors = "telemetry_collectors"
)

// ResolveEffectiveTelemetry сливает манифест-политику `telemetry:` с
// essence-override-ом в эффективный конфиг host-vitals (ADR-072, NIM-87). Чистая
// функция: PG/IO не трогает — тестируется изолированно.
//
//   - enabled  = m.EnabledOrDefault() (essence НЕ переопределяет enabled);
//   - interval = essence[telemetry_interval] (string) иначе m.IntervalOrDefault();
//     ParseDuration + clamp [floor, ceil]; непарсибельный override → default 30s;
//   - collectors = essence[telemetry_collectors] ([]string|[]any) иначе
//     m.CollectorsOrDefault(); список REPLACE (не union), отфильтрован по
//     config.IsKnownCollector. Пустой после фильтра (essence целиком из
//     неизвестных имён) → откат на манифест/дефолт: на провод ВСЕГДА уходит
//     непустой явный набор (Soul трактует [] как fail-open «все 5» ≠ намерение).
//
// m == nil (нет блока `telemetry:`) — валиден: nil-safe геттеры дают дефолты.
func ResolveEffectiveTelemetry(m *config.TelemetryConfig, essence map[string]any) *keeperv1.TelemetryConfig {
	interval := m.IntervalOrDefault()
	if ov, ok := essenceString(essence, essenceKeyTelemetryInterval); ok {
		interval = ov
	}
	collectors := filterKnownCollectors(m.CollectorsOrDefault())
	if ov, ok := essenceStringSlice(essence, essenceKeyTelemetryCollectors); ok {
		if filtered := filterKnownCollectors(ov); len(filtered) > 0 {
			collectors = filtered
		}
	}
	return &keeperv1.TelemetryConfig{
		Enabled:     m.EnabledOrDefault(),
		IntervalSec: clampIntervalSec(interval),
		Collectors:  collectors,
	}
}

// clampIntervalSec парсит duration-строку и клампит секунды в [floor, ceil].
// Непарсибельная строка (essence на load НЕ валидируется, в отличие от манифеста)
// → откат на built-in default 30s: опечатка в essence не должна разгонять весь
// флот до 10s-флора.
func clampIntervalSec(s string) int32 {
	floor := int64(config.TelemetryIntervalFloor / time.Second)
	d, err := config.ParseDuration(s)
	if err != nil {
		d = 30 * time.Second
	}
	sec := int64(d / time.Second)
	if sec < floor {
		sec = floor
	}
	if sec > telemetryIntervalCeilSec {
		sec = telemetryIntervalCeilSec
	}
	return int32(sec)
}

// filterKnownCollectors оставляет только коллекторы из закрытого набора
// (config.IsKnownCollector), сохраняя порядок. Всегда новый слайс (не мутирует
// вход). Пустой результат возможен (список из одних неизвестных) — caller
// (ResolveEffectiveTelemetry) откатывается на дефолт, на провод пусто не уходит.
func filterKnownCollectors(in []string) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		if config.IsKnownCollector(c) {
			out = append(out, c)
		}
	}
	return out
}

// UnknownTelemetryCollectors возвращает essence-collectors, не входящие в
// закрытый набор (observability: опечатка оператора видна в логах keeper). nil,
// если ключа нет / он не список строк / все имена известны.
func UnknownTelemetryCollectors(essence map[string]any) []string {
	ov, ok := essenceStringSlice(essence, essenceKeyTelemetryCollectors)
	if !ok {
		return nil
	}
	var unknown []string
	for _, c := range ov {
		if !config.IsKnownCollector(c) {
			unknown = append(unknown, c)
		}
	}
	return unknown
}

// essenceString читает строковый essence-override: (v, true) только если ключ
// есть И это непустая строка (пустая/не-строка → fallback на манифест).
func essenceString(essence map[string]any, key string) (string, bool) {
	raw, ok := essence[key]
	if !ok {
		return "", false
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// essenceStringSlice читает списочный essence-override collectors: []string либо
// []any строк (YAML-unmarshal даёт []any). (nil, false) если ключ отсутствует
// или не распознан как список строк — тогда fallback на манифест.
func essenceStringSlice(essence map[string]any, key string) ([]string, bool) {
	raw, ok := essence[key]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	default:
		return nil, false
	}
}
