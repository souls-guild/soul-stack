package validate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ladderFixture is the service whose ladder these tests mutate: a real one, six
// rungs deep, out of the repository's own bundled corpus.
//
// It is copied rather than pointed at, and every case below mutates the COPY in the
// form a service author would — a directory removed, a directory renamed, a key
// written into a step's own document. Nothing here patches expected text: the
// mutation is the same edit the check exists to catch, so a check that stopped
// looking at the tree could not pass by looking at a string.
const ladderFixture = "../../../examples/service/dragonfly"

// dragonflyLadder is that service's ladder as it stands. Written out so the cases
// can name a rung without reading the directory first, and so a step renamed
// upstream fails here loudly instead of silently skipping a mutation.
var dragonflyLadder = []string{
	"002_install_layout_to_vars",
	"003_cloud_provision_read_model",
	"004_provisioned_sids",
	"005_monitoring_read_model",
	"006_logging_read_model",
	"007_system_acl_users",
}

// copyServiceTree copies the fixture service into a temporary directory and
// returns the copy's root.
func copyServiceTree(t *testing.T) string {
	t.Helper()
	src, err := filepath.Abs(ladderFixture)
	if err != nil {
		t.Fatalf("abs %s: %v", ladderFixture, err)
	}
	dst := t.TempDir()
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", src, err)
	}
	return dst
}

// lintService runs `validate-service` over the manifest of a copied tree and
// returns the exit code with every diagnostic it printed.
func lintLadderService(t *testing.T, root string) (int, []diagRecord) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: filepath.Join(root, "service.yml"), JSON: true, Kind: KindService}, &out, &errOut)
	var got []diagRecord
	dec := json.NewDecoder(&out)
	for {
		var d diagRecord
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v (stderr=%s)", err, errOut.String())
		}
		got = append(got, d)
	}
	return code, got
}

// diagRecord is the subset of a diagnostic these tests judge: the code, the
// address, and the message that has to name what is wrong.
type diagRecord struct {
	Code    string `json:"code"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
	Level   string `json:"level"`
}

func findLadderDiag(got []diagRecord, code string) *diagRecord {
	for i := range got {
		if got[i].Code == code {
			return &got[i]
		}
	}
	return nil
}

func ladderCodes(got []diagRecord) []string {
	out := make([]string, 0, len(got))
	for _, d := range got {
		out = append(out, d.Code)
	}
	sort.Strings(out)
	return out
}

// TestLadder_UnmutatedFixtureIsClean is the control every case below leans on.
//
// Without it a check that reported `migration_chain_broken` on every service would
// pass every negative case in this file.
func TestLadder_UnmutatedFixtureIsClean(t *testing.T) {
	root := copyServiceTree(t)
	code, got := lintLadderService(t, root)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitOK, ladderCodes(got))
	}
}

// TestLadder_DeletedStepIsAGap — a rung removed from the middle is an error naming
// the version that lost its step.
//
// The mutation is the deletion itself, on a real ladder: this is the failure that
// went from "a chain the loader could not follow" to "a chain the loader cannot
// follow AND a version that did not move to say so". Deleting the middle is the
// worse half — the top of the ladder is unchanged, so the service still claims the
// same version while every incarnation below the gap has become unupgradable.
func TestLadder_DeletedStepIsAGap(t *testing.T) {
	root := copyServiceTree(t)
	// 004_provisioned_sids: strictly inside the ladder, so the top does not move.
	if err := os.RemoveAll(filepath.Join(root, config.MigrationsDirName, dragonflyLadder[2])); err != nil {
		t.Fatalf("remove step: %v", err)
	}

	code, got := lintLadderService(t, root)
	if code != ExitHasErrors {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
	}
	d := findLadderDiag(got, "migration_chain_broken")
	if d == nil {
		t.Fatalf("no migration_chain_broken; got %v", ladderCodes(got))
	}
	// The address has to be the missing version, not just "the ladder is broken":
	// with six rungs, "somewhere" is a directory listing away from useless.
	if !strings.Contains(d.Message, "version 4") {
		t.Errorf("message %q does not name the missing version", d.Message)
	}
	if !strings.Contains(d.File, config.MigrationsDirName) {
		t.Errorf("file %q does not address the ladder", d.File)
	}
}

// TestLadder_DeletedTopStepLowersTheVersion — deleting the TOP rung is NOT a gap,
// and the linter must not invent one.
//
// This is the derived-version edge case ADR-019's amendment left open. Removing the
// top step legitimately lowers the service's version from 7 to 6; the ladder is
// still gapless and still describes a consistent service, and a linter that reported
// a gap here would fire on every deliberate step removal.
//
// ⚠ What this test pins is a KNOWN HOLE, not a clean outcome. An incarnation at the
// old top (7) gets a loud refusal — its target now reads as a downgrade. One at the
// NEW top (6) sees `target == current`, an empty chain, and a silent ref-bump, and
// nothing offline or online catches it; `schema.lock` (NIM-737) is the artifact that
// remembers where the ladder used to end. One BELOW 6 simply migrates forward to 6,
// which is correct and needs no answer. Recorded in ADR-019's Consequences and in
// docs/migrations.md — if that changes, this test changes with it.
func TestLadder_DeletedTopStepLowersTheVersion(t *testing.T) {
	root := copyServiceTree(t)
	top := dragonflyLadder[len(dragonflyLadder)-1]
	if err := os.RemoveAll(filepath.Join(root, config.MigrationsDirName, top)); err != nil {
		t.Fatalf("remove top step: %v", err)
	}

	code, got := lintLadderService(t, root)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitOK, ladderCodes(got))
	}
	ladder, diags := config.ScanMigrationLadder(root)
	if len(diags) != 0 {
		t.Fatalf("scan diagnostics on a gapless ladder: %v", diags)
	}
	if ladder.Version() != 6 {
		t.Errorf("derived version = %d, want 6 (the ladder is one rung shorter)", ladder.Version())
	}
}

// TestLadder_StepStatesItsOwnPlace — a step carrying the retired header is refused
// at the line it is written on.
//
// The mutation writes a real `from_version:` into a real step document, which is
// exactly the shape a half-migrated service repository has. Ignoring the key would
// be the quiet failure: the step's position would then be stated twice, and the
// copy the engine does not read is the one the author is looking at.
func TestLadder_StepStatesItsOwnPlace(t *testing.T) {
	root := copyServiceTree(t)
	step := filepath.Join(root, config.MigrationsDirName, dragonflyLadder[1], config.MigrationStepFile)
	body, err := os.ReadFile(step)
	if err != nil {
		t.Fatalf("read step: %v", err)
	}
	if err := os.WriteFile(step, append([]byte("from_version: 2\nto_version: 3\n"), body...), 0o644); err != nil {
		t.Fatalf("write step: %v", err)
	}

	code, got := lintLadderService(t, root)
	if code != ExitHasErrors {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
	}
	var found int
	for _, d := range got {
		if d.Code != "migration_version_key" {
			continue
		}
		found++
		if d.Line == 0 || d.Column == 0 {
			t.Errorf("%s: refused at %d:%d — a retired key needs the line it is written on", d.Message, d.Line, d.Column)
		}
		if !strings.HasSuffix(d.File, filepath.Join(dragonflyLadder[1], config.MigrationStepFile)) {
			t.Errorf("file %q does not address the step that carries the key", d.File)
		}
	}
	// Both keys are reported: an author who wrote one wrote the other, and a single
	// diagnostic would send them back for a second run.
	if found != 2 {
		t.Errorf("migration_version_key count = %d, want 2 (from_version and to_version)", found)
	}
}

// TestLadder_RetiredFlatLayoutIsNamed — the pre-NIM-735 form is refused by name.
//
// A service half-way through the move has a step file the engine does not read at
// all; "unknown entry" would leave the author guessing, so the diagnostic says
// which layout this is and what the directory should be called instead.
func TestLadder_RetiredFlatLayoutIsNamed(t *testing.T) {
	root := copyServiceTree(t)
	migDir := filepath.Join(root, config.MigrationsDirName)
	stepDoc := filepath.Join(migDir, dragonflyLadder[0], config.MigrationStepFile)
	body, err := os.ReadFile(stepDoc)
	if err != nil {
		t.Fatalf("read step: %v", err)
	}
	// Put the step back in the shape it had before the move: a flat file, and the
	// tests beside it rather than inside it.
	if err := os.WriteFile(filepath.Join(migDir, "001_to_002.yml"), body, 0o644); err != nil {
		t.Fatalf("write flat step: %v", err)
	}
	if err := os.Rename(filepath.Join(migDir, dragonflyLadder[0]), filepath.Join(migDir, "001_to_002")); err != nil {
		t.Fatalf("rename step dir: %v", err)
	}

	code, got := lintLadderService(t, root)
	if code != ExitHasErrors {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
	}
	d := findLadderDiag(got, "migration_layout_retired")
	if d == nil {
		t.Fatalf("no migration_layout_retired; got %v", ladderCodes(got))
	}
	if !strings.Contains(d.Hint, "002_") {
		t.Errorf("hint %q does not say what to rename it to", d.Hint)
	}
}

// TestLadder_StructuralDefects covers the remaining ways a ladder stops describing
// the version it looks like it describes. Each mutation is a real edit to the copied
// tree; each assertion is the code the author has to see.
func TestLadder_StructuralDefects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, migDir string)
		code   string
	}{
		{
			// A directory the engine cannot read as a step is a step that does not
			// exist — and here it takes the top of the ladder down with it.
			name: "unrecognised_directory",
			mutate: func(t *testing.T, migDir string) {
				rename(t, migDir, dragonflyLadder[5], "system-acl-users")
			},
			code: "migration_step_name_invalid",
		},
		{
			// Two rungs claiming one version: the engine takes one and drops the
			// other without saying which.
			name: "duplicate_number",
			mutate: func(t *testing.T, migDir string) {
				rename(t, migDir, dragonflyLadder[5], "006_system_acl_users")
			},
			code: "migration_step_duplicate",
		},
		{
			// Version 1 is the empty ladder, so no step can lead to it.
			name: "step_leads_to_version_one",
			mutate: func(t *testing.T, migDir string) {
				rename(t, migDir, dragonflyLadder[0], "001_install_layout_to_vars")
			},
			code: "migration_step_number_invalid",
		},
		{
			// A step that is a FILE, not a directory: the shape an author writes when
			// they adopt the new slug naming but leave the document flat. The engine
			// cannot see it, so the top of the ladder drops from 7 to 6 in silence.
			name: "step_that_is_a_file",
			mutate: func(t *testing.T, migDir string) {
				top := filepath.Join(migDir, dragonflyLadder[5])
				body, err := os.ReadFile(filepath.Join(top, config.MigrationStepFile))
				if err != nil {
					t.Fatalf("read top step: %v", err)
				}
				if err := os.RemoveAll(top); err != nil {
					t.Fatalf("remove top step dir: %v", err)
				}
				if err := os.WriteFile(top+".yml", body, 0o644); err != nil {
					t.Fatalf("write flat step: %v", err)
				}
			},
			code: "migration_step_name_invalid",
		},
		{
			// `migrations/` itself committed as a regular file — the whole ladder
			// gone, and version 1 answered for a service on 7.
			name: "ladder_root_is_a_file",
			mutate: func(t *testing.T, migDir string) {
				if err := os.RemoveAll(migDir); err != nil {
					t.Fatalf("remove ladder: %v", err)
				}
				if err := os.WriteFile(migDir, []byte("oops\n"), 0o644); err != nil {
					t.Fatalf("write ladder-as-a-file: %v", err)
				}
			},
			code: "migration_ladder_unreadable",
		},
		{
			// A step directory with no document is not a step, and the version it
			// claims is unreachable.
			name: "step_without_document",
			mutate: func(t *testing.T, migDir string) {
				p := filepath.Join(migDir, dragonflyLadder[3], config.MigrationStepFile)
				if err := os.Remove(p); err != nil {
					t.Fatalf("remove %s: %v", p, err)
				}
			},
			code: "migration_step_main_missing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := copyServiceTree(t)
			tc.mutate(t, filepath.Join(root, config.MigrationsDirName))

			code, got := lintLadderService(t, root)
			if code != ExitHasErrors {
				t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
			}
			d := findLadderDiag(got, tc.code)
			if d == nil {
				t.Fatalf("no %s; got %v", tc.code, ladderCodes(got))
			}
			if d.Level != "error" {
				t.Errorf("%s level = %q, want error", tc.code, d.Level)
			}
			if d.File == "" {
				t.Errorf("%s carries no address", tc.code)
			}
		})
	}
}

func rename(t *testing.T, migDir, from, to string) {
	t.Helper()
	if err := os.Rename(filepath.Join(migDir, from), filepath.Join(migDir, to)); err != nil {
		t.Fatalf("rename %s → %s: %v", from, to, err)
	}
}
