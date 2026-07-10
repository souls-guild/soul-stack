package config

import (
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadServiceManifest_Golden(t *testing.T) {
	path := filepath.FromSlash("../../examples/service/redis/service.yml")
	cfg, doc, diags, err := LoadServiceManifest(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if cfg == nil || doc == nil {
		t.Fatalf("cfg/doc must be non-nil")
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("[%s] %s:%d:%d %s %s", d.Code, d.File, d.Line, d.Column, d.Message, d.YAMLPath)
		}
		t.Fatalf("expected 0 errors on golden service example, got %d diagnostics", len(diags))
	}
	if cfg.Name != "redis" {
		t.Errorf("name: got %q want redis", cfg.Name)
	}
	if cfg.StateSchemaVersion != 14 {
		t.Errorf("state_schema_version: got %d want 14", cfg.StateSchemaVersion)
	}
	if len(cfg.Destiny) != 4 {
		t.Errorf("destiny len: got %d want 4", len(cfg.Destiny))
	}
	if cfg.Destiny[0].Name != "redis" || cfg.Destiny[0].Ref != "v1.0.0" {
		t.Errorf("destiny[0]: %#v", cfg.Destiny[0])
	}
	if cfg.Destiny[1].Name != "node-exporter" || cfg.Destiny[1].Ref != "v1.0.0" {
		t.Errorf("destiny[1]: %#v", cfg.Destiny[1])
	}
	if cfg.Destiny[2].Name != "redis-exporter" || cfg.Destiny[2].Ref != "v1.0.0" {
		t.Errorf("destiny[2]: %#v", cfg.Destiny[2])
	}
	if cfg.Destiny[3].Name != "vector" || cfg.Destiny[3].Ref != "v1.0.0" {
		t.Errorf("destiny[3]: %#v", cfg.Destiny[3])
	}
	if len(cfg.Modules) != 1 || cfg.Modules[0].Name != "community.redis" || cfg.Modules[0].Ref != "v1.0.0" {
		t.Errorf("modules: %#v", cfg.Modules)
	}
}

func TestLoadServiceManifest_MissingName(t *testing.T) {
	src := `description: no name here
state_schema_version: 1
state_schema:
  type: object
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent name")
	}
}

func TestLoadServiceManifest_BadName(t *testing.T) {
	src := `name: RedisCluster
state_schema_version: 1
state_schema:
  type: object
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format")
	}
}

func TestLoadServiceManifest_EmptyName(t *testing.T) {
	src := `name: ""
state_schema_version: 1
state_schema:
  type: object
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for empty name")
	}
	if hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("must not emit missing_required_field when key present with empty string")
	}
}

// TestLoadServiceManifest_Lifecycle — блок lifecycle с обоими флагами
// принимается (НЕ unknown_key), флаги декодятся в *bool.
func TestLoadServiceManifest_Lifecycle(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
lifecycle:
  auto_create: false
  auto_destroy: true
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("lifecycle block дал ошибки, ожидалось 0")
	}
	if cfg.Lifecycle == nil {
		t.Fatal("Lifecycle nil, ожидался разобранный блок")
	}
	if cfg.Lifecycle.AutoCreateEnabled() {
		t.Error("auto_create=false должно дать AutoCreateEnabled()=false")
	}
	if !cfg.Lifecycle.AutoDestroyEnabled() {
		t.Error("auto_destroy=true должно дать AutoDestroyEnabled()=true")
	}
}

// TestLoadServiceManifest_LifecycleAbsent — без блока lifecycle оба флага
// дефолтно true (backcompat), nil-safe аксессоры работают.
func TestLoadServiceManifest_LifecycleAbsent(t *testing.T) {
	src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n"
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("неожиданные ошибки")
	}
	if cfg.Lifecycle != nil {
		t.Error("Lifecycle должен быть nil без блока")
	}
	// nil-safe: оба true.
	if !cfg.Lifecycle.AutoCreateEnabled() || !cfg.Lifecycle.AutoDestroyEnabled() {
		t.Error("nil-блок должен трактоваться как оба true (backcompat)")
	}
}

// TestLoadServiceManifest_LifecycleUnknownKey — опечатка под lifecycle:
// (напр. auto_creat) ловится reflect-walker-ом как unknown_key.
func TestLoadServiceManifest_LifecycleUnknownKey(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
lifecycle:
  auto_creat: false
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.lifecycle.auto_creat") {
		dump(t, diags)
		t.Fatal("ожидался unknown_key для опечатки auto_creat под lifecycle")
	}
}

func TestLoadServiceManifest_DeprecatedKeys(t *testing.T) {
	cases := []string{"version", "tasks", "steps", "input", "scenarios"}
	for _, key := range cases {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n" + key + ": foo\n"
			_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "unknown_key" && d.Hint != "" && d.YAMLPath == "$."+key {
					found = true
					break
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected unknown_key with hint for deprecated key %q", key)
			}
		})
	}
}

func TestLoadServiceManifest_DeprecatedKeyNoDuplicate(t *testing.T) {
	// Аналог destiny: deprecated top-level ключ должен дать ровно одну
	// диагностику (от schemaValidateService с hint), а не дубль из reflect-walker-а.
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
tasks: foo
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	count := 0
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks" {
			count++
		}
	}
	if count != 1 {
		dump(t, diags)
		t.Fatalf("expected exactly 1 unknown_key diag for deprecated `tasks`, got %d", count)
	}
}

func TestLoadServiceManifest_MissingStateSchemaVersion(t *testing.T) {
	src := `name: svc-golden
state_schema:
  type: object
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent state_schema_version")
	}
}

func TestLoadServiceManifest_BadStateSchemaVersion(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 0
state_schema:
  type: object
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for state_schema_version: 0")
	}
}

func TestLoadServiceManifest_MissingStateSchema(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent state_schema")
	}
}

func TestLoadServiceManifest_StateSchemaNotObject(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: string
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_root_not_object") {
		dump(t, diags)
		t.Fatalf("expected state_schema_root_not_object")
	}
}

func TestLoadServiceManifest_StateSchemaNullValue(t *testing.T) {
	// state_schema: (null) — присутствует ключ, но не mapping.
	src := `name: svc-golden
state_schema_version: 1
state_schema:
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_root_not_object") {
		dump(t, diags)
		t.Fatalf("expected state_schema_root_not_object for null state_schema value")
	}
}

func TestLoadServiceManifest_StateSchemaNoType(t *testing.T) {
	// type на корне отсутствует — `state_schema_root_not_object` (как root not object).
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  properties:
    foo: { type: string }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_root_not_object") {
		dump(t, diags)
		t.Fatalf("expected state_schema_root_not_object when type is absent on root")
	}
}

func TestLoadServiceManifest_StateSchemaRequiredNotArray(t *testing.T) {
	// required должен быть массивом строк; nested-схема (под `users`) → recursive.
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
  properties:
    users:
      type: object
      required: "not-an-array"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_invalid") {
		dump(t, diags)
		t.Fatalf("expected state_schema_invalid for required: scalar inside nested schema")
	}
}

func TestLoadServiceManifest_StateSchemaPropertiesRecursive(t *testing.T) {
	// Корректный nested state_schema со вложенными properties/required/items.
	// Регрессия: рекурсия не должна добавлять ложные diagnostics.
	src := `name: svc-golden
state_schema_version: 2
state_schema:
  type: object
  required: [version, hosts]
  properties:
    version: { type: string }
    hosts:
      type: array
      items:
        type: object
        required: [sid, role]
        properties:
          sid: { type: string }
          role: { type: string }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors on valid recursive state_schema")
	}
}

func TestLoadServiceManifest_DestinyBadRef(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
destiny:
  - { name: redis, ref: "" }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.destiny[0].ref" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on destiny[0].ref")
	}
}

func TestLoadServiceManifest_DestinyBadName(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
destiny:
  - { name: BAD_NAME, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.destiny[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format on destiny[0].name")
	}
}

func TestLoadServiceManifest_ModuleBadName(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: BAD_NAME, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.modules[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format on modules[0].name")
	}
}

func TestLoadServiceManifest_ModuleNamespacedName(t *testing.T) {
	// Двухуровневая форма — единственная валидная для modules[] (strict).
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb.haproxy, ref: v1.2.0 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors on namespaced module name wb.haproxy")
	}
}

func TestLoadServiceManifest_ModuleSingleLevelName(t *testing.T) {
	// Одноуровневая форма больше не принимается (strict <ns>.<module>).
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: redis-failover, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.modules[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format on one-level module name (strict two-level form required)")
	}
}

func TestLoadServiceManifest_ModuleUnderscoreInName(t *testing.T) {
	// underscore запрещён обеими частями (kebab-case naming-rules.md §57/§186).
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb_x.haproxy, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.modules[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format on underscore in namespace")
	}
}

func TestLoadServiceManifest_DependencyMissingName(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
destiny:
  - { name: "", ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.destiny[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on destiny[0].name when name is empty")
	}
}

// TestLoadServiceManifest_DestinyGitOverride — per-entry git override валиден
// для destiny[] (гибрид источника, override default_destiny_source).
func TestLoadServiceManifest_DestinyGitOverride(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
destiny:
  - { name: redis, ref: v2.0.0, git: "git@github.com:custom/destiny-special.git" }
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors on destiny[].git override")
	}
	if cfg.Destiny[0].Git != "git@github.com:custom/destiny-special.git" {
		t.Fatalf("destiny[0].git = %q, want override URL", cfg.Destiny[0].Git)
	}
}

// TestLoadServiceManifest_ModuleGitRejected — per-entry git override запрещён
// для modules[] (поддержан только destiny[]); один unknown_key на $.modules[0].git.
func TestLoadServiceManifest_ModuleGitRejected(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb.haproxy, ref: v1, git: "git@github.com:custom/mod.git" }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	count := 0
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.modules[0].git" {
			count++
		}
	}
	if count != 1 {
		dump(t, diags)
		t.Fatalf("expected exactly 1 unknown_key on $.modules[0].git, got %d", count)
	}
}

// TestLoadServiceManifest_ModuleCoreModule — core-модули в `modules:` не
// перечисляются (ADR-009/ADR-015), отдельный код вместо name_invalid_format.
func TestLoadServiceManifest_ModuleCoreModule(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: core.haproxy, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "core_module_in_modules_list" && d.YAMLPath == "$.modules[0].name" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected core_module_in_modules_list on core.haproxy in modules[]")
	}
	// Не должно быть параллельного name_invalid_format на той же ноде.
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.modules[0].name" {
			dump(t, diags)
			t.Fatalf("must not emit name_invalid_format alongside core_module_in_modules_list")
		}
	}
}

// TestLoadServiceManifest_KebabCaseStrict — canonical kebab-case: dash только
// между алфанумериков, без trailing/leading/double-dash. Симметрично для
// reServiceName / reDependencyDestinyName / reDependencyModuleName.
func TestLoadServiceManifest_KebabCaseStrict(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		yamlAt  string
		wantErr bool
	}{
		{
			name: "trailing dash in module namespace",
			src: `name: x
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb-.foo, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "trailing dash in module module-part",
			src: `name: x
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb.foo-, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "double dash in module namespace",
			src: `name: x
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb--foo.bar, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "multi-dash valid in module both parts",
			src: `name: x
state_schema_version: 1
state_schema:
  type: object
modules:
  - { name: wb-foo-bar.haproxy, ref: v1 }
`,
			wantErr: false,
		},
		{
			name: "trailing dash in service name",
			src: `name: redis-
state_schema_version: 1
state_schema:
  type: object
`,
			yamlAt:  "$.name",
			wantErr: true,
		},
		{
			name: "double dash in destiny name",
			src: `name: x
state_schema_version: 1
state_schema:
  type: object
destiny:
  - { name: wb--foo, ref: v1 }
`,
			yamlAt:  "$.destiny[0].name",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(tc.src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "name_invalid_format" && (tc.yamlAt == "" || d.YAMLPath == tc.yamlAt) {
					found = true
					break
				}
			}
			if tc.wantErr && !found {
				dump(t, diags)
				t.Fatalf("expected name_invalid_format at %s", tc.yamlAt)
			}
			if !tc.wantErr && diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("expected 0 errors for valid kebab-case sample")
			}
		})
	}
}

func TestLoadServiceManifest_UnknownTopKey(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
mystery: 42
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for non-deprecated unknown top-level field")
	}
}

// TestLoadServiceManifest_TelemetryAbsent — без блока telemetry геттеры дают
// дефолты (nil-safe), манифест парсится без ошибок (backcompat, NIM-87).
func TestLoadServiceManifest_TelemetryAbsent(t *testing.T) {
	src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n"
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("неожиданные ошибки без блока telemetry")
	}
	if cfg.Telemetry != nil {
		t.Error("Telemetry должен быть nil без блока")
	}
	if !cfg.Telemetry.EnabledOrDefault() {
		t.Error("nil-блок → EnabledOrDefault()=true")
	}
	if got := cfg.Telemetry.IntervalOrDefault(); got != "30s" {
		t.Errorf("nil-блок → IntervalOrDefault()=30s, got %q", got)
	}
	if got := cfg.Telemetry.CollectorsOrDefault(); len(got) != 5 {
		t.Errorf("nil-блок → CollectorsOrDefault()=все 5, got %v", got)
	}
}

// TestLoadServiceManifest_Telemetry — заданные значения читаются в *bool/*string/[]string.
func TestLoadServiceManifest_Telemetry(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
telemetry:
  enabled: false
  interval: "45s"
  collectors: [cpu, mem]
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("валидный блок telemetry дал ошибки")
	}
	if cfg.Telemetry == nil {
		t.Fatal("Telemetry nil, ожидался разобранный блок")
	}
	if cfg.Telemetry.EnabledOrDefault() {
		t.Error("enabled=false → EnabledOrDefault()=false")
	}
	if got := cfg.Telemetry.IntervalOrDefault(); got != "45s" {
		t.Errorf("IntervalOrDefault()=45s, got %q", got)
	}
	if got := cfg.Telemetry.CollectorsOrDefault(); len(got) != 2 || got[0] != "cpu" || got[1] != "mem" {
		t.Errorf("CollectorsOrDefault()=[cpu mem], got %v", got)
	}
}

// TestLoadServiceManifest_TelemetryBadCollector — неизвестный collector → unknown_collector.
func TestLoadServiceManifest_TelemetryBadCollector(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
telemetry:
  collectors: [cpu, foobar]
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_collector", "$.telemetry.collectors") {
		dump(t, diags)
		t.Fatal("ожидался unknown_collector для foobar")
	}
}

// TestLoadServiceManifest_TelemetryIntervalFloor — interval < 10s → value_out_of_range.
func TestLoadServiceManifest_TelemetryIntervalFloor(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
telemetry:
  interval: "3s"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "value_out_of_range", "$.telemetry.interval") {
		dump(t, diags)
		t.Fatal("ожидался value_out_of_range для interval 3s (< floor)")
	}
}

// TestLoadServiceManifest_TelemetryIntervalInvalid — interval не парсится → duration_invalid.
func TestLoadServiceManifest_TelemetryIntervalInvalid(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
telemetry:
  interval: "nonsense"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "duration_invalid", "$.telemetry.interval") {
		dump(t, diags)
		t.Fatal("ожидался duration_invalid для interval nonsense")
	}
}

// TestLoadServiceManifest_TelemetryUnknownKey — опечатка под telemetry: ловится
// reflect-walker-ом как unknown_key (авто, TelemetryConfig не в stop-типах).
func TestLoadServiceManifest_TelemetryUnknownKey(t *testing.T) {
	src := `name: svc-golden
state_schema_version: 1
state_schema:
  type: object
telemetry:
  bogus: 1
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.telemetry.bogus") {
		dump(t, diags)
		t.Fatal("ожидался unknown_key для опечатки bogus под telemetry")
	}
}
