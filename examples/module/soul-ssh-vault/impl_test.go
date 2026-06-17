package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

// --- mock Vault server (httptest) ---

// mockVault — HTTP-сервер, имитирующий относящиеся к нам пути Vault SSH CA:
//
//	POST /v1/<mount>/sign/<role>  → { "data": { "signed_key": "<openssh-cert>" } }
//
// Любой другой path → status задаётся в полях (404/403), для проверки fail-веток.
type mockVault struct {
	signMount    string
	signRole     string
	signedKey    string
	signedStatus int  // 200 по умолчанию
	requireToken bool // если true — проверяем заголовок X-Vault-Token
	expectedReq  func(t *testing.T, body map[string]any)
	gotToken     string
	gotPath      string
	gotBody      map[string]any
}

func (m *mockVault) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.gotToken = r.Header.Get("X-Vault-Token")
		m.gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.gotBody = body

		expectedPath := fmt.Sprintf("/v1/%s/sign/%s", m.signMount, m.signRole)
		if r.URL.Path != expectedPath {
			http.Error(w, fmt.Sprintf("mock vault: unexpected path %q", r.URL.Path), http.StatusNotFound)
			return
		}
		if m.requireToken && m.gotToken == "" {
			http.Error(w, "missing token", http.StatusForbidden)
			return
		}
		if m.expectedReq != nil {
			m.expectedReq(t, body)
		}
		status := m.signedStatus
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			http.Error(w, "mock vault error", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"signed_key":   m.signedKey,
				"serial_number": "0001",
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// realClientForMock строит production-vaultClient (через defaultClient), но
// указывая на mock-сервер. Используется для тестов SSHSign-пути end-to-end.
func realClientForMock(addr, token string) func(p params) (vaultClient, error) {
	return func(_ params) (vaultClient, error) {
		cfg := vaultapi.DefaultConfig()
		cfg.Address = addr
		c, err := vaultapi.NewClient(cfg)
		if err != nil {
			return nil, err
		}
		c.SetToken(token)
		return &realVaultClient{c: c}, nil
	}
}

// --- Sign happy-path ---

func TestSign_HappyPath_KeeperEphemeral(t *testing.T) {
	mock := &mockVault{
		signMount: "ssh",
		signRole:  "keeper-push",
		signedKey: "ssh-ed25519-cert-v01@openssh.com AAAA-fake-cert host-1@keeper",
		requireToken: true,
		expectedReq: func(t *testing.T, body map[string]any) {
			if body["public_key"] != "ssh-ed25519 AAAAtest" {
				t.Errorf("Vault не получил pubkey: %v", body["public_key"])
			}
			if body["valid_principals"] != "soul" {
				t.Errorf("Vault не получил valid_principals=soul: %v", body["valid_principals"])
			}
			if body["cert_type"] != "user" {
				t.Errorf("ждали cert_type=user, got %v", body["cert_type"])
			}
		},
	}
	srv := mock.start(t)

	p := params{
		VaultAddr:  srv.URL,
		VaultMount: "ssh",
		Role:       "keeper-push",
		AuthMethod: authMethodToken,
		Token:      "test-token",
	}
	v := &VaultProvider{cfg: p, newClient: realClientForMock(srv.URL, p.Token)}

	reply, err := v.Sign(context.Background(), &pluginv1.SignRequest{
		Host:      "web-1",
		User:      "soul",
		PublicKey: "ssh-ed25519 AAAAtest",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if reply.GetCertificate() != mock.signedKey {
		t.Errorf("certificate не совпал: got %q want %q", reply.GetCertificate(), mock.signedKey)
	}
	if reply.GetPrivateKey() != "" {
		t.Errorf("private_key должен быть пустым в Vault SSH CA flow, got %q", reply.GetPrivateKey())
	}
	if mock.gotToken != "test-token" {
		t.Errorf("Vault не получил token: got %q", mock.gotToken)
	}
}

// --- Sign fail-closed: пустая pubkey ---

func TestSign_RejectsEmptyPublicKey(t *testing.T) {
	v := &VaultProvider{cfg: params{
		VaultAddr: "http://nowhere", VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
	}}
	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: ""})
	if err == nil {
		t.Fatal("ждали ошибку на пустой public_key (Vault SSH CA требует Keeper-ephemeral)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: Vault auth-fail (403) ---

func TestSign_AuthFail(t *testing.T) {
	mock := &mockVault{
		signMount: "ssh", signRole: "r",
		requireToken: true,
	}
	srv := mock.start(t)
	// Передаём ПУСТОЙ token → mockVault ответит 403.
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "ignored",
	}, newClient: realClientForMock(srv.URL, "")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при auth-fail (Vault 403)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: ssh-engine/role not found (404) ---

func TestSign_RoleNotFound(t *testing.T) {
	// mockVault обслуживает только signMount=ssh+signRole=keeper-push; запрос
	// под role=missing уйдёт на другой path → 404.
	mock := &mockVault{signMount: "ssh", signRole: "keeper-push", signedKey: "x"}
	srv := mock.start(t)
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "missing", AuthMethod: authMethodToken, Token: "t",
	}, newClient: realClientForMock(srv.URL, "t")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при role=missing (404)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: empty signed_key в response ---

func TestSign_EmptySignedKey(t *testing.T) {
	mock := &mockVault{signMount: "ssh", signRole: "r", signedKey: ""} // вернёт data{signed_key:""}
	srv := mock.start(t)
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
	}, newClient: realClientForMock(srv.URL, "t")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при пустом signed_key")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: user вне valid_principals ---

func TestSign_PrincipalAllowlistRejects(t *testing.T) {
	v := &VaultProvider{cfg: params{
		VaultAddr: "http://nowhere", VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
		ValidPrincipals: []string{"soul", "deploy"},
	}}
	// root не в allowlist → fail без обращения в Vault.
	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "root", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали отказ для user не в valid_principals")
	}
	if !strings.Contains(err.Error(), "valid_principals") {
		t.Errorf("err=%q, ждали упоминание valid_principals", err.Error())
	}
}

// --- Authorize allow-by-default / deny / wildcard ---

func TestAuthorize_AllowByDefault(t *testing.T) {
	v := &VaultProvider{cfg: params{}}
	r, err := v.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "web-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !r.GetAllowed() {
		t.Error("ждали allow при пустом deny-list")
	}
}

func TestAuthorize_DenyExplicit(t *testing.T) {
	v := &VaultProvider{cfg: params{Deny: []denyRule{{Host: "prod-1", User: "root"}}}}
	r, err := v.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "prod-1", User: "root"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if r.GetAllowed() {
		t.Fatal("ждали deny для пары из deny-list")
	}
	if !strings.HasPrefix(r.GetReason(), string(sshprovider.DenyExplicitDeny)) {
		t.Errorf("reason=%q, ждали префикс %q", r.GetReason(), sshprovider.DenyExplicitDeny)
	}
}

func TestAuthorize_DenyWildcard(t *testing.T) {
	// host:"" → wildcard: root запрещён везде (симметрично static).
	v := &VaultProvider{cfg: params{Deny: []denyRule{{User: "root"}}}}
	for _, host := range []string{"web-1", "db-2"} {
		r, err := v.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: host, User: "root"})
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if r.GetAllowed() {
			t.Errorf("ждали deny root на %s", host)
		}
	}
}

// --- loadParams ---

func TestLoadParams(t *testing.T) {
	t.Run("ok token", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"token","token":"t"}`)
		p, err := loadParams()
		if err != nil {
			t.Fatalf("loadParams: %v", err)
		}
		if p.VaultAddr != "https://v" || p.Role != "r" || p.Token != "t" {
			t.Errorf("params=%+v", p)
		}
		if p.VaultMount != "ssh" {
			t.Errorf("ждали default mount=ssh, got %q", p.VaultMount)
		}
	})
	t.Run("ok approle", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"approle","approle":{"role_id":"id","secret_id":"s"}}`)
		p, err := loadParams()
		if err != nil {
			t.Fatalf("loadParams: %v", err)
		}
		if p.AuthMethod != "approle" || p.AppRole.RoleID != "id" || p.AppRole.SecretID != "s" {
			t.Errorf("params=%+v", p)
		}
		if p.AppRole.Mount != "approle" {
			t.Errorf("ждали default approle.mount=approle, got %q", p.AppRole.Mount)
		}
	})
	t.Run("empty env fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, "")
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку на пустой env")
		}
	})
	t.Run("missing vault_addr", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"role":"r","auth_method":"token","token":"t"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку без vault_addr")
		}
	})
	t.Run("missing role", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","auth_method":"token","token":"t"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку без role")
		}
	})
	t.Run("token method without token", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"token"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку: auth_method=token требует token")
		}
	})
	t.Run("approle method without creds", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"approle"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку: approle без role_id/secret_id")
		}
	})
	t.Run("unsupported auth_method", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"kubernetes"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку для неподдерживаемого auth_method")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		t.Setenv(paramsEnv, `{not json`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку на битый JSON")
		}
	})
}

// --- newClient injection fail propagates ---

func TestSign_NewClientFailPropagated(t *testing.T) {
	v := &VaultProvider{
		cfg:       params{VaultAddr: "x", Role: "r", AuthMethod: authMethodToken, Token: "t"},
		newClient: func(_ params) (vaultClient, error) { return nil, errors.New("synthetic auth fail") },
	}
	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при newClient fail")
	}
	if !strings.Contains(err.Error(), "synthetic auth fail") {
		t.Errorf("err=%q, ждали обёрнутый текст newClient ошибки", err.Error())
	}
}
