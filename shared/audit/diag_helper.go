package audit

import "github.com/souls-guild/soul-stack/shared/diag"

// FormatDiagnostics converts a list of validator diagnostics into an
// audit-friendly slice for the `payload.validation_errors[]` field of the
// `config.reload_failed` event (see [ADR-022(j)](docs/architecture.md)).
//
// Each entry holds a set of keys per the ADR-022(j) convention:
//
//   - "code"      — stable snake_case code ([diag.Diagnostic.Code]).
//   - "message"   — human-readable description.
//   - "phase"     — validation-pipeline phase ([diag.Phase]).
//   - "level"     — diagnostic level ([diag.Level]).
//   - "yaml_path" — goccy/go-yaml path; omitted if empty.
//   - "line"      — position in the file; omitted if 0.
//   - "column"    — position in the file; omitted if 0.
//
// The helper is reused by any audit-pipeline write-path initiators whose payload
// includes a diagnostic list (hot-reload on SIGHUP, API/MCP reload endpoints,
// lint-runs).
//
// nil/empty input → nil. Deliberate: a caller that got no diagnostics may omit the
// `validation_errors` key from the payload entirely.
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
