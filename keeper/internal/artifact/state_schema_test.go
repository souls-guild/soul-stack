package artifact

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeServiceManifest is a helper: puts `service.yml` into the test
// serviceRoot. Parallel with writeScenario.
func writeServiceManifest(t *testing.T, root, body string) {
	t.Helper()
	p := filepath.Join(root, serviceManifestFile)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile service.yml: %v", err)
	}
}

// writeMigrationStep is a helper: puts `migrations/<dir>/main.yml` with an empty
// body (content is not parsed; the listing works from the directory names alone).
func writeMigrationStep(t *testing.T, root, dir, body string) {
	t.Helper()
	stepDir := filepath.Join(root, config.MigrationsDirName, dir)
	if err := os.MkdirAll(stepDir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", stepDir, err)
	}
	if err := os.WriteFile(filepath.Join(stepDir, config.MigrationStepFile), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", dir, err)
	}
}

// writeMigrationFile is a helper: puts a plain file directly under `migrations/`.
func writeMigrationFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, config.MigrationsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll migrations: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

// stateSchemaManifest is the manifest these tests project from. It states no
// version — since NIM-735 there is no such key; the version comes from the ladder.
const stateSchemaManifest = `state_schema:
  master_host:
    required: true
    type: string
  replicas:
    required: true
    type: integer
`

// TestListStateSchema_ReadsManifest is the happy path: the derived version +
// structure + migrations are present, response contains everything, sorted by `to`
// ASC.
func TestListStateSchema_ReadsManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, stateSchemaManifest)
	writeMigrationStep(t, root, "002_widen_users", "transform: []\n")
	writeMigrationStep(t, root, "003_drop_install", "transform: []\n")

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	// The manifest states no version: 3 is the top of the ladder written above.
	if info.Version != 3 {
		t.Errorf("Version = %d, want 3 (top of the ladder)", info.Version)
	}
	if info.Schema == nil {
		t.Fatal("Schema=nil, want state_schema declaration")
	}
	// The projection is the RAW mapping of the input dialect ([NIM-740]): state field
	// -> schema, with no `type: object` wrapper above it.
	field, ok := info.Schema["master_host"].(map[string]any)
	if !ok {
		t.Fatalf("Schema.master_host = %#v, want the field's schema", info.Schema["master_host"])
	}
	if field["type"] != "string" {
		t.Errorf("Schema.master_host.type = %v, want string", field["type"])
	}
	if len(info.Migrations) != 2 {
		t.Fatalf("Migrations len = %d, want 2; %+v", len(info.Migrations), info.Migrations)
	}
	if info.Migrations[0].From != 1 || info.Migrations[0].To != 2 {
		t.Errorf("Migrations[0] = %+v", info.Migrations[0])
	}
	if info.Migrations[1].From != 2 || info.Migrations[1].To != 3 {
		t.Errorf("Migrations[1] = %+v", info.Migrations[1])
	}
	if info.Migrations[0].Path != "migrations/002_widen_users/main.yml" {
		t.Errorf("Migrations[0].Path = %q", info.Migrations[0].Path)
	}
}

// TestListStateSchema_NoMigrationsDir covers a missing `migrations/` directory;
// it should return an empty list without error (parity with ListScenarios for
// scenario/).
func TestListStateSchema_NoMigrationsDir(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, stateSchemaManifest)

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	if info.Migrations == nil {
		t.Errorf("Migrations should be an empty slice, not nil")
	}
	if len(info.Migrations) != 0 {
		t.Errorf("want empty list, got %+v", info.Migrations)
	}
	// An empty ladder IS version 1 — the floor, not a missing answer.
	if info.Version != config.BaseStateSchemaVersion {
		t.Errorf("Version = %d, want %d", info.Version, config.BaseStateSchemaVersion)
	}
}

// TestListStateSchema_SortByToAsc returns migrations sorted by `to` (the chain
// graph grows), regardless of os.ReadDir order.
func TestListStateSchema_SortByToAsc(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, stateSchemaManifest)
	// Put directories in reverse name order to verify that sorting actually works.
	writeMigrationStep(t, root, "004_c", "")
	writeMigrationStep(t, root, "002_a", "")
	writeMigrationStep(t, root, "003_b", "")

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	wantTo := []int{2, 3, 4}
	if len(info.Migrations) != len(wantTo) {
		t.Fatalf("Migrations len = %d, want 3", len(info.Migrations))
	}
	for i, w := range wantTo {
		if info.Migrations[i].To != w {
			t.Errorf("Migrations[%d].To = %d, want %d", i, info.Migrations[i].To, w)
		}
	}
}

// TestListStateSchema_IgnoresNonStepEntries verifies that entries in `migrations/`
// which are not step directories are left out of the listing: a plain file (README,
// and later `schema.lock`) and a directory whose name is not `<NNN>_<slug>`.
//
// The listing is the permissive read — the engine reports what is there rather than
// refusing; `soul-lint validate-service` is what calls the unrecognised directory a
// defect (`migration_step_name_invalid`).
func TestListStateSchema_IgnoresNonStepEntries(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, stateSchemaManifest)
	writeMigrationStep(t, root, "002_widen_users", "")
	writeMigrationFile(t, root, "README.md", "docs")
	writeMigrationFile(t, root, "schema.lock", "version: 2\n")
	writeMigrationStep(t, root, "2_widen_users", "no leading zeros")

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	if len(info.Migrations) != 1 {
		t.Fatalf("len = %d, want 1; %+v", len(info.Migrations), info.Migrations)
	}
	if info.Migrations[0].From != 1 || info.Migrations[0].To != 2 {
		t.Errorf("Migrations[0] = %+v", info.Migrations[0])
	}
}

// TestListStateSchema_MissingManifest covers missing `service.yml` -> error
// (broken snapshot; caller returns 502).
func TestListStateSchema_MissingManifest(t *testing.T) {
	root := t.TempDir()
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("want error when service.yml is missing")
	}
}

// TestListStateSchema_BrokenManifest covers invalid YAML -> error.
func TestListStateSchema_BrokenManifest(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, "{ this is: not valid: yaml :::\n")
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("want error for invalid service.yml")
	}
}

// TestListStateSchema_NoStateSchemaField covers a manifest without
// `state_schema:` -> normative validation treats it as an error (state_schema
// required in MVP). It ensures we do not mask drift between the UI response and
// the normative schema.
func TestListStateSchema_NoStateSchemaField(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, "description: no state_schema here\n")
	_, err := ListStateSchema(root, discardLogger())
	if err == nil {
		t.Fatalf("want validation error (state_schema required)")
	}
}
