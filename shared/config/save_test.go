package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Round-trip: Load → SaveToBytes без мутаций → байт в байт совпадает с
// исходником. Это базовая гарантия preserve comments/order/anchors:
// для немутированного Document Save отдаёт `doc.source` напрямую.
func TestSaveKeeper_GoldenRoundTrip(t *testing.T) {
	path := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, doc, diags, err := LoadKeeperFromBytes(path, src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io err: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("load returned errors")
	}
	out, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected 0 warnings on unmutated round-trip, got %d", len(warns))
	}
	if string(out) != string(src) {
		t.Fatalf("round-trip not byte-identical\nlen src=%d out=%d", len(src), len(out))
	}
}

func TestSaveSoul_GoldenRoundTrip(t *testing.T) {
	path := filepath.FromSlash("../../examples/soul/soul.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	_, doc, diags, err := LoadSoulFromBytes(path, src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io err: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("load returned errors")
	}
	out, warns, err := SaveSoulToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected 0 warnings on unmutated round-trip, got %d", len(warns))
	}
	if string(out) != string(src) {
		t.Fatalf("round-trip not byte-identical for soul")
	}
}

// SaveKeeper(path, doc) пишет в файл, файл существует и совпадает с in-memory
// рендером. На немутированном документе — тоже byte-identical.
func TestSaveKeeper_WritesToFile(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	diags, err := SaveKeeper(dst, doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(diags) != 0 {
		dump(t, diags)
		t.Fatalf("unexpected diagnostics")
	}
	out, _ := os.ReadFile(dst)
	if string(out) != string(src) {
		t.Fatalf("on-disk write not byte-identical")
	}
}

// Permission-preservation: исходный 0640 → результат 0640 (mode унаследован
// через Chmod tmp до Write).
func TestSaveKeeper_PreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveKeeper(dst, doc); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("perm not preserved: got %v, want 0640", info.Mode().Perm())
	}
}

// Symlink reject: keeper.yml — symlink, Save отвергает с error и diagnostic
// `symlink_write_not_supported`. Файл-цель не модифицируется.
func TestSaveKeeper_SymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	target := filepath.Join(dir, "real.yml")
	if err := os.WriteFile(target, []byte("kid: real-keeper\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keeper.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	diags, err := SaveKeeper(link, doc)
	if err == nil {
		t.Fatalf("expected error for symlink write, got nil")
	}
	found := false
	for _, d := range diags {
		if d.Code == "symlink_write_not_supported" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected symlink_write_not_supported diagnostic")
	}
	// Target untouched.
	tgtAfter, _ := os.ReadFile(target)
	if string(tgtAfter) != "kid: real-keeper\n" {
		t.Fatalf("symlink target was modified")
	}
}

// Atomic-rename fault injection: tmp успешно создан, но rename упасть не
// можем эмулировать встроенными средствами без приватного API. Минимально
// проверяем инвариант: если до Save исходник существовал, после ошибки
// Save он сохранён (это покрывается symlink-case и любой ошибкой stage
// после CreateTemp). Тут — case с непишущей директорией.
func TestSaveKeeper_FailedWriteDoesNotCorruptSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	dst := filepath.Join(dir, "keeper.yml")
	if err := os.WriteFile(dst, src, 0o640); err != nil {
		t.Fatal(err)
	}
	// Делаем директорию read-only: CreateTemp упадёт.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := SaveKeeper(dst, doc)
	if err == nil {
		t.Fatalf("expected write error in read-only dir")
	}
	// Source должен быть на месте, нетронут.
	after, statErr := os.ReadFile(dst)
	if statErr != nil {
		t.Fatalf("source disappeared after failed save: %v", statErr)
	}
	if string(after) != string(src) {
		t.Fatalf("source was modified after failed save")
	}
}

// PatchKeeper меняет scalar и при последующем Save новый файл содержит новое
// значение. Mutated-флаг переключает на AST-рендер.
func TestPatchKeeper_ScalarReplace(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-new-name"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(string(out), "kid: keeper-new-name") {
		t.Fatalf("patched value not present in output\n%s", out)
	}
	if strings.Contains(string(out), "kid: keeper-eu-west-01") {
		t.Fatalf("old value still present in output")
	}
}

// Inline-comment рядом со scalar-узлом, который стал target Patch-а,
// сохраняется через snapshot+restore (см. PatchKeeper doc-comment).
func TestPatchKeeper_PreservesInlineComment(t *testing.T) {
	src := []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:
      addr: "0.0.0.0:9442"  # MVP listener
      tls:
        cert: /c
        key: /k
    event_stream:
      addr: "0.0.0.0:8443"
      tls:
        cert: /c
        key: /k
        ca: /a
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, doc, _, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.listen.grpc.bootstrap.addr", "0.0.0.0:5443"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// Goccy normalises multi-space to single space before `#`, проверяем
	// одиночный-пробел-вариант:
	if !strings.Contains(string(out), "MVP listener") {
		t.Fatalf("inline comment lost\n%s", out)
	}
	if !strings.Contains(string(out), "0.0.0.0:5443") {
		t.Fatalf("patched value missing\n%s", out)
	}
}

// Несуществующий path → ErrPathNotFound (без молчаливого create-on-write).
func TestPatchKeeper_PathNotFound(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	err := PatchKeeper(doc, "$.no.such.path", "x")
	if err == nil {
		t.Fatalf("expected ErrPathNotFound, got nil")
	}
	if !errors.Is(err, ErrPathNotFound) {
		t.Fatalf("expected ErrPathNotFound, got %v", err)
	}
}

// Patch + Save → round_trip_warning (флаг mutated, отличия от исходника
// почти неизбежны).
func TestPatchKeeper_EmitsRoundTripWarning(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-new"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	_, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	found := false
	for _, d := range warns {
		if d.Code == "round_trip_warning" {
			found = true
		}
	}
	if !found {
		dump(t, warns)
		t.Fatalf("expected round_trip_warning after mutating patch")
	}
}

// Save-диагностики (round_trip_warning / symlink_write_not_supported /
// atomic_rename_failed) генерируются на фазе записи, поэтому Phase должна
// быть PhaseWriteBack, а не PhaseParse.
func TestSaveDiagnostics_PhaseWriteBack(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	// round_trip_warning — через Patch + SaveToBytes.
	if err := PatchKeeper(doc, "$.kid", "keeper-new"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	_, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	foundRT := false
	for _, d := range warns {
		if d.Code == "round_trip_warning" {
			foundRT = true
			if d.Phase != diag.PhaseWriteBack {
				t.Fatalf("round_trip_warning: expected PhaseWriteBack, got %q", d.Phase)
			}
		}
	}
	if !foundRT {
		t.Fatalf("expected round_trip_warning diagnostic after mutation, got: %+v", warns)
	}

	// symlink_write_not_supported — через SaveKeeper на symlink.
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		target := filepath.Join(dir, "real.yml")
		if err := os.WriteFile(target, []byte("kid: real\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "keeper.yml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		diags, _ := SaveKeeper(link, doc)
		foundSym := false
		for _, d := range diags {
			if d.Code == "symlink_write_not_supported" {
				foundSym = true
				if d.Phase != diag.PhaseWriteBack {
					t.Fatalf("symlink_write_not_supported: expected PhaseWriteBack, got %q", d.Phase)
				}
			}
		}
		if !foundSym {
			t.Fatalf("expected symlink_write_not_supported diagnostic for symlink, got: %+v", diags)
		}
	}
}

// PatchSoul-вариант: проверка симметрии API на soul.yml.
func TestPatchSoul_ScalarReplace(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/soul/soul.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadSoulFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchSoul(doc, "$.keeper.endpoints[0].host", "k9.dc1.example"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveSoulToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(string(out), "k9.dc1.example") {
		t.Fatalf("patched value missing in soul.yml\n%s", out)
	}
}

// Nil-Document → ошибка, не panic.
func TestSave_NilDocument(t *testing.T) {
	if _, _, err := SaveKeeperToBytes(nil); err == nil {
		t.Fatalf("expected error for nil doc")
	}
	if err := PatchKeeper(nil, "$.kid", "x"); err == nil {
		t.Fatalf("expected error for nil doc")
	}
}

// CG1 — пустой / whitespace-only / без `$`-префикса yaml-path должен
// отвергаться явным error-ом ДО вызова goccy `PathString`+`FilterFile`,
// иначе goccy улетает в SIGSEGV (`path.go:491`). Regression-тест к bug,
// найденному qa.1.
func TestPatchKeeper_InvalidPathRejected(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	cases := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"no_dollar_prefix", "kid"},
		{"dotted_no_dollar", "listen.grpc.bootstrap.addr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := PatchKeeper(doc, tc.path, "x")
			if err == nil {
				t.Fatalf("expected error for invalid path %q, got nil", tc.path)
			}
		})
	}
}

// CG2 — non-scalar target (целый mapping/sequence) reject-ается с error,
// содержащим path и тип ноды. Regression-тест к bug, найденному qa.1.
// Дополнительно: после неудачного Patch документ не помечен mutated,
// SaveToBytes отдаёт byte-identical с source.
func TestPatchKeeper_RejectNonScalarTarget(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	// $.listen — целый mapping (grpc/openapi/mcp/metrics), не scalar.
	err := PatchKeeper(doc, "$.listen", map[string]any{"grpc": "x"})
	if err == nil {
		t.Fatalf("expected reject for non-scalar target, got nil")
	}
	if !strings.Contains(err.Error(), "non-scalar") {
		t.Fatalf("expected 'non-scalar' in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "$.listen") {
		t.Fatalf("expected path '$.listen' in error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "non_scalar_patch_target") {
		t.Fatalf("expected error code 'non_scalar_patch_target' in error message, got: %v", err)
	}

	// Sequence-target — $.services тоже не scalar.
	err = PatchSoul_AsKeeper_sequenceCheck(t, doc)
	_ = err
	// (sanity: реальный sequence-кейс — на soul.yml через PatchSoul ниже)

	// После failed Patch — byte-identical Save.
	out, warns, saveErr := SaveKeeperToBytes(doc)
	if saveErr != nil {
		t.Fatalf("save after failed patch: %v", saveErr)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings after failed patch (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed patch; expected byte-identical save")
	}
}

// Хелпер: на soul.yml есть sequence `$.keeper.endpoints`. Проверяем reject
// non-scalar для sequence-target отдельно.
func PatchSoul_AsKeeper_sequenceCheck(t *testing.T, _ *Document) error {
	t.Helper()
	srcPath := filepath.FromSlash("../../examples/soul/soul.yml")
	src, _ := os.ReadFile(srcPath)
	_, soulDoc, _, _ := LoadSoulFromBytes(srcPath, src, ValidateOptions{})

	err := PatchSoul(soulDoc, "$.keeper.endpoints", []string{"x"})
	if err == nil {
		t.Fatalf("expected reject for non-scalar (sequence) target")
	}
	if !strings.Contains(err.Error(), "non-scalar") {
		t.Fatalf("expected 'non-scalar' in error, got: %v", err)
	}
	return err
}

// CG3 — конкурентные Save на разные файлы из одного doc — race detector чист,
// файлы байт-идентичны исходнику (немутированный doc → source).
func TestSave_ConcurrentDifferentFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	const N = 4
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := filepath.Join(dir, "keeper-"+itoa(i)+".yml")
			_, err := SaveKeeper(dst, doc)
			errs[i] = err
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("save #%d: %v", i, err)
		}
	}
	for i := 0; i < N; i++ {
		dst := filepath.Join(dir, "keeper-"+itoa(i)+".yml")
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
		if string(got) != string(src) {
			t.Fatalf("file #%d not byte-identical to source", i)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// CG4 — после failed Patch (invalid path) документ не помечен mutated и
// SaveToBytes отдаёт byte-identical с source.
func TestPatchKeeper_FailedPatchPreservesByteIdentity(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.no.such.path", "x"); err == nil {
		t.Fatalf("expected ErrPathNotFound")
	}
	if err := PatchKeeper(doc, "", "x"); err == nil {
		t.Fatalf("expected empty-path error")
	}
	if err := PatchKeeper(doc, "$.listen", map[string]any{"x": 1}); err == nil {
		t.Fatalf("expected non-scalar reject")
	}

	out, warns, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed patches; expected byte-identical save")
	}
}

// CG5 — Load → Patch → SaveToBytes → LoadFromBytes повторно без parse errors.
// Гарантирует, что AST-рендер после мутации остаётся валидным YAML
// в смысле full Load-pipeline (parse + schema + semantic).
func TestPatchKeeper_RoundTripReloadable(t *testing.T) {
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	if err := PatchKeeper(doc, "$.kid", "keeper-reloaded"); err != nil {
		t.Fatalf("patch: %v", err)
	}
	out, _, err := SaveKeeperToBytes(doc)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, _, diags, err := LoadKeeperFromBytes(srcPath, out, ValidateOptions{})
	if err != nil {
		t.Fatalf("re-load io: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("re-load returned errors")
	}
	if cfg == nil || cfg.KID != "keeper-reloaded" {
		var got string
		if cfg != nil {
			got = cfg.KID
		}
		t.Fatalf("expected KID 'keeper-reloaded' after reload, got %q", got)
	}
}

// CG6 — Patch на документ с anchor/alias. Фиксируем реальное поведение
// goccy: anchor — это сам "mapping value" узел (block-anchor над mapping),
// resolve через `$.listen.grpc.addr` упирается в anchor-узел и
// возвращает ошибку «expected node type is map or map value. but got
// Anchor». Это не наша регрессия, а свойство goccy/go-yaml v1.19 —
// фиксируем явно, чтобы regression-тест поймал, если поведение изменится.
//
// Что важно проверить с нашей стороны: ошибка обработана через
// fmt.Errorf, без SIGSEGV и без mutated=true.
func TestPatchKeeper_AnchorPathHandledCleanly(t *testing.T) {
	src := []byte(`kid: keeper-anchor-fixture
listen:
  grpc:
    bootstrap: &g
      addr: "0.0.0.0:9442"
      tls:
        cert: /c
        key: /k
    event_stream:
      addr: "0.0.0.0:8443"
      tls:
        cert: /c
        key: /k
        ca: /a
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, doc, _, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})

	err := PatchKeeper(doc, "$.listen.grpc.bootstrap.addr", "0.0.0.0:5443")
	if err == nil {
		t.Fatalf("expected resolve error for path under anchor (current goccy v1.19 behaviour)")
	}
	// После failed Patch документ не помечен mutated → byte-identical Save.
	out, warns, saveErr := SaveKeeperToBytes(doc)
	if saveErr != nil {
		t.Fatalf("save after failed anchor patch: %v", saveErr)
	}
	if len(warns) != 0 {
		dump(t, warns)
		t.Fatalf("expected no warnings (doc not mutated)")
	}
	if string(out) != string(src) {
		t.Fatalf("doc was modified by failed anchor patch")
	}
}

// CG7 — permissions 0o600 / 0o400 preserved. Расширение существующего
// TestSaveKeeper_PreservesPermissions на более узкие mode.
func TestSaveKeeper_PreservesPermissions_TighterModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)

	cases := []os.FileMode{0o600, 0o400}
	for _, mode := range cases {
		mode := mode
		t.Run(mode.String(), func(t *testing.T) {
			_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})
			dir := t.TempDir()
			dst := filepath.Join(dir, "keeper.yml")
			if err := os.WriteFile(dst, src, mode); err != nil {
				t.Fatal(err)
			}
			// 0o400 read-only: writeFileAtomically делает Chmod tmp,
			// потом Rename. Rename работает по правам директории, не файла.
			if _, err := SaveKeeper(dst, doc); err != nil {
				t.Fatalf("save with mode %v: %v", mode, err)
			}
			info, err := os.Stat(dst)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if info.Mode().Perm() != mode {
				t.Fatalf("perm not preserved: got %v, want %v", info.Mode().Perm(), mode)
			}
		})
	}
}

// CG8 — dangling symlink (target не существует): Save должен reject-ить
// с error и diagnostic `symlink_write_not_supported`. Lstat обнаружит
// симлинк, не следуя ему.
func TestSaveKeeper_DanglingSymlinkRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	srcPath := filepath.FromSlash("../../examples/keeper/keeper.yml")
	src, _ := os.ReadFile(srcPath)
	_, doc, _, _ := LoadKeeperFromBytes(srcPath, src, ValidateOptions{})

	dir := t.TempDir()
	link := filepath.Join(dir, "keeper.yml")
	// Цель симлинка намеренно не существует.
	if err := os.Symlink(filepath.Join(dir, "nonexistent.yml"), link); err != nil {
		t.Fatal(err)
	}

	diags, err := SaveKeeper(link, doc)
	if err == nil {
		t.Fatalf("expected error for dangling symlink, got nil")
	}
	found := false
	for _, d := range diags {
		if d.Code == "symlink_write_not_supported" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected symlink_write_not_supported diagnostic")
	}
}
