// Package plugin — типизированный парсер `manifest.yaml` плагина и валидатор
// под нормативную спеку [`docs/keeper/plugins.md`] и [ADR-020].
//
// Источник правды для wire-формата — proto-message
// `soulstack.plugin.v1.Manifest` (см. `proto/plugin/v1/manifest.proto`). На
// host-е и в линтере манифест читается напрямую с диска как YAML, поэтому
// нам важнее точные line/column-ошибки от goccy/go-yaml, чем wire-совместимость
// через protojson (manifest никуда не сериализуется, только читается).
//
// Пакет используется и в `soul/internal/pluginhost`, и в `soul-lint`, чтобы
// один и тот же парсер валидировал плагин и при discovery в Soul-демоне, и
// при офлайн-проверке `soul-lint validate-manifest`.
package plugin

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Manifest — типизированное представление `manifest.yaml`.
//
// Поля повторяют proto-message `soulstack.plugin.v1.Manifest` с YAML-тегами.
// `Spec` — kind-specific блок: для `soul_module` заполняется `States`, для
// `cloud_driver`/`ssh_provider` — `ProfileSchema`/`ParamsSchema`/`ProviderKind`.
//
// Парсинг полной input-схемы внутри `Spec.States[*].Input` ограничен формальными
// проверками: тип значения (`string`/`int`/`bool`/`list`/`map`), `required`-флаг,
// `secret`-флаг + `pattern` (для secret — `^vault:.*`). Полную input-DSL
// (`docs/input.md`) валидирует destiny/scenario-парсер при проверке `params:`
// шага против manifest-схемы — это другая фаза.
type Manifest struct {
	Kind                 string          `yaml:"kind"`
	ProtocolVersion      int32           `yaml:"protocol_version"`
	Namespace            string          `yaml:"namespace"`
	Name                 string          `yaml:"name"`
	RequiredCapabilities []string        `yaml:"required_capabilities,omitempty"`
	SideEffects          []SideEffectRaw `yaml:"side_effects,omitempty"`
	Spec                 ManifestSpec    `yaml:"spec"`
}

// ManifestSpec — kind-specific блок. Структурно объединяет поля всех четырёх
// kind-ов: validate() проверяет, какие из них релевантны для текущего `kind`.
type ManifestSpec struct {
	// soul_module:
	States map[string]StateDef `yaml:"states,omitempty"`

	// cloud_driver / ssh_provider:
	ProviderKind string `yaml:"provider_kind,omitempty"`
	// ProfileSchema / ParamsSchema — JSON Schema (любой произвольный YAML-объект).
	// Семантическая проверка JSON Schema выходит за рамки manifest-валидатора
	// (это работа JSON Schema-валидатора post-MVP).
	//
	// ParamsSchema — общее поле для ssh_provider и soul_beacon (V5-2): оба
	// несут схему params в один и тот же YAML-ключ `spec.params_schema`,
	// различаются только семантикой (params SSH-провайдера vs Vigil).
	ProfileSchema map[string]any `yaml:"profile_schema,omitempty"`
	ParamsSchema  map[string]any `yaml:"params_schema,omitempty"`
}

// StateDef — описание одного state-а в `spec.states`.
type StateDef struct {
	Description string                   `yaml:"description,omitempty"`
	Input       map[string]InputParamDef `yaml:"input,omitempty"`
}

// InputParamDef — формальное описание одного параметра в manifest-input.
//
// Это **не** полная DSL `docs/input.md` — manifest-валидатор проверяет только
// то, что выражается на уровне `manifest.yaml`: тип (∈ {string,int,bool,list,
// map}), required/secret/default/pattern/description плюс form-DSL поля ADR-045,
// из которых backend строит форму модуля: enum (closed-set допустимых значений),
// pattern (regex-ограничение), format+source (cluster-aware picker, напр. sid),
// items (тип элемента list / тип значения map), multiline+example (UI-подсказки
// textarea). Остальное из полной схемы (object с properties, числовые границы)
// допустимо — лишние ключи парсер сохраняет в `Extra`, не валидирует.
type InputParamDef struct {
	Type        string `yaml:"type,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
	Secret      bool   `yaml:"secret,omitempty"`
	Pattern     string `yaml:"pattern,omitempty"`
	Description string `yaml:"description,omitempty"`
	Default     any    `yaml:"default,omitempty"`

	// Поля UI-формы (ADR-045 S1). Дублируют семантику `config.InputSchema`
	// (Enum/Format/Source), но описывают параметр на уровне manifest.yaml —
	// backend строит из них форму модуля. Семантику валидируем здесь
	// structurally (см. validateInputParam); полный input-DSL валидирует
	// destiny/scenario-парсер.
	Enum   []any        `yaml:"enum,omitempty"`
	Format string       `yaml:"format,omitempty"`
	Source *InputSource `yaml:"source,omitempty"`

	// Multiline и Example (ADR-045 B3) — чисто декларативные UI-подсказки
	// (большое textarea + placeholder), валидатором не проверяются.
	Multiline bool   `yaml:"multiline,omitempty"`
	Example   string `yaml:"example,omitempty"`

	// Items — тип элемента списка или тип значения map (ADR-045 S7 + amend).
	// Рекурсивный *InputParamDef, зеркало `config.InputSchema.Items`:
	// `type: list, items: {type: int}` даёт list[int], из которого backend строит
	// типизированный список в форме (а не свободный список строк). Для list/array —
	// тип ЭЛЕМЕНТА; для map/object — тип ЗНАЧЕНИЯ (`map[string]<items>`).
	// Валидируется structurally в validateInputParam (известный тип элемента).
	Items *InputParamDef `yaml:"items,omitempty"`
}

// InputSource — объект-дискриминатор каталога-источника значений поля формы
// (ADR-044 S-T1, ADR-045 S1). Ровно один под-ключ задаёт множество:
//   - IncarnationHosts (`incarnation_hosts: true`) — все SID текущей инкарнации;
//   - Choir (`choir: <name>`) — SID-ы конкретной Choir-партии инкарнации.
//
// Дублирует `config.InputSource` намеренно: `shared/config` уже импортирует
// `shared/plugin` (config/module_params.go), поэтому обратный импорт создал бы
// цикл. Прецедент дублирования ради разрыва цикла — `SupportedProtocolVersions`.
// Оба определения структурно идентичны и должны меняться синхронно.
type InputSource struct {
	IncarnationHosts bool   `yaml:"incarnation_hosts,omitempty" json:"incarnation_hosts,omitempty"`
	Choir            string `yaml:"choir,omitempty" json:"choir,omitempty"`
}

// SideEffectRaw — одна запись `side_effects[]` (ровно одна пара
// `<resource-type>: <value>`, см. docs/keeper/plugins.md → side_effects).
type SideEffectRaw map[string]any

// Kind-константы manifest-а. Дублируют значения proto-enum-а `pluginv1.Kind`,
// чтобы YAML-форма (lowercase snake_case) не зависела от proto-сериализации.
const (
	KindSoulModule  = "soul_module"
	KindCloudDriver = "cloud_driver"
	KindSSHProvider = "ssh_provider"
	KindSoulBeacon  = "soul_beacon"

	// FileName — имя файла манифеста рядом с бинарём плагина (ADR-020(a)).
	FileName = "manifest.yaml"
)

// SupportedProtocolVersions — версии plugin-протокола, которые понимают
// host и линтер (ADR-020(c) → naming-rules.md). MVP — только v1. Forward-compat
// only-add: при появлении v2 сюда добавится `2` с сохранением `1`.
//
// Дублирует константу `pluginhost.SupportedProtocolVersions` (тот же
// инвариант) — оба массива должны меняться синхронно. Дублирование осознанное:
// soul-host не импортирует soul-lint, soul-lint не импортирует pluginhost;
// shared/plugin — общий источник правды для статической проверки manifest-а.
var SupportedProtocolVersions = []int32{1}

// Closed enum по docs/keeper/plugins.md → `required_capabilities`-таблица.
var validCapabilities = map[string]pluginv1.Capability{
	"run_as_root":      pluginv1.Capability_CAPABILITY_RUN_AS_ROOT,
	"network_outbound": pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND,
	"network_inbound":  pluginv1.Capability_CAPABILITY_NETWORK_INBOUND,
	"vault_access":     pluginv1.Capability_CAPABILITY_VAULT_ACCESS,
	"fs_write_root":    pluginv1.Capability_CAPABILITY_FS_WRITE_ROOT,
	"exec_subprocess":  pluginv1.Capability_CAPABILITY_EXEC_SUBPROCESS,
}

// Closed enum по docs/keeper/plugins.md → `side_effects`-таблица.
// Значение в map не используется — это просто множество.
var validSideEffectTypes = map[string]struct{}{
	"service":   {},
	"file":      {},
	"package":   {},
	"port":      {},
	"user":      {},
	"group":     {},
	"directory": {},
	"cron":      {},
	"mount":     {},
}

// Closed enum по delegation ТЗ (docs/keeper/plugins.md → manifest.spec.states.<state>.input).
var validInputTypes = map[string]struct{}{
	"string": {}, "int": {}, "bool": {}, "list": {}, "map": {},
	// `docs/input.md` использует расширенный набор (`integer`/`number`/`boolean`/
	// `array`/`object`); принимаем их как синонимы, чтобы существующие manifest-ы
	// не ломались. Drift между двумя DSL зафиксирован, нормирование — отдельная
	// задача (см. observations).
	"integer": {}, "number": {}, "boolean": {}, "array": {}, "object": {},
}

var (
	reNamespace = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reName      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reStateName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// validInputFormats — closed-set string-форматов поля формы (ADR-045 S1).
// Зеркало `config.inputFormatEnum` (docs/input.md), включая `sid` (FQDN-форма
// SID, ADR-044 S-T1). Дублирование вынужденное — см. InputSource.
var validInputFormats = map[string]struct{}{
	"hostname": {}, "fqdn": {}, "ipv4": {}, "ipv6": {}, "cidr": {},
	"email": {}, "uri": {}, "uuid": {}, "semver": {}, "duration": {},
	"sid": {},
}

// Load читает и валидирует `manifest.yaml` по пути `path` через diag-pipeline.
//
// Контракт возврата симметричен `shared/config.Load*`:
//   - `error != nil` — только I/O fatal (open/read). Manifest = nil.
//   - parse-fatal → `error == nil`, Manifest = nil, в diagnostics одна запись
//     с `Phase=PhaseParse`.
//   - schema/semantic errors → Manifest частично заполнен, diagnostics
//     содержат все найденные validation-errors.
func Load(path string) (*Manifest, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	m, diags := LoadFromBytes(path, src)
	return m, diags, nil
}

// LoadFromBytes — основная точка входа без I/O. Полезна в тестах с in-memory
// фикстурами и в soul-lint (читает байты сам). `filename` нужен только как
// метка `Diagnostic.File`.
func LoadFromBytes(filename string, src []byte) (*Manifest, []diag.Diagnostic) {
	src = stripBOM(src)
	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(filename, err)}
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    filename,
			Code:    "empty_document",
			Message: "manifest is empty or contains no mapping",
		}}
	}
	if len(file.Docs) > 1 {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    filename,
			Code:    "multi_document_not_allowed",
			Message: fmt.Sprintf("manifest must contain exactly one YAML document; got %d", len(file.Docs)),
			Hint:    "remove '---' separators",
		}}
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		t := file.Docs[0].Body.GetToken()
		line, col := 0, 0
		if t != nil {
			line, col = t.Position.Line, t.Position.Column
		}
		return nil, []diag.Diagnostic{{
			Level:    diag.LevelError,
			Phase:    diag.PhaseSchemaValidate,
			File:     filename,
			Line:     line,
			Column:   col,
			Code:     "type_mismatch",
			Message:  "root of manifest must be a mapping",
			YAMLPath: "$",
		}}
	}

	m := &Manifest{}
	var diags []diag.Diagnostic
	if err := yaml.NodeToValue(root, m, yaml.Strict()); err != nil {
		diags = append(diags, decodeErrorDiag(filename, err))
		// При strict-ошибке частично заполненный Manifest остаётся, но дальше
		// валидируем по-возможности (поля, успевшие декодироваться).
	}
	diags = append(diags, validateManifest(filename, root, m)...)
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = filename
		}
	}
	return m, diags
}

// Address — `<namespace>.<name>`; используется в логах и OTel-тегах.
func (m *Manifest) Address() string {
	return m.Namespace + "." + m.Name
}

// BinaryName — конвенция именования бинаря по kind (docs/keeper/plugins.md →
// таблица kind-host-binary):
//
//   - kind=soul_module   → `soul-mod-<name>`
//   - kind=cloud_driver  → `soul-cloud-<name>`
//   - kind=ssh_provider  → `soul-ssh-<name>`
//   - kind=soul_beacon   → `soul-beacon-<name>` (ADR-030 V5-2)
//
// Используется host-discovery при поиске бинаря рядом с manifest.yaml.
// Возвращает "" для неизвестного kind-а (defensive — manifest уже проходит
// closed-enum-валидацию в validateManifest).
func (m *Manifest) BinaryName() string {
	switch m.Kind {
	case KindSoulModule:
		return "soul-mod-" + m.Name
	case KindCloudDriver:
		return "soul-cloud-" + m.Name
	case KindSSHProvider:
		return "soul-ssh-" + m.Name
	case KindSoulBeacon:
		return "soul-beacon-" + m.Name
	default:
		return ""
	}
}

// ProtoKind переводит Manifest.Kind в pluginv1.Kind для cross-check с
// handshake (handshake кладёт enum как строку через protojson).
func (m *Manifest) ProtoKind() pluginv1.Kind {
	switch m.Kind {
	case KindSoulModule:
		return pluginv1.Kind_KIND_SOUL_MODULE
	case KindCloudDriver:
		return pluginv1.Kind_KIND_CLOUD_DRIVER
	case KindSSHProvider:
		return pluginv1.Kind_KIND_SSH_PROVIDER
	case KindSoulBeacon:
		return pluginv1.Kind_KIND_SOUL_BEACON
	default:
		return pluginv1.Kind_KIND_UNSPECIFIED
	}
}

// CapabilityFromString — словарь YAML-форм capabilities (lowercase snake_case)
// → `pluginv1.Capability`. Возвращает (cap, true) при совпадении, (_, false)
// для неизвестного значения. Используется host-ом для сравнения с
// `allowed_capabilities`.
func CapabilityFromString(s string) (pluginv1.Capability, bool) {
	c, ok := validCapabilities[s]
	return c, ok
}

// validateManifest — основной валидатор. Проходит по AST-узлу root и по уже
// декодированной структуре m, выдаёт diag-список.
func validateManifest(path string, root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic

	// (1) kind: required + closed enum.
	switch m.Kind {
	case "":
		out = append(out, atPath(root, "$.kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "kind is required at top-level",
			Hint:    "set kind: soul_module | cloud_driver | ssh_provider | soul_beacon",
		}))
	case KindSoulModule, KindCloudDriver, KindSSHProvider, KindSoulBeacon:
		// ok
	default:
		out = append(out, atPath(root, "$.kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "kind_invalid",
			Message: fmt.Sprintf("kind=%q is not in {soul_module,cloud_driver,ssh_provider,soul_beacon}", m.Kind),
		}))
	}

	// (2) protocol_version: > 0 + ∈ SupportedProtocolVersions.
	if m.ProtocolVersion <= 0 {
		out = append(out, atPath(root, "$.protocol_version", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "protocol_version_invalid",
			Message: fmt.Sprintf("protocol_version=%d must be a positive int32", m.ProtocolVersion),
		}))
	} else if !containsInt32(SupportedProtocolVersions, m.ProtocolVersion) {
		out = append(out, atPath(root, "$.protocol_version", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "protocol_version_unsupported",
			Message: fmt.Sprintf("protocol_version=%d not in supported %v", m.ProtocolVersion, SupportedProtocolVersions),
			Hint:    "upgrade soul-stack toolchain or set protocol_version to a supported value",
		}))
	}

	// (3) namespace / name: kebab-case lowercase, ≤63 chars.
	if m.Namespace == "" {
		out = append(out, atPath(root, "$.namespace", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "missing_required_field", Message: "namespace is required at top-level",
		}))
	} else if !reNamespace.MatchString(m.Namespace) {
		out = append(out, atPath(root, "$.namespace", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "namespace_invalid_format",
			Message: fmt.Sprintf("namespace=%q does not match %s", m.Namespace, reNamespace),
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter; ≤63 chars",
		}))
	}
	if m.Name == "" {
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "missing_required_field", Message: "name is required at top-level",
		}))
	} else if !reName.MatchString(m.Name) {
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("name=%q does not match %s", m.Name, reName),
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter; ≤63 chars",
		}))
	}

	// (4) required_capabilities[] — closed enum.
	for i, c := range m.RequiredCapabilities {
		if _, ok := validCapabilities[c]; !ok {
			out = append(out, atPath(root, fmt.Sprintf("$.required_capabilities[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "capability_unknown",
				Message: fmt.Sprintf("required_capabilities[%d]=%q is not a known capability", i, c),
				Hint:    "see docs/keeper/plugins.md → required_capabilities table",
			}))
		}
	}

	// (5) side_effects[] — closed enum keys, ровно одна пара на запись.
	for i, e := range m.SideEffects {
		switch len(e) {
		case 0:
			out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "side_effect_empty_entry",
				Message: fmt.Sprintf("side_effects[%d] must have exactly one resource-type entry, got 0", i),
			}))
		case 1:
			for k := range e {
				if _, ok := validSideEffectTypes[k]; !ok {
					out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:    "side_effect_type_unknown",
						Message: fmt.Sprintf("side_effects[%d] resource-type %q is not a known side-effect", i, k),
						Hint:    "see docs/keeper/plugins.md → side_effects table",
					}))
				}
			}
		default:
			out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "multiple_resource_types_in_side_effect_entry",
				Message: fmt.Sprintf("side_effects[%d] must have exactly one resource-type entry, got %d", i, len(e)),
				Hint:    "split multi-resource entry into separate list items",
			}))
		}
	}

	// (6) kind-specific spec.
	switch m.Kind {
	case KindSoulModule:
		out = append(out, validateSoulModuleSpec(root, m)...)
	case KindCloudDriver:
		out = append(out, validateCloudDriverSpec(root, m)...)
	case KindSSHProvider:
		out = append(out, validateSSHProviderSpec(root, m)...)
	case KindSoulBeacon:
		out = append(out, validateSoulBeaconSpec(root, m)...)
	}

	return out
}

func validateSoulModuleSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(m.Spec.States) == 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_empty",
			Message: "spec.states is required and must be non-empty for kind=soul_module",
			Hint:    "declare at least one state (e.g. installed/running/promoted)",
		}))
		return out
	}
	for state, def := range m.Spec.States {
		statePath := "$.spec.states." + state
		if !reStateName.MatchString(state) {
			out = append(out, atPath(root, statePath, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "state_name_invalid",
				Message: fmt.Sprintf("spec.states key %q does not match %s", state, reStateName),
				Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
			}))
		}
		if def.Description == "" {
			out = append(out, atPath(root, statePath+".description", diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:    "state_description_missing",
				Message: fmt.Sprintf("spec.states.%s.description is empty", state),
				Hint:    "human-readable description helps operators and UI",
			}))
		}
		for paramName, p := range def.Input {
			paramPath := statePath + ".input." + paramName
			out = append(out, validateInputParam(root, paramPath, paramName, p)...)
		}
	}
	return out
}

func validateInputParam(root *ast.MappingNode, path, name string, p InputParamDef) []diag.Diagnostic {
	var out []diag.Diagnostic
	if p.Type == "" {
		out = append(out, atPath(root, path+".type", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "input_type_missing",
			Message: fmt.Sprintf("input parameter %q has no type", name),
			Hint:    "set type: string | int | bool | list | map",
		}))
	} else if _, ok := validInputTypes[p.Type]; !ok {
		out = append(out, atPath(root, path+".type", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "input_type_unknown",
			Message: fmt.Sprintf("input parameter %q type=%q is not in {string,int,bool,list,map}", name, p.Type),
		}))
	}
	if p.Secret {
		// secret-флаг означает, что значение приходит через `vault:`-ref;
		// pattern должен это форсировать. Если оператор не задал явный
		// pattern — поднимаем error: secret без vault-ref легко проносится
		// мимо аудита.
		if p.Pattern == "" {
			out = append(out, atPath(root, path, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_secret_without_vault_pattern",
				Message: fmt.Sprintf("input parameter %q is secret but has no pattern", name),
				Hint:    `set pattern: "^vault:.*" for secrets`,
			}))
		} else if p.Pattern != "^vault:.*" {
			// Жёсткая проверка: единственный допустимый pattern для secret.
			// Если кто-то хочет расширить — это отдельный ADR на форму secret-ref.
			out = append(out, atPath(root, path+".pattern", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_secret_pattern_invalid",
				Message: fmt.Sprintf("input parameter %q secret pattern=%q must be ^vault:.*", name, p.Pattern),
			}))
		}
	}

	// enum — если задан, непустой и каждый элемент совместим с Type.
	// (config.validateCommonInvariants проверяет то же; здесь — structurally,
	//  без AST-позиций под-элементов: enum-литерал не имеет per-item YAML-path.)
	if p.Enum != nil {
		if len(p.Enum) == 0 {
			out = append(out, atPath(root, path+".enum", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "input_enum_empty",
				Message: fmt.Sprintf("input parameter %q has empty enum", name),
				Hint:    "drop enum or list at least one allowed value",
			}))
		}
		for i, v := range p.Enum {
			if !enumValueMatchesType(v, p.Type) {
				out = append(out, atPath(root, path+".enum", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_enum_type_mismatch",
					Message: fmt.Sprintf("input parameter %q enum[%d] does not match type %q", name, i, p.Type),
				}))
			}
		}
	}

	// format — closed-set (string-форматы вкл. sid).
	if p.Format != "" {
		if _, ok := validInputFormats[p.Format]; !ok {
			out = append(out, atPath(root, path+".format", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "input_format_invalid",
				Message: fmt.Sprintf("input parameter %q format=%q is not a known format", name, p.Format),
				Hint:    "see docs/input.md → format enum (hostname/fqdn/ipv4/.../sid)",
			}))
		}
	}

	// items — описатель типа для коллекций (ADR-045 S7 + amend). Для list/array —
	// тип ЭЛЕМЕНТА; для map/object — тип ЗНАЧЕНИЯ (`map[string]<items>`). В обоих
	// случаях задан — тип ∈ validInputTypes. Зеркало config: items осмыслен лишь
	// для коллекций. Глубже одного уровня не валидируем — manifest-input не несёт
	// вложенные коллекции.
	if p.Items != nil {
		switch p.Type {
		case "list", "array", "map", "object":
			if p.Items.Type == "" {
				out = append(out, atPath(root, path+".items.type", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_items_type_missing",
					Message: fmt.Sprintf("input parameter %q items has no type", name),
					Hint:    "set items.type: string | int | bool | ...",
				}))
			} else if _, ok := validInputTypes[p.Items.Type]; !ok {
				out = append(out, atPath(root, path+".items.type", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_items_type_unknown",
					Message: fmt.Sprintf("input parameter %q items.type=%q is not a known type", name, p.Items.Type),
				}))
			}
		default:
			out = append(out, atPath(root, path+".items", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_items_invalid_for_type",
				Message: fmt.Sprintf("input parameter %q has items but type=%q is not a collection (list/array/map/object)", name, p.Type),
				Hint:    "items applies only to collection types: list/array (element type) or map/object (value type)",
			}))
		}
	}

	// source — структурная валидность дискриминатора (ровно один активный
	// источник: incarnation_hosts XOR choir). Зеркало config.validateSource —
	// только инвариант «active==1»; резолв множества делает backend.
	if p.Source != nil {
		active := 0
		if p.Source.IncarnationHosts {
			active++
		}
		if p.Source.Choir != "" {
			active++
		}
		if active != 1 {
			out = append(out, atPath(root, path+".source", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_source_invalid",
				Message: fmt.Sprintf("input parameter %q source must declare exactly one active catalog, got %d", name, active),
				Hint:    "set exactly one: incarnation_hosts: true OR choir: <name>",
			}))
		}
	}
	return out
}

// enumValueMatchesType — совместимость literal-значения enum с manifest-input
// type. Принимает синонимы (`int`/`integer`, `bool`/`boolean`, …) из
// validInputTypes. list/map/array/object — прозрачны (composite-enum не
// валидируем по элементам, симметрично config). Неизвестный type — прозрачен
// (его уже отметил input_type_unknown).
func enumValueMatchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool", "boolean":
		_, ok := v.(bool)
		return ok
	case "int", "integer":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			return x == float64(int64(x))
		}
		return false
	case "number":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return true
		}
		return false
	default:
		// list/map/array/object и неизвестные — прозрачны.
		return true
	}
}

func validateCloudDriverSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if m.Spec.ProfileSchema == nil {
		out = append(out, atPath(root, "$.spec.profile_schema", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_profile_schema_missing",
			Message: "spec.profile_schema is required for kind=cloud_driver",
			Hint:    "embed JSON Schema (draft 2020-12) describing VM profile parameters",
		}))
	}
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	return out
}

func validateSSHProviderSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if m.Spec.ProviderKind == "" {
		out = append(out, atPath(root, "$.spec.provider_kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_provider_kind_missing",
			Message: "spec.provider_kind is required for kind=ssh_provider",
			Hint:    "set provider_kind to a convention value (vault_ssh_ca / static_key / teleport) or your own",
		}))
	}
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	return out
}

// validateSoulBeaconSpec — kind-specific валидатор для `kind: soul_beacon`
// (ADR-030 V5-2). spec.params_schema опционален (beacon без params, например
// systemd-monotonic-health-check, валиден); spec.states/provider_kind/
// profile_schema не разрешены — soul_beacon имеет один тип операции (Check)
// и не маппится в state-семантику SoulModule.
func validateSoulBeaconSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	if m.Spec.ProviderKind != "" {
		out = append(out, atPath(root, "$.spec.provider_kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_provider_kind_not_allowed",
			Message: "spec.provider_kind is only valid for kind=ssh_provider",
		}))
	}
	if m.Spec.ProfileSchema != nil {
		out = append(out, atPath(root, "$.spec.profile_schema", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_profile_schema_not_allowed",
			Message: "spec.profile_schema is only valid for kind=cloud_driver",
		}))
	}
	return out
}

// ValidateSimple — convenience-wrapper для legacy-кода (soul/internal/pluginhost),
// который ожидает `error` вместо `[]diag.Diagnostic`. Возвращает первую
// error-уровневую запись или nil.
func (m *Manifest) ValidateSimple() error {
	diags := validateManifest("", nil, m)
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}

func atPath(root *ast.MappingNode, path string, d diag.Diagnostic) diag.Diagnostic {
	d.YAMLPath = path
	if root == nil {
		return d
	}
	if line, col := lookupPathPosition(root, path); line > 0 {
		d.Line, d.Column = line, col
	}
	return d
}

// lookupPathPosition — упрощённый walker по `$.a.b[N]`-форме path. Достаёт
// line/col из AST. Не покрывает все случаи (escaping, .input.<key>), для
// неподдерживаемой формы возвращает 0/0 — без позиции, но с YAMLPath.
func lookupPathPosition(root *ast.MappingNode, path string) (int, int) {
	if !looksLikeSimplePath(path) {
		return 0, 0
	}
	// Откусываем `$.` префикс.
	rest := path
	if len(rest) >= 2 && rest[0] == '$' && rest[1] == '.' {
		rest = rest[2:]
	}
	var node ast.Node = root
	for rest != "" {
		// Берём следующий сегмент до `.` или `[`.
		segEnd := len(rest)
		for i, ch := range rest {
			if ch == '.' || ch == '[' {
				segEnd = i
				break
			}
		}
		seg := rest[:segEnd]
		rest = rest[segEnd:]
		if rest != "" && rest[0] == '[' {
			// Индекс не разрешаем — позицию вернём для key seg.
			rest = ""
		}
		if rest != "" && rest[0] == '.' {
			rest = rest[1:]
		}
		m, ok := node.(*ast.MappingNode)
		if !ok {
			return 0, 0
		}
		var matched ast.Node
		for _, kv := range m.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			if tok.Value == seg {
				if rest == "" {
					return tok.Position.Line, tok.Position.Column
				}
				matched = kv.Value
				break
			}
		}
		if matched == nil {
			return 0, 0
		}
		node = matched
	}
	return 0, 0
}

func looksLikeSimplePath(path string) bool {
	return len(path) > 2 && path[0] == '$' && path[1] == '.'
}

func yamlParseDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseParse,
		File:    path,
		Code:    "yaml_parse_error",
		Message: err.Error(),
	}
	var sErr *yaml.SyntaxError
	if errors.As(err, &sErr) && sErr.Token != nil {
		d.Line = sErr.Token.Position.Line
		d.Column = sErr.Token.Position.Column
		d.Message = sErr.Message
	}
	return d
}

func decodeErrorDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
		File:    path,
		Code:    "type_mismatch",
		Message: err.Error(),
	}
	var yErr yaml.Error
	if errors.As(err, &yErr) {
		if tok := yErr.GetToken(); tok != nil {
			d.Line = tok.Position.Line
			d.Column = tok.Position.Column
		}
		if msg := yErr.GetMessage(); msg != "" {
			d.Message = msg
		}
		// goccy strict-mode сообщает «unknown field "foo"»; маппим в наш код.
		if isUnknownFieldError(err) {
			d.Code = "unknown_key"
		}
	}
	return d
}

func isUnknownFieldError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsCI(msg, "unknown field") || containsCI(msg, "unknown key")
}

func containsCI(s, sub string) bool {
	// Простейший case-insensitive contains: lowercase оба и ищем подстроку.
	// Полноценный strings.EqualFold-по-окнам не нужен — голый ASCII.
	return indexCI(s, sub) >= 0
}

func indexCI(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
loop:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			c1, c2 := s[i+j], sub[j]
			if 'A' <= c1 && c1 <= 'Z' {
				c1 += 'a' - 'A'
			}
			if 'A' <= c2 && c2 <= 'Z' {
				c2 += 'a' - 'A'
			}
			if c1 != c2 {
				continue loop
			}
		}
		return i
	}
	return -1
}

func stripBOM(data []byte) []byte {
	return StripBOM(data)
}

// StripBOM срезает ведущий UTF-8 BOM (EF BB BF), если он есть. Экспортирован
// для переиспользования в канонизации manifest-байтов Sigil
// (shared/pluginhost.NormalizeManifestBytes, ADR-026): один источник правды
// для среза BOM, чтобы хеш manifest-а на Keeper и Soul совпадал.
func StripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

func containsInt32(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
