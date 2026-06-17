package audit

import "github.com/souls-guild/soul-stack/shared/diag"

// FormatDiagnostics конвертирует список диагностик валидатора в
// audit-friendly slice для поля `payload.validation_errors[]` события
// `config.reload_failed` (см. [ADR-022(j)](docs/architecture.md)).
//
// Каждая запись содержит набор ключей по convention ADR-022(j):
//
//   - "code"      — стабильный snake_case-код ([diag.Diagnostic.Code]).
//   - "message"   — человеко-читаемое описание.
//   - "phase"     — фаза validation pipeline ([diag.Phase]).
//   - "level"     — уровень диагностики ([diag.Level]).
//   - "yaml_path" — путь goccy/go-yaml; опускается, если пустой.
//   - "line"      — позиция в файле; опускается, если 0.
//   - "column"    — позиция в файле; опускается, если 0.
//
// Helper переиспользуется любыми write-path-инициаторами audit-pipeline-а,
// у которых payload включает diagnostic-список (hot-reload по SIGHUP,
// API/MCP reload-endpoints, lint-runs).
//
// nil/пустой вход → nil. Это сознательно: caller, не получивший ни одной
// диагностики, может опустить ключ `validation_errors` в payload вообще.
func FormatDiagnostics(diags []diag.Diagnostic) []map[string]any {
	if len(diags) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(diags))
	for i := range diags {
		d := &diags[i]
		entry := map[string]any{
			"code":    d.Code,
			"message": d.Message,
			"phase":   string(d.Phase),
			"level":   string(d.Level),
		}
		if d.YAMLPath != "" {
			entry["yaml_path"] = d.YAMLPath
		}
		if d.Line != 0 {
			entry["line"] = d.Line
		}
		if d.Column != 0 {
			entry["column"] = d.Column
		}
		out = append(out, entry)
	}
	return out
}
