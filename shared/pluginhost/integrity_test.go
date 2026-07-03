package pluginhost

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writePluginBin кладёт фейковый бинарь плагина в dir и возвращает его путь.
func writePluginBin(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "soul-mod-x")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}
	return p
}

func TestComputeFileDigestStable(t *testing.T) {
	dir := t.TempDir()
	bin := writePluginBin(t, dir, "payload")

	d1, err := computeFileDigest(bin)
	if err != nil {
		t.Fatalf("computeFileDigest: %v", err)
	}
	d2, err := computeFileDigest(bin)
	if err != nil {
		t.Fatalf("computeFileDigest (repeat): %v", err)
	}
	if d1 != d2 {
		t.Errorf("digest not stable: %q != %q", d1, d2)
	}
	// SHA-256("payload") — фиксированный известный вектор.
	const want = "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5"
	if d1 != want {
		t.Errorf("digest = %q, want %q", d1, want)
	}
}

func TestComputeFileDigestMissing(t *testing.T) {
	_, err := computeFileDigest(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// Re-exec sidecar (defense-in-depth, ADR-026 S6b): sidecar пишется после
// Sigil-verify и сверяется на повторных exec из кеша. Тесты ниже целят
// именно sidecar-уровень (sealDigest/verifyDigest); fail-closed Sigil-verify
// первой загрузки покрыт в sigil_verify_test.go.

func TestSealDigestWritesReadOnlySidecar(t *testing.T) {
	dir := t.TempDir()
	bin := writePluginBin(t, dir, "payload")

	sidecar := filepath.Join(dir, DigestSidecarName)
	if err := sealDigest(bin, sidecar); err != nil {
		t.Fatalf("sealDigest: %v", err)
	}

	content, err := os.ReadFile(sidecar)
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	want, _ := computeFileDigest(bin)
	if string(content) != want {
		t.Errorf("sidecar = %q, want %q", content, want)
	}

	st, err := os.Stat(sidecar)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if perm := st.Mode().Perm(); perm != digestSidecarMode {
		t.Errorf("sidecar mode = %o, want %o", perm, digestSidecarMode)
	}
}

func TestVerifyDigestValidBinaryPasses(t *testing.T) {
	dir := t.TempDir()
	bin := writePluginBin(t, dir, "payload")
	want, _ := computeFileDigest(bin)

	if err := verifyDigest(bin, want); err != nil {
		t.Fatalf("verifyDigest valid: %v", err)
	}
}

func TestVerifyDigestTamperedBinaryMismatch(t *testing.T) {
	dir := t.TempDir()
	bin := writePluginBin(t, dir, "original")
	sealed, _ := computeFileDigest(bin)

	// Подмена бинаря после seal.
	if err := os.WriteFile(bin, []byte("malicious"), 0o755); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err := verifyDigest(bin, sealed)
	if !errors.Is(err, ErrPluginDigestMismatch) {
		t.Fatalf("expected ErrPluginDigestMismatch, got %v", err)
	}
}

func TestVerifyDigestWhitespaceTolerant(t *testing.T) {
	dir := t.TempDir()
	bin := writePluginBin(t, dir, "payload")
	digest, _ := computeFileDigest(bin)

	// Sidecar с окружающими пробелами/trailing-newline (как мог бы записать
	// оператор вручную) — verify должен их игнорировать.
	if err := verifyDigest(bin, "  "+digest+"\n"); err != nil {
		t.Errorf("verify with whitespace sidecar: %v", err)
	}
}
