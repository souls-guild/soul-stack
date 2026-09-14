package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// ★ NIM-726. A manifest without `name:` is the NORMAL shape now — the field is gone,
// and a service is named once, at registration. This is the guard the whole ticket
// turns on: the keeper's ServiceLoader.parseManifest treats an error diagnostic as a
// refusal to load, so re-introducing ANY error for an absent name would make every
// current service unloadable, which is the exact breakage this change removes.
func TestLoadServiceManifest_NoNameIsValid(t *testing.T) {
	src := `description: a service states no name of its own
state_schema: {}
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("a manifest without name: must load clean; got %d diagnostics", len(diags))
	}
}

// The other half of the same cut: `name:` is REFUSED, not ignored. A key that decodes
// into nothing is worse than either alternative — the author reads it as identity, the
// engine reads nothing, and the two never meet. The refusal must carry the deprecation
// hint (naming where the name lives now) rather than a bare unknown_key, and must be
// raised EXACTLY ONCE: the reflect walker and the deprecated-key pass both see this key,
// and a duplicate shows up as a twin line in the JSON output.
func TestLoadServiceManifest_NameIsRefused(t *testing.T) {
	src := `name: redis
state_schema: {}
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	n := 0
	for _, d := range diags {
		if d.YAMLPath != "$.name" {
			continue
		}
		n++
		if d.Code != "unknown_key" || d.Level != diag.LevelError {
			dump(t, diags)
			t.Fatalf("name: diagnostic = [%s] %s, want an unknown_key error", d.Code, d.Level)
		}
		if !strings.Contains(d.Hint, "registration") {
			dump(t, diags)
			t.Fatalf("hint %q must say where the name lives now", d.Hint)
		}
	}
	if n != 1 {
		dump(t, diags)
		t.Fatalf("got %d diagnostics at $.name, want exactly 1", n)
	}
}

// TestLoadServiceManifest_Lifecycle — a lifecycle block with both flags is accepted
// (NOT unknown_key), the flags decode into *bool.
func TestLoadServiceManifest_Lifecycle(t *testing.T) {
	src := `state_schema: {}
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
	src := "state_schema: {}\n"
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
	src := `state_schema: {}
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
	cases := []string{"version", "tasks", "steps", "input", "scenarios", "revealable_secrets"}
	for _, key := range cases {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "state_schema: {}\n" + key + ": foo\n"
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

func TestLoadServiceManifest_RevealableSecretsRetired(t *testing.T) {
	// The block in the shape an author actually wrote it (ADR-070), not a scalar
	// stand-in: a nested sequence takes a different route through the reflect-walker
	// than `key: foo` does, and this is the form every migrating service carries.
	src := `name: redis
state_schema: {}
revealable_secrets:
  - id: user-password
    label: "Redis user password"
    enumerate: state.users
    vault_ref: "secret/{service}/{incarnation}/users/{key}#password"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	var hint string
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.revealable_secrets" {
			hint = d.Hint
		}
	}
	if hint == "" {
		dump(t, diags)
		t.Fatal("revealable_secrets must be refused at load with a hint naming the replacement")
	}
	// The point of the hint is that it says what to write instead. A generic
	// "unknown field" leaves the author guessing, so assert on the replacement
	// itself and not merely on the hint being non-empty.
	for _, want := range []string{"type: secret", "ADR-0083"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q does not name %q", hint, want)
		}
	}
}

func TestLoadServiceManifest_DeprecatedKeyNoDuplicate(t *testing.T) {
	// Like destiny: a deprecated top-level key must yield exactly one diagnostic
	// (from schemaValidateService with a hint), not a duplicate from the reflect-walker.
	src := `state_schema: {}
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

// TestLoadServiceManifest_StateSchemaVersionKeyRetired — the manifest key is gone
// (NIM-735), and writing it is `unknown_key`, not a silently-ignored extra.
//
// Guarded in every spelling the old validator distinguished — a good integer, the
// float it used to catch, the zero it used to reject — because the point is no
// longer what the value is. There is no value: whatever stands there is a second
// record of a number the `migrations/` ladder already states, and the two are free
// to disagree. A manifest carrying `state_schema_version: 15` beside a ladder
// topping out at 14 must not load as either.
func TestLoadServiceManifest_StateSchemaVersionKeyRetired(t *testing.T) {
	for _, val := range []string{"1", "0", "1.5", `"abc"`, "15"} {
		src := "state_schema_version: " + val + "\nstate_schema: {}\n"
		_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
		if !hasCode(diags, "unknown_key") {
			dump(t, diags)
			t.Fatalf("state_schema_version: %s — expected unknown_key", val)
		}
	}
}

// TestLoadServiceManifest_NoStateSchemaVersionIsClean — the other half: a manifest
// that states no version is VALID. Without this, a validator that refused every
// manifest would pass the test above.
func TestLoadServiceManifest_NoStateSchemaVersionIsClean(t *testing.T) {
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte("state_schema: {}\n"), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("a manifest without state_schema_version must be clean")
	}
}

func TestLoadServiceManifest_MissingStateSchema(t *testing.T) {
	// A manifest with something in it but no state_schema. The empty document is a
	// different diagnostic (`empty_document`), and since the retired version key was
	// the other required top-level field, an empty file is now exactly that.
	src := "description: nothing else here\n"
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent state_schema")
	}
}

// The root of state_schema is a mapping of state field -> schema. A sequence there is
// not one, and it is refused rather than decoded into an empty schema — which would
// load a service whose state contract is silently nothing.
func TestLoadServiceManifest_StateSchemaNotObject(t *testing.T) {
	src := `state_schema:
  - foo
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_root_not_object") {
		dump(t, diags)
		t.Fatalf("expected state_schema_root_not_object")
	}
}

// An EMPTY mapping is valid, though: a service whose scenarios write no state declares
// no fields, and several of the examples do exactly that. Refusing it would force a
// dummy field on every one of them.
func TestLoadServiceManifest_StateSchemaEmptyIsValid(t *testing.T) {
	src := `state_schema: {}
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("an empty state_schema must load clean")
	}
}

func TestLoadServiceManifest_StateSchemaNullValue(t *testing.T) {
	// state_schema: (null) — key present, but not a mapping.
	src := `state_schema:
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "state_schema_root_not_object") {
		dump(t, diags)
		t.Fatalf("expected state_schema_root_not_object for null state_schema value")
	}
}

func TestLoadServiceManifest_StateSchemaRequiredNotArray(t *testing.T) {
	// `required:` takes a bool at the field level or a list of property names inside an
	// object; a scalar string is neither, and the input dialect says so at the key.
	src := `state_schema:
  users:
    type: object
    properties:
      name: { type: string }
    required: "not-an-array"
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_required_value_invalid") {
		dump(t, diags)
		t.Fatalf("expected input_required_value_invalid for required: scalar inside nested schema")
	}
}

func TestLoadServiceManifest_StateSchemaPropertiesRecursive(t *testing.T) {
	// A correct nested state_schema with nested properties/required/items.
	// Regression: recursion must not add spurious diagnostics.
	src := `state_schema:
  version: { type: string, required: true }
  hosts:
    required: true
    type: array
    items:
      type: object
      properties:
        sid: { type: string, required: true }
        role: { type: string, required: true }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors on valid recursive state_schema")
	}
}

func TestLoadServiceManifest_DestinyBadRef(t *testing.T) {
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
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

// The two-level form is still ACCEPTED — an error here would break every live
// manifest at once — but it is warned about, with the alias it collapses to named in
// the diagnostic (NIM-829, transition window to 2026-12-01).
func TestLoadServiceManifest_ModuleNamespacedName(t *testing.T) {
	src := `state_schema: {}
modules:
  - { name: acme.haproxy, ref: v1.2.0 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors on namespaced module name acme.haproxy")
	}
	d := codeAt(diags, ModuleNameTwoLevelCode, "$.modules[0].name")
	if d == nil {
		dump(t, diags)
		t.Fatalf("the deprecated two-level form passed without a %s warning", ModuleNameTwoLevelCode)
	}
	if d.Level != diag.LevelWarning {
		t.Errorf("%s level = %q, want warning — an error would break every live manifest at once", d.Code, d.Level)
	}
	if !strings.Contains(d.Hint, "name: acme") {
		t.Errorf("hint does not name the alias to write instead: %q", d.Hint)
	}
}

// TestLoadServiceManifest_ModuleSingleLevelName — the canonical form (NIM-829).
//
// `modules[]` declares an ARTIFACT: one entry, one ref, one slot. The bare alias was
// refused outright until NIM-829, which is why a downstream redis manifest had to spell out
// six entries for one binary — and why `conflicting_module_ref` had to exist to catch
// the two-ref state that spelling makes writable.
func TestLoadServiceManifest_ModuleSingleLevelName(t *testing.T) {
	src := `state_schema: {}
modules:
  - { name: redis-failover, ref: v1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("the single-segment form was refused; it is the canonical one (NIM-829)")
	}
	if codeAt(diags, ModuleNameTwoLevelCode, "$.modules[0].name") != nil {
		dump(t, diags)
		t.Fatalf("the canonical form was warned about as deprecated")
	}
}

// TestLoadServiceManifest_ModuleSingleLevelReservedName — `core` alone claims the
// reserved name as squarely as `core.file` does, and gets the same diagnostic.
//
// The check used to read the second return of ModuleAlias as "is two-level", so a bare
// reserved name reached only the format regex. Widening that regex without moving the
// reserved check would have opened the shorter spelling of the exact claim the longer
// one is refused for.
func TestLoadServiceManifest_ModuleSingleLevelReservedName(t *testing.T) {
	for _, name := range plugin.ReservedNames() {
		src := "state_schema: {}\nmodules:\n  - { name: " + name + ", ref: v1 }\n"
		_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
		want := "reserved_module_namespace"
		if name == "core" {
			want = "core_module_in_modules_list"
		}
		if codeAt(diags, want, "$.modules[0].name") == nil {
			dump(t, diags)
			t.Errorf("modules[0].name = %s (bare) → want %s", name, want)
		}
	}
}

// TestLoadServiceManifest_ModuleSixObjectsCollapseToOne — the ticket's own manifest,
// before and after, asserted as the pair of facts that makes the migration safe:
// the old spelling still validates (with six warnings, one per entry), and the new one
// validates clean.
func TestLoadServiceManifest_ModuleSixObjectsCollapseToOne(t *testing.T) {
	const head = "state_schema: {}\nmodules:\n"
	objects := []string{"cluster", "command", "instance", "replica", "sentinel", "user"}

	var old strings.Builder
	old.WriteString(head)
	for _, o := range objects {
		fmt.Fprintf(&old, "  - { name: redis.%s, ref: v1.0.0 }\n", o)
	}
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(old.String()), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("the pre-NIM-829 six-entry manifest stopped validating; the window has not opened yet")
	}
	warned := 0
	for _, d := range diags {
		if d.Code == ModuleNameTwoLevelCode {
			warned++
		}
	}
	if warned != len(objects) {
		dump(t, diags)
		t.Fatalf("%s warnings = %d, want %d (one per entry — the author must see every line to delete)",
			ModuleNameTwoLevelCode, warned, len(objects))
	}

	_, _, diags, _ = LoadServiceManifestFromBytes("service.yml",
		[]byte(head+"  - { name: redis, ref: v1.0.0 }\n"), ValidateOptions{})
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("the migrated form is not clean: %v", diagCodesP(diags))
	}
}

// TestLoadServiceManifest_ConflictingRefStillCaught — `conflicting_module_ref` is NOT
// removed while both forms are accepted.
//
// One binary in two versions is the state the two-level form makes writable, and the
// single-segment form makes unwritable. Between those two facts sits a half-migrated
// manifest, which is exactly this input — so the guard comes off with the form, not
// with the fix that deprecates it (NIM-836).
func TestLoadServiceManifest_ConflictingRefStillCaught(t *testing.T) {
	src := `state_schema: {}
modules:
  - { name: redis.cluster, ref: v1.0.0 }
  - { name: redis, ref: v2.0.0 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if codeAt(diags, "conflicting_module_ref", "$.modules[1].ref") == nil {
		dump(t, diags)
		t.Fatalf("two refs for one alias passed validation: %v", diagCodesP(diags))
	}
}

func TestLoadServiceManifest_ModuleUnderscoreInName(t *testing.T) {
	// underscore is forbidden in both parts (kebab-case naming-rules.md §57/§186).
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
modules:
  - { name: acme.haproxy, ref: v1, git: "git@github.com:custom/mod.git" }
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
	src := `state_schema: {}
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
// alphanumerics, no trailing/leading/double-dash. Symmetric for
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
			src: `state_schema: {}
modules:
  - { name: acme-.foo, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "trailing dash in module module-part",
			src: `state_schema: {}
modules:
  - { name: acme.foo-, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "double dash in module namespace",
			src: `state_schema: {}
modules:
  - { name: acme--foo.bar, ref: v1 }
`,
			yamlAt:  "$.modules[0].name",
			wantErr: true,
		},
		{
			name: "multi-dash valid in module both parts",
			src: `state_schema: {}
modules:
  - { name: acme-foo-bar.haproxy, ref: v1 }
`,
			wantErr: false,
		},
		{
			name: "double dash in destiny name",
			src: `state_schema: {}
destiny:
  - { name: acme--foo, ref: v1 }
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
	src := `state_schema: {}
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
	src := "state_schema: {}\n"
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
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
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
	src := `state_schema: {}
telemetry:
  bogus: 1
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_key", "$.telemetry.bogus") {
		dump(t, diags)
		t.Fatal("expected unknown_key for the bogus typo under telemetry")
	}
}

// TestLoadServiceManifest_ConflictingModuleRef — two entries under one alias
// pinning different refs is a contradiction, not a last-writer-wins race.
//
// One alias is one slot holding one artifact (ADR-065 amendment 2026-08-07), so
// once NIM-524 collapsed the several `modules[]` entries of one artifact into a
// single synthesized install there is no longer a ref per entry to honour. The
// synthesizer takes one of them; without this diagnostic the other `ref:` would
// be read, ignored, and the pin check would fail later naming a ref the author
// never wrote beside the module that failed.
func TestLoadServiceManifest_ConflictingModuleRef(t *testing.T) {
	src := `state_schema: {}
modules:
  - { name: redis.instance, ref: v1.0.0 }
  - { name: redis.sentinel, ref: v2.0.0 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if n := countCode(diags, "conflicting_module_ref"); n != 1 {
		dump(t, diags)
		t.Fatalf("conflicting_module_ref fired %d times, want exactly 1", n)
	}
	if !hasCodeAt(diags, "conflicting_module_ref", "$.modules[1].ref") {
		dump(t, diags)
		t.Fatal("expected conflicting_module_ref on the disagreeing entry, $.modules[1].ref")
	}
}

// TestLoadServiceManifest_SharedAliasSameRefIsClean — the load-bearing half.
//
// Several entries under one alias is the NORMAL way to declare one artifact
// serving several modules, and NIM-524's collapse-into-one-install depends on
// that shape staying legal. A diagnostic firing here would make the very thing
// the alias exists to express unusable, so this asserts an absence — the
// direction a test written only for the error case leaves open.
func TestLoadServiceManifest_SharedAliasSameRefIsClean(t *testing.T) {
	src := `state_schema: {}
modules:
  - { name: redis.instance, ref: v1.0.0 }
  - { name: redis.sentinel, ref: v1.0.0 }
  - { name: acme-tools.probe, ref: v0.3.1 }
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "conflicting_module_ref") {
		dump(t, diags)
		t.Fatal("conflicting_module_ref on entries that agree on ref")
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("unexpected error diagnostic on a valid manifest: %s at %s", d.Code, d.YAMLPath)
		}
	}
}
