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
	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- test-doubles ---

// fakeProvider is a mock of push.SshProvider. Records Authorize/Sign calls and
// allows denying (deny/error) for fail-closed testing.
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
	// static mode: return valid ed25519 private key so
	// push.AuthMethodsFromSign builds ssh.AuthMethod without ephemeral-cert.
	return &pluginv1.SignReply{PrivateKey: testPrivateKeyPEM}, nil
}

// runCall is one Session.Run: command + stdin (to verify token in STDIN, not argv).
type runCall struct {
	cmd   string
	stdin []byte
}

// fakeSession is a mock of push.Session. Captures all Run calls; optional error on N-th call.
type fakeSession struct {
	calls    []runCall
	failAtN  int // 1-based index of Run that returns error; 0 = no errors
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

// dialRecorder is a mock of push.Dialer: returns a preset session, records cfg.
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

// fakeInstallResolver is a mock of coremodbootstrap.InstallResolver: returns
// a preset cloudinit.Config (or error) for install mode. Records call count:
// install-blueprint resolves once per step, not per-host.
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

// testCAPem is a minimal PEM marker satisfying soulinstall.Blueprint.Validate
// (checks for "BEGIN CERTIFICATE" substring, does not parse cert). Content is arbitrary:
// install steps are mocked, no real TLS handshake occurs.
const testCAPem = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

// validInstallConfig returns a cloudinit.Config passing blueprint validation
// (host:port endpoint, PEM-CA, https-URL). Base for install-guard tests.
func validInstallConfig() cloudinit.Config {
	return cloudinit.Config{
		BootstrapEndpoint: "keeper.example.com:8443",
		TLSCAPem:          testCAPem,
		SoulBinaryURL:     "https://nexus.example.com/soul",
		SoulVersion:       "v1.2.3",
	}
}

// testPrivateKeyPEM is a fixed ed25519 private key (OpenSSH PEM) for
// static-mode SignReply in tests. Generated once with ssh-keygen; used only
// to satisfy ssh.ParsePrivateKey.
const testPrivateKeyPEM = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDMq91FxvZmBhL9iPmqOXxepRSXvF5305Sqw6X3hyfvbgAAAJh3XT4wd10+
MAAAAAtzc2gtZWQyNTUxOQAAACDMq91FxvZmBhL9iPmqOXxepRSXvF5305Sqw6X3hyfvbg
AAAEC8n7wb1918K3/nfl6TqXblQOA/c0VAprQjHM1tW8zvIMyr3UXG9mYGEv2I+ao5fF6l
FJe8XnfTlKrDpfeHJ+9uAAAAD3Jvb3RAc291bC1zdGFjawECAwQFBg==
-----END OPENSSH PRIVATE KEY-----
`

// caAuthorities returns a non-empty host-CA set for verify (concrete value is arbitrary:
// Dial is mocked, real host-cert verification does not occur; only non-emptiness
// matters for the fail-closed gate).
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

// hostEntry is one element of the hosts list in register.<provision>.hosts form.
func hostEntry(sid, ip, token string) map[string]any {
	return map[string]any{
		"sid":             sid,
		"primary_ip":      ip,
		"bootstrap_token": token,
	}
}

// hostsParam returns one host in register.<provision>.hosts form.
func hostsParam(sid, ip, token string) []any {
	return []any{hostEntry(sid, ip, token)}
}

// newModule builds a Module with one "ssh-static" provider and a mock Dialer.
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
	// Fail-closed: deny stops before Sign and before Dial.
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
		// start_soul defaults to true — not set, verify started=true below.
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

	// Authorize + Sign called exactly once.
	if len(prov.authCalls) != 1 {
		t.Fatalf("Authorize calls = %d, want 1", len(prov.authCalls))
	}
	if len(prov.signCalls) != 1 {
		t.Fatalf("Sign calls = %d, want 1", len(prov.signCalls))
	}
	// Sign received a public ephemeral key (not empty).
	if prov.signCalls[0].GetPublicKey() == "" {
		t.Errorf("Sign got empty ephemeral public key")
	}

	// Three commands: write token (via stdin) + soul init + unit activation
	// (start_soul defaults to true).
	if len(sess.calls) != 3 {
		t.Fatalf("Session.Run calls = %d, want 3 (write token + soul init + activate); calls=%v", len(sess.calls), cmds(sess))
	}
	writeCall := sess.calls[0]
	// Token is in STDIN.
	if string(writeCall.stdin) != token {
		t.Errorf("token not in stdin: got %q, want %q", writeCall.stdin, token)
	}
	// Token NOT in argv (command).
	if strings.Contains(writeCall.cmd, token) {
		t.Errorf("SECURITY: token leaked into argv: cmd=%q", writeCall.cmd)
	}
	// Command writes to default token_path via `cat >`.
	if !strings.Contains(writeCall.cmd, "/etc/soul/token") || !strings.Contains(writeCall.cmd, "cat >") {
		t.Errorf("write command unexpected: %q", writeCall.cmd)
	}
	if !strings.Contains(sess.calls[1].cmd, "soul init") {
		t.Errorf("second command = %q, want soul init", sess.calls[1].cmd)
	}
	if !strings.Contains(sess.calls[2].cmd, "systemctl start soul") {
		t.Errorf("third command = %q, want systemctl activation chain", sess.calls[2].cmd)
	}
	if len(sess.calls[2].stdin) != 0 {
		t.Errorf("activation command must have empty stdin, got %q", sess.calls[2].stdin)
	}
	if !sess.closed {
		t.Errorf("session not closed")
	}

	// Dial received primary_ip as Host and non-empty host-CA set.
	if dialer.lastCfg.Host != "10.0.0.1" {
		t.Errorf("Dial Host = %q, want primary_ip 10.0.0.1", dialer.lastCfg.Host)
	}
	if len(dialer.lastCfg.HostAuthorities) == 0 {
		t.Errorf("Dial HostAuthorities empty — CA-verify not passed")
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

	// Output does not contain token anywhere.
	out := last.GetOutput().AsMap()
	if containsTokenDeep(out, token) {
		t.Fatalf("SECURITY: token leaked into register output: %+v", out)
	}
	// Output carries hosts[]={sid,delivered,started} + count.
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

	// Audit payload has no token, only count + sids.
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
	// Write token + soul init, no systemctl (start_soul=false).
	if len(sess.calls) != 2 {
		t.Fatalf("Session.Run calls = %d, want 2 (write + init); cmds=%v", len(sess.calls), cmds(sess))
	}
	if !strings.Contains(sess.calls[1].cmd, "soul init") {
		t.Errorf("second command = %q, want soul init", sess.calls[1].cmd)
	}
	for _, c := range sess.calls {
		if strings.Contains(c.cmd, "systemctl") {
			t.Errorf("systemctl must not run when start_soul=false: %q", c.cmd)
		}
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
	// soul init reads token from custom token_path (subshell on VM).
	if len(sess.calls) < 2 || !strings.Contains(sess.calls[1].cmd, `$(cat /opt/soul/seed)`) {
		t.Errorf("soul init must read token from custom token_path; cmds=%v", cmds(sess))
	}
	if dialer.lastCfg.User != "deploy" || dialer.lastCfg.Port != 2222 {
		t.Errorf("custom ssh_user/ssh_port not used: user=%q port=%d", dialer.lastCfg.User, dialer.lastCfg.Port)
	}
}

func TestApply_HostError_FailsStep_B1Strict(t *testing.T) {
	prov := &fakeProvider{allow: true}
	// Write token (1st command) fails.
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
	// On delivery failure, audit is not written (step failed before audit).
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
		HostCAs:   nil, // not configured → fail-closed
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
	// Valid.
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
	// Without ssh_provider and hosts.
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  coremodbootstrap.StateDelivered,
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Errorf("missing ssh_provider/hosts accepted")
	}
	// Invalid state.
	rep, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Errorf("wrong state accepted")
	}
	// ssh_port out of range.
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
	// join_wait_timeout < 0 is rejected (wait ceiling cannot be negative).
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
	// join_wait_timeout == 0 is accepted (immediate deadline: one attempt without waiting).
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
	// install=true in direct mode (m.Transport=="") is rejected: setch sets
	// cloud-init, install not needed (ADR-063 amendment, MVP teleport only).
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
	// install=false in direct is accepted (token-only, backward-compat).
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
	// install=true in teleport mode is accepted.
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

// newTeleportModule builds a Module in teleport mode: empty Providers/HostCAs
// (not used in teleport), Dial only.
func newTeleportModule(dialer push.Dialer, a coremodbootstrap.AuditWriter) *coremodbootstrap.Module {
	return &coremodbootstrap.Module{
		Transport: coremodbootstrap.TransportTeleport,
		Dial:      dialer,
		Audit:     a,
	}
}

// TestApply_Teleport_DialsBySID is guard #1 (SID addressing): in teleport mode
// DialConfig.Host == h.sid (node-name), not primary_ip; Authorize/Sign not called.
func TestApply_Teleport_DialsBySID(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Provider still declared (Validate requires ssh_provider in params), but
	// in teleport mode it must not be called.
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
	// Target is SID, not primary_ip.
	if dialer.lastCfg.Host != "vm1.example.com" {
		t.Errorf("teleport Dial Host = %q, want SID vm1.example.com (NOT primary_ip)", dialer.lastCfg.Host)
	}
	// In teleport mode, DialConfig has no Auth/HostAuthorities/ProxyJump.
	if dialer.lastCfg.Auth != nil {
		t.Errorf("teleport DialConfig.Auth must be nil, got %v", dialer.lastCfg.Auth)
	}
	if len(dialer.lastCfg.HostAuthorities) != 0 {
		t.Errorf("teleport DialConfig.HostAuthorities must be empty, got %v", dialer.lastCfg.HostAuthorities)
	}
	if dialer.lastCfg.ProxyJump != "" {
		t.Errorf("teleport DialConfig.ProxyJump must be empty, got %q", dialer.lastCfg.ProxyJump)
	}
	// Authorize/Sign not called.
	if len(prov.authCalls) != 0 || len(prov.signCalls) != 0 {
		t.Errorf("teleport must not call Authorize/Sign: authorize=%d sign=%d", len(prov.authCalls), len(prov.signCalls))
	}
}

// TestApply_Direct_DialsByIPAndAuthorizes is guard #2 (transport-selection,
// direct-half): direct mode addresses by primary_ip, calls Authorize/Sign and
// passes host-CA. (teleport-half covered by TestApply_Teleport_DialsBySID.)
func TestApply_Direct_DialsByIPAndAuthorizes(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	// Default Transport ("") => direct.
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

// TestApply_Teleport_RetriesUntilJoin is guard #3 (retry-until-join): first N
// Dials fail (VM not yet in Teleport), then success — step does not fail on 1st
// error, reaches success.
func TestApply_Teleport_RetriesUntilJoin(t *testing.T) {
	sess := &fakeSession{}
	dialer := &flakyDialer{sess: sess, failFirst: 3, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Short backoff so 3 retries don't sleep ~12s (field exists for this,
	// see Module.RetryBase).
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
	// Step did not fail on 1st error — >=4 attempts (3 fail + success).
	if dialer.calls < 4 {
		t.Errorf("expected >=4 Dial attempts (3 fail + success), got %d", dialer.calls)
	}
}

// TestApply_Teleport_FailsAfterDeadline is guard #3 (deadline-half): if VM never
// appears in Teleport before join_wait_timeout — step fails (B1-strict,
// error_locked), not hanging forever.
func TestApply_Teleport_FailsAfterDeadline(t *testing.T) {
	sess := &fakeSession{}
	// Always fails.
	dialer := &flakyDialer{sess: sess, failFirst: 1 << 30, err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})

	stream := internaltest.NewApplyStream()
	// join_wait_timeout=0 → deadline immediate: first attempt fails, second
	// interval doesn't fit budget → failed without long wait.
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

// TestApply_Teleport_CtxCancel_NoHang is a guard: if run is cancelled (ctx
// canceled) DURING retry-until-join, step exits fast with ctx error, not hanging
// until join_wait_timeout. Dialer always fails; ctx cancelled with short timeout,
// join_wait budget is large (60s) — exit must happen by ctx, not deadline.
func TestApply_Teleport_CtxCancel_NoHang(t *testing.T) {
	dialer := &alwaysFailDialer{err: errors.New("node offline or does not exist")}
	m := newTeleportModule(dialer.dial, &fakeAudit{})
	// Small backoff → several iterations until cancel (catch cancel mid-stream, not
	// on first attempt).
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
		"join_wait_timeout": float64(60), // large budget: exit must be via ctx, not deadline
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	elapsed := time.Since(start)

	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event on ctx cancel, got %+v", last)
	}
	// No hang: exited near timeout (40ms), far from join_wait (60s).
	if elapsed > 5*time.Second {
		t.Fatalf("ctx cancel did not short-circuit retry: elapsed=%s (join_wait was 60s)", elapsed)
	}
	// Retries actually ran (caught cancel mid-stream, not before first attempt).
	if dialer.calls < 1 {
		t.Errorf("expected at least 1 Dial attempt before cancel, got %d", dialer.calls)
	}
}

// TestApply_Teleport_MultiHost_PerHostDeadline is guard #5 (multi-host, B1-strict):
// ≥2 hosts in one deliver, transport=teleport. One (vm1) joins immediately,
// second (vm2) always fails → whole step fails AFTER exhausting join_wait for
// second host.
//
// Actual behavior (fixed AS-IS): deadline per-host independent, not global budget.
// dialWithJoinRetry computes `deadline = time.Now().Add(joinWait)` on EVERY call
// (per host), so hosts process sequentially and each gets full join_wait.
// First host delivered (2 Runs — write token + init, start_soul:false),
// then step fails on second. join_wait=0 → second deadline expires immediately
// (one fail attempt), test doesn't wait real minutes.
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
		"join_wait_timeout": float64(0), // second deadline expires immediately
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Whole step failed (B1-strict): second host never joined.
	last := stream.Last()
	if last == nil || !last.GetFailed() {
		t.Fatalf("expected failed event when one of N hosts never joins, got %+v", last)
	}
	// failed-event indicates failing host (vm2), not first one.
	if det := last.GetMessage(); det != "" && !strings.Contains(det, failSID) {
		t.Errorf("failed message does not name the failing host %q: %q", failSID, det)
	}

	// First host was delivered BEFORE second failed: exactly one Dial returned
	// session (okSID), ran token write + init on it (start_soul:false).
	okSess, ok := dialer.byHost[okSID]
	if !ok || okSess == nil {
		t.Fatalf("first host %q was never dialed successfully (per-host independent deadline expected)", okSID)
	}
	if len(okSess.calls) != 2 {
		t.Errorf("first host: Session.Run calls = %d, want 2 (token write + init, start_soul=false); cmds=%v", len(okSess.calls), cmds(okSess))
	}
	if string(okSess.calls[0].stdin) != "tok-1" {
		t.Errorf("first host token not delivered via stdin: got %q, want tok-1", okSess.calls[0].stdin)
	}
	if !okSess.closed {
		t.Errorf("first host session not closed")
	}

	// On step failure, audit not written (B1-strict: failed before audit).
	if len(aud.events) != 0 {
		t.Errorf("audit written despite step failure: %+v", aud.events)
	}
}

// --- install-mode guard tests (ADR-063 amendment «full-install over SSH») ---

// newTeleportInstallModule builds a teleport-mode Module with install-resolver (full-install).
func newTeleportInstallModule(dialer push.Dialer, inst coremodbootstrap.InstallResolver, a coremodbootstrap.AuditWriter) *coremodbootstrap.Module {
	return &coremodbootstrap.Module{
		Transport: coremodbootstrap.TransportTeleport,
		Dial:      dialer,
		Install:   inst,
		Audit:     a,
	}
}

// TestApply_InstallOmitted_TokenOnly_BitForBit is guard (a), reverse-guard: without
// `install` param (omitted), behavior is token-only — NO install steps,
// install-resolver not called. Catches regression if install ran unconditionally.
// Teleport mode (install-mode only there), Install-resolver set, but without
// `install` param it must stay untouched. soul init is part of delivery
// (redeem token), not install-step: it runs in both modes.
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
		// install not set → token-only.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// install-resolver not called.
	if inst.calls != 0 {
		t.Errorf("install resolver called %d times despite install omitted — must be 0", inst.calls)
	}
	// Two Runs — write token + init (start_soul:false). No install steps.
	if len(sess.calls) != 2 {
		t.Fatalf("Session.Run calls = %d, want 2 (token + init, no install); cmds=%v", len(sess.calls), cmds(sess))
	}
	if string(sess.calls[0].stdin) != token {
		t.Errorf("token-only: expected token in stdin, got %q", sess.calls[0].stdin)
	}
	if !strings.Contains(sess.calls[0].cmd, "cat >") || !strings.Contains(sess.calls[0].cmd, "/etc/soul/token") {
		t.Errorf("token-only: first Run must be token-write, got %q", sess.calls[0].cmd)
	}
	if !strings.Contains(sess.calls[1].cmd, "soul init") {
		t.Errorf("token-only: second Run must be soul init, got %q", sess.calls[1].cmd)
	}
}

// TestApply_Install_FullSequenceBeforeToken is guard (b): install=true+teleport →
// sequence of install steps (directories → keeper-ca.pem → soul.yml →
// soul.service → curl binary) BEFORE token, token BEFORE `systemctl start`.
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
		// start_soul defaults to true.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	// install-resolver called EXACTLY once (one blueprint per step, not per-host).
	if inst.calls != 1 {
		t.Errorf("install resolver calls = %d, want 1 (resolved once per step)", inst.calls)
	}

	// install (6 steps: directories/CA/soul.yml/unit/curl/chmod) + token + init + activate = 9.
	if len(sess.calls) != 9 {
		t.Fatalf("Session.Run calls = %d, want 9 (6 install + token + init + activate); cmds=%v", len(sess.calls), cmds(sess))
	}

	// Order of install steps before token (by characteristic substrings).
	wantSubstr := []string{
		"install -d",                  // directories
		"/etc/soul/tls/keeper-ca.pem", // CA
		"/etc/soul/soul.yml",          // soul.yml
		"soul.service",                // systemd-unit
		"curl",                        // download binary
		"chmod 0755",                  // binary permissions
	}
	for i, want := range wantSubstr {
		if !strings.Contains(sess.calls[i].cmd, want) {
			t.Errorf("install step %d cmd = %q, want substring %q", i, sess.calls[i].cmd, want)
		}
	}

	// Token is AFTER install steps (index 6), then init (7) and activation (8).
	tokenCall := sess.calls[6]
	if !strings.Contains(tokenCall.cmd, "/etc/soul/token") || !strings.Contains(tokenCall.cmd, "cat >") {
		t.Errorf("call[6] must be token-write, got %q", tokenCall.cmd)
	}
	if string(tokenCall.stdin) != token {
		t.Errorf("token not in stdin at call[6]: got %q, want %q", tokenCall.stdin, token)
	}
	if !strings.Contains(sess.calls[7].cmd, "soul init") {
		t.Errorf("call[7] = %q, want soul init", sess.calls[7].cmd)
	}
	if !strings.Contains(sess.calls[8].cmd, "systemctl start soul") {
		t.Errorf("call[8] = %q, want systemctl activation chain", sess.calls[8].cmd)
	}
}

// TestApply_Install_SecretsInStdinNotArgv is guard (c), argv-leak: CA-PEM (install)
// and token go to Run.stdin, NOT cmd-argv. argv visible in `ps`/journald on VM itself.
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

	// CA-PEM (testCAPem) in exactly one step via stdin, NOT in any command.
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

	// Token in stdin of token step, NOT in argv of any command (including init:
	// it carries literal $(cat …), expanded only on VM).
	for _, c := range sess.calls {
		if strings.Contains(c.cmd, token) {
			t.Fatalf("SECURITY: token leaked into argv: cmd=%q", c.cmd)
		}
	}
	tokenCall := sess.calls[len(sess.calls)-2] // start_soul:false → token, then init
	if string(tokenCall.stdin) != token {
		t.Errorf("token not in stdin of token step: got %q", tokenCall.stdin)
	}
}

// TestApply_Install_StepError_FailsStep_B1Strict is guard (d): non-zero exit of
// install step → deliverHost fail (B1-strict). Token NOT written (half-configured
// VM not onboarded), audit NOT written.
func TestApply_Install_StepError_FailsStep_B1Strict(t *testing.T) {
	// 2nd Run (write keeper-ca.pem) fails.
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
	// Token NOT written: didn't reach token step (failed on install step 2),
	// token not in any executed Run (stdin or argv).
	for _, c := range sess.calls {
		if string(c.stdin) == token || strings.Contains(c.cmd, token) {
			t.Fatalf("token written despite install failure: cmd=%q stdin=%q", c.cmd, c.stdin)
		}
	}
	// On install step failure, audit not written (B1-strict: failed before audit).
	if len(aud.events) != 0 {
		t.Errorf("audit written despite install failure: %+v", aud.events)
	}
}

// TestApply_SoulInit_SeedGuardAndUnitActivation is guard of 5th wall + bonus holes
// (ADR-063 amendment): delivered token without redeem is dead weight (soul-side
// has no /etc/soul/token hook, seed created only by `soul init`), so init-step
// must follow token-write; unit activation — daemon-reload + enable + start
// (parity cloud-init.tmpl: without enable, VM reboot loses soul.service, without
// daemon-reload systemd doesn't see fresh unit).
func TestApply_SoulInit_SeedGuardAndUnitActivation(t *testing.T) {
	prov := &fakeProvider{allow: true}
	sess := &fakeSession{}
	dialer := &dialRecorder{sess: sess}
	m := newModule(t, prov, dialer.dial, &fakeAudit{})

	const token = "tok-init-secret"
	stream := internaltest.NewApplyStream()
	if err := m.Apply(deliverReq(t, map[string]any{
		"ssh_provider": "ssh-static",
		"hosts":        hostsParam("vm1.example.com", "10.0.0.1", token),
		// start_soul defaults to true.
	}), stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || last.GetFailed() {
		t.Fatalf("expected success, got %+v", last)
	}
	if len(sess.calls) != 3 {
		t.Fatalf("Session.Run calls = %d, want 3 (token + init + activate); cmds=%v", len(sess.calls), cmds(sess))
	}

	initCall := sess.calls[1]
	// Idempotency: guard by seed-cert — token is single-use, repeated init
	// after successful redeem would fail host on step retry.
	if !strings.HasPrefix(initCall.cmd, "test -e "+soulinstall.SeedCertPath+" || ") {
		t.Errorf("init cmd must be guarded by seed-cert existence check: %q", initCall.cmd)
	}
	// Secret floor: command carries LITERAL unexpanded $(cat token_path) —
	// expanded by subshell on VM, token NOT in keeper argv (symmetry with
	// cloud-init.tmpl self-onboard phase). STDIN empty.
	if !strings.Contains(initCall.cmd, `SOUL_BOOTSTRAP_TOKEN="$(cat /etc/soul/token)"`) {
		t.Errorf("init cmd must read token via literal subshell from token_path: %q", initCall.cmd)
	}
	if !strings.Contains(initCall.cmd, soulinstall.SoulBinaryPath+" init --config "+soulinstall.SoulConfigPath) {
		t.Errorf("init cmd must run soul init with soul.yml config: %q", initCall.cmd)
	}
	if strings.Contains(initCall.cmd, token) {
		t.Errorf("SECURITY: token leaked into init argv: %q", initCall.cmd)
	}
	if len(initCall.stdin) != 0 {
		t.Errorf("init cmd must have empty stdin, got %q", initCall.stdin)
	}

	// Unit activation in one chain: daemon-reload → enable → start.
	if sess.calls[2].cmd != "systemctl daemon-reload && systemctl enable soul && systemctl start soul" {
		t.Errorf("activation cmd = %q, want 'systemctl daemon-reload && systemctl enable soul && systemctl start soul'", sess.calls[2].cmd)
	}
}

// flakyDialer is a mock of push.Dialer: first failFirst calls return err, then
// return the preset session. For retry-until-join guard test.
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

// alwaysFailDialer is a mock of push.Dialer that ALWAYS returns err and counts
// attempts. For ctx-cancel guard test (retry must not hang).
type alwaysFailDialer struct {
	err   error
	calls int
}

func (d *alwaysFailDialer) dial(_ context.Context, _ push.DialConfig) (push.Session, error) {
	d.calls++
	return nil, d.err
}

// perHostDialer is a mock of push.Dialer that gives a new session for one SID
// and always error for others. For multi-host guard test (teleport mode addresses
// by SID = cfg.Host). Each successful Dial gets separate *fakeSession (token + init
// on host in teleport mode with start_soul:false) to verify delivery independently.
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

// cmds extracts commands from a session's calls.
func cmds(s *fakeSession) []string {
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = c.cmd
	}
	return out
}

// TestDefaultJoinWaitTimeout_Is15m is a guard of Teleport-join default:
// exported DefaultJoinWaitTimeout == 15m. Catches regression (was 6m — fresh
// cloud-VM joins later via reverse-tunnel agent, step failed needlessly). Value
// is part of observable contract: provision-run-timeout invariant relies on it
// (keeper/internal/scenario TestProvisionTimeoutExceedsJoinWait).
func TestDefaultJoinWaitTimeout_Is15m(t *testing.T) {
	if coremodbootstrap.DefaultJoinWaitTimeout != 15*time.Minute {
		t.Errorf("DefaultJoinWaitTimeout = %s, want 15m", coremodbootstrap.DefaultJoinWaitTimeout)
	}
}

// TestValidate_JoinWaitTimeout_Forms is a guard of `join_wait_timeout` param format:
// accepts duration-string (convention `duration`, symmetry with await_timeout —
// essence.provision_join_wait_timeout uses it) AND seconds number (back-compat
// ADR-063). Negative/invalid string rejected.
func TestValidate_JoinWaitTimeout_Forms(t *testing.T) {
	m := &coremodbootstrap.Module{}
	tests := []struct {
		name string
		val  any
		ok   bool
	}{
		{"duration-string 15m", "15m", true},
		{"duration-string 90s", "90s", true},
		{"duration-string 1d", "1d", true},
		{"seconds number 60 (back-compat)", float64(60), true},
		{"number 0 (immediate deadline)", float64(0), true},
		{"invalid string", "nonsense", false},
		{"negative string", "-5m", false},
		{"negative number", float64(-1), false},
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

// TestApply_Teleport_JoinWaitDurationString is a guard: duration-string (form of
// essence.provision_join_wait_timeout) really reaches retry-loop as wait budget,
// not hardcoded. "60s"-string + flakyDialer(3 fail) → step reaches success
// (budget positive, retries fit). Counterexample with tiny budget ("1ms") in
// TestApply_Teleport_JoinWaitTiny.
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
		"join_wait_timeout": "60s", // duration-string, not number
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

// TestApply_Teleport_JoinWaitTiny: counterexample to prior test. Tiny budget via
// duration-string ("1ms") exhausts on first attempt → failed. Together with
// TestApply_Teleport_JoinWaitDurationString proves that param value controls deadline
// (not ignored in favor of hardcoded default).
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

// containsTokenDeep recursively searches for token substring in any string value
// within a structure (output/audit payload) — safeguard against secret leak.
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
