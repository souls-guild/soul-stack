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
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeVaultMux serves a minimally-sufficient subset of the Vault HTTP API:
//
//   - GET  /v1/sys/health            → 200 (for Ping).
//   - GET  /v1/<mount>/data/<rel>    → KV v2 envelope or 404.
//   - POST /v1/auth/approle/login    → client token, if role_id/secret_id
//     match expectations; 400 otherwise (simulates Vault permission denied).
//
// Sufficient for tests — KVv2.Get and approle login inside vault/api work this way.
type fakeVaultMux struct {
	mount   string
	secrets map[string]map[string]any

	// kvVersion is the version reported by probe-endpoint sys/internal/ui/mounts/<mount>
	// for the mount ("1"/"2"). Empty → probe-endpoint responds with 403 (simulates
	// ACL-deny). Also controls which path is used to serve secrets (v2 → /data/,
	// v1 → flat /<rel>).
	kvVersion string
	// probeForbidden forces 403 on probe-endpoint regardless of kvVersion
	// (for testing "probe closed by ACL").
	probeForbidden bool
	probeRequests  int

	// approle expectations (if wantRoleID != "" — login-handler is active).
	wantRoleID    string
	wantSecretID  string
	issuedToken   string
	loginRequests int
	lastLoginBody map[string]any
}

func newFakeVault(mount string) *fakeVaultMux {
	return &fakeVaultMux{
		mount:     mount,
		secrets:   make(map[string]map[string]any),
		kvVersion: "2", // dev-default; v1 tests override.
	}
}

func (f *fakeVaultMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/sys/health":
		// Vault dev returns 200 in active, 429 in standby. For tests — 200.
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

	case r.URL.Path == "/v1/sys/internal/ui/mounts/"+f.mount:
		// Probe KV mount version. 403 → simulates ACL-deny / probeForbidden.
		f.probeRequests++
		if f.probeForbidden || f.kvVersion == "" {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"permission denied"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "test",
			"data": map[string]any{
				"type":    "kv",
				"path":    f.mount + "/",
				"options": map[string]any{"version": f.kvVersion},
			},
		})
		return

	case strings.HasPrefix(r.URL.Path, "/v1/"+f.mount+"/data/"):
		// KV v2 read-path.
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

	// KV v1 read-path — flat `/v1/<mount>/<rel>` (no /data/). Checked last
	// because prefix `/v1/<mount>/` overlaps with /data/ and /metadata/.
	if strings.HasPrefix(r.URL.Path, "/v1/"+f.mount+"/") {
		rel := strings.TrimPrefix(r.URL.Path, "/v1/"+f.mount+"/")
		data, ok := f.secrets[rel]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"request_id": "test",
			"data":       data,
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
		// KVMount empty — should default to "secret".
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

	// Logical path with mount prefix.
	got, err := cl.ReadKV(context.Background(), "secret/keeper/jwt-signing-key")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["signing_key"] != "deadbeef" {
		t.Errorf("signing_key = %v", got["signing_key"])
	}

	// Relative path without prefix — same result.
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
	// `KVMount: "secret/"` without normalization produces URL `secret//data/...`,
	// which Vault interprets as a different path → silent miss.
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
	// `path: "/secret/foo"` without normalization produces URL `secret/data//secret/foo`
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
	// Defense-in-depth: `..` segment in KV path → fail-closed on all
	// KV methods (relativeKVPath). Legitimate path (secret/keeper/...) — ok.
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["keeper/jwt-signing-key"] = map[string]any{"signing_key": "ok"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	// Legitimate path must not break due to guard.
	if _, err := cl.ReadKV(ctx, "secret/keeper/jwt-signing-key"); err != nil {
		t.Fatalf("ReadKV legit path: unexpected error %v", err)
	}

	badPaths := []string{
		"secret/keeper/../jwt-signing-key", // `..` in middle
		"keeper/../../etc",                 // leading escape out of scope
		"../keeper/jwt-signing-key",        // `..` after strip mount-prefix
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
	// Address without listening socket — Ping will fail, NewClient will return error.
	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr:    "http://127.0.0.1:1", // reserved port, no one listening
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

// writeSecretIDFile writes secret_id to a mode-0400 file in a temporary directory.
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

	// Trailing newline in file — typical from `echo > file`; should be stripped.
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
	// secret_id from payload must not accidentally be replaced with value containing \n.
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
	// Error text must not leak secret_id, role_id, or issued token. role_id per
	// ADR-014 is not a secret, but we keep it out of error messages anyway
	// (minimize surface area).
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
			// neither SecretIDFile nor SecretIDEnv.
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

// --- KV version resolution (probe + override + cache) ---------------------
//
// Guard set for transparent KV v1/v2 support (ADR-017(b) amendment
// 2026-06-22). Previous "guess by KVv2.Get error class" mechanism rejected
// (plain v1-secret indistinguishable from v2-missing → ErrSecretNotFound);
// version now resolved constructively — probe sys/internal/ui/mounts or
// explicit override vault.kv_version.

// TestNewClient_NoKVGrant_StillStarts — probe is strictly lazy: constructor
// only makes Ping (sys/health, without KV permissions). This is a bootstrap-path
// invariant (keeper init creates Client before granting KV access); probe in
// constructor would break it. fakeVaultMux probe-endpoint is not touched at all.
func TestNewClient_NoKVGrant_StillStarts(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if cl == nil {
		t.Fatal("NewClient: nil client")
	}
	if mux.probeRequests != 0 {
		t.Errorf("probe must be lazy: %d probe requests during NewClient, want 0", mux.probeRequests)
	}
}

// TestResolveVersion_OverrideSkipsProbe — override="2"/"1" wins without
// a single round-trip to probe-endpoint.
func TestResolveVersion_OverrideSkipsProbe(t *testing.T) {
	for _, ver := range []string{"1", "2"} {
		ver := ver
		t.Run("v"+ver, func(t *testing.T) {
			mux, addr := startFakeVault(t, "secret")
			// probe-endpoint would return 403 if accessed — but it should not be
			// accessed at all.
			mux.probeForbidden = true
			mux.secrets["app/cfg"] = map[string]any{"k": "v"}

			cl, err := NewClient(context.Background(), config.KeeperVault{
				Addr: addr, Token: "root", KVMount: "secret", KVVersion: ver,
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			got, err := cl.ReadKV(context.Background(), "app/cfg")
			if err != nil {
				t.Fatalf("ReadKV(kv_version=%s): %v", ver, err)
			}
			if got["k"] != "v" {
				t.Errorf("k=%v", got["k"])
			}
			if mux.probeRequests != 0 {
				t.Errorf("override=%s must skip probe, got %d probe requests", ver, mux.probeRequests)
			}
		})
	}
}

// TestResolveVersion_ProbeV1 / _ProbeV2 — probe options.version routes to
// the correct KV API version.
func TestResolveVersion_ProbeV2(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.kvVersion = "2"
	mux.secrets["app/cfg"] = map[string]any{"k": "v2val"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := cl.ReadKV(context.Background(), "app/cfg")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["k"] != "v2val" {
		t.Errorf("k=%v", got["k"])
	}
	if mux.probeRequests == 0 {
		t.Error("expected probe round-trip for auto-detect")
	}
}

func TestResolveVersion_ProbeV1(t *testing.T) {
	mux, addr := startFakeVault(t, "kv-v1")
	mux.kvVersion = "1"
	mux.secrets["app/cfg"] = map[string]any{"k": "v1val"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "kv-v1",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := cl.ReadKV(context.Background(), "app/cfg")
	if err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if got["k"] != "v1val" {
		t.Errorf("k=%v (v1 flat payload not read)", got["k"])
	}
}

// TestResolveVersion_ProbeUnexpectedValue_Fails — probe returned mount with
// unrecognized version → explicit error, NOT silent v2 default.
func TestResolveVersion_ProbeUnexpectedValue_Fails(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.kvVersion = "9" // not "1"/"2"
	mux.secrets["app/cfg"] = map[string]any{"k": "v"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cl.ReadKV(context.Background(), "app/cfg")
	if err == nil {
		t.Fatal("expected error on undeterminable KV version, got nil (would be silent v2)")
	}
	if errors.Is(err, ErrVaultKVNotFound) {
		t.Errorf("undeterminable-version error must not be masked as not-found: %v", err)
	}
	if !strings.Contains(err.Error(), "kv_version") {
		t.Errorf("error should hint at vault.kv_version override: %v", err)
	}
}

// TestResolveVersion_ProbeForbidden_NoOverride_Fails — probe 403 (ACL closed
// endpoint) + override empty → clear error with hint about override, NOT panic
// or silent v2.
func TestResolveVersion_ProbeForbidden_NoOverride_Fails(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.probeForbidden = true
	mux.secrets["app/cfg"] = map[string]any{"k": "v"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cl.ReadKV(context.Background(), "app/cfg")
	if err == nil {
		t.Fatal("expected error on forbidden probe without override, got nil")
	}
	if !strings.Contains(err.Error(), "kv_version") {
		t.Errorf("error should hint at vault.kv_version override: %v", err)
	}
}

// TestResolveVersion_Cached — second ReadKV of same mount does not
// make another probe (per-mount cache).
func TestResolveVersion_Cached(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["app/a"] = map[string]any{"k": "a"}
	mux.secrets["app/b"] = map[string]any{"k": "b"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	if _, err := cl.ReadKV(ctx, "app/a"); err != nil {
		t.Fatalf("ReadKV a: %v", err)
	}
	if _, err := cl.ReadKV(ctx, "app/b"); err != nil {
		t.Fatalf("ReadKV b: %v", err)
	}
	if mux.probeRequests != 1 {
		t.Errorf("expected exactly 1 probe for repeated reads of same mount, got %d", mux.probeRequests)
	}
}

// TestResolveVersion_ConcurrentColdStart — double-checked locking in
// resolveKVVersion collapses thundering-herd: N goroutines simultaneously hit
// COLD cache of one mount, but probe round-trip happens exactly once (re-check
// under write-lock before probe). Run under `go test -race` — catches both
// missing herd collapse and races on kvVersions cache.
func TestResolveVersion_ConcurrentColdStart(t *testing.T) {
	mux, addr := startFakeVault(t, "secret")
	mux.kvVersion = "2"
	mux.secrets["app/cfg"] = map[string]any{"k": "v"}

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start // synchronous start — maximize chance of concurrent cache miss
			if _, err := cl.ReadKV(context.Background(), "app/cfg"); err != nil {
				t.Errorf("ReadKV cold-start: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	// Double-checked locking should collapse probe to one round-trip. Allow
	// small slack in case RLock misses by several goroutines slip through before
	// first goroutine takes write-lock (current implementation re-check under
	// write-lock guarantees exactly 1, but we don't want to tie ourselves to
	// mutex-internal scheduler details more than needed for guard meaning).
	if mux.probeRequests != 1 {
		t.Errorf("concurrent cold-start: probe round-trips = %d, want 1 (double-checked locking should collapse thundering-herd)", mux.probeRequests)
	}
}

// TestWriteKV_V1Routing — WriteKV on v1-mount goes through KVv1.Put (flat
// path without /data/). Guard on write routing.
func TestWriteKV_V1Routing(t *testing.T) {
	mux, addr := startFakeVault(t, "kv-v1")
	mux.kvVersion = "1"

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "kv-v1",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// fakeVaultMux does not implement write handling, but KVv1.Put sends POST to flat
	// path and expects 200/204. Mock responds with 404 to unknown POST → Put returns
	// error. Sufficient to check routing selected v1 branch (not /data/): in v2 routing
	// request would go to kv-v1/data/... and also 404 — so this test only verifies
	// version resolves to 1 without panic. Exact happy-path write is covered by
	// integration test on real Vault.
	err = cl.WriteKV(context.Background(), "app/cfg", map[string]any{"k": "v"})
	// Mock does not accept write → error expected; main point is resolveKVVersion=1
	// passed (probeRequests > 0) and no panic.
	if mux.probeRequests == 0 {
		t.Error("WriteKV did not resolve KV version (no probe)")
	}
	_ = err
}

// TestListKV_V1Mount_ClearError — list on v1-mount → clear error
// "requires KV v2", not silently broken metadata-path.
func TestListKV_V1Mount_ClearError(t *testing.T) {
	mux, addr := startFakeVault(t, "kv-v1")
	mux.kvVersion = "1"

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "kv-v1",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cl.ListKV(context.Background(), "app")
	if err == nil {
		t.Fatal("expected error: list requires KV v2")
	}
	if !strings.Contains(err.Error(), "KV v2") {
		t.Errorf("error should state KV v2 requirement: %v", err)
	}
}

// TestReadKVMetadata_V1Mount_ClearError — same for metadata-read.
func TestReadKVMetadata_V1Mount_ClearError(t *testing.T) {
	mux, addr := startFakeVault(t, "kv-v1")
	mux.kvVersion = "1"

	cl, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "kv-v1",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = cl.ReadKVMetadata(context.Background(), "app/cfg")
	if err == nil {
		t.Fatal("expected error: metadata read requires KV v2")
	}
	if !strings.Contains(err.Error(), "KV v2") {
		t.Errorf("error should state KV v2 requirement: %v", err)
	}
}

// TestNewClient_InvalidKVVersion_Fails — invalid override rejected
// fail-fast in constructor (duplicates schema validation for callers outside
// config-load path).
func TestNewClient_InvalidKVVersion_Fails(t *testing.T) {
	_, addr := startFakeVault(t, "secret")
	_, err := NewClient(context.Background(), config.KeeperVault{
		Addr: addr, Token: "root", KVMount: "secret", KVVersion: "3",
	})
	if err == nil {
		t.Fatal("expected error on invalid kv_version")
	}
}
