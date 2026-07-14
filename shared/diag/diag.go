// Package diag describes the structured diagnostics of Soul Stack validators.
//
// Used by the YAML parsers (`shared/config`, `shared/manifest`, the destiny/scenario
// parsers), the offline linter (`soul-lint`), and the hot-reload pipeline
// (`config.reload_failed → validation_errors[]`, [ADR-021](docs/architecture.md),
// [ADR-022](docs/architecture.md)). The `Code` catalog holds stable snake_case names
// from the Parser / validation errors category ([docs/naming-rules.md → Error codes]).
package diag

// Level is the open but bounded enum of diagnostic levels. Maps onto apply-channel /
// Operator API semantics: error blocks, warning does not block, hint is only shown.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelHint    Level = "hint"
)

// Phase is the validation-pipeline phase at which an error was found. Used in the
// `config.reload_failed` audit event ([ADR-021](docs/architecture.md)) and in
// `soul-lint` output for grouping.
//
// Values:
//   - PhaseParse            — syntactic YAML → AST parse.
//   - PhaseSchemaValidate   — structure/type/enum/range checks.
//   - PhaseSemanticValidate — regex and cross-field invariants.
//   - PhaseWriteBack        — the write phase (rendering AST → bytes, atomic
//     rename, symlink-reject, round-trip degradation) under [ADR-021].
type Phase string

const (
	PhaseParse            Phase = "parse"
	PhaseSchemaValidate   Phase = "schema_validate"
	PhaseSemanticValidate Phase = "semantic_validate"
	PhaseWriteBack        Phase = "write_back"
)

// Diagnostic is a single validator record.
//
// `File`/`Line`/`Column` — position in the source file (if known; for a cross-field
// invariant Line/Column may be 0).
// `YAMLPath` — path in goccy/go-yaml format (`$.auth.jwt.signing_key_ref`).
// `Hint` is optional: a hint to the operator on how to fix it.
type Diagnostic struct {
	Level    Level  `json:"level"`
	Phase    Phase  `json:"phase"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	YAMLPath string `json:"yaml_path,omitempty"`
}

// HasErrors returns true if the slice has at least one error-level record.
func HasErrors(ds []Diagnostic) bool {
	for i := range ds {
		if ds[i].Level == LevelError {
			return true
		}
	}
	return false
}
