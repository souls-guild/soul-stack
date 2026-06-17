package seed

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// readVersions возвращает отсортированный список номеров версий vN в каталоге.
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

// assertCurrent проверяет, что current — относительный симлинк на want.
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

// TestPathsIn: пути активной версии идут через dir/current/.
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

// TestLoad_PartialVersion: current указывает на версию, где ca.pem отсутствует
// (повреждённая версия) — Load даёт ErrIncomplete с упоминанием ca.pem.
func TestLoad_PartialVersion(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Удаляем ca.pem из активной версии.
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

// TestLoad_Mismatched: на диске cert от одной пары, key от другой — Load даёт
// ErrMismatched (отличается от ErrIncomplete).
func TestLoad_Mismatched(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Подменяем key.pem активной версии чужим (валидным самим по себе) ключом.
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

// TestLoad_UnreadableCert покрывает не-NotExist I/O-ошибку чтения члена версии.
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

// TestWrite_MismatchedFailFast: несогласованная пара cert↔key отвергается до
// любой записи на диск — vN+1 не создаётся.
func TestWrite_MismatchedFailFast(t *testing.T) {
	dir := t.TempDir()
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	err := Write(dir, &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca")})
	if !errors.Is(err, ErrMismatched) {
		t.Fatalf("Write несогласованной пары: %v; want ErrMismatched", err)
	}
	// Ни одной версии на диске.
	if v := readVersions(t, dir); len(v) != 0 {
		t.Fatalf("после fail-fast на диске есть версии %v; ждём пусто", v)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, currentLink)); statErr == nil {
		t.Fatal("current не должен существовать после fail-fast")
	}
}

// TestWrite_FailFastKeepsOldCurrent: ротация несогласованной парой не трогает
// существующую активную версию (crash-safety: сбой до swap оставляет старый
// current).
func TestWrite_FailFastKeepsOldCurrent(t *testing.T) {
	dir := t.TempDir()
	first := validMaterial(t, "ca1")
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// Ротация заведомо битой парой.
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	if err := Write(dir, &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca2")}); !errors.Is(err, ErrMismatched) {
		t.Fatalf("rotation битой парой: %v; want ErrMismatched", err)
	}
	// current по-прежнему v1, и Load отдаёт старый материал.
	assertCurrent(t, dir, "v1")
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load после неудачной ротации: %v", err)
	}
	if string(got.CAPEM) != "ca1" {
		t.Fatalf("после неудачной ротации Load вернул %q; ждём старый ca1", got.CAPEM)
	}
	// Битая версия v2 либо не создана, либо откатана — на диске только v1.
	if v := readVersions(t, dir); len(v) != 1 || v[0] != 1 {
		t.Fatalf("versions = %v; ждём [1] (битая ротация ничего не оставила)", v)
	}
}

// TestWrite_Modes: режимы файлов и каталогов.
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

// TestWrite_RelativeSymlink: current — относительный симлинк (`v1`, не абсолют).
func TestWrite_RelativeSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	assertCurrent(t, dir, "v1")
}

// TestWrite_MkdirFails: на пути dir лежит обычный файл — MkdirAll падает.
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

// TestWrite_RotationOverwritesKeyMode: новая версия пишет key.pem заново с
// 0o400 — старая версия с расслабленными правами не влияет на новую.
func TestWrite_RotationOverwritesKeyMode(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	// Расслабим права key.pem старой версии.
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

// TestWrite_DoesNotLeaveTempArtifacts: после Write в активной версии ровно три
// PEM-файла, в самом dir нет .tmp-хвостов (ни от atomicWrite, ни от swap).
func TestWrite_DoesNotLeaveTempArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// В dir: current + v1, без .tmp-.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("остался temp в seed-dir: %s", e.Name())
		}
	}
	// В версии — ровно три файла, без .tmp-.
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

// TestWrite_VersionMkdirFails: seed-dir read-only после первого Write —
// создание новой версии (mkdir verDir) падает, активная версия не тронута
// (crash-safety: сбой до swap оставляет старый current).
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
	// Crash-safety: вернём права и убедимся, что current всё ещё v1.
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

// TestWrite_SwapFailsKeepsVersion: запись новой версии прошла, но swap-rename
// падает (на месте current лежит непустой каталог) — материал v2 уже на диске,
// но активной остаётся прежняя версия. Покрывает error-путь swapCurrent.
func TestWrite_SwapFailsKeepsVersion(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, validMaterial(t, "ca1")); err != nil {
		t.Fatalf("seed Write v1: %v", err)
	}
	// Заменяем симлинк current непустым каталогом — rename поверх него упадёт.
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
	// v2 записана на диск (запись прошла до swap), но current не переключён.
	if v := readVersions(t, dir); len(v) != 2 || v[1] != 2 {
		t.Fatalf("versions = %v; ждём [1 2] (v2 записана, swap не дошёл)", v)
	}
}

// TestWrite_KeyPEMNotInErrorText: ни на одном провале Write секретный материал
// KeyPEM не попадает в текст ошибки (защита от утечки в логи). Проверяем на
// двух достижимых провалах:
//  1. рассинхрон пары (fail-fast X509, до записи) — KeyPEM в Material;
//  2. провал swap-а после успешной записи key (KeyPEM уже на диске).
func TestWrite_KeyPEMNotInErrorText(t *testing.T) {
	// (1) Несогласованная пара — провал на X509-валидации.
	cert, _ := keypair(t)
	_, otherKey := keypair(t)
	if err := Write(t.TempDir(), &Material{CertPEM: cert, KeyPEM: otherKey, CAPEM: []byte("ca")}); err == nil {
		t.Fatal("ждём ошибку рассинхрона пары")
	} else if strings.Contains(err.Error(), string(otherKey)) {
		t.Fatalf("приватный ключ утёк в текст ошибки X509-валидации: %v", err)
	}

	// (2) Провал swap-а: current занят непустым каталогом, key.pem уже записан.
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

// canStillRead сообщает, читается ли файл несмотря на права (root/особая FS).
func canStillRead(path string) bool {
	_, err := os.ReadFile(path)
	return err == nil
}

// canCreateVersion проверяет, можно ли создать подкаталог в dir несмотря на
// права (root/особая FS игнорируют write-bit).
func canCreateVersion(dir string) bool {
	probe := filepath.Join(dir, ".probe-version")
	if err := os.Mkdir(probe, 0o700); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}
