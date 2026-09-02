package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// writeLadder materialises a ladder of step directories under root, each holding
// the given document body.
func writeLadder(t *testing.T, root string, body string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		stepDir := filepath.Join(root, MigrationsDirName, d)
		if err := os.MkdirAll(stepDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", stepDir, err)
		}
		if err := os.WriteFile(filepath.Join(stepDir, MigrationStepFile), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", d, err)
		}
	}
}

// TestMigrationLadder_EmptyIsVersionOne — a service that never changed its state
// schema is at version 1, and "no migrations/ directory" and "an empty one" are the
// same answer.
//
// The floor is the load-bearing half of the derivation: every service in the corpus
// that used to write `state_schema_version: 1` writes nothing now, and if the empty
// ladder answered 0 all of them would resolve below their incarnations.
func TestMigrationLadder_EmptyIsVersionOne(t *testing.T) {
	noDir := t.TempDir()
	ladder, diags := ScanMigrationLadder(noDir)
	if len(diags) != 0 {
		t.Fatalf("a service with no migrations/ is not a defect: %v", diags)
	}
	if ladder.Version() != BaseStateSchemaVersion {
		t.Errorf("no migrations/: version = %d, want %d", ladder.Version(), BaseStateSchemaVersion)
	}

	emptyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(emptyDir, MigrationsDirName), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ladder, diags = ScanMigrationLadder(emptyDir)
	if len(diags) != 0 {
		t.Fatalf("an empty migrations/ is not a defect: %v", diags)
	}
	if ladder.Version() != BaseStateSchemaVersion {
		t.Errorf("empty migrations/: version = %d, want %d", ladder.Version(), BaseStateSchemaVersion)
	}
}

// TestMigrationLadder_VersionIsTheTop — the version is the highest rung, and the
// scan sorts regardless of what order the filesystem hands the entries back in.
func TestMigrationLadder_VersionIsTheTop(t *testing.T) {
	root := t.TempDir()
	writeLadder(t, root, "transform: []\n", "004_c", "002_a", "003_b")

	ladder, diags := ScanMigrationLadder(root)
	if diag.HasErrors(diags) {
		t.Fatalf("clean ladder reported errors: %v", diags)
	}
	if ladder.Version() != 4 {
		t.Fatalf("version = %d, want 4", ladder.Version())
	}
	for i, want := range []int{2, 3, 4} {
		if ladder.Steps[i].Version != want {
			t.Errorf("Steps[%d].Version = %d, want %d", i, ladder.Steps[i].Version, want)
		}
	}
	step, ok := ladder.Step(3)
	if !ok {
		t.Fatal("Step(3) not found")
	}
	// The path is the step's, slug included — it is what the loader reads and what
	// the API reports, and it is not reconstructible from the number alone.
	if step.Path != "migrations/003_b/main.yml" {
		t.Errorf("Step(3).Path = %q", step.Path)
	}
	if _, ok := ladder.Step(5); ok {
		t.Error("Step(5) found on a four-rung ladder")
	}
}

// TestMigrationLadder_GapIsReportedPerMissingVersion — two holes are two things to
// put back, and each is named.
func TestMigrationLadder_GapIsReportedPerMissingVersion(t *testing.T) {
	root := t.TempDir()
	writeLadder(t, root, "transform: []\n", "002_a", "005_d")

	diags := ValidateMigrationLadder(root)
	var broken int
	for _, d := range diags {
		if d.Code == "migration_chain_broken" {
			broken++
		}
	}
	if broken != 2 {
		t.Fatalf("migration_chain_broken count = %d, want 2 (versions 3 and 4); diags: %v", broken, diags)
	}
}

// TestValidateMigrationStepFile_RetiredHeader — both retired keys are refused, each
// at its own line, and a document without them is clean.
func TestValidateMigrationStepFile_RetiredHeader(t *testing.T) {
	src := []byte("description: x\nfrom_version: 1\nto_version: 2\ntransform: []\n")
	diags := ValidateMigrationStepFile("migrations/002_a/main.yml", src)
	if len(diags) != 2 {
		t.Fatalf("diags = %d, want 2; %v", len(diags), diags)
	}
	wantLine := map[string]int{"$.from_version": 2, "$.to_version": 3}
	for _, d := range diags {
		if d.Code != "migration_version_key" {
			t.Errorf("code = %q, want migration_version_key", d.Code)
		}
		if d.Line != wantLine[d.YAMLPath] || d.Column == 0 {
			t.Errorf("%s addressed at %d:%d, want line %d with a column", d.YAMLPath, d.Line, d.Column, wantLine[d.YAMLPath])
		}
	}

	clean := ValidateMigrationStepFile("migrations/002_a/main.yml", []byte("description: x\ntransform: []\n"))
	if len(clean) != 0 {
		t.Errorf("a step that states no place is clean, got %v", clean)
	}
}

// TestMigrationLadder_StepThatIsNotADirectory — an entry named like a step but not a
// directory is reported, and the ladder loses the rung.
//
// This is the half-migrated repository: an author moves to the new slug naming but
// leaves the document flat (`015_system_acl_users.yml`), which the retired-form
// regex does not match because that regex only knows `<NNN>_to_<MMM>`. Silently
// skipping it drops the top of the ladder by one, and the version drops with it —
// which then reads as "nothing to upgrade" rather than as a defect.
func TestMigrationLadder_StepThatIsNotADirectory(t *testing.T) {
	root := t.TempDir()
	writeLadder(t, root, "transform: []\n", "002_a", "003_b")
	flat := filepath.Join(root, MigrationsDirName, "004_c.yml")
	if err := os.WriteFile(flat, []byte("transform: []\n"), 0o644); err != nil {
		t.Fatalf("write flat step: %v", err)
	}

	ladder, diags := ScanMigrationLadder(root)
	d := findDiagCode(diags, "migration_step_name_invalid")
	if d == nil {
		t.Fatalf("a step-named file was skipped in silence; diags: %v", ladderDiagCodes(diags))
	}
	if d.File != flat {
		t.Errorf("File = %q, want %q", d.File, flat)
	}
	// And the ladder honestly reports what the engine can actually see.
	if ladder.Version() != 3 {
		t.Errorf("version = %d, want 3 — the flat entry is not a rung", ladder.Version())
	}
}

// TestMigrationLadder_NeighbouringFilesStaySilent is the counterweight: without it,
// a scanner that reported every non-directory entry would pass the test above.
//
// `schema.lock` in particular is specified to live exactly here (NIM-737), so a
// diagnostic on it would make the documented layout un-lintable.
func TestMigrationLadder_NeighbouringFilesStaySilent(t *testing.T) {
	root := t.TempDir()
	writeLadder(t, root, "transform: []\n", "002_a")
	for _, name := range []string{"schema.lock", "README.md", ".gitkeep"} {
		p := filepath.Join(root, MigrationsDirName, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	ladder, diags := ScanMigrationLadder(root)
	if len(diags) != 0 {
		t.Fatalf("neighbouring files are not defects: %v", ladderDiagCodes(diags))
	}
	if ladder.Version() != 2 {
		t.Errorf("version = %d, want 2", ladder.Version())
	}
}

// TestMigrationLadder_UnreadableRootIsReported — `migrations` that is not a
// directory is the whole ladder disappearing, and must not read as version 1 in
// silence.
//
// Committing `migrations` as a regular file takes redis from 15 to 1: every live
// incarnation then reads its own target as a downgrade, and before this the linter
// was green while it happened.
func TestMigrationLadder_UnreadableRootIsReported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, MigrationsDirName), []byte("oops\n"), 0o644); err != nil {
		t.Fatalf("write migrations-as-a-file: %v", err)
	}

	ladder, diags := ScanMigrationLadder(root)
	if findDiagCode(diags, "migration_ladder_unreadable") == nil {
		t.Fatalf("an unreadable ladder root was swallowed; diags: %v", ladderDiagCodes(diags))
	}
	if ladder.Version() != BaseStateSchemaVersion {
		t.Errorf("version = %d, want the floor", ladder.Version())
	}
	// The linter has to surface it too — it reads through the same scan.
	if !diag.HasErrors(ValidateMigrationLadder(root)) {
		t.Error("ValidateMigrationLadder is green on an unreadable ladder root")
	}
}

// TestMigrationLadder_DuplicateSurvivorIsDeterministic pins WHICH of two steps
// claiming one version wins.
//
// os.ReadDir sorts by name and the map is first-wins, so the answer is stable — but
// nothing said so, and "a duplicate is reported" would still hold if the surviving
// rung flipped between runs. The version a service resolves to must not depend on
// filesystem order.
func TestMigrationLadder_DuplicateSurvivorIsDeterministic(t *testing.T) {
	root := t.TempDir()
	writeLadder(t, root, "transform: []\n", "002_a", "003_zulu", "003_alpha")

	ladder, diags := ScanMigrationLadder(root)
	if findDiagCode(diags, "migration_step_duplicate") == nil {
		t.Fatalf("duplicate not reported; diags: %v", ladderDiagCodes(diags))
	}
	step, ok := ladder.Step(3)
	if !ok {
		t.Fatal("version 3 has no step at all")
	}
	if step.Dir != "003_alpha" {
		t.Errorf("survivor = %q, want 003_alpha (lexically first, so the answer is stable)", step.Dir)
	}
}

func findDiagCode(diags []diag.Diagnostic, code string) *diag.Diagnostic {
	for i := range diags {
		if diags[i].Code == code {
			return &diags[i]
		}
	}
	return nil
}

func ladderDiagCodes(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
