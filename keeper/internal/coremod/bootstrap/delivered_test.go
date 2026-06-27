package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"

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

// hostsParam — один хост в форме register.<provision>.hosts.
func hostsParam(sid, ip, token string) []any {
	return []any{map[string]any{
		"sid":             sid,
		"primary_ip":      ip,
		"bootstrap_token": token,
	}}
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
}

// --- helpers ---

func cmds(s *fakeSession) []string {
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.cmd
	}
	return out
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
