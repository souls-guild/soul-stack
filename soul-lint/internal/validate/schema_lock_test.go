package validate

// The schema lock, end to end over a real service.
//
// Every case below mutates a COPY of the repository's own bundled corpus in the form
// a service author would — a comment typed into `state_schema`, a field added to it,
// a step directory deleted. Nothing here patches expected text, so a check that
// stopped reading the tree could not pass by matching a string.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// editStateSchema inserts body immediately under the `state_schema:` key of a copied
// service manifest — the first line of the block, where an author adds a field or a
// comment.
func editStateSchema(t *testing.T, root, body string) {
	t.Helper()
	path := filepath.Join(root, "service.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	const key = "\nstate_schema:\n"
	i := bytes.Index(src, []byte(key))
	if i < 0 {
		t.Fatalf("%s has no top-level state_schema: block", path)
	}
	at := i + len(key)
	edited := append(append(append([]byte{}, src[:at]...), []byte(body)...), src[at:]...)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// stampService runs the real `schema-stamp` over a copied tree and returns its exit
// code with everything it printed on both streams.
func stampService(t *testing.T, root string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunStamp(StampOptions{Root: root}, &out, &errOut)
	return code, out.String(), errOut.String()
}

func readLock(t *testing.T, root string) []byte {
	t.Helper()
	data, err := os.ReadFile(config.SchemaLockPath(root))
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	return data
}

var fingerprintRE = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// TestSchemaLock_ACommentIsNotAStructuralChange — the reason the fingerprint is over
// the PARSED schema and never over the bytes.
//
// A text hash would move here, and an author who has to re-stamp for a comment stops
// reading what a re-stamp means — which is the entire signal the artifact carries,
// since its own bypass is closed by review and not by the tool.
func TestSchemaLock_ACommentIsNotAStructuralChange(t *testing.T) {
	root := copyServiceTree(t)
	before := readLock(t, root)

	editStateSchema(t, root, "  # Written while reviewing: says what the block is for, declares nothing.\n")

	code, got := lintLadderService(t, root)
	if code != ExitOK {
		t.Fatalf("a comment in state_schema turned the service red: exit %d, %v", code, ladderCodes(got))
	}
	if after := readLock(t, root); !bytes.Equal(before, after) {
		t.Error("the lint rewrote the lock - the check must not stamp")
	}
}

// TestSchemaLock_AStateFieldAddedWithNoStepIsRefused is the failure the whole artifact
// exists for: `state_schema` says one shape, the ladder implements another, and
// nothing else in the repository compares the two — not offline, and not inside the
// upgrade transaction, which applies the chain and never loads the target schema.
func TestSchemaLock_AStateFieldAddedWithNoStepIsRefused(t *testing.T) {
	root := copyServiceTree(t)
	editStateSchema(t, root, "  seeded_from:\n    type: string\n")

	code, got := lintLadderService(t, root)
	if code != ExitHasErrors {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
	}
	d := findLadderDiag(got, "schema_lock_stale")
	if d == nil {
		t.Fatalf("no schema_lock_stale; got %v", ladderCodes(got))
	}
	// BOTH hashes, in the message. "The lock is stale" alone leaves the author with
	// no way to tell a schema edit from a lock written by a different build, and no
	// way to quote the disagreement into a review.
	found := fingerprintRE.FindAllString(d.Message, -1)
	if len(found) != 2 || found[0] == found[1] {
		t.Errorf("message names %d fingerprint(s), want the stamped one and the current one, distinct: %q", len(found), d.Message)
	}
	if !strings.Contains(d.File, config.SchemaLockFile) {
		t.Errorf("file %q does not address the lock", d.File)
	}
	// And the ladder is NOT accused: it is intact, and the edit is the schema's.
	for _, invented := range []string{"migration_chain_broken", "schema_lock_version_stale"} {
		if findLadderDiag(got, invented) != nil {
			t.Errorf("an untouched ladder was reported as %s: %v", invented, ladderCodes(got))
		}
	}
}

// TestSchemaLock_ReStampingIsAllowedAndVisible pins the bypass as DECIDED, not as an
// oversight ([ADR-019] clause 4): a re-stamp with no new step succeeds, and what
// stops it being free is that the diff shows a changed lock beside an unchanged
// ladder. The mechanical ban was considered and rejected — it would force an empty
// migration step for every harmless schema edit.
//
// If a future reader "fixes" this by making the stamp refuse until the ladder moves,
// this test is what says the refusal was the rejected option.
func TestSchemaLock_ReStampingIsAllowedAndVisible(t *testing.T) {
	root := copyServiceTree(t)
	before := readLock(t, root)
	stepsBefore, _ := config.ScanMigrationLadder(root)

	editStateSchema(t, root, "  seeded_from:\n    type: string\n")
	if code, _ := lintLadderService(t, root); code != ExitHasErrors {
		t.Fatalf("the edit was not caught in the first place: exit %d", code)
	}

	if code, out, errOut := stampService(t, root); code != ExitOK {
		t.Fatalf("re-stamp = %d:\n%s%s", code, out, errOut)
	}
	code, got := lintLadderService(t, root)
	if code != ExitOK {
		t.Fatalf("still red after a re-stamp: exit %d, %v", code, ladderCodes(got))
	}

	after := readLock(t, root)
	if bytes.Equal(before, after) {
		t.Error("the lock did not change - then nothing in the diff shows the bypass was taken")
	}
	stepsAfter, _ := config.ScanMigrationLadder(root)
	if len(stepsAfter.Steps) != len(stepsBefore.Steps) {
		t.Errorf("the ladder grew a rung: %d -> %d; the stamp must not write steps", len(stepsBefore.Steps), len(stepsAfter.Steps))
	}
}

// TestSchemaStamp_IsDeterministic — a generated file in git is worth having only if
// re-running the generator produces no diff. A timestamp, an absolute path or an
// unsorted walk in the output would put every service repository into a permanent
// one-line churn and train its reviewers to skip the file.
func TestSchemaStamp_IsDeterministic(t *testing.T) {
	root := copyServiceTree(t)

	code, firstOut, errOut := stampService(t, root)
	if code != ExitOK {
		t.Fatalf("stamp = %d:\n%s%s", code, firstOut, errOut)
	}
	first := readLock(t, root)

	code, secondOut, errOut := stampService(t, root)
	if code != ExitOK {
		t.Fatalf("second stamp = %d:\n%s%s", code, secondOut, errOut)
	}
	if second := readLock(t, root); !bytes.Equal(first, second) {
		t.Errorf("two stamps of one tree differ:\n%s\n---\n%s", first, second)
	}
	if firstOut != secondOut {
		t.Errorf("stdout differs between runs:\n%q\n%q", firstOut, secondOut)
	}
	// The committed lock is the one the stamp writes: a corpus service whose lock
	// was hand-adjusted would make every test above assert against a fiction.
	committed, err := os.ReadFile(filepath.Join(ladderFixture, config.MigrationsDirName, config.SchemaLockFile))
	if err != nil {
		t.Fatalf("read the committed lock: %v", err)
	}
	if !bytes.Equal(committed, first) {
		t.Errorf("the committed lock of %s is not what the stamp writes:\n%s\n---\n%s", ladderFixture, committed, first)
	}
}

// TestSchemaStamp_RefusesARedService — a stamp claims that this schema and this
// ladder were, at one moment, both sound and in agreement. Generated over a ladder
// with a gap it would freeze a version nobody meant, and would then MATCH silently
// for as long as nobody touched the schema again: a confident wrong artifact rather
// than a missing one.
func TestSchemaStamp_RefusesARedService(t *testing.T) {
	root := copyServiceTree(t)
	before := readLock(t, root)
	// A gap, made the way a gap is really made: a step directory deleted from the
	// middle of the ladder.
	if err := os.RemoveAll(filepath.Join(root, config.MigrationsDirName, dragonflyLadder[2])); err != nil {
		t.Fatalf("remove step: %v", err)
	}

	code, out, errOut := stampService(t, root)
	if code != ExitHasErrors {
		t.Fatalf("stamp over a broken ladder = %d, want %d:\n%s%s", code, ExitHasErrors, out, errOut)
	}
	if !strings.Contains(errOut, "migration_chain_broken") {
		t.Errorf("the refusal does not name what is wrong:\n%s", errOut)
	}
	if out != "" {
		t.Errorf("a refused stamp still printed a result on stdout: %q", out)
	}
	if after := readLock(t, root); !bytes.Equal(before, after) {
		t.Error("a refused stamp rewrote the lock anyway")
	}
}

// TestSchemaLock_IsCheckedByTheWholeServiceWalk — the check has to be INSIDE
// NIM-753's mode, not beside it. It reaches the walk through the manifest's own check
// set, which is what makes `validate-service-tree` report it without a part of its
// own; a lock check wired only into the per-file command would be missing from the
// one invocation a service repository's `make validate` actually runs.
func TestSchemaLock_IsCheckedByTheWholeServiceWalk(t *testing.T) {
	root := copyServiceTree(t)
	editStateSchema(t, root, "  seeded_from:\n    type: string\n")

	code, out, _ := runTree(t, TreeOptions{Root: root, ServiceName: "dragonfly"})
	if code != ExitHasErrors {
		t.Fatalf("exit = %d, want %d:\n%s", code, ExitHasErrors, out)
	}
	if !strings.Contains(out, "schema_lock_stale") {
		t.Errorf("the whole-service walk did not check the lock:\n%s", out)
	}
	// The manifest part carries it, so that part is not clean — and no `OK:` line
	// may claim otherwise.
	if strings.Contains(out, "OK: "+filepath.Join(root, "service.yml")) {
		t.Errorf("the manifest part printed OK with a stale lock against it:\n%s", out)
	}
}

// TestSchemaLock_AnUnresolvedTypeRefIsNotHashed — the fingerprint is over the
// RESOLVED schema, so a `{$type: X}` still standing means there is nothing honest to
// compare. Hashing what is left would report the schema as edited when the real fault
// is a broken catalog, and would send the author to write a migration step for it.
func TestSchemaLock_AnUnresolvedTypeRefIsNotHashed(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "service.yml"),
		"description: Typed\n\nstate_schema:\n  accounts:\n    type: array\n    items: { $type: AclUser }\n")
	writeFile(t, filepath.Join(root, "types.yml"),
		"types:\n  AclUser:\n    type: object\n    properties:\n      name: { type: string }\n")
	writeFile(t, filepath.Join(root, config.MigrationsDirName, "002_add_accounts", config.MigrationStepFile),
		"transform: []\n")

	if code, out, errOut := stampService(t, root); code != ExitOK {
		t.Fatalf("stamp = %d:\n%s%s", code, out, errOut)
	}
	if code, got := lintLadderService(t, root); code != ExitOK {
		t.Fatalf("a stamped typed service is not clean: exit %d, %v", code, ladderCodes(got))
	}

	// Now break the catalog the reference resolves against. The reference itself is
	// untouched, so the schema is not what the author wrote any more.
	if err := os.Remove(filepath.Join(root, "types.yml")); err != nil {
		t.Fatalf("remove types.yml: %v", err)
	}
	code, got := lintLadderService(t, root)
	// RED, and that is the assertion the rest of this case turns on. A check that
	// could not run must not leave the run green: from here an arbitrary
	// `state_schema` edit is compared against nothing, and the one route where the
	// upstream `$type` failure is itself only a warning — a types.yml that exists
	// and cannot be read — would otherwise exit 0 over an unreconciled schema.
	if code != ExitHasErrors {
		t.Fatalf("exit code = %d, want %d; diagnostics: %v", code, ExitHasErrors, ladderCodes(got))
	}
	if findLadderDiag(got, "schema_lock_unchecked") == nil {
		t.Errorf("the lock check did not say it could not run: %v", ladderCodes(got))
	}
	if d := findLadderDiag(got, "schema_lock_stale"); d != nil {
		t.Errorf("half a schema was hashed and reported as an edit: %s", d.Message)
	}
	// And the stamp refuses for the same reason, rather than freezing half a schema.
	code, _, errOut := stampService(t, root)
	if code != ExitHasErrors {
		t.Errorf("stamp over an unresolved $type = %d, want %d:\n%s", code, ExitHasErrors, errOut)
	}
}
