package artifact

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeScenario is a helper for placing scenario/<name>/main.yml inside the
// test serviceRoot. It returns the absolute path to main.yml.
func writeScenario(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, scenarioDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, scenarioMainFile)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// writeUpgrade is a helper for placing upgrade/<slug>/main.yml inside the test
// serviceRoot (mirror of writeScenario for the second discovery channel,
// ADR-0068).
func writeUpgrade(t *testing.T, root, slug, body string) string {
	t.Helper()
	dir := filepath.Join(root, upgradeDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	p := filepath.Join(dir, scenarioMainFile)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestListScenarios_ReadsAllValid covers three valid scenarios, name sorting,
// and reading description / input_schema / tags.
func TestListScenarios_ReadsAllValid(t *testing.T) {
	root := t.TempDir()

	writeScenario(t, root, "create", `description: Creates incarnation
input_schema:
  shards:
    type: integer
  replicas:
    type: integer
tags: [create]
`)
	writeScenario(t, root, "add_replicas", `description: Add replicas
input:
  count:
    type: integer
`)
	writeScenario(t, root, "rolling_restart", `description: Rolling restart
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3; got = %+v", len(got), got)
	}
	// Name sorting (alphabetical asc): add_replicas, create, rolling_restart.
	wantOrder := []string{"add_replicas", "create", "rolling_restart"}
	for i, n := range wantOrder {
		if got[i].Name != n {
			t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, n)
		}
	}
	// create: input_schema is picked up (input_schema takes precedence over input), tags are filled.
	create := got[1]
	if create.Description != "Creates incarnation" {
		t.Errorf("create.Description = %q", create.Description)
	}
	if len(create.InputSchema) != 2 {
		t.Errorf("create.InputSchema len = %d, want 2", len(create.InputSchema))
	}
	if len(create.Tags) != 1 || create.Tags[0] != "create" {
		t.Errorf("create.Tags = %+v", create.Tags)
	}
	if create.Path != "scenario/create/main.yml" {
		t.Errorf("create.Path = %q", create.Path)
	}
	// add_replicas: top-level `input` (without _schema) should land in InputSchema.
	add := got[0]
	if len(add.InputSchema) != 1 {
		t.Errorf("add_replicas.InputSchema len = %d (input fallback did not work)", len(add.InputSchema))
	}
	// rolling_restart: description only, everything else is empty.
	rr := got[2]
	if rr.Description == "" {
		t.Errorf("rolling_restart.Description is empty")
	}
	if rr.InputSchema != nil {
		t.Errorf("rolling_restart.InputSchema should be nil, got %+v", rr.InputSchema)
	}
}

// TestListScenarios_PreferInputSchemaOverInput verifies that when both fields
// are set, input_schema wins (it is the normative name; `input` is fallback for
// fresh examples).
func TestListScenarios_PreferInputSchemaOverInput(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `input_schema:
  schema_key: 1
input:
  input_key: 2
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if _, ok := got[0].InputSchema["schema_key"]; !ok {
		t.Errorf("schema_key should win: %+v", got[0].InputSchema)
	}
	if _, ok := got[0].InputSchema["input_key"]; ok {
		t.Errorf("input_key should not appear: %+v", got[0].InputSchema)
	}
}

// TestListScenarios_ResolvesCovenantInput is a guard for a UI bug: scenario
// inherits input through `extends: covenant`, with a zero local input delta.
// ListScenarios must MERGE covenant.yml.input into InputSchema (same add-only
// merge as runtime), otherwise the create UI form arrives empty (covenant fields
// are absent). Before the fix, loadScenario parsed raw main.yml without
// covenant resolution -> empty InputSchema.
func TestListScenarios_ResolvesCovenantInput(t *testing.T) {
	root := t.TempDir()
	// covenant.yml carries the FULL input contract (like redis covenant.yml).
	writeCovenant(t, root, "covenant", `input:
  version:
    type: string
    required: true
    enum: ["8.6.1", "6.2.21"]
  redis_type:
    type: string
    default: sentinel
    enum: [sentinel, cluster]
  memory_mb:
    type: integer
    min: 64
`)
	// Scenario delta is ZERO input (everything is inherited from covenant), as in
	// create_from_souls/main.yml.
	writeScenario(t, root, "create_from_souls", `name: create_from_souls
create: true
extends: covenant
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got = %+v", len(got), got)
	}
	sc := got[0]
	// covenant fields must land in InputSchema (otherwise the UI form is empty).
	for _, field := range []string{"version", "redis_type", "memory_mb"} {
		if _, ok := sc.InputSchema[field]; !ok {
			t.Errorf("covenant field %q was not merged into InputSchema: %#v", field, sc.InputSchema)
		}
	}
	// Field shape is preserved raw (UI reads type/enum/required directly).
	ver, ok := sc.InputSchema["version"].(map[string]any)
	if !ok {
		t.Fatalf("version is not a raw map: %T", sc.InputSchema["version"])
	}
	if ver["type"] != "string" {
		t.Errorf("version.type = %v, want string", ver["type"])
	}
	if _, ok := ver["enum"]; !ok {
		t.Errorf("version.enum lost after merge: %#v", ver)
	}
}

// TestListScenarios_CovenantMergeAddOnly verifies that covenant input and the
// scenario's OWN input both land in InputSchema (add-only union, delta
// complements covenant).
func TestListScenarios_CovenantMergeAddOnly(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "covenant", `input:
  shared_field:
    type: string
`)
	writeScenario(t, root, "create", `name: create
extends: covenant
tasks: []
input:
  local_field:
    type: integer
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	sc := got[0]
	if _, ok := sc.InputSchema["shared_field"]; !ok {
		t.Errorf("covenant field shared_field lost: %#v", sc.InputSchema)
	}
	if _, ok := sc.InputSchema["local_field"]; !ok {
		t.Errorf("local field local_field lost: %#v", sc.InputSchema)
	}
}

// TestListScenarios_NoExtendsUnaffected verifies that a scenario WITHOUT
// extends resolves as before (covenant resolution no-op, bit-for-bit
// forward-compat).
func TestListScenarios_NoExtendsUnaffected(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
tasks: []
input:
  only_field:
    type: string
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if len(got[0].InputSchema) != 1 {
		t.Errorf("without extends, InputSchema should carry only its own field, got %#v", got[0].InputSchema)
	}
}

// TestListScenarios_MissingScenarioDir covers missing scenario/ directory; it
// should return an empty list without error (service without scenarios is
// valid).
func TestListScenarios_MissingScenarioDir(t *testing.T) {
	root := t.TempDir()
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty list, got %+v", got)
	}
}

// TestListScenarios_SkipsBrokenYAML verifies that invalid YAML in one scenario
// must not break listing for the others (partial success).
func TestListScenarios_SkipsBrokenYAML(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "good", `description: ok
`)
	writeScenario(t, root, "bad", "{ this is: not: valid yaml :::\n")

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("want only good, got %+v", got)
	}
}

// TestListScenarios_SkipsFolderWithoutMain verifies that a scenario/<n>
// directory without main.yml is skipped (warning, no error).
func TestListScenarios_SkipsFolderWithoutMain(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "good", `description: ok
`)
	// Bare directory without main.yml.
	if err := os.MkdirAll(filepath.Join(root, scenarioDir, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("want only good, got %+v", got)
	}
}

// TestListScenarios_IgnoresFilesAtTopLevel verifies that file
// `scenario/foo.txt` next to directories is ignored (subdirectories only).
func TestListScenarios_IgnoresFilesAtTopLevel(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: ok\n")
	if err := os.WriteFile(filepath.Join(root, scenarioDir, "README.md"), []byte("docs"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Name != "create" {
		t.Errorf("want only create, got %+v", got)
	}
}

// TestListScenarios_IgnoresUnknownTopLevelFields verifies that the parser
// ignores non-standard top-level YAML fields (`tasks:`, `state_changes:`, etc.)
// and reads only name/description/input/input_schema/tags.
func TestListScenarios_IgnoresUnknownTopLevelFields(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
tasks:
  - name: foo
    module: core.exec.run
state_changes:
  sets:
    key: value
random_field: 123
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Description != "ok" {
		t.Errorf("Description = %q", got[0].Description)
	}
}

// TestListScenarios_FormProjection verifies that top-level `form:` is parsed
// into Scenario.Form: sections with key/title/collapsed/show_when and fields
// with name/label/show_when/placeholder/hint land in the listing projection.
func TestListScenarios_FormProjection(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  tls_enabled: { type: boolean }
  tls_port: { type: integer }
  redis_password: { type: string }
form:
  sections:
    - key: connection
      title: "Connection"
      collapsed: false
      show_when: "input.tls_enabled"
      fields:
        - { name: tls_enabled, label: "TLS" }
        - { name: tls_port, show_when: "input.tls_enabled", placeholder: "6379", hint: "TCP port" }
    - key: secrets
      title: "Secrets"
      collapsed: true
      fields:
        - { name: redis_password, label: "Redis password" }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Form == nil {
		t.Fatalf("Form was not parsed: %+v", got)
	}
	f := got[0].Form
	if len(f.Sections) != 2 {
		t.Fatalf("Sections len = %d, want 2", len(f.Sections))
	}
	if f.Sections[0].Key != "connection" || f.Sections[0].Title != "Connection" || f.Sections[0].Collapsed {
		t.Errorf("section[0] = %#v", f.Sections[0])
	}
	if f.Sections[0].ShowWhen != "input.tls_enabled" {
		t.Errorf("section[0].show_when = %q, want input.tls_enabled", f.Sections[0].ShowWhen)
	}
	if f.Sections[1].Key != "secrets" || !f.Sections[1].Collapsed {
		t.Errorf("section[1] = %#v, want collapsed=true", f.Sections[1])
	}
	if f.Sections[0].Fields[0].Name != "tls_enabled" || f.Sections[0].Fields[0].Label != "TLS" {
		t.Errorf("field[0] = %#v", f.Sections[0].Fields[0])
	}
	f1 := f.Sections[0].Fields[1]
	if f1.ShowWhen != "input.tls_enabled" || f1.Placeholder != "6379" || f1.Hint != "TCP port" {
		t.Errorf("field[1] show_when/placeholder/hint = %#v", f1)
	}
}

// TestListScenarios_FormUXKeysOmitted verifies that a field without
// show_when/placeholder/hint omits those keys from the JSON reply (omitempty,
// bit-for-bit as before the feature; forward-compat).
func TestListScenarios_FormUXKeysOmitted(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  a: { type: string }
form:
  sections:
    - key: s1
      fields:
        - { name: a, label: "A" }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	out, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"show_when"`, `"placeholder"`, `"hint"`} {
		if strings.Contains(string(out), key) {
			t.Errorf("key %s should not be present without a value (omitempty), got %s", key, out)
		}
	}
}

// TestListScenarios_FormAbsentOmitted verifies that without `form:`, Form==nil
// and the field is absent from the JSON reply (omitempty, bit-for-bit as before
// the feature; forward-compat).
func TestListScenarios_FormAbsentOmitted(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: ok
input:
  a: { type: string }
`)
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 || got[0].Form != nil {
		t.Fatalf("Form should be nil without form:, got %+v", got)
	}
	out, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), `"form"`) {
		t.Errorf("key \"form\" should not be present in JSON when form: is absent, got %s", out)
	}
}

// TestScenarioListerFunc_CompileTime is a compile-time guarantee that Scenario
// carries expected exported fields for JSON serialization (handler relies on
// json-tags).
func TestScenarioListerFunc_CompileTime(t *testing.T) {
	s := Scenario{
		Name:        "create",
		Path:        "scenario/create/main.yml",
		Kind:        ScenarioKindLifecycle,
		Runnable:    true,
		Description: "d",
		InputSchema: map[string]any{"k": 1},
		Tags:        []string{"a"},
	}
	_ = s
}

// TestListScenarios_CreateFlag verifies that top-level `create: true|false` is
// parsed into Scenario.Create; missing key -> false (back-compat). The create
// kind discriminator is used by the UI filter "choose starter scenario".
func TestListScenarios_CreateFlag(t *testing.T) {
	root := t.TempDir()

	writeScenario(t, root, "create", `description: default bootstrap
create: true
`)
	writeScenario(t, root, "create_cluster", `description: cluster-bootstrap
create: true
`)
	writeScenario(t, root, "add_user", `description: day-2 operation
create: false
`)
	writeScenario(t, root, "restart", `description: restart without flag
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	want := map[string]bool{
		"create":         true,
		"create_cluster": true,
		"add_user":       false,
		"restart":        false,
	}
	for _, s := range got {
		exp, ok := want[s.Name]
		if !ok {
			t.Fatalf("unexpected scenario %q", s.Name)
		}
		if s.Create != exp {
			t.Errorf("%s.Create = %v, want %v", s.Name, s.Create, exp)
		}
	}
}

// TestListScenarios_CreateFlagJSONOmitempty verifies that Scenario.Create is
// serialized to JSON under key `create` and omitted when false (omitempty:
// bit-for-bit as before the feature for non-create scenarios).
func TestListScenarios_CreateFlagJSONOmitempty(t *testing.T) {
	withCreate, err := json.Marshal(Scenario{Name: "create", Create: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(withCreate), `"create":true`) {
		t.Errorf("create=true scenario JSON must carry \"create\":true, got %s", withCreate)
	}
	noCreate, err := json.Marshal(Scenario{Name: "restart", Create: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(noCreate), `"create"`) {
		t.Errorf("create=false scenario JSON must omit \"create\" (omitempty), got %s", noCreate)
	}
}

// --- ListUpgrades: second auto-discovery channel upgrade/<slug>/ (ADR-0068 §3) ---

// TestListUpgrades_FindsUpgradeWithFrom verifies that upgrade/<slug>/main.yml
// with top-level `from:` is found and projected into Scenario.FromVersions;
// Path points to upgrade/.
func TestListUpgrades_FindsUpgradeWithFrom(t *testing.T) {
	root := t.TempDir()
	writeUpgrade(t, root, "v2", `description: sentinel -> cluster on v2
from: ["v1.0.0", "v1.2.0"]
tasks: []
`)

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; got = %+v", len(got), got)
	}
	up := got[0]
	if up.Name != "v2" {
		t.Errorf("Name = %q, want v2", up.Name)
	}
	if up.Path != "upgrade/v2/main.yml" {
		t.Errorf("Path = %q, want upgrade/v2/main.yml", up.Path)
	}
	want := []string{"v1.0.0", "v1.2.0"}
	if len(up.FromVersions) != len(want) || up.FromVersions[0] != want[0] || up.FromVersions[1] != want[1] {
		t.Errorf("FromVersions = %+v, want %+v", up.FromVersions, want)
	}
}

// TestListUpgrades_IgnoresNonDirs verifies that a loose file under upgrade/
// (not a directory) is skipped, like in ListScenarios.
func TestListUpgrades_IgnoresNonDirs(t *testing.T) {
	root := t.TempDir()
	writeUpgrade(t, root, "v2", `from: ["v1.0.0"]
tasks: []
`)
	// Loose file next to upgrade/ directories should not land in listing.
	if err := os.WriteFile(filepath.Join(root, upgradeDir, "README.md"), []byte("noise"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(got) != 1 || got[0].Name != "v2" {
		t.Fatalf("got = %+v, want exactly [v2]", got)
	}
}

// TestListUpgrades_MissingDir_Empty verifies that missing upgrade/ directory
// returns an empty list, NOT an error (service without upgrade scenarios is
// valid, ADR-0068 §5 legacy path).
func TestListUpgrades_MissingDir_Empty(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: x\ntasks: []\n")

	got, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades without upgrade/ should return nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %+v, want empty list", got)
	}
}

// TestListScenarios_IgnoresUpgradeDir is an isolation regression guard
// (ADR-0068 §3): upgrade/<slug>/ must NOT leak into the day-2 scenario list,
// and scenario/ must not leak into the upgrade list. Channels are strictly
// separated.
func TestListScenarios_IgnoresUpgradeDir(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "description: starter\ntasks: []\n")
	writeUpgrade(t, root, "v2", "from: [\"v1.0.0\"]\ntasks: []\n")

	scenarios, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(scenarios) != 1 || scenarios[0].Name != "create" {
		t.Fatalf("ListScenarios = %+v, want exactly [create] (upgrade/ must not leak)", scenarios)
	}

	upgrades, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(upgrades) != 1 || upgrades[0].Name != "v2" {
		t.Fatalf("ListUpgrades = %+v, want exactly [v2] (scenario/ must not leak)", upgrades)
	}
}

// TestListScenarios_StrayFromNotProjected is the PHYSICAL field-isolation gate
// (ADR-0068 §3): stray top-level `from:` in scenario/<name>/main.yml must NOT
// leak into the day-2 reply. FromVersions is filled only on the upgrade/
// channel (dir==upgradeDir), not indirectly by directory. Regression guard for
// an operator accidentally writing `from:` in a regular scenario.
func TestListScenarios_StrayFromNotProjected(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `description: starter
from: ["v1.0.0"]
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].FromVersions != nil {
		t.Fatalf("scenario/ entry should not carry FromVersions (stray from: leaked): %+v", got[0].FromVersions)
	}

	// Same stray guard symmetrically: upgrade/ channel MUST carry from:.
	writeUpgrade(t, root, "v2", "from: [\"v1.0.0\"]\ntasks: []\n")
	ups, err := ListUpgrades(root, discardLogger())
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	if len(ups) != 1 || len(ups[0].FromVersions) != 1 || ups[0].FromVersions[0] != "v1.0.0" {
		t.Fatalf("upgrade/ entry should carry FromVersions=[v1.0.0], got %+v", ups)
	}
}
