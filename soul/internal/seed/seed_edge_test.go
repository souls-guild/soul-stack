package seed

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readVersions returns the sorted list of vN version numbers in dir.
func readVersions(t *testing.T, dir string) []int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var out []int
	for _, e := range ents {
		if n, ok := parseVersion(e.Name()); ok {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// assertCurrent checks that current is a relative symlink to want.
func assertCurrent(t *testing.T, dir, want string) {
	t.Helper()
	link := filepath.Join(dir, currentLink)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if filepath.IsAbs(target) {
		t.Errorf("current -> %q; ждём относительный симлинк", target)
	}
	if target != want {
		t.Errorf("current -> %q; want %q", target, want)
	}
}

// TestPathsIn: active-version paths go through dir/current/.
func TestPathsIn(t *testing.T) {
	p := PathsIn("/var/lib/soul-stack/seed")
	if p.Cert != "/var/lib/soul-stack/seed/current/cert.pem" {
		t.Errorf("Cert = %q", p.Cert)
	}
	if p.Key != "/var/lib/soul-stack/seed/current/key.pem" {
		t.Errorf("Key = %q", p.Key)
	}
	if p.CA != "/var/lib/soul-stack/seed/current/ca.pem" {
		t.Errorf("CA = %q", p.CA)
	}
}

func TestLoad_EmptyDir(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("Load(\"\") должна вернуть ошибку")
	}
	if errors.Is(err, ErrIncomplete) {
		t.Fatalf("пустой dir — не ErrIncomplete, а ошибка конфига: %v", err)
	}
	if !strings.Contains(err.Error(), "paths.seed is empty") {
		t.Fatalf("ошибка = %v; ждём про пустой paths.seed", err)
	}
}

// TestLoad_PartialVersion: current points at a version missing ca.pem
// (corrupted version) — Load returns ErrIncomplete mentioning ca.pem.
func TestLoad_PartialVersion(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Remove ca.pem from the active version.
	if err := os.Remove(filepath.Join(dir, "v1", CAFile)); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Load с неполной версией: %v; want ErrIncomplete", err)
	}
	if !strings.Contains(err.Error(), CAFile) {
		t.Fatalf("ошибка должна называть ca.pem: %v", err)
	}
}

// TestLoad_Mismatched: cert on disk from one pair, key from another — Load
// returns ErrMismatched (distinct from ErrIncomplete).
func TestLoad_Mismatched(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Swap the active version's key.pem for an unrelated (individually valid) key.
	_, otherKey := keypair(t)
	keyPath := filepath.Join(dir, "v1", KeyFile)
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, otherKey, 0o400); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if !errors.Is(err, ErrMismatched) {
		t.Fatalf("Load с рассинхроном пары: %v; want ErrMismatched", err)
	}
	if errors.Is(err, ErrIncomplete) {
		t.Fatalf("рассинхрон — не ErrIncomplete: %v", err)
	}
}

// TestLoad_UnreadableCert covers a non-NotExist I/O error reading a version member.
func TestLoad_UnreadableCert(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cert := filepath.Join(dir, "v1", CertFile)
	if err := os.Chmod(cert, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cert, 0o644) })
	if canStillRead(cert) {
		t.Skip("FS/уровень привилегий игнорирует права на чтение (root?) — пропускаем")
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load с нечитаемым cert должна вернуть ошибку")
	}
	if errors.Is(err, ErrIncomplete) {
		t.Fatalf("нечитаемый существующий файл — не ErrIncomplete: %v", err)
	}
	if !strings.Contains(err.Error(), "read") || !strings.Contains(err.Error(), CertFile) {
		t.Fatalf("ждём обёрнутую read-ошибку cert: %v", err)
	}
}

func TestWrite_EmptyDir(t *testing.T) {
	err := Write("", validMaterial(t, "ca"))
	if err == nil {
		t.Fatal("Write(\"\") должна вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "paths.seed is empty") {
		t.Fatalf("ошибка = %v; ждём про пустой paths.seed", err)
	}
}

func TestWrite_NilMaterial(t *testing.T) {
	err := Write(t.TempDir(), nil)
	if err == nil {
		t.Fatal("Write(nil material) должна вернуть ошибку")
	}
	if !strings.Contains(err.Error(), "material is nil") {
		t.Fatalf("ошибка = %v; ждём про nil material", err)
	}
}

// TestWrite_MismatchedFailFast: a mismatched cert↔key pair is rejected before
// any disk write — vN+1 is never created.
func TestWrite_MismatchedFailFast(t *testing.T) {
	dir := t.TempDir()
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	err := Write(dir, &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca")})
	if !errors.Is(err, ErrMismatched) {
		t.Fatalf("Write несогласованной пары: %v; want ErrMismatched", err)
	}
	// No version on disk.
	if v := readVersions(t, dir); len(v) != 0 {
		t.Fatalf("после fail-fast на диске есть версии %v; ждём пусто", v)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, currentLink)); statErr == nil {
		t.Fatal("current не должен существовать после fail-fast")
	}
}

// TestWrite_FailFastKeepsOldCurrent: rotating with a mismatched pair doesn't
// touch the existing active version (crash-safety: a failure before swap
// leaves the old current in place).
func TestWrite_FailFastKeepsOldCurrent(t *testing.T) {
	dir := t.TempDir()
	first := validMaterial(t, "ca1")
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// Rotate with a deliberately broken pair.
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	if err := Write(dir, &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca2")}); !errors.Is(err, ErrMismatched) {
		t.Fatalf("rotation битой парой: %v; want ErrMismatched", err)
	}
	// current is still v1, and Load returns the old material.
	assertCurrent(t, dir, "v1")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load после неудачной ротации: %v", err)
	}
	if string(got.CAPEM) != "ca1" {
		t.Fatalf("после неудачной ротации Load вернул %q; ждём старый ca1", got.CAPEM)
	}
	// The broken v2 version is either never created or rolled back — only v1 on disk.
	if v := readVersions(t, dir); len(v) != 1 || v[0] != 1 {
		t.Fatalf("versions = %v; ждём [1] (битая ротация ничего не оставила)", v)
	}
}

// TestWrite_Modes: file and directory modes.
func TestWrite_Modes(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "seed")
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// dir 0o700.
	if st, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("seed dir mode = %o, want 0700", perm)
	}
	// v1 0o700.
	if st, err := os.Stat(filepath.Join(dir, "v1")); err != nil {
		t.Fatal(err)
	} else if perm := st.Mode().Perm(); perm != 0o700 {
		t.Errorf("v1 mode = %o, want 0700", perm)
	}
	// key.pem 0o400.
	if st, err := os.Stat(filepath.Join(dir, "v1", KeyFile)); err != nil {
		t.Fatal(err)
	} else if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("key.pem mode = %o, want 0400", perm)
	}
	// cert.pem / ca.pem 0o644.
	for _, f := range []string{CertFile, CAFile} {
		st, err := os.Stat(filepath.Join(dir, "v1", f))
		if err != nil {
			t.Fatal(err)
		}
		if perm := st.Mode().Perm(); perm != 0o644 {
			t.Errorf("%s mode = %o, want 0644", f, perm)
		}
	}
}

// TestWrite_RelativeSymlink: current is a relative symlink (`v1`, not absolute).
func TestWrite_RelativeSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertCurrent(t, dir, "v1")
}

// TestWrite_MkdirFails: a regular file sits on dir's path — MkdirAll fails.
func TestWrite_MkdirFails(t *testing.T) {
	base := t.TempDir()
	fileOnPath := filepath.Join(base, "occupied")
	if err := os.WriteFile(fileOnPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(fileOnPath, "seed")
	err := Write(dir, validMaterial(t, "ca"))
	if err == nil {
		t.Fatal("Write в недопустимый путь должна вернуть ошибку mkdir")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("ждём mkdir-ошибку: %v", err)
	}
}

// TestWrite_RotationOverwritesKeyMode: the new version writes key.pem fresh
// with 0o400 — the old version's relaxed permissions don't affect it.
func TestWrite_RotationOverwritesKeyMode(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// Relax permissions on the old version's key.pem.
	if err := os.Chmod(filepath.Join(dir, "v1", KeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, validMaterial(t, "ca2")); err != nil {
		t.Fatalf("rotation Write: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "v2", KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o400 {
		t.Errorf("после ротации v2 key.pem mode = %o, want 0400", perm)
	}
}

// TestWrite_DoesNotLeaveTempArtifacts: after Write the active version has
// exactly three PEM files, and dir has no leftover .tmp files (from
// atomicWrite or swap).
func TestWrite_DoesNotLeaveTempArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// dir: current + v1, no .tmp files.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("остался temp в seed-dir: %s", e.Name())
		}
	}
	// The version has exactly three files, no .tmp files.
	vents, err := os.ReadDir(filepath.Join(dir, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(vents))
	for _, e := range vents {
		names[e.Name()] = true
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("остался temp в версии: %s", e.Name())
		}
	}
	for _, want := range []string{CertFile, KeyFile, CAFile} {
		if !names[want] {
			t.Errorf("в версии отсутствует %s", want)
		}
	}
	if len(vents) != 3 {
		t.Errorf("в версии %d записей, ждём ровно 3", len(vents))
	}
}

// TestWrite_VersionMkdirFails: seed-dir is read-only after the first Write —
// creating a new version (mkdir verDir) fails, active version untouched
// (crash-safety: a failure before swap leaves the old current in place).
func TestWrite_VersionMkdirFails(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("seed Write v1: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if canCreateVersion(dir) {
		t.Skip("FS/уровень привилегий игнорирует права на запись (root?) — пропускаем")
	}
	err := Write(dir, validMaterial(t, "ca2"))
	if err == nil {
		t.Fatal("Write в read-only seed-dir должна упасть на mkdir версии")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("ждём mkdir-ошибку версии: %v", err)
	}
	// Crash-safety: restore permissions and confirm current is still v1.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertCurrent(t, dir, "v1")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load после неудачной записи: %v", err)
	}
	if string(got.CAPEM) != "ca1" {
		t.Fatalf("Load вернул %q; ждём старый ca1", got.CAPEM)
	}
}

// TestWrite_SwapFailsKeepsVersion: the new version is written but swap-rename
// fails (current is a non-empty directory) — v2's material is already on
// disk, but the previous version stays active. Covers swapCurrent's error path.
func TestWrite_SwapFailsKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("seed Write v1: %v", err)
	}
	// Replace the current symlink with a non-empty directory — rename over it will fail.
	link := filepath.Join(dir, currentLink)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "inside"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Write(dir, validMaterial(t, "ca2"))
	if err == nil {
		t.Fatal("Write со swap поверх непустого каталога должна упасть")
	}
	if !strings.Contains(err.Error(), "swap") {
		t.Fatalf("ждём swap-ошибку: %v", err)
	}
	// v2 is written to disk (write happened before swap), but current wasn't switched.
	if v := readVersions(t, dir); len(v) != 2 || v[1] != 2 {
		t.Fatalf("versions = %v; ждём [1 2] (v2 записана, swap не дошёл)", v)
	}
}

// TestWrite_KeyPEMNotInErrorText: on no Write failure does the secret KeyPEM
// material leak into the error text (log-leak protection). Covers two
// reachable failures:
//  1. mismatched pair (fail-fast X509, before write) — KeyPEM in Material;
//  2. swap failure after key is already written (KeyPEM already on disk).
func TestWrite_KeyPEMNotInErrorText(t *testing.T) {
	// (1) Mismatched pair — fails X509 validation.
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	if err := Write(t.TempDir(), &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca")}); err == nil {
		t.Fatal("ждём ошибку рассинхрона пары")
	} else if strings.Contains(err.Error(), string(otherKey)) {
		t.Fatalf("приватный ключ утёк в текст ошибки X509-валидации: %v", err)
	}

	// (2) Swap failure: current occupied by a non-empty directory, key.pem already written.
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("seed Write v1: %v", err)
	}
	link := filepath.Join(dir, currentLink)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "inside"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	certV, keyV := keypair(t)
	err := Write(dir, &Material{CertPEM: certV, KeyPEM: keyV, CAPEM: []byte("ca2")})
	if err == nil {
		t.Fatal("ждём ошибку swap-а")
	}
	if strings.Contains(err.Error(), string(keyV)) {
		t.Fatalf("приватный ключ утёк в текст ошибки swap-а: %v", err)
	}
}

// canStillRead reports whether the file is readable despite permissions (root/special FS).
func canStillRead(path string) bool {
	_, err := os.ReadFile(path)
	return err == nil
}

// canCreateVersion checks whether a subdirectory can be created in dir despite
// permissions (root/special FS ignore the write bit).
func canCreateVersion(dir string) bool {
	probe := filepath.Join(dir, ".probe-version")
	if err := os.Mkdir(probe, 0o700); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}
