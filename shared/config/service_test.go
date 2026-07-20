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

// TestLoadServiceManifest_Lifecycle — a lifecycle block with both flags is accepted
// (NOT unknown_key), the flags decode into *bool.
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
		t.Fatalf("lifecycle block gave errors, expected 0")
	}
	if cfg.Lifecycle == nil {
		t.Fatal("Lifecycle nil, expected a parsed block")
	}
	if cfg.Lifecycle.AutoCreateEnabled() {
		t.Error("auto_create=false must give AutoCreateEnabled()=false")
	}
	if !cfg.Lifecycle.AutoDestroyEnabled() {
		t.Error("auto_destroy=true must give AutoDestroyEnabled()=true")
	}
}

// TestLoadServiceManifest_LifecycleAbsent — without a lifecycle block both flags
// default to true (backcompat), the nil-safe accessors work.
func TestLoadServiceManifest_LifecycleAbsent(t *testing.T) {
	src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n"
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("unexpected errors")
	}
	if cfg.Lifecycle != nil {
		t.Error("Lifecycle must be nil without a block")
	}
	// nil-safe: both true.
	if !cfg.Lifecycle.AutoCreateEnabled() || !cfg.Lifecycle.AutoDestroyEnabled() {
		t.Error("a nil block must be treated as both true (backcompat)")
	}
}

// TestLoadServiceManifest_LifecycleUnknownKey — a typo under lifecycle:
// (e.g. auto_creat) is caught by the reflect-walker as unknown_key.
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
		t.Fatal("expected unknown_key for the auto_creat typo under lifecycle")
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
	// Like destiny: a deprecated top-level key must yield exactly one diagnostic
	// (from schemaValidateService with a hint), not a duplicate from the reflect-walker.
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
	// state_schema: (null) — key present, but not a mapping.
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
	// type absent on the root — `state_schema_root_not_object` (as root not object).
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
	// required must be an array of strings; nested schema (under `users`) → recursive.
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
	// A correct nested state_schema with nested properties/required/items.
	// Regression: recursion must not add spurious diagnostics.
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
	// The two-level form is the only valid one for modules[] (strict).
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
	// The one-level form is no longer accepted (strict <ns>.<module>).
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
	// underscore is forbidden in both parts (kebab-case naming-rules.md §57/§186).
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

// TestLoadServiceManifest_DestinyGitOverride — a per-entry git override is valid
// for destiny[] (hybrid source, overrides default_destiny_source).
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

// TestLoadServiceManifest_ModuleGitRejected — a per-entry git override is forbidden
// for modules[] (supported only for destiny[]); one unknown_key at $.modules[0].git.
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

// TestLoadServiceManifest_ModuleCoreModule — core modules are not listed in
// `modules:` (ADR-009/ADR-015), a dedicated code instead of name_invalid_format.
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
	// Must not have a parallel name_invalid_format on the same node.
	for _, d := range diags {
		if d.Code == "name_invalid_format" && d.YAMLPath == "$.modules[0].name" {
			dump(t, diags)
			t.Fatalf("must not emit name_invalid_format alongside core_module_in_modules_list")
		}
	}
}

// TestLoadServiceManifest_KebabCaseStrict — canonical kebab-case: dash only between
// alphanumerics, no trailing/leading/double-dash. Symmetric for reServiceName /
// reDependencyDestinyName / reDependencyModuleName.
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

// TestLoadServiceManifest_TelemetryAbsent — without a telemetry block the getters give
// defaults (nil-safe), the manifest parses without errors (backcompat, NIM-87).
func TestLoadServiceManifest_TelemetryAbsent(t *testing.T) {
	src := "name: svc-golden\nstate_schema_version: 1\nstate_schema:\n  type: object\n"
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("unexpected errors without a telemetry block")
	}
	if cfg.Telemetry != nil {
		t.Error("Telemetry must be nil without a block")
	}
	if !cfg.Telemetry.EnabledOrDefault() {
		t.Error("nil block → EnabledOrDefault()=true")
	}
	if got := cfg.Telemetry.IntervalOrDefault(); got != "30s" {
		t.Errorf("nil block → IntervalOrDefault()=30s, got %q", got)
	}
	if got := cfg.Telemetry.CollectorsOrDefault(); len(got) != len(KnownCollectors) {
		t.Errorf("nil block → CollectorsOrDefault()=all %d, got %v", len(KnownCollectors), got)
	}
}

// TestLoadServiceManifest_Telemetry — set values are read into *bool/*string/[]string.
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
		t.Fatal("a valid telemetry block produced errors")
	}
	if cfg.Telemetry == nil {
		t.Fatal("Telemetry nil, expected a parsed block")
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

// TestLoadServiceManifest_TelemetryBadCollector — unknown collector → unknown_collector.
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
		t.Fatal("expected unknown_collector for foobar")
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
		t.Fatal("expected value_out_of_range for interval 3s (< floor)")
	}
}

// TestLoadServiceManifest_TelemetryIntervalInvalid — interval fails to parse → duration_invalid.
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
		t.Fatal("expected duration_invalid for interval nonsense")
	}
}

// TestLoadServiceManifest_TelemetryUnknownKey — a typo under telemetry is caught
// by the reflect-walker as unknown_key (auto, TelemetryConfig is not in the stop types).
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
		t.Fatal("expected unknown_key for the bogus typo under telemetry")
	}
}
