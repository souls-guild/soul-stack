package statemigrate

import (
	"encoding/json"
	"sort"
	"strings"
)

// deepCopyMap делает глубокую копию map[string]any через JSON round-trip.
// Значения incarnation.state — JSON-safe (читались из JSONB / YAML-фикстур),
// поэтому marshal не падает. nil/пустой → пустой map (state ядра не nil).
//
// Узкая копия паттерна keeper/internal/scenario/state.deepCopyMap: ядро
// statemigrate не тянет зависимость от scenario-пакета.
func deepCopyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// sortedKeys возвращает ключи map в лексикографическом порядке (детерминизм
// foreach-итерации над map: миграция — воспроизводимая функция).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// joinKeys собирает плоский список ключей в человекочитаемый адрес (для
// диагностики путей). Сегменты в state JSON-safe строковые.
func joinKeys(keys []string) string {
	return "state." + strings.Join(keys, ".")
}
