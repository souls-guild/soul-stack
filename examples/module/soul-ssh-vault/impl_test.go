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

// mockVault is an HTTP server that imitates the Vault SSH CA paths we use:
//
//	POST /v1/<mount>/sign/<role>  → { "data": { "signed_key": "<openssh-cert>" } }
//
// Any other path -> status is controlled by fields (404/403), for checking fail
// branches.
type mockVault struct {
	signMount    string
	signRole     string
	signedKey    string
	signedStatus int  // 200 by default
	requireToken bool // if true, check the X-Vault-Token header
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
				"signed_key":    m.signedKey,
				"serial_number": "0001",
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// realClientForMock builds a production vaultClient (through defaultClient) but
// points it at the mock server. Used for end-to-end SSHSign path tests.
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
		signMount:    "ssh",
		signRole:     "keeper-push",
		signedKey:    "ssh-ed25519-cert-v01@openssh.com AAAA-fake-cert host-1@keeper",
		requireToken: true,
		expectedReq: func(t *testing.T, body map[string]any) {
			if body["public_key"] != "ssh-ed25519 AAAAtest" {
				t.Errorf("Vault did not receive pubkey: %v", body["public_key"])
			}
			if body["valid_principals"] != "soul" {
				t.Errorf("Vault did not receive valid_principals=soul: %v", body["valid_principals"])
			}
			if body["cert_type"] != "user" {
				t.Errorf("expected cert_type=user, got %v", body["cert_type"])
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
		t.Errorf("certificate did not match: got %q want %q", reply.GetCertificate(), mock.signedKey)
	}
	if reply.GetPrivateKey() != "" {
		t.Errorf("private_key must be empty in Vault SSH CA flow, got %q", reply.GetPrivateKey())
	}
	if mock.gotToken != "test-token" {
		t.Errorf("Vault did not receive token: got %q", mock.gotToken)
	}
}

// --- Sign fail-closed: empty pubkey ---

func TestSign_RejectsEmptyPublicKey(t *testing.T) {
	v := &VaultProvider{cfg: params{
		VaultAddr: "http://nowhere", VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
	}}
	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: ""})
	if err == nil {
		t.Fatal("expected error for empty public_key (Vault SSH CA requires Keeper-ephemeral)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, expected prefix %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: Vault auth-fail (403) ---

func TestSign_AuthFail(t *testing.T) {
	mock := &mockVault{
		signMount: "ssh", signRole: "r",
		requireToken: true,
	}
	srv := mock.start(t)
	// Pass an EMPTY token -> mockVault responds with 403.
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "ignored",
	}, newClient: realClientForMock(srv.URL, "")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("expected error on auth-fail (Vault 403)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, expected prefix %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: ssh-engine/role not found (404) ---

func TestSign_RoleNotFound(t *testing.T) {
	// mockVault serves only signMount=ssh+signRole=keeper-push; a request with
	// role=missing goes to another path -> 404.
	mock := &mockVault{signMount: "ssh", signRole: "keeper-push", signedKey: "x"}
	srv := mock.start(t)
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "missing", AuthMethod: authMethodToken, Token: "t",
	}, newClient: realClientForMock(srv.URL, "t")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("expected error for role=missing (404)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, expected prefix %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: empty signed_key in response ---

func TestSign_EmptySignedKey(t *testing.T) {
	mock := &mockVault{signMount: "ssh", signRole: "r", signedKey: ""} // returns data{signed_key:""}
	srv := mock.start(t)
	v := &VaultProvider{cfg: params{
		VaultAddr: srv.URL, VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
	}, newClient: realClientForMock(srv.URL, "t")}

	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("expected error for empty signed_key")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, expected prefix %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: user outside valid_principals ---

func TestSign_PrincipalAllowlistRejects(t *testing.T) {
	v := &VaultProvider{cfg: params{
		VaultAddr: "http://nowhere", VaultMount: "ssh", Role: "r", AuthMethod: authMethodToken, Token: "t",
		ValidPrincipals: []string{"soul", "deploy"},
	}}
	// root is not in allowlist -> fail without calling Vault.
	_, err := v.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "root", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("expected rejection for user outside valid_principals")
	}
	if !strings.Contains(err.Error(), "valid_principals") {
		t.Errorf("err=%q, expected valid_principals mention", err.Error())
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
		t.Error("expected allow with empty deny-list")
	}
}

func TestAuthorize_DenyExplicit(t *testing.T) {
	v := &VaultProvider{cfg: params{Deny: []denyRule{{Host: "prod-1", User: "root"}}}}
	r, err := v.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "prod-1", User: "root"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if r.GetAllowed() {
		t.Fatal("expected deny for deny-list pair")
	}
	if !strings.HasPrefix(r.GetReason(), string(sshprovider.DenyExplicitDeny)) {
		t.Errorf("reason=%q, expected prefix %q", r.GetReason(), sshprovider.DenyExplicitDeny)
	}
}

func TestAuthorize_DenyWildcard(t *testing.T) {
	// host:"" -> wildcard: root is denied everywhere (symmetrical with static).
	v := &VaultProvider{cfg: params{Deny: []denyRule{{User: "root"}}}}
	for _, host := range []string{"web-1", "db-2"} {
		r, err := v.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: host, User: "root"})
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if r.GetAllowed() {
			t.Errorf("expected deny for root on %s", host)
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
			t.Errorf("expected default mount=ssh, got %q", p.VaultMount)
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
			t.Errorf("expected default approle.mount=approle, got %q", p.AppRole.Mount)
		}
	})
	t.Run("empty env fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, "")
		if _, err := loadParams(); err == nil {
			t.Error("expected error for empty env")
		}
	})
	t.Run("missing vault_addr", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"role":"r","auth_method":"token","token":"t"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error without vault_addr")
		}
	})
	t.Run("missing role", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","auth_method":"token","token":"t"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error without role")
		}
	})
	t.Run("token method without token", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"token"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error: auth_method=token requires token")
		}
	})
	t.Run("approle method without creds", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"approle"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error: approle without role_id/secret_id")
		}
	})
	t.Run("unsupported auth_method", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"vault_addr":"https://v","role":"r","auth_method":"kubernetes"}`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error for unsupported auth_method")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		t.Setenv(paramsEnv, `{not json`)
		if _, err := loadParams(); err == nil {
			t.Error("expected error for bad JSON")
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
		t.Fatal("expected error on newClient fail")
	}
	if !strings.Contains(err.Error(), "synthetic auth fail") {
		t.Errorf("err=%q, expected wrapped newClient error text", err.Error())
	}
}
