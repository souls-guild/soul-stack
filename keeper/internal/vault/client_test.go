package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeVaultMux отдаёт минимально-достаточный subset Vault HTTP API:
//
//   - GET  /v1/sys/health            → 200 (для Ping).
//   - GET  /v1/<mount>/data/<rel>    → KV v2 envelope или 404.
//   - POST /v1/auth/approle/login    → client-token, если role_id/secret_id
//     совпали с ожидаемыми; 400 иначе (имитирует Vault permission denied).
//
// Для тестов достаточно — KVv2.Get и approle login внутри vault/api идут так.
type fakeVaultMux struct {
	mount   string
	secrets map[string]map[string]any

	// approle-ожидания (если wantRoleID != "" — login-handler активен).
	wantRoleID    string
	wantSecretID  string
	issuedToken   string
	loginRequests int
	lastLoginBody map[string]any
}

func newFakeVault(mount string) *fakeVaultMux {
	return &fakeVaultMux{
		mount:   mount,
		secrets: make(map[string]map[string]any),
	}
}

func (f *fakeVaultMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/sys/health":
		// Vault dev возвращает 200 в active, 429 в standby. Для тестов — 200.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false,"version":"test"}`))
		return

	case r.URL.Path == "/v1/"+approleLoginPath:
		f.loginRequests++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastLoginBody = body
		role, _ := body["role_id"].(string)
		sid, _ := body["secret_id"].(string)
		if role != f.wantRoleID || sid != f.wantSecretID {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errors": []string{"invalid role or secret id"},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "test",
			"auth": map[string]any{
				"client_token":   f.issuedToken,
				"renewable":      true,
				"lease_duration": 3600,
			},
		})
		return

	case strings.HasPrefix(r.URL.Path, "/v1/"+f.mount+"/data/"):
		rel := strings.TrimPrefix(r.URL.Path, "/v1/"+f.mount+"/data/")
		data, ok := f.secrets[rel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "test",
			"data": map[string]any{
				"data": data,
				"metadata": map[string]any{
					"version": 1,
				},
			},
		})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func startFakeVault(t *testing.T, mount string) (*fakeVaultMux, string) {
	t.Helper()
	mux := newFakeVault(mount)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return mux, srv.URL
}

func TestNewClient_RejectsEmptyAddr(t *testing.T) {
	_, err := NewClient(context.Background(), config.KeeperVault{Token: "root"})
	if err == nil {
		t.Fatalf("NewClient empty addr: expected error, got nil")
	}
}

func TestNewClient_RejectsEmptyToken(t *testing.T) {
	_, err := NewClient(context.Background(), config.KeeperVault{Addr: "http://x"})
	if err == nil {
		t.Fatalf("NewClient empty token: expected error, got nil")
	}
}

func TestNewClient_HappyPath(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    addr,
		Token:   "root",
		KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if cl == nil {
		t.Fatalf("NewClient: nil client")
	}
}

func TestNewClient_DefaultMount(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{"signing_key": "abc"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr:  addr,
		Token: "root",
		// KVMount пуст — должен подставиться "secret".
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := cl.ReadKV(context.Background(), "secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != "abc" {
		t.Errorf("signing_key = %v", got["signing_key"])
	}
}

func TestReadKV_HappyPath(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{
		"signing_key": "deadbeef",
		"version":     1,
	}
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Logical path с префиксом mount-а.
	got, err := cl.ReadKV(context.Background(), "secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != "deadbeef" {
		t.Errorf("signing_key = %v", got["signing_key"])
	}

	// Relative path без префикса — тот же результат.
	got2, err := cl.ReadKV(context.Background(), "keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV (relative): %v", err)
	}
	if got2["signing_key"] != "deadbeef" {
		t.Errorf("relative signing_key = %v", got2["signing_key"])
	}
}

func TestReadKV_NotFound(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cl.ReadKV(context.Background(), "secret/keeper/missing")
	if !errors.Is(err, ErrVaultKVNotFound) {
		t.Fatalf("ReadKV missing: err=%v, want errors.Is(ErrVaultKVNotFound)", err)
	}
}

func TestReadKV_EmptyPath(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := cl.ReadKV(context.Background(), "secret/"); err == nil {
		t.Errorf("ReadKV(\"secret/\"): expected error, got nil")
	}
	if _, err := cl.ReadKV(context.Background(), ""); err == nil {
		t.Errorf("ReadKV(\"\"): expected error, got nil")
	}
}

func TestPing(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := cl.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestReadKV_TrailingSlashInMount(t *testing.T) {
	// `KVMount: "secret/"` без нормализации даёт URL `secret//data/...`,
	// который Vault интерпретирует как другой path → silent miss.
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{"signing_key": "trailing"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    addr,
		Token:   "root",
		KVMount: "secret/",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := cl.ReadKV(context.Background(), "secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != "trailing" {
		t.Errorf("signing_key = %v", got["signing_key"])
	}
}

func TestReadKV_LeadingSlashInPath(t *testing.T) {
	// `path: "/secret/foo"` без нормализации даёт URL `secret/data//secret/foo`
	// — silent wrong-path.
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{"signing_key": "leading"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := cl.ReadKV(context.Background(), "/secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != "leading" {
		t.Errorf("signing_key = %v", got["signing_key"])
	}
}

func TestRelativeKVPath_RejectsDotDot(t *testing.T) {
	// Defense-in-depth: `..`-сегмент в KV-пути → fail-closed на всех
	// KV-методах (relativeKVPath). Легитимный путь (secret/keeper/...) — ок.
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{"signing_key": "ok"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	// Легитимный путь не должен ломаться guard-ом.
	if _, err := cl.ReadKV(ctx, "secret/keeper/jwt-signing-key"); err != nil {
		t.Fatalf("ReadKV legit path: unexpected error %v", err)
	}

	badPaths := []string{
		"secret/keeper/../jwt-signing-key", // `..` в середине
		"keeper/../../etc",                 // ведущий выход за scope
		"../keeper/jwt-signing-key",        // `..` после strip mount-prefix
	}
	for _, bad := range badPaths {
		bad := bad
		t.Run(bad, func(t *testing.T) {
			if _, err := cl.ReadKV(ctx, bad); err == nil {
				t.Errorf("ReadKV(%q): expected '..' rejection, got nil", bad)
			}
			if _, err := cl.ReadKVMetadata(ctx, bad); err == nil {
				t.Errorf("ReadKVMetadata(%q): expected '..' rejection, got nil", bad)
			}
			if _, err := cl.ListKV(ctx, bad); err == nil {
				t.Errorf("ListKV(%q): expected '..' rejection, got nil", bad)
			}
			if err := cl.WriteKV(ctx, bad, map[string]any{"k": "v"}); err == nil {
				t.Errorf("WriteKV(%q): expected '..' rejection, got nil", bad)
			}
		})
	}
}

func TestNewClient_PingFails_OnInvalidAddr(t *testing.T) {
	// Адрес без слушающего сокета — Ping упадёт, NewClient вернёт ошибку.
	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    "http://127.0.0.1:1", // зарезервированный порт, никто не слушает
		Token:   "root",
		KVMount: "secret",
	})
	if err == nil {
		t.Fatalf("NewClient with unreachable addr: expected error, got nil")
	}
}

const (
	testApproleRoleID   = "keeper-role-01"
	testApproleSecretID = "s3cr3t-id-value"
	testApproleToken    = "s.issued-client-token"
)

// writeSecretIDFile кладёт secret_id в mode-0400 файл во временной директории.
func writeSecretIDFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "vault-secret-id")
	if err := os.WriteFile(p, []byte(content), 0o400); err != nil {
		t.Fatalf("write secret_id file: %v", err)
	}
	return p
}

func TestNewClient_AppRole_FileSource(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.wantRoleID = testApproleRoleID
	mux.wantSecretID = testApproleSecretID
	mux.issuedToken = testApproleToken

	// Trailing newline в файле — типично для `echo > file`; должен сниматься.
	sidFile := writeSecretIDFile(t, testApproleSecretID+"\n")

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    addr,
		KVMount: "secret",
		Auth: config.KeeperVaultAuth{
			Method:       config.AuthMethodAppRole,
			RoleID:       testApproleRoleID,
			SecretIDFile: sidFile,
		},
	})
	if err != nil {
		t.Fatalf("NewClient approle: %v", err)
	}
	if mux.loginRequests != 1 {
		t.Fatalf("expected 1 login request, got %d", mux.loginRequests)
	}
	if cl.c.Token() != testApproleToken {
		t.Errorf("client token not set from login response")
	}
	// secret_id из payload не должен случайно подмениться значением с \n.
	if got, _ := mux.lastLoginBody["secret_id"].(string); got != testApproleSecretID {
		t.Errorf("secret_id payload = %q, want trimmed %q", got, testApproleSecretID)
	}
}

func TestNewClient_AppRole_EnvSource(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.wantRoleID = testApproleRoleID
	mux.wantSecretID = testApproleSecretID
	mux.issuedToken = testApproleToken

	const envName = "KEEPER_TEST_VAULT_SECRET_ID"
	t.Setenv(envName, testApproleSecretID)

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    addr,
		KVMount: "secret",
		Auth: config.KeeperVaultAuth{
			Method:      config.AuthMethodAppRole,
			RoleID:      testApproleRoleID,
			SecretIDEnv: envName,
		},
	})
	if err != nil {
		t.Fatalf("NewClient approle env: %v", err)
	}
	if cl.c.Token() != testApproleToken {
		t.Errorf("client token not set from login response")
	}
}

func TestNewClient_AppRole_WrongSecret_NoLeak(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.wantRoleID = testApproleRoleID
	mux.wantSecretID = "correct-secret"
	mux.issuedToken = testApproleToken

	const wrongSecret = "WRONG-SECRET-MUST-NOT-LEAK"
	sidFile := writeSecretIDFile(t, wrongSecret)

	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    addr,
		KVMount: "secret",
		Auth: config.KeeperVaultAuth{
			Method:       config.AuthMethodAppRole,
			RoleID:       testApproleRoleID,
			SecretIDFile: sidFile,
		},
	})
	if err == nil {
		t.Fatalf("expected login error on wrong secret_id")
	}
	// Утечь в текст ошибки не должны ни secret_id, ни role_id, ни выпускаемый
	// токен. role_id по ADR-014 не секрет, но его всё равно держим вне
	// сообщений об ошибках (минимизация поверхности).
	msg := err.Error()
	for _, leak := range []struct{ name, val string }{
		{"secret_id", wrongSecret},
		{"role_id", testApproleRoleID},
		{"issued token", testApproleToken},
	} {
		if strings.Contains(msg, leak.val) {
			t.Errorf("error text leaked %s (%q): %q", leak.name, leak.val, msg)
		}
	}
}

func TestNewClient_AppRole_MissingSecretSource(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr,
		Auth: config.KeeperVaultAuth{
			Method: config.AuthMethodAppRole,
			RoleID: testApproleRoleID,
			// ни SecretIDFile, ни SecretIDEnv.
		},
	})
	if err == nil {
		t.Fatalf("expected error when no secret_id source configured")
	}
}

func TestNewClient_AppRole_EmptyFile_NoLeakPath(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	sidFile := writeSecretIDFile(t, "   \n")
	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr,
		Auth: config.KeeperVaultAuth{
			Method:       config.AuthMethodAppRole,
			RoleID:       testApproleRoleID,
			SecretIDFile: sidFile,
		},
	})
	if err == nil {
		t.Fatalf("expected error on empty secret_id file")
	}
}
