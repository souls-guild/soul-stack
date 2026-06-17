package pluginhost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// validCloudManifest — минимально-валидный manifest cloud_driver-плагина для
// фикстуры слота.
const validCloudManifest = `kind: cloud_driver
protocol_version: 1
namespace: cloud
name: hetzner
spec:
  profile_schema:
    type: object
`

// writeSlot создаёт R-nested-слот (A1-S1): <root>/<ns>-<name>/<commit>/ с
// manifest.yaml и бинарём по конвенции BinaryName + current → <commit>.
// commit — синтетический 40-hex для фикстуры (тест чтения, не git-резолва).
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
		t.Fatalf("ReadSlot отсутствующего слота: err = %v, want ErrSlotNotFound", err)
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
		t.Fatalf("ReadSlot без manifest: err = %v, want ErrSlotNotFound", err)
	}
}

func TestReadSlot_BinaryMissing(t *testing.T) {
	root := t.TempDir()
	// manifest есть, бинаря нет.
	writeSlot(t, root, "cloud", "hetzner", "", []byte(validCloudManifest), nil)
	_, err := ReadSlot(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("ReadSlot без бинаря: err = %v, want ErrSlotNotFound", err)
	}
}

func TestReadSlot_InvalidManifest(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "cloud", "hetzner", "soul-cloud-hetzner",
		[]byte("kind: cloud_driver\nprotocol_version: 1\n"), // нет namespace/name/spec
		[]byte("bin"))
	_, err := ReadSlot(root, "cloud", "hetzner")
	if err == nil {
		t.Fatal("ReadSlot с невалидным manifest должен вернуть ошибку")
	}
	if errors.Is(err, ErrSlotNotFound) {
		t.Errorf("невалидный manifest не должен маппиться в ErrSlotNotFound: %v", err)
	}
}

// TestReadSlot_RefIgnored — вариант C: чтение по (ns, name) без ref; один и тот
// же слот возвращает тот же бинарь независимо от того, под какой меткой ref он
// будет допущен (ref в путь кеша не входит).
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
		t.Errorf("single-slot должен давать стабильный digest: %q != %q", a.BinarySHA256, b.BinarySHA256)
	}
}

// commitFixtureSHA — synthetic 40-hex commit, которым writeSlot именует слот и
// нацеливает current (тест чтения target-а, не git-резолва).
const commitFixtureSHA = "0123456789abcdef0123456789abcdef01234567"

// TestSlotCommitSHA_Success — current-symlink указывает на <commit_sha>-каталог;
// SlotCommitSHA возвращает имя этого каталога (A1-S4: источник commit_sha для
// plugin_sigils при allow).
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

// TestSlotCommitSHA_NoSlot — нет каталога <ns>-<name>/ → ErrSlotNotFound.
func TestSlotCommitSHA_NoSlot(t *testing.T) {
	root := t.TempDir()
	_, err := SlotCommitSHA(root, "cloud", "absent")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_LegacyNoCurrent — legacy-слот: каталог <ns>-<name>/ есть,
// но symlink current отсутствует (commit_sha надёжно не извлечь). fail-closed →
// ErrSlotNotFound (Allow обязан отклонить такой допуск).
func TestSlotCommitSHA_LegacyNoCurrent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cloud-hetzner", "somedir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_, err := SlotCommitSHA(root, "cloud", "hetzner")
	if !errors.Is(err, ErrSlotNotFound) {
		t.Fatalf("legacy-слот без current: err = %v, want ErrSlotNotFound", err)
	}
}

// TestSlotCommitSHA_CurrentNotSymlink — current существует, но это обычный
// каталог, а не symlink (R-nested-инвариант нарушен). fail-closed →
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
