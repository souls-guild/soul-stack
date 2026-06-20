package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

// --- mock Teleport client ---

// mockTeleportClient — узкий mock teleportClient: подменяет поведение
// GenerateUserSSHCert/Close, фиксирует, как был вызван Sign (pubkey/principal/roles).
// Симметрично vault/realClientForMock: тесту нужны только наблюдаемые точки
// контракта, не реальный Teleport API.
type mockTeleportClient struct {
	signedCert   string
	signErr      error
	gotPubkey    string
	gotPrincipal string
	gotRoles     []string
	closed       atomic.Bool
}

func (m *mockTeleportClient) GenerateUserSSHCert(_ context.Context, pubkey, principal string, roles []string) (string, error) {
	m.gotPubkey = pubkey
	m.gotPrincipal = principal
	m.gotRoles = append([]string(nil), roles...)
	if m.signErr != nil {
		return "", m.signErr
	}
	return m.signedCert, nil
}

func (m *mockTeleportClient) Close() error {
	m.closed.Store(true)
	return nil
}

// mockFactory строит фабрику, возвращающую заранее подготовленный mock-клиент
// (или ошибку, если factoryErr != nil). Позволяет покрыть identity-file-fail
// (фабрика не смогла собрать клиент) и auth-error (фабрика ок, но
// GenerateUserSSHCert упал) разными путями.
func mockFactory(client *mockTeleportClient, factoryErr error) func(context.Context, params) (teleportClient, error) {
	return func(_ context.Context, _ params) (teleportClient, error) {
		if factoryErr != nil {
			return nil, factoryErr
		}
		return client, nil
	}
}

// --- Sign happy-path ---

func TestSign_HappyPath_KeeperEphemeral(t *testing.T) {
	mock := &mockTeleportClient{
		signedCert: "ssh-ed25519-cert-v01@openssh.com AAAA-fake-cert host-1@teleport",
	}
	p := params{
		ProxyAddr:    "tp-proxy.internal:3023",
		IdentityFile: "/etc/teleport/identity",
		Roles:        []string{"node-admin"},
	}
	tp := &TeleportProvider{cfg: p, newClient: mockFactory(mock, nil)}

	reply, err := tp.Sign(context.Background(), &pluginv1.SignRequest{
		Host:      "web-1",
		User:      "soul",
		PublicKey: "ssh-ed25519 AAAAtest",
	})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if reply.GetCertificate() != mock.signedCert {
		t.Errorf("certificate не совпал: got %q want %q", reply.GetCertificate(), mock.signedCert)
	}
	if reply.GetPrivateKey() != "" {
		t.Errorf("private_key должен быть пустым в Teleport flow, got %q", reply.GetPrivateKey())
	}
	if reply.GetProxyJump() != p.ProxyAddr {
		t.Errorf("proxy_jump=%q, ждали %q (cfg.ProxyAddr)", reply.GetProxyJump(), p.ProxyAddr)
	}
	if mock.gotPubkey != "ssh-ed25519 AAAAtest" {
		t.Errorf("Teleport не получил pubkey: %q", mock.gotPubkey)
	}
	if mock.gotPrincipal != "soul" {
		t.Errorf("Teleport не получил principal=soul: %q", mock.gotPrincipal)
	}
	if !mock.closed.Load() {
		t.Error("Teleport-клиент должен закрываться после Sign (defer Close)")
	}
}

// --- Sign fail-closed: пустая pubkey ---

func TestSign_RejectsEmptyPublicKey(t *testing.T) {
	tp := &TeleportProvider{cfg: params{
		ProxyAddr: "p:3023", IdentityFile: "/x",
	}}
	_, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: ""})
	if err == nil {
		t.Fatal("ждали ошибку на пустой public_key (Teleport = Keeper-ephemeral)")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: identity-file/auth fail на фабрике (factoryErr) ---

func TestSign_IdentityFileFail(t *testing.T) {
	tp := &TeleportProvider{
		cfg:       params{ProxyAddr: "p:3023", IdentityFile: "/nonexistent"},
		newClient: mockFactory(nil, errors.New("identity-file: open: no such file or directory")),
	}
	_, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при недоступной identity-file")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
	if !strings.Contains(err.Error(), "identity-file") {
		t.Errorf("err=%q, ждали обёрнутый текст factory-ошибки", err.Error())
	}
}

// --- Sign fail: Teleport вернул ошибку из GenerateUserCerts ---

func TestSign_AuthError(t *testing.T) {
	mock := &mockTeleportClient{signErr: errors.New("auth: access denied for role node-admin")}
	tp := &TeleportProvider{
		cfg:       params{ProxyAddr: "p:3023", IdentityFile: "/x"},
		newClient: mockFactory(mock, nil),
	}
	_, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку, когда Teleport вернул auth-fail")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
	if !mock.closed.Load() {
		t.Error("клиент должен закрываться даже при fail на GenerateUserCerts (defer Close)")
	}
}

// --- Sign fail: пустой signed-cert от Teleport ---

func TestSign_EmptySignedCert(t *testing.T) {
	mock := &mockTeleportClient{signedCert: ""}
	tp := &TeleportProvider{
		cfg:       params{ProxyAddr: "p:3023", IdentityFile: "/x"},
		newClient: mockFactory(mock, nil),
	}
	_, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "u", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали ошибку при пустом ssh-cert от Teleport")
	}
	if !strings.HasPrefix(err.Error(), string(sshprovider.SignFailIssue)+": ") {
		t.Errorf("reason=%q, ждали префикс %q", err.Error(), sshprovider.SignFailIssue)
	}
}

// --- Sign fail: user вне valid_principals ---

func TestSign_PrincipalAllowlistRejects(t *testing.T) {
	tp := &TeleportProvider{cfg: params{
		ProxyAddr: "p:3023", IdentityFile: "/x",
		ValidPrincipals: []string{"soul", "deploy"},
	}}
	// root не в allowlist → fail без обращения в Teleport.
	_, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "root", PublicKey: "ssh-ed25519 AAAA"})
	if err == nil {
		t.Fatal("ждали отказ для user не в valid_principals")
	}
	if !strings.Contains(err.Error(), "valid_principals") {
		t.Errorf("err=%q, ждали упоминание valid_principals", err.Error())
	}
}

// --- Sign: proxy_jump в reply (отдельный явный тест) ---

func TestSign_ProxyJumpEchoedInReply(t *testing.T) {
	const expectedProxy = "teleport.example.com:3023"
	mock := &mockTeleportClient{signedCert: "ssh-ed25519-cert-v01@openssh.com AAAA-cert"}
	tp := &TeleportProvider{
		cfg:       params{ProxyAddr: expectedProxy, IdentityFile: "/x"},
		newClient: mockFactory(mock, nil),
	}
	reply, err := tp.Sign(context.Background(), &pluginv1.SignRequest{Host: "h", User: "soul", PublicKey: "ssh-ed25519 AAAA"})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if reply.GetProxyJump() != expectedProxy {
		t.Errorf("proxy_jump=%q, ждали %q", reply.GetProxyJump(), expectedProxy)
	}
}

// --- Authorize allow-by-default / deny / wildcard ---

func TestAuthorize_AllowByDefault(t *testing.T) {
	tp := &TeleportProvider{cfg: params{}}
	r, err := tp.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "web-1", User: "soul"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !r.GetAllowed() {
		t.Error("ждали allow при пустом deny-list")
	}
}

func TestAuthorize_DenyExplicit(t *testing.T) {
	tp := &TeleportProvider{cfg: params{Deny: []denyRule{{Host: "prod-1", User: "root"}}}}
	r, err := tp.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: "prod-1", User: "root"})
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
	// host:"" → wildcard: root запрещён везде (симметрично vault/static).
	tp := &TeleportProvider{cfg: params{Deny: []denyRule{{User: "root"}}}}
	for _, host := range []string{"web-1", "db-2"} {
		r, err := tp.Authorize(context.Background(), &pluginv1.AuthorizeRequest{Host: host, User: "root"})
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
	t.Run("ok identity_file", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"proxy_addr":"p:3023","identity_file":"/etc/teleport/id"}`)
		p, err := loadParams()
		if err != nil {
			t.Fatalf("loadParams: %v", err)
		}
		if p.ProxyAddr != "p:3023" || p.IdentityFile != "/etc/teleport/id" {
			t.Errorf("params=%+v", p)
		}
	})
	t.Run("ok tbot_socket", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"proxy_addr":"p:3023","tbot_socket":"/var/run/tbot.sock"}`)
		p, err := loadParams()
		if err != nil {
			t.Fatalf("loadParams: %v", err)
		}
		if p.TbotSocket != "/var/run/tbot.sock" {
			t.Errorf("params=%+v", p)
		}
	})
	t.Run("empty env fail-closed", func(t *testing.T) {
		t.Setenv(paramsEnv, "")
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку на пустой env")
		}
	})
	t.Run("missing proxy_addr", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"identity_file":"/x"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку без proxy_addr")
		}
	})
	t.Run("missing credentials source", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"proxy_addr":"p:3023"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку без identity_file/tbot_socket")
		}
	})
	t.Run("both identity_file and tbot_socket", func(t *testing.T) {
		t.Setenv(paramsEnv, `{"proxy_addr":"p:3023","identity_file":"/x","tbot_socket":"/y"}`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку: identity_file и tbot_socket взаимоисключающи")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		t.Setenv(paramsEnv, `{not json`)
		if _, err := loadParams(); err == nil {
			t.Error("ждали ошибку на битый JSON")
		}
	})
}
