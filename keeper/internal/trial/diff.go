package trial

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// deepEqualJSON сравнивает две Go-структуры, прошедшие через structpb
// (map[string]any / []any / float64 / string / bool / nil). Прямой
// reflect.DeepEqual достаточен: обе стороны нормализованы одинаковым
// structpb.AsMap (числа → float64, обход map детерминирован сравнением).
func deepEqualJSON(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// mergeStateChanges накладывает отрендеренные `state_changes.sets` поверх
// базового state и возвращает новый map (зеркало прод-коммита,
// scenario.mergeStateChanges / orchestration.md §7.1). Deep-copy базы через
// JSON round-trip (ожидаемый итог не должен держать ссылку на fixtures.state) +
// перезапись объявленных полей (last-wins на уровне поля). Пустой/nil base или
// sets обрабатываются естественно. Данные JSON-safe (YAML-фикстуры: maps/slices/
// скаляры), marshal не падает; при сбое — пустая база, расхождение поймает сверка.
func mergeStateChanges(base, sets map[string]any) map[string]any {
	out := map[string]any{}
	if len(base) > 0 {
		if b, err := json.Marshal(base); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	for field, val := range sets {
		out[field] = val
	}
	return out
}

// hasErrors — есть ли среди диагностик ошибки уровня error.
func hasErrors(ds []diag.Diagnostic) bool {
	return diag.HasErrors(ds)
}

// formatDiags сводит диагностики в одну строку для сообщения об ошибке.
func formatDiags(ds []diag.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.Code)
		if d.YAMLPath != "" {
			b.WriteString(" @ " + d.YAMLPath)
		}
		b.WriteString(": " + d.Message)
	}
	return b.String()
}
