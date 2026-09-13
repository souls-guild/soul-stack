package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// commitFixtureSHA is the synthetic 40-hex commit [writeSlot] names the immutable slot
// with and points `current` at (these tests exercise reading a slot, not git-resolve).
const commitFixtureSHA = "0123456789abcdef0123456789abcdef01234567"

// stampedArtifact writes an executable at path holding body, with doc stamped into its
// trailer. Every fixture goes through the real serializer and the real trailer writer:
// a hand-assembled trailer would be testing a format nothing else produces.
func stampedArtifact(t *testing.T, path string, doc schema.Document, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	payload, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if err := schema.WriteTrailerFile(path, payload); err != nil {
		t.Fatalf("stamp artifact: %v", err)
	}
}

// writeSlot creates the R-nested slot (A1-S1) `<root>/<alias>/<commit>/` holding the
// named artifacts, plus `current → <commit>`. Each entry of artifacts is a file name;
// a nil doc means "write it unstamped".
func writeSlot(t *testing.T, root, alias string, doc *schema.Document, artifacts ...string) string {
	t.Helper()
	pluginDir := filepath.Join(root, alias)
	dir := filepath.Join(pluginDir, commitFixtureSHA)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	for _, name := range artifacts {
		path := filepath.Join(dir, name)
		if doc == nil {
			if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write artifact: %v", err)
			}
			continue
		}
		stampedArtifact(t, path, *doc, "#!/bin/sh\nexit 0\n# "+name+"\n")
	}
	if err := os.Symlink(commitFixtureSHA, filepath.Join(pluginDir, CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
	return dir
}

func TestReadSlot_Success(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	dir := writeSlot(t, root, "hetzner", &doc, "hetzner")

	got, err := ReadSlot(root, "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "hetzner"))
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	wantDigest := sha256.Sum256(raw)
	// A git slot reads back as a one-row unplatformed release: the repository states
	// no platform, so the grant records that absence rather than inventing one.
	if got.Kind != sharedplugin.SourceKindGit {
		t.Errorf("Kind = %q, want %q", got.Kind, sharedplugin.SourceKindGit)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("Artifacts = %d, want exactly one for a git slot", len(got.Artifacts))
	}
	if got.Artifacts[0].OS != sharedhost.AnyPlatform || got.Artifacts[0].Arch != sharedhost.AnyPlatform {
		t.Errorf("git artifact declares a platform (%q/%q), want none",
			got.Artifacts[0].OS, got.Artifacts[0].Arch)
	}
	if got.Artifacts[0].SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("SHA256 = %q, want %q", got.Artifacts[0].SHA256, hex.EncodeToString(wantDigest[:]))
	}

	// The schema bytes must be the trailer's payload byte for byte: they are what the
	// Sigil signature is placed over, so anything re-serialized here would be a
	// different document than the one approved.
	wantSchema, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got.SchemaBytes) != string(wantSchema) {
		t.Errorf("SchemaBytes are not the canonical trailer payload")
	}
	if got.Doc == nil || got.Doc.Kind != schema.KindSSHProvider {
		t.Errorf("Doc = %+v, want a parsed ssh_provider document", got.Doc)
	}
	if filepath.Base(got.Artifacts[0].BinaryPath) != "hetzner" {
		t.Errorf("BinaryPath = %q, want the slot's single artifact", got.Artifacts[0].BinaryPath)
	}
}

// TestReadSlot_ArtifactNameIsIrrelevant pins the removal of the filename convention:
// the artifact carries no self-name, so the slot's single executable is taken whatever
// it is called.
func TestReadSlot_ArtifactNameIsIrrelevant(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "hetzner", &doc, "some-completely-unrelated-filename")

	got, err := ReadSlot(root, "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}
	if filepath.Base(got.Artifacts[0].BinaryPath) != "some-completely-unrelated-filename" {
		t.Errorf("BinaryPath = %q, want the slot's single artifact regardless of its name",
			got.Artifacts[0].BinaryPath)
	}
}

func TestReadSlot_NoSlot(t *testing.T) {
	root := t.TempDir()
	_, err := ReadSlot(root, "absent")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot absent slot: err = %v, want ErrSlotNotFound", err)
	}
}

// TestReadSlot_EmptySlot — zero executables fails closed. An empty slot is not "nothing
// to do", it is a registration that resolves to no code.
func TestReadSlot_EmptySlot(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "hetzner", nil)
	_, err := ReadSlot(root, "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot on an empty slot: err = %v, want ErrSlotNotFound", err)
	}
}

// TestReadSlot_TwoExecutables — the guard that matters most in this file. With no
// filename convention left, "take the first" would let directory listing order decide
// which bytes get signed and later executed. Two executables must stop the read.
func TestReadSlot_TwoExecutables(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "hetzner", &doc, "artifact-a", "artifact-b")

	_, err := ReadSlot(root, "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot with two executables: err = %v, want ErrSlotNotFound", err)
	}
}

// TestReadSlot_NoTrailer — an unstamped artifact has no disclosure to approve. Fail
// closed with an error, NOT a fallback to a sibling schema.json and NOT an empty
// document: a host that guessed would be running code nobody agreed to.
func TestReadSlot_NoTrailer(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "hetzner", nil, "hetzner")

	_, err := ReadSlot(root, "hetzner")
	if err == nil {
		t.Fatal("ReadSlot on an unstamped artifact must fail")
	}
	if !errors.Is(err, schema.ErrNoTrailer) {
		t.Errorf("err = %v, want it to wrap schema.ErrNoTrailer", err)
	}
}

// TestReadSlot_NoTrailerIgnoresSiblingSchemaFile — the fallback that must not exist. A
// published `schema.json` next to an unstamped artifact is exactly the file an attacker
// could drop, and reading it would approve a disclosure the bytes never carried.
func TestReadSlot_NoTrailerIgnoresSiblingSchemaFile(t *testing.T) {
	root := t.TempDir()
	dir := writeSlot(t, root, "hetzner", nil, "hetzner")
	payload, err := schema.Marshal(sshProviderDoc())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, schema.SchemaFileName), payload, 0o644); err != nil {
		t.Fatalf("write sibling schema.json: %v", err)
	}

	if _, err := ReadSlot(root, "hetzner"); err == nil {
		t.Fatal("ReadSlot must not fall back to a sibling schema.json")
	}
}

// TestReadSlot_CorruptTrailer — a truncated or tampered trailer fails closed for the
// same reason a missing one does.
func TestReadSlot_CorruptTrailer(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	dir := writeSlot(t, root, "hetzner", &doc, "hetzner")

	path := filepath.Join(dir, "hetzner")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	// Chop the last byte: the magic footer no longer matches its payload.
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o755); err != nil {
		t.Fatalf("truncate artifact: %v", err)
	}

	if _, err := ReadSlot(root, "hetzner"); err == nil {
		t.Fatal("ReadSlot on a corrupt trailer must fail")
	}
}

// TestReadSlot_InvalidDocument — a trailer whose payload parses but does not validate
// is refused too: an invalid disclosure is not a disclosure.
func TestReadSlot_InvalidDocument(t *testing.T) {
	root := t.TempDir()
	// kind=soul_module with no modules: the validator rejects it.
	bad := schema.Document{Kind: schema.KindSoulModule, ProtocolVersion: 1}
	writeSlot(t, root, "redis", &bad, "redis")

	_, err := ReadSlot(root, "redis")
	if err == nil {
		t.Fatal("ReadSlot with an invalid document must fail")
	}
	if errors.Is(err, ErrSlotNotFound) {
		t.Errorf("an invalid document should not read as a missing slot: %v", err)
	}
}

// TestReadSlot_RefIgnored is variant C: the lookup key is the alias alone, so the same
// slot yields the same artifact regardless of the ref a grant labels it with.
func TestReadSlot_RefIgnored(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "hetzner", &doc, "hetzner")

	a, err := ReadSlot(root, "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot #1: %v", err)
	}
	b, err := ReadSlot(root, "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot #2: %v", err)
	}
	if a.Artifacts[0].SHA256 != b.Artifacts[0].SHA256 {
		t.Errorf("single-slot must give a stable digest: %q != %q",
			a.Artifacts[0].SHA256, b.Artifacts[0].SHA256)
	}
}

// TestSlotCommitSHA_Success — `current` points at the `<commit_sha>` directory and
// SlotCommitSHA returns that name (A1-S4: the commit_sha written to plugin_sigils on
// allow).
func TestSlotCommitSHA_Success(t *testing.T) {
	root := t.TempDir()
	doc := sshProviderDoc()
	writeSlot(t, root, "hetzner", &doc, "hetzner")

	got, err := SlotCommitSHA(root, "hetzner")
	if err != nil {
		t.Fatalf("SlotCommitSHA: %v", err)
	}
	if got != commitFixtureSHA {
		t.Errorf("commit_sha = %q, want %q", got, commitFixtureSHA)
	}
}

func TestSlotCommitSHA_NoSlot(t *testing.T) {
	root := t.TempDir()
	_, err := SlotCommitSHA(root, "absent")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_NoCurrent — the `<alias>/` directory exists but `current` does not,
// so the commit_sha cannot be extracted. Fail-closed: Allow must refuse rather than
// approve with unpinned provenance.
func TestSlotCommitSHA_NoCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "hetzner", "somedir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := SlotCommitSHA(root, "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("slot without current: err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_CurrentNotSymlink — `current` exists but is a plain directory (the
// R-nested invariant is broken). Fail-closed → ErrSlotNotFound.
func TestSlotCommitSHA_CurrentNotSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "hetzner", CurrentLink), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := SlotCommitSHA(root, "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("current is not a symlink: err = %v, want ErrSlotNotFound", err)
	}
}

// TestSingleArtifactIn_Arithmetic pins the rule directly: exactly one executable, and a
// non-executable sibling (the published schema.json) does not make a directory
// ambiguous.
func TestSingleArtifactIn_Arithmetic(t *testing.T) {
	t.Run("one executable plus schema.json", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "redis"), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, schema.SchemaFileName), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := SingleArtifactIn(dir)
		if err != nil {
			t.Fatalf("SingleArtifactIn: %v", err)
		}
		if filepath.Base(got) != "redis" {
			t.Errorf("got %q, want the executable", got)
		}
	})

	t.Run("zero executables", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, schema.SchemaFileName), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := SingleArtifactIn(dir); err == nil {
			t.Fatal("zero executables must fail closed")
		}
	})

	t.Run("two executables", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"a", "b"} {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := SingleArtifactIn(dir); err == nil {
			t.Fatal("two executables must fail closed, not pick one")
		}
	})
}
