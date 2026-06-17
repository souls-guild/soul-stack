// Package diag описывает структурную диагностику валидаторов Soul Stack.
//
// Используется парсерами YAML (`shared/config`, `shared/manifest`, парсеры
// destiny/scenario), офлайн-линтером (`soul-lint`) и hot-reload-pipeline
// (`config.reload_failed → validation_errors[]`, [ADR-021](docs/architecture.md),
// [ADR-022](docs/architecture.md)). Каталог `Code` — стабильные snake_case-имена
// из категории Parser / validation errors ([docs/naming-rules.md → Error codes]).
package diag

// Level — открытый, но ограниченный enum уровней диагностики.
// Maps на семантику apply-channel-а / Operator API: error блокирует, warning
// не блокирует, hint только показывается.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelHint    Level = "hint"
)

// Phase — фаза validation pipeline, на которой обнаружена ошибка.
// Используется в audit-event `config.reload_failed` ([ADR-021](docs/architecture.md))
// и в выводе `soul-lint` для группировки.
//
// Значения:
//   - PhaseParse            — синтаксический parse YAML → AST.
//   - PhaseSchemaValidate   — проверка структуры/типов/enum/range.
//   - PhaseSemanticValidate — regex и cross-field invariant-ы.
//   - PhaseWriteBack        — фаза записи (rendering AST → bytes, atomic
//     rename, symlink-reject, round-trip degradation) под [ADR-021].
type Phase string

const (
	PhaseParse            Phase = "parse"
	PhaseSchemaValidate   Phase = "schema_validate"
	PhaseSemanticValidate Phase = "semantic_validate"
	PhaseWriteBack        Phase = "write_back"
)

// Diagnostic — одна запись валидатора.
//
// `File`/`Line`/`Column` — позиция в исходном файле (если известна; для
// cross-field invariant Line/Column могут быть 0).
// `YAMLPath` — путь в формате goccy/go-yaml (`$.auth.jwt.signing_key_ref`).
// `Hint` опционален: подсказка оператору, как починить.
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

// HasErrors — true, если в slice есть хотя бы одна запись уровня error.
func HasErrors(ds []Diagnostic) bool {
	for i := range ds {
		if ds[i].Level == LevelError {
			return true
		}
	}
	return false
}
