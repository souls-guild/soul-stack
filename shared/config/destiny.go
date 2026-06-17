package config

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// DestinyManifest — типизированное представление `destiny.yml` по нормативной
// спеке [`docs/destiny/manifest.md`].
//
// Манифест содержит только декларацию (имя, описание, входной/выходной
// контракт, список custom-модулей). Список задач лежит в `tasks/main.yml`,
// `vars:` — в `vars.yml`; в манифесте этих секций нет (см. deprecated keys
// ниже).
type DestinyManifest struct {
	Name            string         `yaml:"name"`
	Description     string         `yaml:"description,omitempty"`
	Input           InputSchemaMap `yaml:"input,omitempty"`
	Output          InputSchemaMap `yaml:"output,omitempty"`
	RequiredModules []string       `yaml:"required_modules,omitempty"`
}

// reDestinyName — canonical kebab-case имя destiny (`redis`, `cert-rotation`).
// Dash только между алфанумериков, без trailing/leading/double-dash.
// Совпадает с именем папки `destiny-<name>/` без префикса.
var reDestinyName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// reRequiredModule — двухуровневая форма `<namespace>.<module>` для custom-
// модулей в `required_modules:`. core-модули не перечисляются. Kebab-case
// без underscore (naming-rules.md §57/§186). Один источник истины с
// `reDependencyModuleName` (service.go) — дубль regex был drift-источником.
var reRequiredModule = reDependencyModuleName

// deprecatedDestinyKeys — устаревшие top-level ключи, для которых даём
// явный hint вместо общего "unknown_key". См. docs/destiny/manifest.md →
// «Что в destiny.yml НЕ лежит».
var deprecatedDestinyKeys = map[string]string{
	"tasks":     "task list moved to tasks/main.yml per ADR-009; destiny.yml is manifest-only",
	"steps":     "task list moved to tasks/main.yml per ADR-009; destiny.yml is manifest-only",
	"vars":      "destiny-locals moved to vars.yml (sibling of destiny.yml); see docs/destiny/vars.md",
	"version":   "version is a git ref under which destiny is committed, not a manifest field; see ADR-007",
	"templates": "templates/ is a folder, not a manifest field; .tmpl files are picked up by convention",
	"tests":     "tests/ is a folder, not a manifest field; molecule-style tests are picked up by convention",
}

// schemaValidateDestiny — пост-decode проверки DestinyManifest.
func schemaValidateDestiny(path string, root *ast.MappingNode, m *DestinyManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) deprecated keys — особый hint, поднимаем по AST (чтобы получить line/col).
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedDestinyKeys[tok.Value]
		if !dep {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     hint,
			YAMLPath: "$." + tok.Value,
		}))
	}

	// 2) `name:` required + format.
	// Ветка `topKeys["name"]` различает «ключ отсутствует» от «ключ есть с
	// пустой/null/невалидной строкой». Пустая строка и `null`-значение
	// типологически валидны для type=string, но как имя destiny нарушают
	// формат — поднимаем `name_invalid_format`, а не `missing_required_field`,
	// чтобы оператор не искал отсутствующий ключ глазами.
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching destiny-<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reDestinyName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reDestinyName)
		if m.Name == "" {
			msg = "name must be non-empty kebab-case string"
		}
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: msg,
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
		}))
	}

	// 3) required_modules — двухуровневая форма `<namespace>.<module>`.
	for i, mod := range m.RequiredModules {
		if !reRequiredModule.MatchString(mod) {
			out = append(out, atPath(root, fmt.Sprintf("$.required_modules[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "required_module_invalid_format",
				Message: fmt.Sprintf("required_modules[%d] = %q does not match <namespace>.<module>", i, mod),
				Hint:    "two-level address per architecture.md → «Адресация модулей»; core-modules are not listed here",
			}))
		}
	}

	// 4) input: / output: — рекурсивная валидация через общий валидатор.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}
	if topKeys["output"] {
		out = append(out, validateInputSchemaMap(m.Output, findInputMapping(root, "output"), "$.output")...)
	}

	return out
}

// semanticValidateDestiny — на M1.2.a отдельных semantic-инвариантов нет.
// Сохраняем сигнатуру для симметрии с Keeper/Soul на случай добавления
// cross-field-проверок (например, default-значение CEL/template-выражение
// против input-type будет проверяться на M1.3+).
func semanticValidateDestiny(_ *DestinyManifest, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}
