package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ServiceManifest — типизированное представление корневого `service.yml` по
// нормативной спеке [`docs/service/manifest.md`].
//
// Манифест содержит только метаданные сервиса (имя/описание), контракт
// `state_schema` для `incarnation.state` в Postgres и плоский список git-
// зависимостей. Сценарии auto-discover-ятся по `scenario/<name>/main.yml`,
// поэтому отдельной секции `scenarios:` здесь нет.
type ServiceManifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`

	// StateSchemaVersion — версия структуры `incarnation.state`. Инкрементируется
	// явно при breaking-изменениях схемы; цепочка миграций живёт в `migrations/`
	// (валидация цепочки — out of scope MVP, M1.5).
	StateSchemaVersion int `yaml:"state_schema_version"`

	// StateSchema хранится плоским `map[string]any` (PM-decision): JSON Schema
	// draft-07 — большой стандарт, полная типизация в Go — отдельная работа.
	// MVP-validate (см. validateStateSchema) проверяет минимум: `type: object`
	// на корне, `required` — []string, `properties` — map<string, recursive>.
	// Расширенная JSON-Schema-валидация (`enum`/`pattern`/`min`/`max`/`items`) —
	// отдельный backlog-айтем.
	StateSchema map[string]any `yaml:"state_schema"`

	Destiny []DependencyRef `yaml:"destiny,omitempty"`
	Modules []DependencyRef `yaml:"modules,omitempty"`

	// RevealableSecrets — секреты инкарнации, раскрываемые оператором через
	// reveal-эндпоинт под правом incarnation.view-secrets (NIM-74). Generic:
	// сервис декларирует, что можно раскрыть, механизм не redis-специфичен.
	RevealableSecrets []RevealableSecret `yaml:"revealable_secrets,omitempty"`

	// Lifecycle — опциональная политика жизненного цикла инкарнаций сервиса
	// (architecture.md → «Service — структура и manifest» § lifecycle:).
	// Отсутствие блока (nil) = оба флага дефолтно true (backcompat): create
	// автоматически запускает scenario `create`, destroy запускает teardown по
	// обычной логике allow_destroy. Разыменование флагов — через
	// [LifecycleConfig.AutoCreateEnabled] / [LifecycleConfig.AutoDestroyEnabled]
	// (nil-safe: и nil-блок, и nil-флаг трактуются как true).
	Lifecycle *LifecycleConfig `yaml:"lifecycle,omitempty"`

	// Telemetry — опциональная политика host-vitals телеметрии (ADR-072, NIM-87).
	// Отсутствие блока (nil) = дефолт: enabled, interval 30s, все коллекторы.
	// Разыменование — через nil-safe геттеры [TelemetryConfig.EnabledOrDefault] /
	// [TelemetryConfig.IntervalOrDefault] / [TelemetryConfig.CollectorsOrDefault].
	Telemetry *TelemetryConfig `yaml:"telemetry,omitempty"`
}

// LifecycleConfig — блок `lifecycle:` манифеста сервиса. Оба флага —
// `*bool` (nil → дефолт true): отличает «оператор не задал» от «явно false».
type LifecycleConfig struct {
	// AutoCreate — `POST /v1/incarnations` автоматически запускает scenario
	// `create` (nil/true). false — инкарнация создаётся в `ready` без прогона,
	// оператор запускает `create` вручную из Run-формы.
	AutoCreate *bool `yaml:"auto_create,omitempty"`

	// AutoDestroy — удаление инкарнации запускает teardown-сценарий `destroy`
	// по обычной логике `allow_destroy` (nil/true). false — удаление всегда
	// прямое, без teardown, приоритет над `allow_destroy`.
	AutoDestroy *bool `yaml:"auto_destroy,omitempty"`
}

// AutoCreateEnabled — nil-safe чтение политики auto_create: nil-блок ИЛИ
// nil-флаг → true (backcompat по architecture.md).
func (l *LifecycleConfig) AutoCreateEnabled() bool {
	if l == nil || l.AutoCreate == nil {
		return true
	}
	return *l.AutoCreate
}

// AutoDestroyEnabled — nil-safe чтение политики auto_destroy: nil-блок ИЛИ
// nil-флаг → true (backcompat по architecture.md).
func (l *LifecycleConfig) AutoDestroyEnabled() bool {
	if l == nil || l.AutoDestroy == nil {
		return true
	}
	return *l.AutoDestroy
}

// KnownCollectors — закрытый набор host-vitals коллекторов (ADR-072, NIM-87).
var KnownCollectors = []string{"cpu", "mem", "disk", "load", "uptime"}

// TelemetryIntervalFloor — нижняя граница telemetry.interval (anti-DoS floor).
const TelemetryIntervalFloor = 10 * time.Second

// IsKnownCollector — принадлежит ли name закрытому набору KnownCollectors.
func IsKnownCollector(name string) bool {
	return contains(KnownCollectors, name)
}

// TelemetryConfig — блок `telemetry:` манифеста сервиса (ADR-072, NIM-87).
// Enabled — `*bool` (nil → дефолт true): отличает «не задано» от «явно false».
type TelemetryConfig struct {
	Enabled    *bool    `yaml:"enabled,omitempty"`
	Interval   *string  `yaml:"interval,omitempty"`
	Collectors []string `yaml:"collectors,omitempty"`
}

// EnabledOrDefault — nil-safe: nil-блок ИЛИ nil-флаг → true (backcompat).
func (t *TelemetryConfig) EnabledOrDefault() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// IntervalOrDefault — nil-safe: nil-блок ИЛИ nil/пустой Interval → "30s".
func (t *TelemetryConfig) IntervalOrDefault() string {
	if t == nil || t.Interval == nil || *t.Interval == "" {
		return "30s"
	}
	return *t.Interval
}

// CollectorsOrDefault — nil-safe: nil-блок ИЛИ пустой список → копия KnownCollectors.
func (t *TelemetryConfig) CollectorsOrDefault() []string {
	if t == nil || len(t.Collectors) == 0 {
		out := make([]string, len(KnownCollectors))
		copy(out, KnownCollectors)
		return out
	}
	return t.Collectors
}

// DependencyRef — запись в `destiny[]` / `modules[]`: `{name, ref}` + опц. `git`.
//
// `name` — имя destiny (kebab-case, одноуровневое) или модуля (двухуровневое
// `<namespace>.<module>`); разный regex применяется в зависимости от
// контекста (см. schemaValidateService → проход по слайсам).
// `ref` — git tag или branch (ADR-007). MVP допускает любую непустую строку;
// детальная проверка ref-формы (semver-tag / branch-naming) — backlog.
// `git` — опциональный per-entry override полного git-URL зависимости.
// Поддержан только для `destiny[]` (гибрид резолва: name → подстановка в
// `default_destiny_source`, git → прямой URL без шаблона). Для `modules[]`
// запрещён (см. validateDependencyRef) — drift до отдельного решения.
type DependencyRef struct {
	Name string `yaml:"name"`
	Ref  string `yaml:"ref"`
	Git  string `yaml:"git,omitempty"`
}

// RevealableSecret — запись секции `revealable_secrets[]` манифеста (NIM-74):
// декларация раскрываемого оператором секрета инкарнации.
//
//   - ID — стабильный идентификатор (kebab/snake, уникален); клиент шлёт его в
//     `secret_id` при reveal;
//   - Label — подпись для UI;
//   - Enumerate — state-путь массива объектов (`state.<segment>`); ключ = поле
//     `name` элемента (конвенция redis AclUser.name) — множество допустимых `key`;
//   - VaultRef — шаблон Vault-пути с плейсхолдерами `{incarnation}`/`{key}`
//     (литеральная подстановка strings.ReplaceAll, обе величины провалидированы +
//     vault.ParseRef режет traversal). Опц. `#field` — выбор поля секрета.
type RevealableSecret struct {
	ID        string `yaml:"id"`
	Label     string `yaml:"label"`
	Enumerate string `yaml:"enumerate"`
	VaultRef  string `yaml:"vault_ref"`
}

var (
	// reServiceName — canonical kebab-case: dash только между алфанумериков,
	// без trailing/leading/double-dash. Симметрично с `reDestinyName`.
	reServiceName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// reRevealID — id секрета revealable_secrets[]: lowercase-идентификатор с
	// `-`/`_`-разделителями (без trailing/leading/double). Допускает snake_case
	// (`user_password`) — контракт reveal фиксирует secret_id этой формы.
	reRevealID = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

	// reRevealEnumerate — форма enumerate: `state.<segment>[.<segment>…]`
	// (симметрично rePrefillFromStatePath).
	reRevealEnumerate = regexp.MustCompile(`^state(\.[a-z][a-z0-9_]*)+$`)

	// reRevealPlaceholder — плейсхолдер `{…}` в vault_ref (для проверки набора).
	reRevealPlaceholder = regexp.MustCompile(`\{[^}]*\}`)

	// reDependencyDestinyName — kebab-case одноуровневое имя destiny в
	// `destiny[]`. Совпадает с `reDestinyName` (destiny.go), переиспользуем
	// напрямую — отдельная копия regex была drift-источником.
	reDependencyDestinyName = reDestinyName

	// reDependencyModuleName — strict двухуровневая форма `<namespace>.<module>`
	// для custom-модулей в `service.yml → modules[]`. Симметрично с
	// `reRequiredModule` (destiny.go); canonical kebab-case в каждой половине
	// (без trailing/leading/double-dash), без underscore, naming-rules.md §57/§186.
	reDependencyModuleName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
)

// deprecatedServiceKeys — устаревшие top-level ключи в `service.yml`. Для
// каждого даём специфический hint, объясняющий «где это лежит на самом деле»
// (см. docs/service/manifest.md → «Что в service.yml НЕ лежит»). Симметрично
// `deprecatedDestinyKeys` в destiny.go.
var deprecatedServiceKeys = map[string]string{
	"version":   "version is a git ref under which service is committed, not a manifest field; see ADR-007",
	"tasks":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"steps":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"input":     "input lives in scenario/<name>/main.yml (input:-block per docs/input.md), not service.yml",
	"scenarios": "scenarios are auto-discovered from scenario/<name>/ directory; do not enumerate them in service.yml",
}

// schemaValidateService — пост-decode проверки ServiceManifest.
func schemaValidateService(path string, root *ast.MappingNode, m *ServiceManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) deprecated top-level ключи (по AST для line/col).
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedServiceKeys[tok.Value]
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

	// 2) name — required + format. Ветка `topKeys["name"]` отличает «ключа нет»
	// от «ключ есть с пустой/null строкой» (симметрично destiny.go).
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching service-<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reServiceName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reServiceName)
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

	// 3) state_schema_version — required + integer ≥ 1.
	// Дополнительно отлавливаем float (`1.5`): goccy при декоде в `int`
	// усекает значение без ошибки, поэтому проверяем AST явно — иначе оператор
	// думает, что записал «1.5», а Keeper хранит «1» (silent truncation).
	if !topKeys["state_schema_version"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_schema_version is required at top-level",
			Hint:     "set state_schema_version: 1 for fresh services; bump on breaking state schema changes (ADR-019)",
			YAMLPath: "$.state_schema_version",
		})
	} else if vn := findScalarValue(root, "state_schema_version"); vn != nil {
		if _, isFloat := vn.(*ast.FloatNode); isFloat {
			tok := vn.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_schema_version must be an integer, got float %q", tok.Value),
				Hint:     "use integer like 1, 2, 3 — version is monotonic, not a semver fraction",
				YAMLPath: "$.state_schema_version",
			}))
		} else if _, isInt := vn.(*ast.IntegerNode); !isInt {
			// Non-integer non-float (string/bool/sequence/mapping/null): decode уже
			// поднял `type_mismatch`, дополнительный `value_out_of_range "got 0"`
			// от zero-value `m.StateSchemaVersion` ввёл бы в заблуждение.
		} else if m.StateSchemaVersion < 1 {
			out = append(out, atPath(root, "$.state_schema_version", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("state_schema_version must be >= 1, got %d", m.StateSchemaVersion),
			}))
		}
	}

	// 4) state_schema — required + структурная валидация.
	if !topKeys["state_schema"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_schema is required at top-level",
			Hint:     "declare state_schema: { type: object, properties: {...} } — see docs/service/manifest.md → state_schema",
			YAMLPath: "$.state_schema",
		})
	} else {
		out = append(out, validateStateSchema(root, findInputMapping(root, "state_schema"), "$.state_schema")...)
	}

	// 5) destiny[] / modules[] — каждая запись валидна как `{name, ref}`.
	for i, dep := range m.Destiny {
		out = append(out, validateDependencyRef(root, "destiny", i, dep, reDependencyDestinyName)...)
	}
	for i, dep := range m.Modules {
		out = append(out, validateDependencyRef(root, "modules", i, dep, reDependencyModuleName)...)
	}

	// 6) revealable_secrets[] — reveal-декларации (NIM-74).
	seenRevealIDs := make(map[string]int, len(m.RevealableSecrets))
	for i, rs := range m.RevealableSecrets {
		out = append(out, validateRevealableSecret(root, i, rs, seenRevealIDs)...)
	}

	// 7) telemetry — опц. host-vitals политика (ADR-072, NIM-87). nil-блок
	// пропускаем (backcompat). Enabled не валидируем; cross-field инвариантов нет.
	if m.Telemetry != nil {
		for _, c := range m.Telemetry.Collectors {
			if !IsKnownCollector(c) {
				out = append(out, atPath(root, "$.telemetry.collectors", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "unknown_collector",
					Message: fmt.Sprintf("telemetry.collectors: unknown collector %q; known set: %s", c, strings.Join(KnownCollectors, ", ")),
					Hint:    "allowed collectors: " + strings.Join(KnownCollectors, ", "),
				}))
			}
		}
		if m.Telemetry.Interval != nil && *m.Telemetry.Interval != "" {
			if d, err := ParseDuration(*m.Telemetry.Interval); err != nil {
				out = append(out, atPath(root, "$.telemetry.interval", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "duration_invalid",
					Message: fmt.Sprintf("telemetry.interval: invalid duration %q: %v", *m.Telemetry.Interval, err),
					Hint:    "use Go-duration (e.g. 30s, 1m) or <N>d for days",
				}))
			} else if d < TelemetryIntervalFloor {
				out = append(out, atPath(root, "$.telemetry.interval", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "value_out_of_range",
					Message: fmt.Sprintf("telemetry.interval must be >= 10s (anti-DoS floor), got %s", d),
				}))
			}
		}
	}

	return out
}

// validateRevealableSecret — проверка одной записи `revealable_secrets[]` (NIM-74):
// id (required + reRevealID + уникален); enumerate (MVP required + форма
// `state.<segment>`); vault_ref (required + содержит `{key}` при заданном enumerate +
// плейсхолдеры только `{incarnation}`/`{key}`).
func validateRevealableSecret(root *ast.MappingNode, idx int, rs RevealableSecret, seen map[string]int) []diag.Diagnostic {
	var out []diag.Diagnostic
	base := fmt.Sprintf("$.revealable_secrets[%d]", idx)

	if rs.ID == "" {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].id is required", idx),
			Hint:    "declare a stable id (client passes it as secret_id)",
		}))
	} else if !reRevealID.MatchString(rs.ID) {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("revealable_secrets[%d].id %q does not match %s", idx, rs.ID, reRevealID),
			Hint:    "lowercase letters/digits with -/_ separators; must start with letter",
		}))
	} else if prev, dup := seen[rs.ID]; dup {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "duplicate_id",
			Message: fmt.Sprintf("revealable_secrets[%d].id %q duplicates revealable_secrets[%d].id", idx, rs.ID, prev),
			Hint:    "each revealable secret id must be unique within the list",
		}))
	} else {
		seen[rs.ID] = idx
	}

	if rs.Enumerate == "" {
		out = append(out, atPath(root, base+".enumerate", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].enumerate is required", idx),
			Hint:    "declare enumerate: state.<array> — element .name yields the keys",
		}))
	} else if !reRevealEnumerate.MatchString(rs.Enumerate) {
		out = append(out, atPath(root, base+".enumerate", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "enumerate_invalid_format",
			Message: fmt.Sprintf("revealable_secrets[%d].enumerate %q must have form state.<segment>", idx, rs.Enumerate),
			Hint:    "example: state.redis_users",
		}))
	}

	if rs.VaultRef == "" {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref is required", idx),
			Hint:    "example: secret/redis/{incarnation}/users/{key}#password",
		}))
		return out
	}
	for _, ph := range reRevealPlaceholder.FindAllString(rs.VaultRef, -1) {
		if ph != "{service}" && ph != "{incarnation}" && ph != "{key}" {
			out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_ref_unknown_placeholder",
				Message: fmt.Sprintf("revealable_secrets[%d].vault_ref uses unknown placeholder %s", idx, ph),
				Hint:    "only {service}, {incarnation} and {key} are supported",
			}))
		}
	}
	// enumerate задан (в MVP всегда) ⇒ reveal per-элементный ⇒ путь ОБЯЗАН нести {key}.
	if rs.Enumerate != "" && !strings.Contains(rs.VaultRef, "{key}") {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_missing_key",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must contain {key} when enumerate is set", idx),
			Hint:    "per-element reveal requires {key}, e.g. .../users/{key}#password",
		}))
	}
	// {service} И {incarnation} ОБЯЗАТЕЛЬНЫ (NIM-74 C1 defense-in-depth): путь
	// привязан к неймспейсу секретов ИМЕННО этого сервиса этой инкарнации
	// (secret/<service>/<incarnation>/…). Статический `secret/keeper/jwt-signing-key`
	// без плейсхолдеров отвергается на load; рантайм-allowlist prefix + floor — 2-й слой.
	if !strings.Contains(rs.VaultRef, "{service}") || !strings.Contains(rs.VaultRef, "{incarnation}") {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_not_service_scoped",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must contain {service} and {incarnation} (per-service/incarnation scoping)", idx),
			Hint:    "scope the path, e.g. secret/{service}/{incarnation}/users/{key}#password",
		}))
	}
	// #<field> ОБЯЗАТЕЛЕН: reveal раскрывает ровно одно скалярное поле секрета
	// (рантайм selectRevealField без поля → вечный 404). Ловим на load.
	if i := strings.LastIndexByte(rs.VaultRef, '#'); i < 0 || i == len(rs.VaultRef)-1 {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_missing_field",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must select a #<field> (single scalar value)", idx),
			Hint:    "append the KV field, e.g. .../users/{key}#password",
		}))
	}

	return out
}

// validateDependencyRef — проверка одной записи `{name, ref}` в destiny[]/modules[].
// `nameRegex` различает одно- и двухуровневую форму имени.
func validateDependencyRef(root *ast.MappingNode, listKey string, idx int, dep DependencyRef, nameRegex *regexp.Regexp) []diag.Diagnostic {
	var out []diag.Diagnostic
	base := fmt.Sprintf("$.%s[%d]", listKey, idx)

	if dep.Name == "" {
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("%s[%d].name is required", listKey, idx),
			Hint:    "dependency entry must declare {name, ref} — both non-empty",
		}))
	} else if listKey == "modules" && strings.HasPrefix(dep.Name, "core.") {
		// ADR-009 / ADR-015: core-модули доступны автоматически и в `modules:`
		// НЕ перечисляются. Отдельный код, чтобы оператор не путал с обычным
		// `name_invalid_format` (имя regex-валидно, но семантика запрещена).
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "core_module_in_modules_list",
			Message: fmt.Sprintf("%s[%d].name %q is a core module — core modules are always available and must not be listed", listKey, idx, dep.Name),
			Hint:    "Core-модули доступны автоматически — не перечисляются в `modules:` (ADR-009)",
		}))
	} else if !nameRegex.MatchString(dep.Name) {
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("%s[%d].name %q does not match %s", listKey, idx, dep.Name, nameRegex),
			Hint:    nameHint(listKey),
		}))
	}

	if dep.Ref == "" {
		out = append(out, atPath(root, base+".ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("%s[%d].ref is required (ADR-007: git tag or branch)", listKey, idx),
			Hint:    "examples: v2.0.0 (tag), main (branch); no semver-range",
		}))
	}

	// git — per-entry override полного URL, поддержан только для destiny[].
	// Для modules[] запрещаем явно, чтобы оператор не считал его поддержанным.
	if listKey == "modules" && dep.Git != "" {
		out = append(out, atPath(root, base+".git", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "unknown_key",
			Message: fmt.Sprintf("modules[%d].git is not supported — per-entry git override is destiny-only", idx),
			Hint:    "per-entry git override is not defined for modules (resolved by name only)",
		}))
	}

	return out
}

func nameHint(listKey string) string {
	if listKey == "modules" {
		return "two-level address <namespace>.<module> per architecture.md → «Адресация модулей»; core-modules are not listed here"
	}
	return "kebab-case: lowercase letters, digits, dashes; must start with letter"
}

// validateStateSchema — MVP-валидация JSON Schema на корне `state_schema:`.
//
// Проверяется минимум, гарантирующий корректность runtime-валидации
// `incarnation.state` Keeper-ом:
//   - корень должен быть mapping с `type: object` (объект — единственная
//     допустимая форма для top-level состояния);
//   - `required` (если есть) — массив строк;
//   - `properties` (если есть) — map<string, mapping>, рекурсивно проверяем
//     каждую вложенную схему по тем же правилам, но без обязательного
//     `type: object` (вложенные могут быть любого типа).
//
// Расширенная JSON Schema (`enum`/`pattern`/`min`/`max`/`items`/
// `additionalProperties` и т.п.) намеренно НЕ типизируется в MVP — это
// большой draft-07 стандарт. Малформированную схему ловим, но семантику
// каждого ключа не валидируем (PM-decision).
func validateStateSchema(root *ast.MappingNode, node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	if node == nil {
		// Ключ присутствует в YAML, но значение не mapping (null/scalar/sequence).
		// goccy НЕ поднимет decode-ошибку для null → map[string]any (просто nil
		// получится), поэтому диагностика нужна здесь. Для scalar/sequence
		// generic `type_mismatch` уже выпущен decode-фазой; но и здесь явная
		// диагностика читается лучше.
		return []diag.Diagnostic{atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "state_schema_root_not_object",
			Message: "state_schema must be a mapping with type: object on root",
			Hint:    "declare state_schema: { type: object, properties: {...} }",
		})}
	}
	var out []diag.Diagnostic

	// На root-уровне `type: object` обязателен (incarnation.state — всегда
	// объект). На вложенных уровнях type — любой допустимый JSON Schema тип.
	tn := findScalarValue(node, "type")
	if tn == nil {
		out = append(out, diagAt(node.GetToken().Position.Line, node.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "state_schema_root_not_object",
			Message:  "state_schema must declare type: object on root",
			Hint:     "incarnation.state is always an object; nested schemas may use other types",
			YAMLPath: pathPrefix + ".type",
		}))
	} else if t, ok := tn.(*ast.StringNode); !ok || t.Value != "object" {
		actual := "<non-string>"
		if t, ok := tn.(*ast.StringNode); ok {
			actual = t.Value
		}
		out = append(out, diagAt(tn.GetToken().Position.Line, tn.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "state_schema_root_not_object",
			Message:  fmt.Sprintf("state_schema.type must be \"object\" on root, got %q", actual),
			Hint:     "incarnation.state is always an object",
			YAMLPath: pathPrefix + ".type",
		}))
	}

	out = append(out, validateJSONSchemaNode(node, pathPrefix)...)
	return out
}

// validateJSONSchemaNode — рекурсивная структурная проверка одной JSON Schema-
// ноды. Корневой уровень `state_schema` отдельной обработки не требует:
// `type: object` уже проверен validateStateSchema-ом, а валидация
// `required`/`properties`/`items`/`additionalProperties` симметрична на всех
// уровнях.
func validateJSONSchemaNode(node *ast.MappingNode, path string) []diag.Diagnostic {
	if node == nil {
		return nil
	}
	var out []diag.Diagnostic

	// required: должен быть sequence строк (если ключ присутствует).
	reqKV := findKV(node, "required")
	if reqKV != nil {
		seq, ok := reqKV.Value.(*ast.SequenceNode)
		if !ok {
			tok := reqKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "required must be an array of strings",
				YAMLPath: path + ".required",
			}))
		} else {
			for i, item := range seq.Values {
				if _, isStr := item.(*ast.StringNode); !isStr {
					tok := item.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "state_schema_invalid",
						Message:  fmt.Sprintf("required[%d] must be a string", i),
						YAMLPath: fmt.Sprintf("%s.required[%d]", path, i),
					}))
				}
			}
		}
	}

	// properties: map<string, mapping>, рекурсия в каждую вложенную схему.
	propsKV := findKV(node, "properties")
	if propsKV != nil {
		propsNode, ok := propsKV.Value.(*ast.MappingNode)
		if !ok {
			tok := propsKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "properties must be a mapping of name → schema",
				YAMLPath: path + ".properties",
			}))
		} else {
			for _, kv := range propsNode.Values {
				keyTok := kv.Key.GetToken()
				if keyTok == nil {
					continue
				}
				subPath := path + ".properties." + keyTok.Value
				subMap, isMap := kv.Value.(*ast.MappingNode)
				if !isMap {
					tok := kv.Value.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "state_schema_invalid",
						Message:  fmt.Sprintf("property %q must be a schema (mapping)", keyTok.Value),
						YAMLPath: subPath,
					}))
					continue
				}
				out = append(out, validateJSONSchemaNode(subMap, subPath)...)
			}
		}
	}

	// items: рекурсия — встречается во вложенных схемах с type=array.
	// Допустим только mapping (вложенная схема); scalar / sequence — invalid.
	itemsKV := findKV(node, "items")
	if itemsKV != nil {
		if subMap, ok := itemsKV.Value.(*ast.MappingNode); ok {
			out = append(out, validateJSONSchemaNode(subMap, path+".items")...)
		} else {
			tok := itemsKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "items must be a schema (mapping)",
				YAMLPath: path + ".items",
			}))
		}
	}

	// additionalProperties: schema-ветка → рекурсия; bool-ветка валидна сама
	// по себе (true/false по JSON Schema draft-07); прочие значения — invalid.
	apKV := findKV(node, "additionalProperties")
	if apKV != nil {
		switch v := apKV.Value.(type) {
		case *ast.MappingNode:
			out = append(out, validateJSONSchemaNode(v, path+".additionalProperties")...)
		case *ast.BoolNode:
			// валидно, рекурсии не требует
		default:
			tok := apKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "additionalProperties must be a boolean or a schema (mapping)",
				YAMLPath: path + ".additionalProperties",
			}))
		}
	}

	return out
}

// findKV возвращает MappingValueNode по имени ключа или nil.
func findKV(m *ast.MappingNode, name string) *ast.MappingValueNode {
	if m == nil {
		return nil
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok != nil && tok.Value == name {
			return kv
		}
	}
	return nil
}

// findScalarValue — value-узел под ключом `name` на одном уровне (без рекурсии).
func findScalarValue(m *ast.MappingNode, name string) ast.Node {
	kv := findKV(m, name)
	if kv == nil {
		return nil
	}
	return kv.Value
}

// semanticValidateService — на M1.2.b отдельных semantic-инвариантов нет
// (cross-file refs и migration chain — out of scope, M1.5). Сохраняем
// сигнатуру для симметрии с destiny.go.
func semanticValidateService(_ *ServiceManifest, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}
