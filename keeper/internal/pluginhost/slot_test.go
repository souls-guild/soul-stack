package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// validCloudManifest is minimally-valid manifest for cloud_driver plugin
// in slot fixture.
const validCloudManifest = `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: hetzner
spec:
  profile_schema:
    type: object
`

// writeSlot creates R-nested slot (A1-S1): <root>/<ns>-<name>/<commit>/ with
// manifest.yaml and binary by BinaryName convention + current → <commit>.
// commit is synthetic 40-hex for fixture (read test, not git-resolve).
func writeSlot(t *testing.T, root, ns, name, binaryName string, manifest, binary []byte) {
	t.Helper()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	pluginDir := filepath.Join(root, ns+"-"+name)
	dir := filepath.Join(pluginDir, commit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir slot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), manifest, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if binaryName != "" {
		if err := os.WriteFile(filepath.Join(dir, binaryName), binary, 0o755); err != nil {
			t.Fatalf("write binary: %v", err)
		}
	}
	if err := os.Symlink(commit, filepath.Join(pluginDir, CurrentLink)); err != nil {
		t.Fatalf("symlink current: %v", err)
	}
}

func TestReadSlot_Success(t *testing.T) {
	root := t.TempDir()
	binary := []byte("fake-cloud-binary-bytes")
	writeSlot(t, root, "cloud", "hetzner", "soul-cloud-hetzner", []byte(validCloudManifest), binary)

	got, err := ReadSlot(root, "cloud", "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot: %v", err)
	}

	wantDigest := sha256.Sum256(binary)
	if got.BinarySHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("BinarySHA256 = %q, want %q", got.BinarySHA256, hex.EncodeToString(wantDigest[:]))
	}
	if string(got.ManifestBytes) != validCloudManifest {
		t.Errorf("ManifestBytes отличается от записанных сырых байтов manifest.yaml")
	}
	if filepath.Base(got.BinaryPath) != "soul-cloud-hetzner" {
		t.Errorf("BinaryPath = %q, want suffix soul-cloud-hetzner", got.BinaryPath)
	}
}

func TestReadSlot_NoSlot(t *testing.T) {
	root := t.TempDir()
	_, err := ReadSlot(root, "cloud", "absent")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot absent slot: err = %v, want ErrSlotNotFound", err)
	}
}

func TestReadSlot_NoManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cloud-hetzner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := ReadSlot(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot without manifest: err = %v, want ErrSlotNotFound", err)
	}
}

func TestReadSlot_BinaryMissing(t *testing.T) {
	root := t.TempDir()
	// manifest present, binary missing.
	writeSlot(t, root, "cloud", "hetzner", "", []byte(validCloudManifest), nil)
	_, err := ReadSlot(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot without binary: err = %v, want ErrSlotNotFound", err)
	}
}

func TestReadSlot_InvalidManifest(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "cloud", "hetzner", "soul-cloud-hetzner",
		[]byte("kind: cloud_driver\nprotocol_version: 1\n"), // missing namespace/name/spec
		[]byte("bin"))
	_, err := ReadSlot(root, "cloud", "hetzner")
	if err == nil {
		t.Fatal("ReadSlot with invalid manifest should return error")
	}
	if errors.Is(err, ErrSlotNotFound) {
		t.Errorf("invalid manifest should not map to ErrSlotNotFound: %v", err)
	}
}

// TestReadSlot_RefIgnored is variant C: read by (ns, name) without ref; same
// slot returns same binary regardless of which ref label it was
// allowed under (ref not in cache path).
func TestReadSlot_RefIgnored(t *testing.T) {
	root := t.TempDir()
	binary := []byte("single-slot-binary")
	writeSlot(t, root, "cloud", "hetzner", "soul-cloud-hetzner", []byte(validCloudManifest), binary)

	a, err := ReadSlot(root, "cloud", "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot #1: %v", err)
	}
	b, err := ReadSlot(root, "cloud", "hetzner")
	if err != nil {
		t.Fatalf("ReadSlot #2: %v", err)
	}
	if a.BinarySHA256 != b.BinarySHA256 {
		t.Errorf("single-slot must give stable digest: %q != %q", a.BinarySHA256, b.BinarySHA256)
	}
}

// commitFixtureSHA is synthetic 40-hex commit that writeSlot uses to name slot
// and target current (test symlink target reading, not git-resolve).
const commitFixtureSHA = "0123456789abcdef0123456789abcdef01234567"

// TestSlotCommitSHA_Success verifies current-symlink points to <commit_sha> directory;
// SlotCommitSHA returns that directory name (A1-S4: source of commit_sha for
// plugin_sigils on allow).
func TestSlotCommitSHA_Success(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "cloud", "hetzner", "soul-cloud-hetzner", []byte(validCloudManifest), []byte("bin"))

	got, err := SlotCommitSHA(root, "cloud", "hetzner")
	if err != nil {
		t.Fatalf("SlotCommitSHA: %v", err)
	}
	if got != commitFixtureSHA {
		t.Errorf("commit_sha = %q, want %q", got, commitFixtureSHA)
	}
}

// TestSlotCommitSHA_NoSlot verifies missing <ns>-<name>/ directory → ErrSlotNotFound.
func TestSlotCommitSHA_NoSlot(t *testing.T) {
	root := t.TempDir()
	_, err := SlotCommitSHA(root, "cloud", "absent")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_LegacyNoCurrent verifies legacy slot: <ns>-<name>/ directory exists
// but current symlink missing (commit_sha cannot be reliably extracted). fail-closed →
// ErrSlotNotFound (Allow must reject such permission).
func TestSlotCommitSHA_LegacyNoCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cloud-hetzner", "somedir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := SlotCommitSHA(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("legacy slot without current: err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_CurrentNotSymlink verifies current exists but is regular
// directory not symlink (R-nested invariant broken). fail-closed →
// ErrSlotNotFound.
func TestSlotCommitSHA_CurrentNotSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cloud-hetzner", CurrentLink), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := SlotCommitSHA(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("current не symlink: err = %v, want ErrSlotNotFound", err)
	}
}
