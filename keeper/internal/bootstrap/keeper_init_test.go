package bootstrap

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// Pure unit-тесты — не требуют PG/Vault. Логика, целиком зависящая от
// pgxpool.Pool / VaultClient.ReadKV (advisory lock, Insert, ReadKV
// round-trip), покрывается integration_test.go под `//go:build integration`.

// Тесты ParseRef переехали в `keeper/internal/vault/parseref_test.go`
// после выноса парсера в shared keeper-vault helper (M0.5d).

func TestExtractSigningKey_Base64String(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 байта
	encoded := base64.StdEncoding.EncodeToString(raw)
	got, err := extractSigningKey(map[string]any{"signing_key": encoded})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("decoded = %q, want %q", got, raw)
	}
}

func TestExtractSigningKey_NonBase64StringFallback(t *testing.T) {
	// Если значение не base64 — отдаём raw-bytes (32 ASCII-байта).
	raw := "0123456789abcdef0123456789abcdef!" // 33 байта, не base64-валидно
	got, err := extractSigningKey(map[string]any{"signing_key": raw})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != raw {
		t.Errorf("got = %q, want raw %q", got, raw)
	}
}

func TestExtractSigningKey_Bytes(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	got, err := extractSigningKey(map[string]any{"signing_key": raw})
	if err != nil {
		t.Fatalf("extractSigningKey: %v", err)
	}
	if string(got) != string(raw) {
		t.Errorf("got = %q, want %q", got, raw)
	}
}

func TestExtractSigningKey_Missing(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"other": "x"}); !errors.Is(err, ErrSigningKeyMissing) {
		t.Errorf("err = %v, want ErrSigningKeyMissing", err)
	}
}

func TestExtractSigningKey_EmptyString(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"signing_key": ""}); !errors.Is(err, ErrSigningKeyMissing) {
		t.Errorf("err = %v, want ErrSigningKeyMissing", err)
	}
}

func TestExtractSigningKey_UnsupportedType(t *testing.T) {
	if _, err := extractSigningKey(map[string]any{"signing_key": 42}); err == nil {
		t.Errorf("unsupported type: expected error, got nil")
	}
}

func TestWriteTokenFile_PermissionsAndContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	const token = "header.payload.signature"
	if err := writeTokenFile(path, token); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != credentialFileMode {
		t.Errorf("file mode = %o, want %o", mode, credentialFileMode)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != token+"\n" {
		t.Errorf("content = %q, want %q", got, token+"\n")
	}
}

func TestWriteTokenFile_OverwritesAndChmodsBackTo0400(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	// Pre-create с 0644 — writeTokenFile должен явно chmod-нуть до 0400.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeTokenFile(path, "new"); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	info, _ := os.Stat(path)
	if mode := info.Mode().Perm(); mode != credentialFileMode {
		t.Errorf("file mode after rewrite = %o, want %o", mode, credentialFileMode)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new\n" {
		t.Errorf("content = %q, want \"new\\n\"", got)
	}
}

func TestDefaultCredentialPath(t *testing.T) {
	got := defaultCredentialPath("archon-alice")
	// Уход от `/tmp/` (review M0.5c #2): должен идти либо через
	// os.UserCacheDir() (→ оканчивается на `keeper/bootstrap-<aid>.token`),
	// либо fallback `/var/lib/keeper/bootstrap-<aid>.token`. В обоих
	// случаях префикс `/tmp/` не разрешён.
	if strings.HasPrefix(got, "/tmp/") {
		t.Errorf("defaultCredentialPath = %q must not be under /tmp/ (world-readable predictable path)", got)
	}
	wantSuffix := filepath.Join("keeper", "bootstrap-archon-alice.token")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("defaultCredentialPath = %q, want suffix %q", got, wantSuffix)
	}
}

func TestEnsureCredentialDir_CreatesParent(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "nested", "keeper", "bootstrap.token")
	if err := ensureCredentialDir(target); err != nil {
		t.Fatalf("ensureCredentialDir: %v", err)
	}
	info, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("Stat parent: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("parent is not a directory")
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent mode = %o, want 0700", mode)
	}
}

func TestEnsureCredentialDir_ExistingDirOK(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "bootstrap.token")
	// tmp уже существует — ensureCredentialDir не должен ошибаться.
	if err := ensureCredentialDir(target); err != nil {
		t.Errorf("ensureCredentialDir on existing dir: %v", err)
	}
}

func TestEnsureCredentialDir_FileNotDir(t *testing.T) {
	tmp := t.TempDir()
	// Создаём файл вместо каталога.
	filePath := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// path под filePath — ensureCredentialDir увидит filePath как "parent" и
	// должен вернуть error (не каталог).
	target := filepath.Join(filePath, "bootstrap.token")
	if err := ensureCredentialDir(target); err == nil {
		t.Error("ensureCredentialDir: expected error when parent is a regular file")
	}
}

// TestValidateConfig_RejectsBadInput — серия dry-runs validateConfig
// без поднятия PG/Vault. Проверяет, что Init возвращает понятную ошибку
// до любого сетевого вызова.
func TestValidateConfig_RejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty aid", Config{ArchonAID: ""}, "invalid ArchonAID"},
		{"invalid aid", Config{ArchonAID: ".alice"}, "invalid ArchonAID"},
		{"zero ttl", Config{ArchonAID: "archon-alice", TTLBootstrap: 0}, "TTLBootstrap"},
		{"negative ttl", Config{ArchonAID: "archon-alice", TTLBootstrap: -time.Second}, "TTLBootstrap"},
		{"nil pool", Config{ArchonAID: "archon-alice", TTLBootstrap: time.Hour}, "Pool is nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(tt.cfg)
			if err == nil {
				t.Fatalf("validateConfig(%+v): expected error", tt.cfg)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
}

// fakeIssuer — JWTIssuer-mock для unit-тестов Init.
type fakeIssuer struct {
	calls int
	token string
	err   error
}

func (f *fakeIssuer) Issue(_ string, _ []string, _ time.Duration, _ bool) (string, error) {
	f.calls++
	return f.token, f.err
}

// TestValidateConfig_FailsOnNilPoolFirst — детерминированный порядок
// проверок в validateConfig: после ArchonAID+TTL первой падает проверка
// Pool. Дальнейшие nil-проверки (VaultClient/IssuerFactory/AuditWriter/
// SigningKeyRef) требуют non-nil *pgxpool.Pool, который в unit-тесте без
// поднятого Postgres сконструировать нельзя — это покрытие ушло в
// integration_test.go.
func TestValidateConfig_FailsOnNilPoolFirst(t *testing.T) {
	cfg := Config{
		ArchonAID:    "archon-alice",
		TTLBootstrap: time.Hour,
	}
	err := validateConfig(cfg)
	if err == nil {
		t.Fatal("validateConfig: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "Pool is nil") {
		t.Errorf("err = %v, want substring \"Pool is nil\"", err)
	}
}

// fakeAuditWriter — Writer-mock, захватывающий написанный event.
type fakeAuditWriter struct {
	events []*audit.Event
	err    error
}

func (f *fakeAuditWriter) Write(_ context.Context, ev *audit.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, ev)
	return nil
}
