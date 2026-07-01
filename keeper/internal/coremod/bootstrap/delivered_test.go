package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cloudinit"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- test-doubles ---

// fakeProvider — мок push.SshProvider. Записывает вызовы Authorize/Sign и
// позволяет отвергать (deny / ошибка) для проверки fail-closed.
type fakeProvider struct {
	allow      bool
	denyReason string
	authErr    error
	signErr    error

	authCalls []*pluginv1.AuthorizeRequest
	signCalls []*pluginv1.SignRequest
}

func (p *fakeProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	p.authCalls = append(p.authCalls, req)
	if p.authErr != nil {
		return nil, p.authErr
	}
	return &pluginv1.AuthorizeReply{Allowed: p.allow, Reason: p.denyReason}, nil
}

func (p *fakeProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	p.signCalls = append(p.signCalls, req)
	if p.signErr != nil {
		return nil, p.signErr
	}
	// static-режим: возвращаем валидный ed25519 private key, чтобы
	// push.AuthMethodsFromSign построил ssh.AuthMethod без ephemeral-cert.
	return &pluginv1.SignReply{PrivateKey: testPrivateKeyPEM}, nil
}

// runCall — один Session.Run: команда + stdin (для проверки «токен в STDIN, не в argv»).
type runCall struct {
	cmd   string
	stdin []byte
}

// fakeSession — мок push.Session. Захватывает все Run; опц. ошибка на N-м вызове.
type fakeSession struct {
	calls    []runCall
	failAtN  int // 1-based индекс Run, который вернёт ошибку; 0 — без ошибок
	closed   bool
	runError error
}

func (s *fakeSession) Run(_ context.Context, cmd string, stdin []byte) (string, error) {
	s.calls = append(s.calls, runCall{cmd: cmd, stdin: append([]byte(nil), stdin...)})
	if s.failAtN != 0 && len(s.calls) == s.failAtN {
		return "", s.runError
	}
	return "", nil
}

func (s *fakeSession) Close() error { s.closed = true; return nil }

// dialRecorder — мок push.Dialer: возвращает заранее заданную сессию, пишет cfg.
type dialRecorder struct {
	sess    *fakeSession
	dialErr error
	lastCfg push.DialConfig
	dialCnt int
}

func (d *dialRecorder) dial(_ context.Context, cfg push.DialConfig) (push.Session, error) {
	d.dialCnt++
	d.lastCfg = cfg
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return d.sess, nil
}

type fakeAudit struct {
	events []*audit.Event
}

func (a *fakeAudit) Write(_ context.Context, e *audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

// fakeInstallResolver — мок coremodbootstrap.InstallResolver: отдаёт
// заранее заданный cloudinit.Config (или ошибку) для install-режима. Записывает
// число вызовов: install-blueprint резолвится ОДИН раз на шаг, не per-host.
type fakeInstallResolver struct {
	cfg   cloudinit.Config
	err   error
	calls int
}

func (r *fakeInstallResolver) Resolve(_ context.Context) (cloudinit.Config, error) {
	r.calls++
	if r.err != nil {
		return cloudinit.Config{}, r.err
	}
	return r.cfg, nil
}

// testCAPem — минимальный PEM-маркер, удовлетворяющий soulinstall.Blueprint.Validate
// (проверяет наличие подстроки "BEGIN CERTIFICATE", не парсит cert). Содержимое
// неважно: install-шаги мокаются, реального TLS-handshake нет.
const testCAPem = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

// validInstallConfig — cloudinit.Config, проходящий blueprint-валидацию
// (host:port endpoint, PEM-CA, https-URL). База для install-guard-тестов.
func validInstallConfig() cloudinit.Config {
	return cloudinit.Config{
		BootstrapEndpoint: "keeper.example.com:8443",
		TLSCAPem:          testCAPem,
		SoulBinaryURL:     "https://nexus.example.com/soul",
		SoulVersion:       "v1.2.3",
	}
}

// testPrivateKeyPEM — фиксированный ed25519 private key (OpenSSH PEM) для
// static-режима SignReply в тестах. Сгенерирован разово ssh-keygen; не
// используется ни для чего, кроме как удовлетворить ssh.ParsePrivateKey.
const testPrivateKeyPEM = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDMq91FxvZmBhL9iPmqOXxepRSXvF5305Sqw6X3hyfvbgAAAJh3XT4wd10+
MAAAAAtzc2gtZWQyNTUxOQAAACDMq91FxvZmBhL9iPmqOXxepRSXvF5305Sqw6X3hyfvbg
AAAEC8n7wb1918K3/nfl6TqXblQOA/c0VAprQjHM1tW8zvIMyr3UXG9mYGEv2I+ao5fF6l
FJe8XnfTlKrDpfeHJ+9uAAAAD3Jvb3RAc291bC1zdGFjawECAwQFBg==
-----END OPENSSH PRIVATE KEY-----
`

// caAuthorities — непустой host-CA-набор для verify (конкретное значение
// неважно: Dial мокается, реальная проверка host-cert не выполняется; важна
// лишь непустота набора — gate fail-closed).
func caAuthorities(t *testing.T) []push.NamedHostKeyAuthority {
	t.Helper()
	signer, _, err := push.NewEphemeralEd25519()
	if err != nil {
		t.Fatalf("NewEphemeralEd25519: %v", err)
	}
	return []push.NamedHostKeyAuthority{{Name: "test", CAPubKey: signer.PublicKey()}}
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// hostEntry — один элемент списка hosts в форме register.<provision>.hosts.
func hostEntry(sid, ip, token string) map[string]any {
	return map[string]any{
		"sid":             sid,
		"primary_ip":      ip,
		"bootstrap_token": token,
	}
}

// hostsParam — один хост в форме register.<provision>.hosts.
func hostsParam(sid, ip, token string) []any {
	return []any{hostEntry(sid, ip, token)}
}

// newModule собирает Module с одним провайдером "ssh-static" и мок-Dialer.
func newModule(t *testing.T, prov coremodbootstrap.SshProviderHost, dialer push.Dialer, a coremodbootstrap.AuditWriter) *coremodbootstrap.Module {
	t.Helper()
	return &coremodbootstrap.Module{
		Providers: map[string]coremodbootstrap.SshProviderHost{"ssh-static": prov},
		HostCAs:   caAuthorities(t),
		Dial:      dialer,
		Audit:     a,
	}
}

func deliverReq(t *testing.T, params map[string]any) *pluginv1.ApplyRequest {
	t.Helper()
	return &pluginv1.ApplyRequest{State: coremodbootstrap.StateDelivered, Params: mustStruct(t, params)}
}

// --- tests ---

func TestApply_UnknownState(t *testing.T) {
	m := newModule(t, &fakeProvider{allow: true}, (&dialRecorder{sess: &fakeSession{}}).dial, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{State: "frobnicated", Params: mustStruct(t, nil)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event for unknown state, got %+v", stream.Last())
	}
}

func TestApply_AuthorizeDeny_FailClosed(t *testing.T) {
	prov := &fakeProvider{allow: false, denyReason: "not in policy"}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok-aaa"),
	}), stream)
	if err != nil {
		t.Fatalf("Apply returned transport error: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event on Authorize deny, got %+v", last)
	}
	// fail-closed: deny прерывает ДО Sign и ДО Dial.
	if len(prov.signCalls) != 0 {
		t.Errorf("Sign called %d times despite Authorize deny — must be 0", len(prov.signCalls))
	}
	if dialer.dialCnt != 0 {
		t.Errorf("Dial called %d times despite Authorize deny — must be 0", dialer.dialCnt)
	}
	if sess.closed {
		t.Errorf("session was opened/closed despite Authorize deny")
	}
}

func TestApply_Success_TokenInStdinNotArgv(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	aud := &fakeAudit{}
	m := newModule(t, prov, dialer.dial, aud)

	const token = "tok-secret-zzz"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		// start_soul по умолчанию true — не задаём, проверим started=true ниже.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("expected success final event, got %+v", last)
	}
	if !last.GetChanged() {
		t.Errorf("changed must be true on delivery")
	}

	// Authorize + Sign вызваны ровно раз.
	if len(prov.authCalls) != 1 {
		t.Fatalf("Authorize calls = %d, want 1", len(prov.authCalls))
	}
	if len(prov.signCalls) != 1 {
		t.Fatalf("Sign calls = %d, want 1", len(prov.signCalls))
	}
	// Sign получил публичный ephemeral-ключ (не пустой).
	if prov.signCalls[0].GetPublicKey() == "" {
		t.Errorf("Sign got empty ephemeral public key")
	}

	// Две команды: запись токена (со stdin) + systemctl start soul (start_soul default true).
	if len(sess.calls) != 2 {
		t.Fatalf("Session.Run calls = %d, want 2 (write token + start soul); calls=%v", len(sess.calls), cmds(sess))
	}
	writeCall := sess.calls[0]
	// ★ Токен — в STDIN.
	if string(writeCall.stdin) != token {
		t.Errorf("token not in stdin: got %q, want %q", writeCall.stdin, token)
	}
	// ★ Токен НЕ в argv (команде).
	if strings.Contains(writeCall.cmd, token) {
		t.Errorf("SECURITY: token leaked into argv: cmd=%q", writeCall.cmd)
	}
	// Команда пишет в дефолтный token_path через `cat >`.
	if !strings.Contains(writeCall.cmd, "/etc/soul/token") || !strings.Contains(writeCall.cmd, "cat >") {
		t.Errorf("write command unexpected: %q", writeCall.cmd)
	}
	if sess.calls[1].cmd != "systemctl start soul" {
		t.Errorf("second command = %q, want 'systemctl start soul'", sess.calls[1].cmd)
	}
	if len(sess.calls[1].stdin) != 0 {
		t.Errorf("start-soul command must have empty stdin, got %q", sess.calls[1].stdin)
	}
	if !sess.closed {
		t.Errorf("session not closed")
	}

	// Dial получил primary_ip как Host и непустой host-CA-набор.
	if dialer.lastCfg.Host != "10.0.0.1" {
		t.Errorf("Dial Host = %q, want primary_ip 10.0.0.1", dialer.lastCfg.Host)
	}
	if len(dialer.lastCfg.HostAuthorities) == 0 {
		t.Errorf("Dial HostAuthorities empty — CA-verify не передан")
	}
	if dialer.lastCfg.Port != 22 || dialer.lastCfg.User != "root" {
		t.Errorf("Dial defaults wrong: port=%d user=%q (want 22/root)", dialer.lastCfg.Port, dialer.lastCfg.User)
	}
}

func TestApply_Output_NoToken(t *testing.T) {
	prov := &fakeProvider{allow: true}
	dialer := &dialRecorder{sess: &fakeSession{}}
	aud := &fakeAudit{}
	m := newModule(t, prov, dialer.dial, aud)

	const token = "tok-must-not-appear"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}

	// ★ Output не содержит токена нигде.
	out := last.GetOutput().AsMap()
	if containsTokenDeep(out, token) {
		t.Fatalf("SECURITY: token leaked into register output: %+v", out)
	}
	// Output несёт hosts[]={sid,delivered,started} + count.
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("output hosts = %v, want 1 entry", out["hosts"])
	}
	h0, _ := hosts[0].(map[string]any)
	if h0["sid"] != "vm1.example.com" || h0["delivered"] != true || h0["started"] != true {
		t.Errorf("output host entry wrong: %+v", h0)
	}
	if out["count"] != float64(1) {
		t.Errorf("output count = %v, want 1", out["count"])
	}

	// ★ Audit-payload — без токена, только count + sids.
	if len(aud.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aud.events))
	}
	ev := aud.events[0]
	if ev.EventType != audit.EventBootstrapDelivered {
		t.Errorf("audit event_type = %q, want %q", ev.EventType, audit.EventBootstrapDelivered)
	}
	if containsTokenDeep(ev.Payload, token) {
		t.Fatalf("SECURITY: token leaked into audit payload: %+v", ev.Payload)
	}
	if ev.Payload["count"] != float64(1) {
		t.Errorf("audit count = %v, want 1", ev.Payload["count"])
	}
}

func TestApply_StartSoulFalse_SkipsStart(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// Только запись токена, без systemctl start.
	if len(sess.calls) != 1 {
		t.Fatalf("Session.Run calls = %d, want 1 (write only); cmds=%v", len(sess.calls), cmds(sess))
	}
	hosts, _ := last.GetOutput().AsMap()["hosts"].([]any)
	h0, _ := hosts[0].(map[string]any)
	if h0["started"] != false {
		t.Errorf("started = %v, want false when start_soul=false", h0["started"])
	}
}

func TestApply_CustomTokenPathAndUserPort(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"token_path":   "/opt/soul/seed",
		"ssh_user":     "deploy",
		"ssh_port":     float64(2222),
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	if !strings.Contains(sess.calls[0].cmd, "/opt/soul/seed") {
		t.Errorf("custom token_path not used: %q", sess.calls[0].cmd)
	}
	if dialer.lastCfg.User != "deploy" || dialer.lastCfg.Port != 2222 {
		t.Errorf("custom ssh_user/ssh_port not used: user=%q port=%d", dialer.lastCfg.User, dialer.lastCfg.Port)
	}
}

func TestApply_HostError_FailsStep_B1Strict(t *testing.T) {
	prov := &fakeProvider{allow: true}
	// Запись токена (1-я команда) падает.
	sess := &fakeSession{failAtN: 1, runError: errors.New("disk full")}
	dialer := &dialRecorder{sess: sess}
	aud := &fakeAudit{}
	m := newModule(t, prov, dialer.dial, aud)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event on host error (B1-strict), got %+v", last)
	}
	// На провале доставки audit НЕ пишется (шаг failed до audit).
	if len(aud.events) != 0 {
		t.Errorf("audit written despite delivery failure: %+v", aud.events)
	}
}

func TestApply_UnknownProvider_Fails(t *testing.T) {
	prov := &fakeProvider{allow: true}
	dialer := &dialRecorder{sess: &fakeSession{}}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-nonexistent",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event for unknown provider, got %+v", last)
	}
}

func TestApply_NoHostCAs_Fails(t *testing.T) {
	prov := &fakeProvider{allow: true}
	dialer := &dialRecorder{sess: &fakeSession{}}
	m := &coremodbootstrap.Module{
		Providers: map[string]coremodbootstrap.SshProviderHost{"ssh-static": prov},
		HostCAs:   nil, // не сконфигурировано → fail-closed
		Dial:      dialer.dial,
	}
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event without host CAs, got %+v", last)
	}
}

func TestApply_EmptyHosts_Fails(t *testing.T) {
	prov := &fakeProvider{allow: true}
	dialer := &dialRecorder{sess: &fakeSession{}}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        []any{},
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event for empty hosts, got %+v", last)
	}
}

func TestValidate(t *testing.T) {
	m := &coremodbootstrap.Module{}
	// Корректный.
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider": "ssh-static",
			"hosts":        hostsParam("vm1", "1.2.3.4", "t"),
		}),
	})
	if !rep.Ok {
		t.Errorf("valid params rejected: %v", rep.Errors)
	}
	// Без ssh_provider и hosts.
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Errorf("missing ssh_provider/hosts accepted")
	}
	// Неверный state.
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Errorf("wrong state accepted")
	}
	// ssh_port вне диапазона.
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider": "ssh-static",
			"hosts":        hostsParam("vm1", "1.2.3.4", "t"),
			"ssh_port":     float64(70000),
		}),
	})
	if rep.Ok {
		t.Errorf("ssh_port=70000 accepted")
	}
	// join_wait_timeout < 0 — отвергается (потолок ожидания не может быть отрицательным).
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider":      "ssh-static",
			"hosts":             hostsParam("vm1", "1.2.3.4", "t"),
			"join_wait_timeout": float64(-1),
		}),
	})
	if rep.Ok {
		t.Errorf("join_wait_timeout=-1 accepted")
	}
	// join_wait_timeout == 0 — принимается (немедленный deadline: одна попытка без ожидания).
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider":      "ssh-static",
			"hosts":             hostsParam("vm1", "1.2.3.4", "t"),
			"join_wait_timeout": float64(0),
		}),
	})
	if !rep.Ok {
		t.Errorf("join_wait_timeout=0 rejected: %v", rep.Errors)
	}
	// ★ install=true в direct-режиме (m.Transport=="") — отвергается: setch ставит
	// cloud-init, install не нужен (ADR-063 amendment, MVP только teleport).
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider": "ssh-static",
			"hosts":        hostsParam("vm1", "1.2.3.4", "t"),
			"install":      true,
		}),
	})
	if rep.Ok {
		t.Errorf("install=true accepted in direct transport")
	}
	// install=false в direct — принимается (token-only, backward-compat).
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider": "ssh-static",
			"hosts":        hostsParam("vm1", "1.2.3.4", "t"),
			"install":      false,
		}),
	})
	if !rep.Ok {
		t.Errorf("install=false rejected in direct: %v", rep.Errors)
	}
	// ★ install=true в teleport-режиме — принимается.
	tm := &coremodbootstrap.Module{Transport: coremodbootstrap.TransportTeleport}
	rep, _ = tm.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{
			"ssh_provider": "ssh-static",
			"hosts":        hostsParam("vm1", "1.2.3.4", "t"),
			"install":      true,
		}),
	})
	if !rep.Ok {
		t.Errorf("install=true rejected in teleport transport: %v", rep.Errors)
	}
}

// --- teleport-transport guard tests (ADR-063 amendment) ---

// newTeleportModule собирает Module в teleport-режиме: пустые Providers/HostCAs
// (в teleport не используются), только Dial.
func newTeleportModule(dialer push.Dialer, a coremodbootstrap.AuditWriter) *coremodbootstrap.Module {
	return &coremodbootstrap.Module{
		Transport: coremodbootstrap.TransportTeleport,
		Dial:      dialer,
		Audit:     a,
	}
}

// TestApply_Teleport_DialsBySID — guard #1 (sid-адресация): в teleport-режиме
// DialConfig.Host == h.sid (node-name), НЕ primary_ip; Authorize/Sign не зовутся.
func TestApply_Teleport_DialsBySID(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Provider всё равно объявлен (Validate требует ssh_provider в params), но
	// в teleport-режиме он не должен дёргаться.
	m.Providers = map[string]coremodbootstrap.SshProviderHost{"ssh-static": prov}

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok-aaa"),
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// ★ target = SID, НЕ primary_ip.
	if dialer.lastCfg.Host != "vm1.example.com" {
		t.Errorf("teleport Dial Host = %q, want SID vm1.example.com (NOT primary_ip)", dialer.lastCfg.Host)
	}
	// ★ В teleport-режиме DialConfig не несёт Auth/HostAuthorities/ProxyJump.
	if dialer.lastCfg.Auth != nil {
		t.Errorf("teleport DialConfig.Auth must be nil, got %v", dialer.lastCfg.Auth)
	}
	if len(dialer.lastCfg.HostAuthorities) != 0 {
		t.Errorf("teleport DialConfig.HostAuthorities must be empty, got %v", dialer.lastCfg.HostAuthorities)
	}
	if dialer.lastCfg.ProxyJump != "" {
		t.Errorf("teleport DialConfig.ProxyJump must be empty, got %q", dialer.lastCfg.ProxyJump)
	}
	// ★ Authorize/Sign НЕ вызывались.
	if len(prov.authCalls) != 0 || len(prov.signCalls) != 0 {
		t.Errorf("teleport must not call Authorize/Sign: authorize=%d sign=%d", len(prov.authCalls), len(prov.signCalls))
	}
}

// TestApply_Direct_DialsByIPAndAuthorizes — guard #2 (transport-selection,
// direct-half): direct-режим адресует по primary_ip, зовёт Authorize/Sign и
// передаёт host-CA. (teleport-half покрыт TestApply_Teleport_DialsBySID.)
func TestApply_Direct_DialsByIPAndAuthorizes(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	// Дефолт Transport ("") => direct.
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	if dialer.lastCfg.Host != "10.0.0.1" {
		t.Errorf("direct Dial Host = %q, want primary_ip 10.0.0.1", dialer.lastCfg.Host)
	}
	if len(prov.authCalls) != 1 || len(prov.signCalls) != 1 {
		t.Errorf("direct must call Authorize+Sign once each: authorize=%d sign=%d", len(prov.authCalls), len(prov.signCalls))
	}
	if len(dialer.lastCfg.HostAuthorities) == 0 {
		t.Errorf("direct must pass host-CA in DialConfig.HostAuthorities")
	}
}

// TestApply_Teleport_RetriesUntilJoin — guard #3 (retry-до-join): первые N
// Dial-ов падают (VM ещё не в Teleport), затем успех — шаг НЕ валится на 1й
// ошибке, доходит до success.
func TestApply_Teleport_RetriesUntilJoin(t *testing.T) {
	sess := &fakeSession{}
	dialer := &flakyDialer{sess: sess, failFirst: 3, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Короткий backoff, чтобы 3 ретрая не спали реальные ~12с (поле существует
	// ровно ради этого, см. Module.RetryBase).
	m.RetryBase = 1 * time.Millisecond
	m.RetryJitter = 1 * time.Millisecond

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider":      "ssh-static",
		"hosts":             hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":        false,
		"join_wait_timeout": float64(60),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || last.GetFailed() {
		t.Fatalf("expected success after retries, got %+v", last)
	}
	// ★ Шаг НЕ упал на 1й ошибке — было >=4 попытки (3 fail + успех).
	if dialer.calls < 4 {
		t.Errorf("expected >=4 Dial attempts (3 fail + success), got %d", dialer.calls)
	}
}

// TestApply_Teleport_FailsAfterDeadline — guard #3 (deadline-half): если VM так
// и не появилась в Teleport до join_wait_timeout — шаг failed (B1-strict,
// error_locked), не висит вечно.
func TestApply_Teleport_FailsAfterDeadline(t *testing.T) {
	sess := &fakeSession{}
	// Всегда падает.
	dialer := &flakyDialer{sess: sess, failFirst: 1 << 30, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	// join_wait_timeout=0 → дедлайн сразу: первая попытка падает, второй
	// интервал не помещается в бюджет → failed без долгого ожидания.
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider":      "ssh-static",
		"hosts":             hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":        false,
		"join_wait_timeout": float64(0),
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event after join deadline, got %+v", last)
	}
}

// TestApply_Teleport_CtxCancel_NoHang — guard: если прогон прерван (ctx
// отменён) ПОСРЕДИ retry-до-join, шаг выходит быстро с ctx-ошибкой, НЕ висит до
// join_wait_timeout. Dialer всегда fail; ctx отменяется коротким timeout-ом,
// бюджет join_wait выставлен большим (60с) — выход обязан случиться по ctx, а не
// по deadline.
func TestApply_Teleport_CtxCancel_NoHang(t *testing.T) {
	dialer := &alwaysFailDialer{err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Мелкий backoff → несколько итераций до отмены (ловим cancel в середине, а
	// не на самой первой попытке).
	m.RetryBase = 2 * time.Millisecond
	m.RetryJitter = 1 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	stream := internaltest.NewApplyStreamCtx(ctx)

	start := time.Now()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider":      "ssh-static",
		"hosts":             hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":        false,
		"join_wait_timeout": float64(60), // большой бюджет: выход обязан быть по ctx, не по deadline
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	elapsed := time.Since(start)

	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event on ctx cancel, got %+v", last)
	}
	// ★ Не зависание: вышли близко к timeout-у (40мс), далеко от join_wait (60с).
	if elapsed > 5*time.Second {
		t.Fatalf("ctx cancel did not short-circuit retry: elapsed=%s (join_wait was 60s)", elapsed)
	}
	// Ретраи реально шли (cancel поймали в середине, а не до первой попытки).
	if dialer.calls < 1 {
		t.Errorf("expected at least 1 Dial attempt before cancel, got %d", dialer.calls)
	}
}

// TestApply_Teleport_MultiHost_PerHostDeadline — guard #5 (multi-host, B1-strict):
// ≥2 хоста в одном deliver, transport=teleport. Один (vm1) join-ится сразу,
// второй (vm2) всегда fail → весь шаг failed ПОСЛЕ исчерпания join_wait именно
// второго хоста.
//
// ★ Фактическое поведение (зафиксировано КАК ЕСТЬ): дедлайн per-host
// independent, НЕ общий бюджет. dialWithJoinRetry вычисляет
// `deadline = time.Now().Add(joinWait)` при КАЖДОМ вызове (на каждый хост),
// поэтому хосты обрабатываются последовательно и каждый получает свой полный
// join_wait. Первый хост доставляется (1 Run — запись токена, start_soul:false),
// затем шаг валится на втором. join_wait=0 → дедлайн второго истекает сразу
// (одна fail-попытка), тест не платит реальные минуты.
func TestApply_Teleport_MultiHost_PerHostDeadline(t *testing.T) {
	const okSID, failSID = "vm1.example.com", "vm2.example.com"
	dialer := &perHostDialer{okSID: okSID, err: errors.New("node offline or does not exist")}
	aud := &fakeAudit{}
	m := newTeleportModule(dialer.dial, aud)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts": []any{
			hostEntry(okSID, "10.0.0.1", "tok-1"),
			hostEntry(failSID, "10.0.0.2", "tok-2"),
		},
		"start_soul":        false,
		"join_wait_timeout": float64(0), // дедлайн второго истекает сразу
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// ★ Весь шаг failed (B1-strict): второй хост не доехал.
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event when one of N hosts never joins, got %+v", last)
	}
	// failed-event указывает на провалившийся хост (vm2), не на первый.
	if det := last.GetMessage(); det != "" && !strings.Contains(det, failSID) {
		t.Errorf("failed message does not name the failing host %q: %q", failSID, det)
	}

	// ★ Первый хост был доставлен ДО провала второго: ровно один Dial вернул
	// сессию (okSID), на ней прошла запись токена (1 Run, start_soul:false).
	okSess, ok := dialer.byHost[okSID]
	if !ok || okSess == nil {
		t.Fatalf("first host %q was never dialed successfully (per-host independent deadline expected)", okSID)
	}
	if len(okSess.calls) != 1 {
		t.Errorf("first host: Session.Run calls = %d, want 1 (token write, start_soul=false); cmds=%v", len(okSess.calls), cmds(okSess))
	}
	if string(okSess.calls[0].stdin) != "tok-1" {
		t.Errorf("first host token not delivered via stdin: got %q, want tok-1", okSess.calls[0].stdin)
	}
	if !okSess.closed {
		t.Errorf("first host session not closed")
	}

	// На провале шага audit НЕ пишется (B1-strict: failed до audit-секции).
	if len(aud.events) != 0 {
		t.Errorf("audit written despite step failure: %+v", aud.events)
	}
}

// --- install-mode guard tests (ADR-063 amendment «full-install over SSH») ---

// newTeleportInstallModule — teleport-режим + install-резолвер (full-install).
func newTeleportInstallModule(dialer push.Dialer, inst coremodbootstrap.InstallResolver, a coremodbootstrap.AuditWriter) *coremodbootstrap.Module {
	return &coremodbootstrap.Module{
		Transport: coremodbootstrap.TransportTeleport,
		Dial:      dialer,
		Install:   inst,
		Audit:     a,
	}
}

// TestApply_InstallOmitted_TokenOnly_BitForBit — guard (a), реверс-guard: без
// param `install` (опущен) поведение БИТ-В-БИТ token-only — НИ ОДНОГО install-шага,
// install-резолвер НЕ дёргается. Ловит регресс, если install выполнился бы
// безусловно. teleport-режим (install-режим только там), Install-резолвер задан,
// но при отсутствии param `install` он обязан остаться нетронутым.
func TestApply_InstallOmitted_TokenOnly_BitForBit(t *testing.T) {
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	inst := &fakeInstallResolver{cfg: validInstallConfig()}
	m := newTeleportInstallModule(dialer.dial, inst, &fakeAudit{})

	const token = "tok-aaa"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		"start_soul":   false,
		// install НЕ задан → token-only.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// ★ install-резолвер НЕ вызван.
	if inst.calls != 0 {
		t.Errorf("install resolver called %d times despite install omitted — must be 0", inst.calls)
	}
	// ★ Ровно один Run — запись токена (start_soul:false). Ни одного install-шага.
	if len(sess.calls) != 1 {
		t.Fatalf("Session.Run calls = %d, want 1 (token only, no install); cmds=%v", len(sess.calls), cmds(sess))
	}
	if string(sess.calls[0].stdin) != token {
		t.Errorf("token-only: expected token in stdin, got %q", sess.calls[0].stdin)
	}
	if !strings.Contains(sess.calls[0].cmd, "cat >") || !strings.Contains(sess.calls[0].cmd, "/etc/soul/token") {
		t.Errorf("token-only: single Run must be token-write, got %q", sess.calls[0].cmd)
	}
}

// TestApply_Install_FullSequenceBeforeToken — guard (b): install=true+teleport →
// последовательность install-шагов (каталоги → keeper-ca.pem → soul.yml →
// soul.service → curl-бинарь) ПЕРЕД токеном, токен ПЕРЕД `systemctl start`.
func TestApply_Install_FullSequenceBeforeToken(t *testing.T) {
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	inst := &fakeInstallResolver{cfg: validInstallConfig()}
	m := newTeleportInstallModule(dialer.dial, inst, &fakeAudit{})

	const token = "tok-secret"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		"install":      true,
		// start_soul по умолчанию true.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// install-резолвер вызван РОВНО раз (один blueprint на шаг, не per-host).
	if inst.calls != 1 {
		t.Errorf("install resolver calls = %d, want 1 (resolved once per step)", inst.calls)
	}

	// install (6 шагов: каталоги/CA/soul.yml/unit/curl/chmod) + токен + start = 8.
	if len(sess.calls) != 8 {
		t.Fatalf("Session.Run calls = %d, want 8 (6 install + token + start); cmds=%v", len(sess.calls), cmds(sess))
	}

	// ★ Порядок install-шагов до токена (по характерным подстрокам команд).
	wantSubstr := []string{
		"install -d",                  // каталоги
		"/etc/soul/tls/keeper-ca.pem", // CA
		"/etc/soul/soul.yml",          // soul.yml
		"soul.service",                // systemd-unit
		"curl",                        // скачивание бинаря
		"chmod 0755",                  // права бинаря
	}
	for i, want := range wantSubstr {
		if !strings.Contains(sess.calls[i].cmd, want) {
			t.Errorf("install step %d cmd = %q, want substring %q", i, sess.calls[i].cmd, want)
		}
	}

	// ★ Токен — ПОСЛЕ install-шагов (индекс 6), затем start soul (индекс 7).
	tokenCall := sess.calls[6]
	if !strings.Contains(tokenCall.cmd, "/etc/soul/token") || !strings.Contains(tokenCall.cmd, "cat >") {
		t.Errorf("call[6] must be token-write, got %q", tokenCall.cmd)
	}
	if string(tokenCall.stdin) != token {
		t.Errorf("token not in stdin at call[6]: got %q, want %q", tokenCall.stdin, token)
	}
	if sess.calls[7].cmd != "systemctl start soul" {
		t.Errorf("call[7] = %q, want 'systemctl start soul'", sess.calls[7].cmd)
	}
}

// TestApply_Install_SecretsInStdinNotArgv — guard (c), argv-leak: CA-PEM (install)
// и токен идут в Run.stdin, НЕ в cmd-argv. argv виден в `ps`/journald на самой VM.
func TestApply_Install_SecretsInStdinNotArgv(t *testing.T) {
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	inst := &fakeInstallResolver{cfg: validInstallConfig()}
	m := newTeleportInstallModule(dialer.dial, inst, &fakeAudit{})

	const token = "tok-zzz-secret"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		"install":      true,
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}

	// ★ CA-PEM (testCAPem) — ровно в одном шаге через stdin, и НИ В ОДНОЙ команде.
	caInStdin := false
	for _, c := range sess.calls {
		if strings.Contains(c.cmd, testCAPem) {
			t.Fatalf("SECURITY: CA-PEM leaked into argv: cmd=%q", c.cmd)
		}
		if strings.Contains(string(c.stdin), "BEGIN CERTIFICATE") {
			caInStdin = true
		}
	}
	if !caInStdin {
		t.Errorf("CA-PEM never delivered via stdin")
	}

	// ★ Токен — в stdin последнего (token) шага, НЕ в argv ни одной команды.
	for _, c := range sess.calls {
		if strings.Contains(c.cmd, token) {
			t.Fatalf("SECURITY: token leaked into argv: cmd=%q", c.cmd)
		}
	}
	tokenCall := sess.calls[len(sess.calls)-1] // start_soul:false → токен последний
	if string(tokenCall.stdin) != token {
		t.Errorf("token not in stdin of final step: got %q", tokenCall.stdin)
	}
}

// TestApply_Install_StepError_FailsStep_B1Strict — guard (d): ненулевой exit
// install-шага → deliverHost fail (B1-strict). Токен НЕ записывается (полу-
// настроенная VM не онбордится), audit НЕ пишется.
func TestApply_Install_StepError_FailsStep_B1Strict(t *testing.T) {
	// 2-й Run (запись keeper-ca.pem) падает.
	sess := &fakeSession{failAtN: 2, runError: errors.New("permission denied")}
	dialer := &dialRecorder{sess: sess}
	inst := &fakeInstallResolver{cfg: validInstallConfig()}
	aud := &fakeAudit{}
	m := newTeleportInstallModule(dialer.dial, inst, aud)

	const token = "tok-never-written"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		"install":      true,
		"start_soul":   false,
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event on install step error (B1-strict), got %+v", last)
	}
	// ★ Токен НЕ записан: до token-шага не дошли (упали на install-шаге 2),
	// и ни в одном выполненном Run токен не фигурирует (ни stdin, ни argv).
	for _, c := range sess.calls {
		if string(c.stdin) == token || strings.Contains(c.cmd, token) {
			t.Fatalf("token written despite install failure: cmd=%q stdin=%q", c.cmd, c.stdin)
		}
	}
	// На провале install-шага audit НЕ пишется (B1-strict: failed до audit-секции).
	if len(aud.events) != 0 {
		t.Errorf("audit written despite install failure: %+v", aud.events)
	}
}

// flakyDialer — мок push.Dialer: первые failFirst вызовов возвращают err, далее
// — заданную сессию. Для retry-до-join guard-теста.
type flakyDialer struct {
	sess      *fakeSession
	failFirst int
	err       error
	calls     int
	lastCfg   push.DialConfig
}

func (d *flakyDialer) dial(_ context.Context, cfg push.DialConfig) (push.Session, error) {
	d.calls++
	d.lastCfg = cfg
	if d.calls <= d.failFirst {
		return nil, d.err
	}
	return d.sess, nil
}

// alwaysFailDialer — мок push.Dialer, который ВСЕГДА возвращает err и считает
// попытки. Для ctx-cancel guard-теста (retry не должен зависнуть).
type alwaysFailDialer struct {
	err   error
	calls int
}

func (d *alwaysFailDialer) dial(_ context.Context, _ push.DialConfig) (push.Session, error) {
	d.calls++
	return nil, d.err
}

// perHostDialer — мок push.Dialer, дающий новую сессию для одного SID и всегда
// ошибку для остальных. Для multi-host guard-теста (teleport-режим адресует по
// SID = cfg.Host). Каждый успешный Dial — отдельная *fakeSession (один Run на
// хост в teleport-режиме при start_soul:false), чтобы проверять доставку
// независимо.
type perHostDialer struct {
	okSID  string
	err    error
	calls  int
	byHost map[string]*fakeSession
}

func (d *perHostDialer) dial(_ context.Context, cfg push.DialConfig) (push.Session, error) {
	d.calls++
	if cfg.Host != d.okSID {
		return nil, d.err
	}
	if d.byHost == nil {
		d.byHost = make(map[string]*fakeSession)
	}
	s := &fakeSession{}
	d.byHost[cfg.Host] = s
	return s, nil
}

// --- helpers ---

func cmds(s *fakeSession) []string {
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.cmd
	}
	return out
}

// TestDefaultJoinWaitTimeout_Is15m — guard дефолта Teleport-join: экспортированный
// DefaultJoinWaitTimeout == 15m. Ловит регресс (был 6m — свежая cloud-VM
// reverse-tunnel-агентом join-ится позже, шаг падал зря). Значение — часть
// наблюдаемого контракта: на него завязан инвариант provision-run-timeout
// (keeper/internal/scenario TestProvisionTimeoutExceedsJoinWait).
func TestDefaultJoinWaitTimeout_Is15m(t *testing.T) {
	if coremodbootstrap.DefaultJoinWaitTimeout != 15*time.Minute {
		t.Errorf("DefaultJoinWaitTimeout = %s, want 15m", coremodbootstrap.DefaultJoinWaitTimeout)
	}
}

// TestValidate_JoinWaitTimeout_Forms — guard формата param `join_wait_timeout`:
// принимается duration-строка (convention `duration`, симметрия с await_timeout —
// так его задаёт essence.provision_join_wait_timeout) И число секунд (back-compat
// ADR-063). Отрицательное / невалидная строка — отвергаются.
func TestValidate_JoinWaitTimeout_Forms(t *testing.T) {
	m := &coremodbootstrap.Module{}
	tests := []struct {
		name string
		val  any
		ok   bool
	}{
		{"duration-строка 15m", "15m", true},
		{"duration-строка 90s", "90s", true},
		{"duration-строка 1d", "1d", true},
		{"число секунд 60 (back-compat)", float64(60), true},
		{"число 0 (немедленный deadline)", float64(0), true},
		{"невалидная строка", "nonsense", false},
		{"отрицательная строка", "-5m", false},
		{"отрицательное число", float64(-1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State: coremodbootstrap.StateDelivered,
				Params: mustStruct(t, map[string]any{
					"ssh_provider":      "ssh-static",
					"hosts":             hostsParam("vm1", "1.2.3.4", "t"),
					"join_wait_timeout": tt.val,
				}),
			})
			if rep.Ok != tt.ok {
				t.Errorf("join_wait_timeout=%v: Ok=%v, want %v (errors=%v)", tt.val, rep.Ok, tt.ok, rep.Errors)
			}
		})
	}
}

// TestApply_Teleport_JoinWaitDurationString — guard: duration-строка (форма
// essence.provision_join_wait_timeout) реально доходит до retry-цикла как бюджет
// ожидания, а не хардкод. "60s"-строкой + flakyDialer(3 fail) → шаг доживает до
// success (бюджет положительный, ретраи укладываются). Контрпример с крошечным
// бюджетом ("1ms") в TestApply_Teleport_JoinWaitTiny.
func TestApply_Teleport_JoinWaitDurationString(t *testing.T) {
	dialer := &flakyDialer{sess: &fakeSession{}, failFirst: 3, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	m.RetryBase = 1 * time.Millisecond
	m.RetryJitter = 1 * time.Millisecond

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider":      "ssh-static",
		"hosts":             hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":        false,
		"join_wait_timeout": "60s", // ★ duration-строка, не число
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success with '60s' budget after retries, got %+v", last)
	}
	if dialer.calls < 4 {
		t.Errorf("expected >=4 Dial attempts (3 fail + success), got %d", dialer.calls)
	}
}

// TestApply_Teleport_JoinWaitTiny — контрпример к предыдущему: крошечный бюджет
// duration-строкой ("1ms") исчерпывается на первой же попытке → failed. Вместе
// с TestApply_Teleport_JoinWaitDurationString доказывает, что ИМЕННО значение из
// param управляет deadline (а не игнорируется в пользу хардкод-дефолта).
func TestApply_Teleport_JoinWaitTiny(t *testing.T) {
	dialer := &flakyDialer{sess: &fakeSession{}, failFirst: 1 << 30, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider":      "ssh-static",
		"hosts":             hostsParam("vm1.example.com", "10.0.0.1", "tok"),
		"start_soul":        false,
		"join_wait_timeout": "1ms",
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed with '1ms' budget, got %+v", last)
	}
}

// containsTokenDeep рекурсивно ищет подстроку token в любом строковом значении
// структуры (output / audit payload) — страховка от утечки секрета.
func containsTokenDeep(v any, token string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, token)
	case map[string]any:
		for _, vv := range t {
			if containsTokenDeep(vv, token) {
				return true
			}
		}
	case []any:
		for _, vv := range t {
			if containsTokenDeep(vv, token) {
				return true
			}
		}
	}
	return false
}
