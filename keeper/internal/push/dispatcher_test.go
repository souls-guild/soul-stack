package push

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/protojson"
)

// --- моки ---

type mockProvider struct {
	authAllowed bool
	authReason  string
	authErr     error
	signReply   *pluginv1.SignReply
	signErr     error
}

func (m *mockProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	if m.authErr != nil {
		return nil, m.authErr
	}
	return &pluginv1.AuthorizeReply{Allowed: m.authAllowed, Reason: m.authReason}, nil
}

func (m *mockProvider) Sign(_ context.Context, _ *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	if m.signErr != nil {
		return nil, m.signErr
	}
	return m.signReply, nil
}

type mockTargets struct {
	target SSHTarget
	err    error
}

func (m *mockTargets) Resolve(_ context.Context, _ string) (SSHTarget, error) {
	return m.target, m.err
}

type mockSouls struct {
	s   *soul.Soul
	err error
}

func (m *mockSouls) SelectBySID(_ context.Context, _ string) (*soul.Soul, error) {
	return m.s, m.err
}

// mockSession ловит stdin и отдаёт заранее заданный stdout/err.
type mockSession struct {
	stdout   string
	runErr   error
	gotStdin []byte
	gotCmd   string
	closed   bool
}

func (m *mockSession) Run(_ context.Context, cmd string, stdinData []byte) (string, error) {
	m.gotCmd = cmd
	m.gotStdin = stdinData
	return m.stdout, m.runErr
}

func (m *mockSession) Close() error { m.closed = true; return nil }

// testSigner — реальный ed25519-ключ в PEM для Sign-ответа (ssh.ParsePrivateKey
// должен распарсить). Генерируем один раз через ssh.NewSignerFromKey не выйдет —
// нужен PEM; используем фиксированный сгенерированный helper.

func validSignReply(t *testing.T) *pluginv1.SignReply {
	t.Helper()
	return &pluginv1.SignReply{PrivateKey: testEd25519PEM(t), TtlSeconds: 600}
}

func sshTarget() SSHTarget {
	return SSHTarget{Host: "host-1.example.com", Port: 22, User: "soul", SoulPath: "/usr/local/bin/soul"}
}

func sshSoul() *soul.Soul {
	return &soul.Soul{SID: "host-1.example.com", Transport: soul.TransportSSH, Status: soul.StatusPending}
}

// testProviderName — default-имя SshProvider в unit-тестах. Используется
// helper-ами newTestDispatcher / testSendApply, чтобы не повторять литерал.
const testProviderName = "vault-ssh"

// testDispatcherOpts — короткая опц.-форма для newTestDispatcher: тест часто
// настраивает один provider + ровно одну реализацию (mockProvider) и не хочет
// руками собирать map[string]ProviderEntry. Если ProviderEntry-map уже задан в
// Deps.Providers — приоритет за ним (multi-provider тест задаёт его сам).
type testDispatcherOpts struct {
	provider SshProvider
	name     string
	closer   io.Closer
}

func newTestDispatcher(t *testing.T, d Deps, single ...testDispatcherOpts) *SshDispatcher {
	t.Helper()
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if len(d.HostAuthorities) == 0 {
		d.HostAuthorities = []NamedHostKeyAuthority{{
			Name:     "default",
			CAPubKey: testCAPub(t),
		}}
	}
	if d.Providers == nil {
		// Single-provider шорткат: если тест передал testDispatcherOpts —
		// собираем карту из неё, иначе берём дефолтный mockProvider.
		var sp SshProvider
		var closer io.Closer
		name := testProviderName
		if len(single) == 1 {
			sp = single[0].provider
			closer = single[0].closer
			if single[0].name != "" {
				name = single[0].name
			}
		}
		if sp == nil {
			sp = &mockProvider{authAllowed: true}
		}
		d.Providers = map[string]ProviderEntry{name: {Provider: sp, Closer: closer}}
	}
	disp, err := NewSshDispatcher(d)
	if err != nil {
		t.Fatalf("NewSshDispatcher: %v", err)
	}
	return disp
}

func successStdout(t *testing.T, applyID string) string {
	t.Helper()
	ev, _ := protojson.Marshal(&keeperv1.TaskEvent{ApplyId: applyID, Status: keeperv1.TaskStatus_TASK_STATUS_OK})
	rr, _ := protojson.Marshal(&keeperv1.RunResult{ApplyId: applyID, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS})
	return string(ev) + "\n" + string(rr) + "\n"
}

func TestSendApply_HappyPath(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-1")}
	var dialedCfg DialConfig
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial: func(_ context.Context, cfg DialConfig) (Session, error) {
			dialedCfg = cfg
			return sess, nil
		},
	})

	req := &keeperv1.ApplyRequest{ApplyId: "ap-1", Tasks: []*keeperv1.RenderedTask{{Name: "noop", Module: "core.exec.run"}}}
	rr, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, req)
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("RunResult.status = %v, want SUCCESS", rr.GetStatus())
	}

	// ApplyRequest должен уехать в stdin как protojson.
	gotReq := &keeperv1.ApplyRequest{}
	if err := protojson.Unmarshal(sess.gotStdin, gotReq); err != nil {
		t.Fatalf("stdin не protojson ApplyRequest: %v", err)
	}
	if gotReq.GetApplyId() != "ap-1" {
		t.Errorf("stdin apply_id = %q", gotReq.GetApplyId())
	}
	if !strings.Contains(sess.gotCmd, "soul") || !strings.Contains(sess.gotCmd, "apply") {
		t.Errorf("команда = %q, ожидался `soul apply`", sess.gotCmd)
	}
	if !sess.closed {
		t.Error("сессия не закрыта (defer Close)")
	}
	// CA должен доехать до Dial (host-cert verification).
	if len(dialedCfg.HostAuthorities) == 0 {
		t.Error("HostAuthorities не переданы в Dial")
	}
}

func TestSendApply_FailedRunResult_NoTransportError(t *testing.T) {
	// soul apply вернул FAILED+exit1: транспорт ОК, RunResult доставлен.
	ev, _ := protojson.Marshal(&keeperv1.TaskEvent{ApplyId: "ap-2", Status: keeperv1.TaskStatus_TASK_STATUS_FAILED})
	rr, _ := protojson.Marshal(&keeperv1.RunResult{ApplyId: "ap-2", Status: keeperv1.RunStatus_RUN_STATUS_FAILED})
	sess := &mockSession{
		stdout: string(ev) + "\n" + string(rr) + "\n",
		runErr: &ssh.ExitError{}, // непустой exit
	}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})

	got, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-2"})
	if err != nil {
		t.Fatalf("FAILED-прогон с валидным RunResult не должен давать transport-ошибку: %v", err)
	}
	if got.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", got.GetStatus())
	}
}

func TestSendApply_AuthorizeDeny(t *testing.T) {
	dialed := false
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: false, authReason: "policy deny", signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { dialed = true; return &mockSession{}, nil },
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-3"})
	if err == nil {
		t.Fatal("ожидалась ошибка при Authorize deny")
	}
	if !strings.Contains(err.Error(), "Authorize отказал") {
		t.Errorf("ошибка не про deny: %v", err)
	}
	if dialed {
		t.Error("connect не должен происходить после deny (fail-closed до connect-а)")
	}
}

func TestSendApply_ConnectFail(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial: func(_ context.Context, _ DialConfig) (Session, error) {
			return nil, errors.New("connection refused")
		},
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-4"})
	if err == nil {
		t.Fatal("ожидалась ошибка при connect-fail")
	}
	if !strings.Contains(err.Error(), "connect") {
		t.Errorf("ошибка не про connect: %v", err)
	}
}

func TestSendApply_RejectsNonSSHTransport(t *testing.T) {
	agentSoul := &soul.Soul{SID: "host-1.example.com", Transport: soul.TransportAgent, Status: soul.StatusConnected}
	dialed := false
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: agentSoul},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { dialed = true; return &mockSession{}, nil },
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-5"})
	if err == nil {
		t.Fatal("ожидалась ошибка для transport=agent")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("ошибка не про transport: %v", err)
	}
	if dialed {
		t.Error("connect не должен происходить для non-ssh транспорта")
	}
}

func TestSendApply_NoRunResultIsTransportError(t *testing.T) {
	// Поток оборвался до RunResult (краш soul apply) → dispatch-level fail.
	ev, _ := protojson.Marshal(&keeperv1.TaskEvent{ApplyId: "ap-6", Status: keeperv1.TaskStatus_TASK_STATUS_OK})
	sess := &mockSession{stdout: string(ev) + "\n", runErr: &ssh.ExitError{}}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-6"})
	if err == nil {
		t.Fatal("обрыв до RunResult должен быть ошибкой транспорта")
	}
	if !errors.Is(err, ErrNoRunResult) {
		t.Errorf("ошибка не оборачивает ErrNoRunResult: %v", err)
	}
}

func TestSendApply_SignError(t *testing.T) {
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signErr: errors.New("vault down")}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return &mockSession{}, nil },
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-7"})
	if err == nil || !strings.Contains(err.Error(), "Sign") {
		t.Fatalf("ожидалась Sign-ошибка, got %v", err)
	}
}

// vaultStyleSignReply имитирует ответ Vault SSH CA-провайдера: только
// certificate, private_key="". Подписант — ephemeral signer, который генерит
// dispatcher. Сертификат подписан caSigner на ephPub (для CA-провайдеров
// host-CA и user-CA — разные сущности; здесь caSigner играет роль user-CA).
func vaultStyleCertOnPub(t *testing.T, ephPubAuthorized string) string {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ephPubAuthorized))
	if err != nil {
		t.Fatalf("parse eph pub: %v", err)
	}
	caSigner, _ := testCAKey(t)
	cert := &ssh.Certificate{
		Key:             pub,
		CertType:        ssh.UserCert,
		ValidPrincipals: []string{"soul"},
		ValidAfter:      0,
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, caSigner); err != nil {
		t.Fatalf("sign user cert: %v", err)
	}
	return string(ssh.MarshalAuthorizedKey(cert))
}

// signCapturingProvider — мок Provider, который запоминает SignRequest для
// проверки, что dispatcher положил туда ephemeral-pubkey, и формирует ответ
// в зависимости от полученного public_key (Vault-style: cert на этой pubkey).
type signCapturingProvider struct {
	t         *testing.T
	gotReq    *pluginv1.SignRequest
	authReply *pluginv1.AuthorizeReply
	makeReply func(t *testing.T, req *pluginv1.SignRequest) *pluginv1.SignReply
}

func (p *signCapturingProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	if p.authReply != nil {
		return p.authReply, nil
	}
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

func (p *signCapturingProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	p.gotReq = req
	return p.makeReply(p.t, req), nil
}

// TestSendApply_VaultEphemeralMode — Vault SSH CA-режим: SignReply без
// private_key + certificate на ephemeral-pubkey, который Keeper передал в
// SignRequest. Должно собраться без ошибок и доехать до RunResult.
func TestSendApply_VaultEphemeralMode(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-vault-1")}
	prov := &signCapturingProvider{
		t: t,
		makeReply: func(t *testing.T, req *pluginv1.SignRequest) *pluginv1.SignReply {
			return &pluginv1.SignReply{
				Certificate: vaultStyleCertOnPub(t, req.GetPublicKey()),
				PrivateKey:  "", // канонический Vault-flow
				TtlSeconds:  1800,
			}
		},
	}
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: prov}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { return sess, nil },
	})

	rr, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-vault-1"})
	if err != nil {
		t.Fatalf("SendApply (vault-mode): %v", err)
	}
	if rr.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", rr.GetStatus())
	}
	// Проверка S2-инварианта: dispatcher положил непустой OpenSSH-pubkey в
	// SignRequest.public_key (без него Vault SSH CA не сможет подписать).
	if prov.gotReq == nil || prov.gotReq.GetPublicKey() == "" {
		t.Fatal("SignRequest.public_key пуст — dispatcher не передал ephemeral pubkey")
	}
	if _, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(prov.gotReq.GetPublicKey())); perr != nil {
		t.Errorf("ephemeral pubkey не парсится как OpenSSH authorized_key: %v", perr)
	}
}

// TestSendApply_VaultEphemeralMode_RejectsEmptyCert — Vault-стиль: private_key
// пуст И certificate пуст → fail-closed (нечем подписать handshake).
func TestSendApply_VaultEphemeralMode_RejectsEmptyCert(t *testing.T) {
	dialed := false
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: &pluginv1.SignReply{PrivateKey: "", Certificate: ""}}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial:      func(_ context.Context, _ DialConfig) (Session, error) { dialed = true; return &mockSession{}, nil },
	})
	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-vault-2"})
	if err == nil {
		t.Fatal("ждали ошибку: SignReply с пустыми private_key и certificate должен отвергаться (fail-closed)")
	}
	if dialed {
		t.Error("connect не должен происходить, если нечем подписать handshake")
	}
}

// TestAuthMethodsFromSign_EphemeralPrivateKeyNotLeaked — приватник ephemeral
// keypair не должен попадать ни в SignRequest, ни в ошибки. Проверяем, что:
//   - SignRequest.public_key — только pubkey (никакого `BEGIN PRIVATE KEY`);
//   - ошибочный путь (битый cert) не подставляет приватник в error-message.
func TestAuthMethodsFromSign_EphemeralPrivateKeyNotLeaked(t *testing.T) {
	signer, pubAuth, err := newEphemeralEd25519()
	if err != nil {
		t.Fatalf("newEphemeralEd25519: %v", err)
	}
	if strings.Contains(pubAuth, "PRIVATE KEY") {
		t.Errorf("ephemeral authorized-key содержит PRIVATE KEY — утечка: %q", pubAuth)
	}
	// Битый cert + valid ephSigner → ошибка должна быть про разбор cert, без
	// приватника в тексте.
	_, err = authMethodsFromSign(&pluginv1.SignReply{Certificate: "not a cert", PrivateKey: ""}, signer)
	if err == nil {
		t.Fatal("ждали ошибку на битый cert")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), "BEGIN OPENSSH PRIVATE") {
		t.Errorf("error-сообщение содержит приватник: %q", err.Error())
	}
}

// TestAuthMethodsFromSign_StaticFlowIgnoresEphSigner — обратная совместимость:
// если private_key непуст, dispatcher должен собрать ssh.AuthMethod-ы из ключа
// плагина, ephSigner игнорировать. Это гарантия не-сломанного static-провайдера.
func TestAuthMethodsFromSign_StaticFlowIgnoresEphSigner(t *testing.T) {
	staticReply := &pluginv1.SignReply{PrivateKey: testEd25519PEM(t), Certificate: ""}
	ephSigner, _, err := newEphemeralEd25519()
	if err != nil {
		t.Fatalf("newEphemeralEd25519: %v", err)
	}
	auth, err := authMethodsFromSign(staticReply, ephSigner)
	if err != nil {
		t.Fatalf("static-flow: %v", err)
	}
	if len(auth) != 1 {
		t.Errorf("ждали ровно один AuthMethod, got %d", len(auth))
	}
	// Без ephSigner тот же reply должен работать (явный тест регрессии S0).
	if _, err := authMethodsFromSign(staticReply, nil); err != nil {
		t.Errorf("static-flow без ephSigner сломался: %v", err)
	}
}

// TestSendApply_ProxyJumpPropagatedToDial — dispatcher должен класть
// SignReply.proxy_jump в DialConfig.ProxyJump (без правки Auth: тот же signed
// cert идёт на оба хопа Teleport-flow).
func TestSendApply_ProxyJumpPropagatedToDial(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-pj-1")}
	prov := &signCapturingProvider{
		t: t,
		makeReply: func(t *testing.T, req *pluginv1.SignRequest) *pluginv1.SignReply {
			return &pluginv1.SignReply{
				Certificate: vaultStyleCertOnPub(t, req.GetPublicKey()),
				ProxyJump:   "teleport.example.com:3023",
				TtlSeconds:  1800,
			}
		},
	}
	var dialedCfg DialConfig
	disp := newTestDispatcher(t, Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: prov}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial: func(_ context.Context, cfg DialConfig) (Session, error) {
			dialedCfg = cfg
			return sess, nil
		},
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-pj-1"})
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if dialedCfg.ProxyJump != "teleport.example.com:3023" {
		t.Errorf("DialConfig.ProxyJump = %q, want %q", dialedCfg.ProxyJump, "teleport.example.com:3023")
	}
	if dialedCfg.Host != "host-1.example.com" || dialedCfg.Port != 22 {
		t.Errorf("target изменился: host=%q port=%d", dialedCfg.Host, dialedCfg.Port)
	}
	// Auth — тот же набор, что для direct-flow (один user-cert на оба хопа).
	if len(dialedCfg.Auth) != 1 {
		t.Errorf("Auth len = %d, want 1", len(dialedCfg.Auth))
	}
}

// TestSendApply_ProxyJumpEmpty_DirectFlow — S0-regression: пустой proxy_jump в
// SignReply не должен попадать в DialConfig.ProxyJump (=> direct dial).
func TestSendApply_ProxyJumpEmpty_DirectFlow(t *testing.T) {
	sess := &mockSession{stdout: successStdout(t, "ap-pj-empty")}
	var dialedCfg DialConfig
	disp := newTestDispatcher(t, Deps{
		// PrivateKey set, ProxyJump=""
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{authAllowed: true, signReply: validSignReply(t)}}},
		Targets:   &mockTargets{target: sshTarget()},
		Souls:     &mockSouls{s: sshSoul()},
		Dial: func(_ context.Context, cfg DialConfig) (Session, error) {
			dialedCfg = cfg
			return sess, nil
		},
	})

	_, err := disp.SendApply(context.Background(), "host-1.example.com", testProviderName, &keeperv1.ApplyRequest{ApplyId: "ap-pj-empty"})
	if err != nil {
		t.Fatalf("SendApply: %v", err)
	}
	if dialedCfg.ProxyJump != "" {
		t.Errorf("пустой SignReply.proxy_jump утёк в DialConfig.ProxyJump=%q (S0-regression)", dialedCfg.ProxyJump)
	}
}

func TestNewSshDispatcher_Validation(t *testing.T) {
	base := Deps{
		Providers: map[string]ProviderEntry{testProviderName: {Provider: &mockProvider{}}},
		Targets:   &mockTargets{},
		Souls:     &mockSouls{},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostAuthorities: []NamedHostKeyAuthority{{
			Name:     "default",
			CAPubKey: testCAPub(t),
		}},
	}
	if _, err := NewSshDispatcher(base); err != nil {
		t.Fatalf("валидный Deps отвергнут: %v", err)
	}

	noCA := base
	noCA.HostAuthorities = nil
	if _, err := NewSshDispatcher(noCA); err == nil {
		t.Error("Deps без CA должен отвергаться (fail-closed host-cert verify)")
	}

	emptyName := base
	emptyName.HostAuthorities = []NamedHostKeyAuthority{{Name: "", CAPubKey: testCAPub(t)}}
	if _, err := NewSshDispatcher(emptyName); err == nil {
		t.Error("Deps c пустым CA.Name должен отвергаться")
	}

	nilKey := base
	nilKey.HostAuthorities = []NamedHostKeyAuthority{{Name: "x", CAPubKey: nil}}
	if _, err := NewSshDispatcher(nilKey); err == nil {
		t.Error("Deps c nil CAPubKey должен отвергаться")
	}

	noProviders := base
	noProviders.Providers = nil
	if _, err := NewSshDispatcher(noProviders); err == nil {
		t.Error("Deps без Providers должен отвергаться")
	}

	emptyProviders := base
	emptyProviders.Providers = map[string]ProviderEntry{}
	if _, err := NewSshDispatcher(emptyProviders); err == nil {
		t.Error("Deps с пустой Providers-map должен отвергаться")
	}

	nilProvider := base
	nilProvider.Providers = map[string]ProviderEntry{testProviderName: {Provider: nil}}
	if _, err := NewSshDispatcher(nilProvider); err == nil {
		t.Error("Deps с nil-Provider в записи должен отвергаться")
	}
}
