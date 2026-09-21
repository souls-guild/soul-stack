package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/structpb"
)

// The token used throughout. Long and unmistakable: the assertions ask whether
// this exact string reached a channel it must not, so a value that could occur
// by accident would make a green test meaningless.
const testToken = "bootstrap-token-4a7f2e91c05b"

// --- doubles ---------------------------------------------------------------

// ranCmd records one executed command with the stdin it was fed. Both halves
// matter: the whole point of the module is that a secret appears in the second
// and never in the first.
type ranCmd struct {
	cmd   string
	stdin string
}

type fakeSession struct {
	ran    *[]ranCmd
	failOn int // 1-based index of the command to fail on; 0 = never
	closed *int
}

func (s *fakeSession) Run(_ context.Context, cmd string, stdin []byte) (string, error) {
	*s.ran = append(*s.ran, ranCmd{cmd: cmd, stdin: string(stdin)})
	if s.failOn > 0 && len(*s.ran) == s.failOn {
		return "", errors.New("exit status 1")
	}
	// A real session returns whatever the command printed. Returning the token
	// here is deliberate: it proves the module drops stdout rather than
	// happening not to receive any.
	return "printed " + testToken, nil
}

func (s *fakeSession) Close() error {
	if s.closed != nil {
		*s.closed++
	}
	return nil
}

type fakeProvider struct {
	allow      bool
	reason     string
	signErr    error
	authorized *int
	signed     *int
}

func (p *fakeProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	if p.authorized != nil {
		*p.authorized++
	}
	return &pluginv1.AuthorizeReply{Allowed: p.allow, Reason: p.reason}, nil
}

func (p *fakeProvider) Sign(_ context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	if p.signed != nil {
		*p.signed++
	}
	if p.signErr != nil {
		return nil, p.signErr
	}
	if req.GetPublicKey() == "" {
		return nil, errors.New("sign called without an ephemeral public key")
	}
	// A static-key provider: hand back a throwaway private key so
	// AuthMethodsFromSign has something to build an auth method from.
	return &pluginv1.SignReply{PrivateKey: testPrivateKeyPEM}, nil
}

// fakeCA is a host authority value; Dial is mocked, so only its presence is
// exercised — the emptiness check is the invariant under test.
func fakeCA() []push.NamedHostKeyAuthority {
	return []push.NamedHostKeyAuthority{{Name: "default", SourceRef: "vault:secret/host-ca"}}
}

// harness wires one Module over fake transport and returns the recorder the
// assertions read.
type harness struct {
	mod      *Module
	ran      *[]ranCmd
	dialed   *[]push.DialConfig
	closed   *int
	provider *fakeProvider
}

func newHarness(t *testing.T, failOn int) *harness {
	t.Helper()
	ran := &[]ranCmd{}
	dialed := &[]push.DialConfig{}
	closed := new(int)
	prov := &fakeProvider{allow: true, authorized: new(int), signed: new(int)}
	return &harness{
		mod: &Module{
			Providers: func() map[string]SshProviderHost { return map[string]SshProviderHost{"vault-ssh": prov} },
			HostCAs:   fakeCA,
			Dial: func(_ context.Context, cfg push.DialConfig) (push.Session, error) {
				*dialed = append(*dialed, cfg)
				return &fakeSession{ran: ran, failOn: failOn, closed: closed}, nil
			},
		},
		ran:      ran,
		dialed:   dialed,
		closed:   closed,
		provider: prov,
	}
}

// --- param builders --------------------------------------------------------

func params(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("build params: %v", err)
	}
	return s
}

// tokenHost is the shape `core.bootstrap.issued` emits for a host that was
// minted for.
func tokenHost(sid, ip string) map[string]any {
	return map[string]any{"sid": sid, "primary_ip": ip, "bootstrap_token": testToken}
}

// converged is the shape it emits for a host of this run that was already up
// (NIM-780): a sid, the flag, and deliberately nothing else — no token and no
// address.
func converged(sid string) map[string]any {
	return map[string]any{"sid": sid, "onboarded": true}
}

func writeTokenSteps() []any {
	return []any{
		map[string]any{"run": "install -d -m 0700 /etc/soul && umask 077 && cat > /etc/soul/token", "stdin_from": "bootstrap_token"},
		map[string]any{"run": "systemctl daemon-reload && systemctl enable --now soul"},
	}
}

func apply(t *testing.T, m *Module, p *structpb.Struct) *internaltest.ApplyStream {
	t.Helper()
	return applyCtx(t, m, p, context.Background())
}

func applyCtx(t *testing.T, m *Module, p *structpb.Struct, ctx context.Context) *internaltest.ApplyStream {
	t.Helper()
	st := internaltest.NewApplyStreamCtx(ctx)
	if err := m.Apply(&pluginv1.ApplyRequest{State: StateRun, Params: p}, st); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	if st.Last() == nil {
		t.Fatal("Apply sent no final event")
	}
	return st
}

func mustSucceed(t *testing.T, st *internaltest.ApplyStream) *pluginv1.ApplyEvent {
	t.Helper()
	if ev := st.Last(); ev.GetFailed() {
		t.Fatalf("step failed: %s", ev.GetMessage())
	}
	return st.Last()
}

func mustFail(t *testing.T, st *internaltest.ApplyStream) string {
	t.Helper()
	ev := st.Last()
	if !ev.GetFailed() {
		t.Fatalf("step succeeded, want a refusal; output=%v", ev.GetOutput().AsMap())
	}
	return ev.GetMessage()
}

// --- the secret floor ------------------------------------------------------

// ★ GUARD (NIM-849, requirement 1 of the ADR-063 amendment). A command argument
// is visible in `ps`, in `audit.log` and in journald ON THE HOST ITSELF, so a
// `run:` rendered from a secret source is refused BEFORE the connect — not
// masked afterwards, which would hide the leak rather than prevent it.
//
// The seal is the run's render-time record of which params cells read a secret
// ([ADR-010] §7.4); keeper_dispatch threads it in on the module context.
//
// Mutation: delete the guardSealedRunCells call in applyRun and this reddens —
// the step runs the command with the secret in argv.
func TestApply_RefusesASealedRunCell(t *testing.T) {
	for _, sealedPath := range []string{"steps[0].run", "steps[0]", "steps"} {
		t.Run(sealedPath, func(t *testing.T) {
			h := newHarness(t, 0)
			ctx := util.WithSealedPaths(context.Background(), map[string]bool{sealedPath: true})
			st := applyCtx(t, h.mod, params(t, map[string]any{
				"ssh_provider": "vault-ssh",
				// ★ The hosts carry NO token and the command quotes no host field:
				// the ONLY thing wrong here is the seal. Sharing the fixture with
				// the argv guard below would let either one alone keep this green,
				// and a guard that cannot fail alone is not a guard.
				"hosts": []any{map[string]any{"sid": "vm-1.example.com", "primary_ip": "10.0.0.1"}},
				// A `${ vars.db_pw }` over a vault-backed var — the commonest way a
				// secret reaches a cell, and what the render seals.
				"steps": []any{map[string]any{"run": "mysql -phunter2 -e 'select 1'"}},
			}), ctx)

			msg := mustFail(t, st)
			if !strings.Contains(msg, "declared secret") {
				t.Errorf("refusal = %q, want the seal guard's refusal", msg)
			}
			if !strings.Contains(msg, sealedPath) {
				t.Errorf("the refusal does not name the offending cell: %q", msg)
			}
			if !strings.Contains(msg, "stdin") {
				t.Errorf("the refusal does not point at the correct form: %q", msg)
			}
			if len(*h.dialed) != 0 {
				t.Errorf("dialed %d host(s) — the refusal must come BEFORE the connect", len(*h.dialed))
			}
			if *h.provider.authorized != 0 {
				t.Error("Authorize was called — the refusal must come before the provider is touched at all")
			}
		})
	}
}

// A sealed `stdin:` is the CORRECT form and must not be refused. Without this
// the guard could "pass" by refusing every step that carries a secret anywhere,
// which would leave no way to deliver one at all.
func TestApply_ASealedStdinIsTheCorrectForm(t *testing.T) {
	h := newHarness(t, 0)
	ctx := util.WithSealedPaths(context.Background(), map[string]bool{"steps[0].stdin": true})
	st := applyCtx(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        []any{map[string]any{"run": "cat > /etc/soul/ca.pem", "stdin": "-----BEGIN CERTIFICATE-----"}},
	}), ctx)

	mustSucceed(t, st)
	if got := (*h.ran)[0].stdin; got != "-----BEGIN CERTIFICATE-----" {
		t.Errorf("stdin = %q, want the sealed value fed to the process", got)
	}
}

// ★ GUARD (NIM-849). The seal only knows what the RENDER knew, and
// `register.<mint>.hosts` is not a sealed source — so a token pulled out of the
// minting register and interpolated into `run:` by hand leaves no sealed cell
// behind. This is the second, seal-independent floor: a command line that
// quotes the value of a secret-NAMED host field is refused.
//
// Mutation: delete the guardHostSecretsInArgv call and this reddens.
func TestApply_RefusesAHostTokenQuotedInArgv(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        []any{map[string]any{"run": `SOUL_BOOTSTRAP_TOKEN="` + testToken + `" soul init`}},
	}))

	msg := mustFail(t, st)
	if strings.Contains(msg, testToken) {
		t.Errorf("the refusal quotes the very value it is refusing: %q", msg)
	}
	if !strings.Contains(msg, "bootstrap_token") {
		t.Errorf("the refusal does not name the offending field: %q", msg)
	}
	if len(*h.dialed) != 0 {
		t.Errorf("dialed %d host(s) — the refusal must come BEFORE the connect", len(*h.dialed))
	}
}

// ★ The same guard must see EVERY host's fields, not only the dialed ones: one
// command line runs on all of them, so a token belonging to a host that will be
// skipped is still a token in argv on the hosts that are not.
//
// Mutation: leave `fields` nil on the onboarded branch of hostFromStruct and
// this reddens.
func TestApply_ASkippedHostsTokenInArgvIsStillRefused(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts": []any{
			map[string]any{"sid": "vm-1.example.com", "onboarded": true, "bootstrap_token": testToken},
			tokenHost("vm-2.example.com", "10.0.0.2"),
		},
		"steps": []any{map[string]any{"run": `SOUL_BOOTSTRAP_TOKEN="` + testToken + `" soul init`}},
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "vm-1.example.com") {
		t.Errorf("refusal = %q, want the skipped host named", msg)
	}
	if len(*h.dialed) != 0 {
		t.Error("dialed before refusing")
	}
}

// ★ The seal set is RUN-wide and carries no task index, so it is walked against
// THIS task's own step count. A sealed path beyond that count belongs to a task
// of another shape and must not refuse this one.
//
// Mutation: scan the sealed map for anything `steps`-shaped instead of indexing
// by this task's steps, and this reddens.
func TestApply_ASealedCellBeyondThisTasksStepsIsNotOurs(t *testing.T) {
	h := newHarness(t, 0)
	ctx := util.WithSealedPaths(context.Background(), map[string]bool{"steps[7].run": true})
	st := applyCtx(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(), // two steps, so steps[7] is somebody else's
	}), ctx)
	mustSucceed(t, st)
}

// The failure text names the step by index and does NOT repeat the command:
// `push.Session.Run` quotes it when it cannot start the process, and the command
// is the site's own text, which the module repeats in no other channel.
//
// Mutation: wrap the push error with %w instead of withoutCommand and this
// reddens.
func TestApply_AFailureDoesNotRepeatTheCommand(t *testing.T) {
	ran := &[]ranCmd{}
	m := &Module{
		HostCAs: fakeCA,
		Providers: func() map[string]SshProviderHost {
			return map[string]SshProviderHost{"vault-ssh": &fakeProvider{allow: true}}
		},
		Dial: func(_ context.Context, _ push.DialConfig) (push.Session, error) {
			return &failingStartSession{ran: ran}, nil
		},
	}
	// ★ The command carries a quote, because `push` formats it with %q: a
	// redaction that only matched the raw string would match nothing in the
	// escaped text, and the canonical redeem step
	// (`SOUL_BOOTSTRAP_TOKEN="$(cat …)"`) is exactly that shape.
	const cmd = `test -e /var/lib/soul-stack/seed/current/cert.pem || SOUL_BOOTSTRAP_TOKEN="$(cat /etc/soul/token)" soul init`
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        []any{map[string]any{"run": cmd}},
	}))

	msg := mustFail(t, st)
	for _, fragment := range []string{"SOUL_BOOTSTRAP_TOKEN", "soul-stack/seed"} {
		if strings.Contains(msg, fragment) {
			t.Errorf("the failure repeats the command (%q): %q", fragment, msg)
		}
	}
	if !strings.Contains(msg, "step 1") || !strings.Contains(msg, "exec request failed") {
		t.Errorf("failure = %q, want the step index and the underlying reason", msg)
	}
}

// failingStartSession models the one push error shape that quotes the command:
// the process could not be STARTED (a restricted shell, exec disabled).
type failingStartSession struct{ ran *[]ranCmd }

func (s *failingStartSession) Run(_ context.Context, cmd string, _ []byte) (string, error) {
	*s.ran = append(*s.ran, ranCmd{cmd: cmd})
	return "", fmt.Errorf("push: running %q: %w", cmd, errors.New("exec request failed"))
}
func (s *failingStartSession) Close() error { return nil }

// The same guard must NOT fire on a non-secret field. A sid or a hostname is a
// plausible substring of an ordinary command, and B1-strict means one false
// positive costs the whole run.
func TestApply_ANonSecretHostFieldInArgvIsFine(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("redis-1.example.com", "10.0.0.1")},
		"steps":        []any{map[string]any{"run": "hostnamectl set-hostname redis-1.example.com"}},
	}))
	mustSucceed(t, st)
}

// ★ GUARD (NIM-849, requirement 1). The per-host secret travels in STDIN and
// appears in no command line — the property the whole module exists for.
//
// Mutation: make stdinFor return nil and format the value into the command
// instead, and this reddens on both halves.
func TestApply_StdinFromFeedsStdinAndNeverArgv(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	mustSucceed(t, st)

	if len(*h.ran) != 2 {
		t.Fatalf("ran %d command(s), want 2", len(*h.ran))
	}
	if got := (*h.ran)[0].stdin; got != testToken {
		t.Errorf("stdin of the write step = %q, want the token", got)
	}
	for i, c := range *h.ran {
		if strings.Contains(c.cmd, testToken) {
			t.Errorf("command %d carries the token in argv: %q", i, c.cmd)
		}
	}
	if got := (*h.ran)[1].stdin; got != "" {
		t.Errorf("the activation step was fed stdin %q, want none", got)
	}
}

// ★ GUARD (NIM-849, requirement 2). The token exists in the minting register
// and nowhere else: the step's own output carries the roster and its counts, and
// no command output at all — a step whose job is to write a secret can echo it,
// and the register row is written verbatim by the runner.
//
// Mutation: put the Run stdout into the output map and this reddens.
func TestApply_OutputCarriesNoTokenAndNoCommandOutput(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	ev := mustSucceed(t, st)

	rendered := ev.GetOutput().String() + " " + ev.GetMessage()
	if strings.Contains(rendered, testToken) {
		t.Fatalf("the token reached the step output: %s", rendered)
	}
	// The fake session returns the token in stdout on every command, so an output
	// that merely happens to be token-free is not the same fact as an output that
	// carries no command output. Assert the shape.
	out := ev.GetOutput().AsMap()
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("output hosts = %v, want one entry", out["hosts"])
	}
	entry, _ := hosts[0].(map[string]any)
	for k := range entry {
		switch k {
		case "sid", "ran", "skipped":
		default:
			t.Errorf("output host entry carries an unexpected key %q — the contract is {sid, ran, skipped}", k)
		}
	}
}

// The audit event is counts and addressing, never the commands: a `run:` is the
// site's own policy and the module cannot vouch for what an author put in one.
func TestApply_AuditCarriesNeitherTokenNorCommands(t *testing.T) {
	h := newHarness(t, 0)
	var written []*audit.Event
	h.mod.Audit = auditFunc(func(_ context.Context, ev *audit.Event) error {
		written = append(written, ev)
		return nil
	})
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	mustSucceed(t, st)

	if len(written) != 1 {
		t.Fatalf("wrote %d audit event(s), want 1", len(written))
	}
	ev := written[0]
	if ev.EventType != audit.EventSSHRun {
		t.Errorf("EventType = %q, want %q", ev.EventType, audit.EventSSHRun)
	}
	// Walk, do not type-assert the top level: `sids` is a []any, so a token that
	// landed inside it would slip past a flat scan.
	for k, v := range ev.Payload {
		if s := findString(v, testToken); s != "" {
			t.Errorf("audit payload key %q carries the token", k)
		}
	}
	if _, ok := ev.Payload["steps"]; !ok {
		t.Error("audit payload has no step count")
	}
	if s, ok := ev.Payload["steps"].(string); ok {
		t.Errorf("audit payload carries the command text (%q), want a count", s)
	}
}

// findString returns the first string anywhere under v that contains needle.
func findString(v any, needle string) string {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, needle) {
			return t
		}
	case []any:
		for _, item := range t {
			if s := findString(item, needle); s != "" {
				return s
			}
		}
	case map[string]any:
		for _, item := range t {
			if s := findString(item, needle); s != "" {
				return s
			}
		}
	}
	return ""
}

type auditFunc func(context.Context, *audit.Event) error

func (f auditFunc) Write(ctx context.Context, ev *audit.Event) error { return f(ctx, ev) }

// --- the onboarded branch --------------------------------------------------

// ★ GUARD (NIM-849, NIM-189/NIM-780). A host of this run that was already up
// carries NO `bootstrap_token` key at all — reaching for it in CEL is an error,
// which is why the skip has to live inside the step. `when:` cannot do it: on an
// `on: keeper` task a predicate reading `register.*` is silently ignored.
//
// Mutation: drop the `if h.onboarded` arm and this reddens — the step dials a
// host it has nothing to say to and fails on the missing field.
func TestApply_ConvergedHostIsSkippedNotDialed(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{converged("vm-1.example.com"), tokenHost("vm-2.example.com", "10.0.0.2")},
		"steps":        writeTokenSteps(),
	}))
	ev := mustSucceed(t, st)

	if len(*h.dialed) != 1 {
		t.Fatalf("dialed %d host(s), want only the one that needs it", len(*h.dialed))
	}
	if got := (*h.dialed)[0].Host; got != "10.0.0.2" {
		t.Errorf("dialed %q, want the host that was not converged", got)
	}
	out := ev.GetOutput().AsMap()
	if got := out["skipped"]; got != float64(1) {
		t.Errorf("skipped = %v, want 1", got)
	}
	if got := out["count"]; got != float64(2) {
		t.Errorf("count = %v, want every host in the roster", got)
	}
	hosts, _ := out["hosts"].([]any)
	first, _ := hosts[0].(map[string]any)
	if first["skipped"] != true || first["ran"] != false {
		t.Errorf("converged host reported as %v, want {ran:false, skipped:true}", first)
	}
}

// ★ The `primary_ip` requirement of the direct transport is settled AFTER the
// flag, not before it. Issuance emits `{sid, onboarded: true}` and nothing else,
// so demanding a dial address would fail the step over the one host it was about
// to skip (ADR-063 amendment 2026-09-04).
//
// Mutation: move the requirePrimaryIP check above the onboarded arm in
// hostFromStruct and this reddens.
func TestApply_ConvergedHostNeedsNoDialAddress(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{converged("vm-1.example.com")},
		"steps":        writeTokenSteps(),
	}))
	ev := mustSucceed(t, st)
	if ev.GetChanged() {
		t.Error("changed = true with every host skipped — nothing ran")
	}
	if len(*h.dialed) != 0 {
		t.Errorf("dialed %d host(s), want none", len(*h.dialed))
	}
}

// A host that is neither converged nor addressable is still a hard error: the
// flag is the only exemption, and skipping such a host silently would leave the
// incarnation short of a member with nothing in the output to show for it.
func TestApply_DirectTransportStillRequiresAnAddress(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{map[string]any{"sid": "vm-1.example.com", "bootstrap_token": testToken}},
		"steps":        writeTokenSteps(),
	}))
	if msg := mustFail(t, st); !strings.Contains(msg, "primary_ip") {
		t.Errorf("refusal = %q, want it to name the missing address", msg)
	}
}

// --- fail-closed before the connect ----------------------------------------

// ★ GUARD (NIM-849, requirement 3). A provider deny stops the run BEFORE an SSH
// session is opened, not after.
func TestApply_AuthorizeDenyAbortsBeforeTheSession(t *testing.T) {
	h := newHarness(t, 0)
	h.provider.allow, h.provider.reason = false, "host not in scope"
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "host not in scope") {
		t.Errorf("refusal = %q, want the provider's reason", msg)
	}
	if len(*h.dialed) != 0 {
		t.Errorf("dialed %d host(s) after a deny", len(*h.dialed))
	}
	if len(*h.ran) != 0 {
		t.Errorf("ran %d command(s) after a deny", len(*h.ran))
	}
}

// ★ GUARD (NIM-849, requirement 3). An empty host-CA set is an error, never a
// blind connect: TOFU is refused outright, the same way push.Dial refuses it.
func TestApply_EmptyHostCAsIsARefusalNotABlindConnect(t *testing.T) {
	h := newHarness(t, 0)
	h.mod.HostCAs = func() []push.NamedHostKeyAuthority { return nil }
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "host_ca_refs") {
		t.Errorf("refusal = %q, want it to name the config key that fixes it", msg)
	}
	if len(*h.dialed) != 0 {
		t.Error("dialed with no host CA configured")
	}
}

// The direct transport signs with an EPHEMERAL key and hands the host CA set to
// the dialer. Neither is observable from outside, so this asserts the two calls
// that would be missing if either were dropped.
func TestApply_DirectTransportSignsEphemerallyAndVerifiesTheHostCert(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	mustSucceed(t, st)

	if *h.provider.authorized != 1 || *h.provider.signed != 1 {
		t.Errorf("authorized=%d signed=%d, want 1 each", *h.provider.authorized, *h.provider.signed)
	}
	if len((*h.dialed)[0].HostAuthorities) == 0 {
		t.Error("DialConfig carries no host authorities — the handshake would have nothing to verify against")
	}
	if len((*h.dialed)[0].Auth) == 0 {
		t.Error("DialConfig carries no auth method")
	}
}

// An unknown provider name is refused by name, which says more than "unknown
// keeper-side module" would: the module IS configured, this provider is not.
func TestApply_UnknownProviderIsRefusedByName(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "no-such-provider",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	msg := mustFail(t, st)
	if !strings.Contains(msg, "no-such-provider") || !strings.Contains(msg, "vault-ssh") {
		t.Errorf("refusal = %q, want both the missing name and the known ones", msg)
	}
}

// --- B1-strict -------------------------------------------------------------

// A failure on any host fails the step, so the run goes to error_locked rather
// than committing state over a group that is only partly up. The message names
// the host and the step, and the session is closed either way.
func TestApply_OneFailedHostFailsTheStep(t *testing.T) {
	h := newHarness(t, 1) // the first command fails
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1"), tokenHost("vm-2.example.com", "10.0.0.2")},
		"steps":        writeTokenSteps(),
	}))

	msg := mustFail(t, st)
	if !strings.Contains(msg, "vm-1.example.com") || !strings.Contains(msg, "step 1") {
		t.Errorf("refusal = %q, want the host and the failing step named", msg)
	}
	if len(*h.dialed) != 1 {
		t.Errorf("dialed %d host(s) — the second must not be reached after the first failed", len(*h.dialed))
	}
	if *h.closed != 1 {
		t.Errorf("closed %d session(s), want the failed one closed", *h.closed)
	}
}

// --- params ----------------------------------------------------------------

// Two stdin sources on one step is a refusal, not a precedence rule: an author
// who wrote both meant one of them, and silently picking would deliver the wrong
// value to a host we cannot inspect.
func TestParse_StdinAndStdinFromAreExclusive(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        []any{map[string]any{"run": "cat > /tmp/x", "stdin": "a", "stdin_from": "bootstrap_token"}},
	}))
	if msg := mustFail(t, st); !strings.Contains(msg, "exactly one") {
		t.Errorf("refusal = %q", msg)
	}
}

// A `stdin_from:` naming a field the host does not carry fails that host rather
// than feeding the process an empty stdin, which would write an empty token file
// and produce a host that never onboards, silently.
func TestApply_StdinFromNamingAMissingFieldFailsTheHost(t *testing.T) {
	h := newHarness(t, 0)
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{map[string]any{"sid": "vm-1.example.com", "primary_ip": "10.0.0.1"}},
		"steps":        []any{map[string]any{"run": "cat > /etc/soul/token", "stdin_from": "bootstrap_token"}},
	}))
	if msg := mustFail(t, st); !strings.Contains(msg, "bootstrap_token") {
		t.Errorf("refusal = %q, want the missing field named", msg)
	}
	if len(*h.ran) != 0 {
		t.Error("a command ran with an unresolved stdin source")
	}
}

func TestApply_EmptyRosterAndEmptyStepsAreRefused(t *testing.T) {
	h := newHarness(t, 0)
	for name, p := range map[string]map[string]any{
		"no hosts": {"ssh_provider": "vault-ssh", "hosts": []any{}, "steps": writeTokenSteps()},
		"no steps": {"ssh_provider": "vault-ssh", "hosts": []any{tokenHost("vm-1.example.com", "10.0.0.1")}, "steps": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if msg := mustFail(t, apply(t, h.mod, params(t, p))); !strings.Contains(msg, "empty list") {
				t.Errorf("refusal = %q", msg)
			}
		})
	}
}

func TestApply_UnknownStateIsRefused(t *testing.T) {
	h := newHarness(t, 0)
	st := internaltest.NewApplyStream()
	if err := h.mod.Apply(&pluginv1.ApplyRequest{State: "delivered", Params: params(t, map[string]any{})}, st); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if msg := mustFail(t, st); !strings.Contains(msg, `unknown state "delivered"`) {
		t.Errorf("refusal = %q", msg)
	}
}

func TestValidate(t *testing.T) {
	m := &Module{}
	t.Run("ok", func(t *testing.T) {
		reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: StateRun,
			Params: params(t, map[string]any{
				"ssh_provider": "vault-ssh",
				"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
				"steps":        writeTokenSteps(),
			}),
		})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !reply.GetOk() {
			t.Errorf("not ok: %v", reply.GetErrors())
		}
	})
	t.Run("port out of range", func(t *testing.T) {
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State: StateRun,
			Params: params(t, map[string]any{
				"ssh_provider": "vault-ssh",
				"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
				"steps":        writeTokenSteps(),
				"ssh_port":     float64(70000),
			}),
		})
		if reply.GetOk() {
			t.Error("accepted an out-of-range port")
		}
	})
	t.Run("unknown state", func(t *testing.T) {
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: "delivered"})
		if reply.GetOk() {
			t.Error("accepted the removed state")
		}
	})
}

// --- teleport --------------------------------------------------------------

// Teleport addresses a node BY NAME and never by IP, and does not call
// Authorize/Sign: transport, user-auth and host-verify all come from the
// identity file.
func TestApply_TeleportDialsBySIDAndSkipsTheProvider(t *testing.T) {
	h := newHarness(t, 0)
	h.mod.Transport = TransportTeleport
	st := apply(t, h.mod, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{map[string]any{"sid": "vm-1.example.com", "bootstrap_token": testToken}},
		"steps":        writeTokenSteps(),
	}))
	mustSucceed(t, st)

	if got := (*h.dialed)[0].Host; got != "vm-1.example.com" {
		t.Errorf("dialed %q, want the node name", got)
	}
	if *h.provider.authorized != 0 || *h.provider.signed != 0 {
		t.Error("the provider was called in teleport mode")
	}
}

// The wait for a host to become reachable is bounded and named: past the
// deadline the step fails (B1-strict) with the setting in the message.
//
// `join_wait_timeout: 0` is the deterministic form of "past the deadline" — one
// attempt, no sleep — so the assertion does not race the scheduler.
func TestApply_TeleportJoinWaitIsBoundedAndNamed(t *testing.T) {
	attempts := 0
	m := &Module{
		Transport: TransportTeleport,
		Dial: func(_ context.Context, _ push.DialConfig) (push.Session, error) {
			attempts++
			return nil, errors.New("node offline or does not exist")
		},
	}
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{map[string]any{"sid": "vm-1.example.com", "bootstrap_token": testToken}},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": float64(0),
	}))

	msg := mustFail(t, st)
	if !strings.Contains(msg, "join_wait_timeout") {
		t.Errorf("refusal = %q, want the setting named", msg)
	}
	if attempts != 1 {
		t.Errorf("attempted %d time(s) with a zero budget, want exactly one (the first attempt is immediate)", attempts)
	}
}

// A node that is not there yet is not a failure: the connect retries until it
// joins. Deterministic — the budget is generous and the dialer decides when to
// succeed, so no assertion depends on wall-clock timing.
func TestApply_TeleportRetriesUntilTheNodeJoins(t *testing.T) {
	attempts := 0
	ran := &[]ranCmd{}
	m := &Module{
		Transport:   TransportTeleport,
		RetryBase:   time.Millisecond,
		RetryJitter: time.Millisecond,
		Dial: func(_ context.Context, _ push.DialConfig) (push.Session, error) {
			attempts++
			if attempts < 3 {
				return nil, errors.New("node offline or does not exist")
			}
			return &fakeSession{ran: ran, closed: new(int)}, nil
		},
	}
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{map[string]any{"sid": "vm-1.example.com", "bootstrap_token": testToken}},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "1m",
	}))
	mustSucceed(t, st)

	if attempts != 3 {
		t.Errorf("attempted %d time(s), want 3 — the first two refusals are a node that has not joined yet", attempts)
	}
	if len(*ran) != 2 {
		t.Errorf("ran %d command(s) after the join, want 2", len(*ran))
	}
}

// --- the direct connect waits for sshd (NIM-872) ---------------------------

// connRefused is what a VM that booted seconds ago answers on port 22, in the
// shape the live path produces it: net.Dialer returns *net.OpError{Op: "dial"}
// and push.Dial wraps it with %w. The classifier reads that shape and nothing
// else, so a double returning a bare errors.New would go green against a
// classifier that reads nothing at all.
func connRefused(ip string) error {
	return fmt.Errorf("push: TCP connection to %s:22: %w", ip, &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: 22},
		Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	})
}

// proxyRefused is the SAME event one hop away: an SshProvider that returns
// `proxy_jump` sends push.Dial through dialViaProxy, where the target connect is
// the bastion's direct-tcpip channel and an absent sshd comes back as the proxy
// refusing to open it. x/crypto returns *ssh.OpenChannelError here and never a
// net.OpError, which is why the classifier has to know both shapes.
func proxyRefused(ip string) error {
	return fmt.Errorf("push: direct-tcpip through proxy bastion:22 to %s:22: %w", ip,
		&ssh.OpenChannelError{Reason: ssh.ConnectionFailed, Message: "connect failed"})
}

// hostCertRejected is the other side of the boundary the classifier draws: the
// host answered on 22 and the SSH handshake refused it. push wraps a handshake
// failure with no dial-level OpError under it, which is exactly what makes the
// two distinguishable without parsing text.
func hostCertRejected(ip string) error {
	return fmt.Errorf("push: SSH handshake with %s:22: %w", ip,
		errors.New("ssh: handshake failed: ssh: no authorities for hostname"))
}

// directModule wires a direct-transport Module over the given dialer, with the
// backoff shortened so a test does not sleep the production interval.
func directModule(prov *fakeProvider, dial push.Dialer) *Module {
	return &Module{
		Providers:   func() map[string]SshProviderHost { return map[string]SshProviderHost{"vault-ssh": prov} },
		HostCAs:     fakeCA,
		RetryBase:   time.Millisecond,
		RetryJitter: time.Millisecond,
		Dial:        dial,
	}
}

func allowingProvider() *fakeProvider {
	return &fakeProvider{allow: true, authorized: new(int), signed: new(int)}
}

// ★ GUARD (NIM-872). A machine whose DHCP lease has arrived is not a machine
// whose sshd is listening: the readiness predicate every cloud here uses is "a
// lease with an address and a name", and sshd starts well after it. The connect
// on `direct` is therefore the same bounded retry `teleport` has always had —
// `join_wait_timeout` is a knob the step advertises, and on this transport it
// used to do nothing at all.
//
// Mutation: call m.Dial directly in dialDirect instead of dialWithJoinRetry and
// this reddens — the first refusal fails the run, ten seconds after creation.
func TestApply_DirectRetriesUntilSshdListens(t *testing.T) {
	attempts := 0
	ran := &[]ranCmd{}
	prov := allowingProvider()
	m := directModule(prov, func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		if attempts < 3 {
			return nil, connRefused("192.168.122.173")
		}
		return &fakeSession{ran: ran, closed: new(int)}, nil
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "1m",
	}))
	mustSucceed(t, st)

	if attempts != 3 {
		t.Errorf("attempted %d time(s), want 3 — the first two refusals are an sshd that has not started yet", attempts)
	}
	if len(*ran) != 2 {
		t.Errorf("ran %d command(s) once the host came up, want 2", len(*ran))
	}
	// The credential is minted once and outlives the wait; re-signing per attempt
	// would burn a provider-side issuance on every refusal.
	if *prov.authorized != 1 || *prov.signed != 1 {
		t.Errorf("authorized=%d signed=%d across 3 attempts, want 1 each", *prov.authorized, *prov.signed)
	}
}

// ★ GUARD (NIM-872). The same wait behind a BASTION. An SshProvider returning
// `proxy_jump` on its SignReply — which the Teleport provider does — sends
// push.Dial through dialViaProxy, where the target connect is the proxy's
// direct-tcpip channel: the identical absent sshd arrives as *ssh.OpenChannelError
// and not as a net.OpError, so a classifier reading only the socket shape would
// leave the bastion half of the direct transport exactly as broken as before.
//
// Mutation: drop the OpenChannelError arm of isConnectFailure and this reddens
// while every other test here stays green.
func TestApply_DirectRetriesThroughABastion(t *testing.T) {
	attempts := 0
	ran := &[]ranCmd{}
	m := directModule(allowingProvider(), func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		if attempts < 3 {
			return nil, proxyRefused("192.168.122.173")
		}
		return &fakeSession{ran: ran, closed: new(int)}, nil
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "1m",
	}))
	mustSucceed(t, st)

	if attempts != 3 {
		t.Errorf("attempted %d time(s) through the bastion, want 3", attempts)
	}
}

// The wait on `direct` is bounded and names the setting, the same way teleport's
// does — a host that never comes up must fail the step (B1-strict) rather than
// hold the run to the run timeout.
//
// `join_wait_timeout: 0` is the deterministic form of "past the deadline" — one
// attempt, no sleep — so the assertion does not race the scheduler.
func TestApply_DirectJoinWaitIsBoundedAndNamed(t *testing.T) {
	attempts := 0
	m := directModule(allowingProvider(), func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		return nil, connRefused("192.168.122.173")
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": float64(0),
	}))

	msg := mustFail(t, st)
	if !strings.Contains(msg, "join_wait_timeout") {
		t.Errorf("refusal = %q, want the setting named", msg)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("refusal = %q, want the last connect error kept", msg)
	}
	if attempts != 1 {
		t.Errorf("attempted %d time(s) with a zero budget, want exactly one (the first attempt is immediate)", attempts)
	}
}

// ★ GUARD (NIM-872). The retry covers the TCP connect and stops there. An SSH
// handshake the host rejected — a host cert not from our CA, a public key it
// will not take — is a misconfiguration, and spending the whole join budget on
// one turns a clear refusal into a fifteen-minute hang.
//
// Mutation: give directJoinRetry teleportJoinRetry's `retryable` (retry
// everything) and this reddens — the handshake rejection is dialed again. The
// budget is short so that mutant reddens on the count in milliseconds instead of
// burning the whole wait first.
func TestApply_DirectDoesNotRetryAHandshakeRejection(t *testing.T) {
	attempts := 0
	m := directModule(allowingProvider(), func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		return nil, hostCertRejected("192.168.122.173")
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "20ms",
	}))

	msg := mustFail(t, st)
	if !strings.Contains(msg, "no authorities for hostname") {
		t.Errorf("refusal = %q, want the handshake reason kept", msg)
	}
	if strings.Contains(msg, "join_wait_timeout") {
		t.Errorf("refusal = %q — a handshake rejection was reported as a wait that ran out", msg)
	}
	if attempts != 1 {
		t.Errorf("dialed %d time(s), want exactly one — waiting cannot fix a rejected handshake", attempts)
	}
}

// ★ GUARD (NIM-872). An `Authorize` deny is POLICY: the same question asked
// again gets the same answer. It is upstream of the retry loop entirely, so a
// deny costs one call and fails the step immediately — it must never consume the
// join budget.
//
// The budget here is short on purpose: a mutant that does retry the deny reddens
// on the count in milliseconds rather than hanging for a minute first.
//
// Mutation: move the Authorize call inside the loop AND broaden `retryable` —
// both halves, because the classifier alone already refuses to retry a deny (it
// is no dial error), so moving the call is not by itself observable. That the
// invariant survives either mutation singly is the defence in depth, not a hole;
// what guards the placement on its own is the one-call assertion in
// TestApply_DirectRetriesUntilSshdListens, which reddens when Authorize is moved
// under a dialer that DOES refuse.
func TestApply_DirectDoesNotRetryAnAuthorizeDeny(t *testing.T) {
	prov := &fakeProvider{allow: false, reason: "host not in scope", authorized: new(int), signed: new(int)}
	attempts := 0
	m := directModule(prov, func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		return &fakeSession{ran: &[]ranCmd{}, closed: new(int)}, nil
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "20ms",
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "host not in scope") {
		t.Errorf("refusal = %q, want the provider's reason", msg)
	}
	if *prov.authorized != 1 {
		t.Errorf("authorized %d time(s), want exactly one — a deny is policy, not a host that is late", *prov.authorized)
	}
	if attempts != 0 {
		t.Errorf("dialed %d time(s) after a deny", attempts)
	}
}

// ★ GUARD (NIM-872). A `Sign` failure is the credential mint refusing, which a
// wait cannot fix either. Same shape as the deny, and a separate test because
// the two are separate calls: a retry wrapped around the wrong one of them would
// leave the other guard green.
//
// Mutation: as for the deny above — move the call inside the loop AND broaden
// `retryable`.
func TestApply_DirectDoesNotRetryASignFailure(t *testing.T) {
	prov := allowingProvider()
	prov.signErr = errors.New("vault: ssh role not found")
	attempts := 0
	m := directModule(prov, func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		return &fakeSession{ran: &[]ranCmd{}, closed: new(int)}, nil
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "20ms",
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "ssh role not found") {
		t.Errorf("refusal = %q, want the sign error", msg)
	}
	if *prov.signed != 1 {
		t.Errorf("signed %d time(s), want exactly one", *prov.signed)
	}
	if attempts != 0 {
		t.Errorf("dialed %d time(s) after a failed sign", attempts)
	}
}

// ★ GUARD (NIM-872). The retry is the CONNECT's, not the step's. A command that
// exits non-zero has reached the host and answered; redialing it would re-run
// every earlier step in the list on a machine that already has them, and the
// bare `retry:` a scenario can put on the task is the author's own decision to
// make, not one the transport makes for them.
//
// Mutation: wrap runHost rather than the dial AND broaden `retryable` — a bare
// `exit status 1` is no dial error, so the classifier blocks the replay even
// from the wrong loop. Moving the loop alone is caught by
// TestApply_DirectRetriesUntilSshdListens and TestApply_DirectRetriesThroughABastion.
func TestApply_DirectDoesNotRetryANonZeroExit(t *testing.T) {
	attempts := 0
	ran := &[]ranCmd{}
	m := directModule(allowingProvider(), func(_ context.Context, _ push.DialConfig) (push.Session, error) {
		attempts++
		return &fakeSession{ran: ran, failOn: 1, closed: new(int)}, nil
	})
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider":      "vault-ssh",
		"hosts":             []any{tokenHost("redis-demo-0", "192.168.122.173")},
		"steps":             writeTokenSteps(),
		"join_wait_timeout": "20ms",
	}))

	if msg := mustFail(t, st); !strings.Contains(msg, "step 1") {
		t.Errorf("refusal = %q, want the failing step named", msg)
	}
	if attempts != 1 {
		t.Errorf("dialed %d time(s), want exactly one — a non-zero exit is an answer, not an absent host", attempts)
	}
	if len(*ran) != 1 {
		t.Errorf("ran %d command(s), want 1 — the failed step must not be replayed", len(*ran))
	}
}

// The classifier is the whole of the difference between the two transports, so
// it is asserted directly as well: the table is the failure modes a booting VM
// actually produces against the ones above the TCP layer.
func TestIsConnectFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"connection refused":      {connRefused("192.168.122.173"), true},
		"host unreachable":        {fmt.Errorf("push: TCP connection to x: %w", &net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}), true},
		"dial timeout":            {fmt.Errorf("push: TCP connection to x: %w", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}), true},
		"target refused by proxy": {proxyRefused("192.168.122.173"), true},
		"proxy declines on policy": {fmt.Errorf("push: direct-tcpip through proxy p to x: %w",
			&ssh.OpenChannelError{Reason: ssh.Prohibited, Message: "administratively prohibited"}), false},
		"handshake rejected":   {hostCertRejected("192.168.122.173"), false},
		"read after handshake": {fmt.Errorf("push: SSH handshake with x: %w", &net.OpError{Op: "read", Err: errors.New("reset")}), false},
		"empty host CA set":    {errors.New("push: HostAuthorities is empty (CA-signed host-cert verification)"), false},
		"nil":                  {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isConnectFailure(tc.err); got != tc.want {
				t.Errorf("isConnectFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// --- the dialer is the whole transport -------------------------------------

func TestApply_NoDialerIsAnExplicitRefusal(t *testing.T) {
	m := &Module{HostCAs: fakeCA}
	st := apply(t, m, params(t, map[string]any{
		"ssh_provider": "vault-ssh",
		"hosts":        []any{tokenHost("vm-1.example.com", "10.0.0.1")},
		"steps":        writeTokenSteps(),
	}))
	if msg := mustFail(t, st); !strings.Contains(msg, "dialer not configured") {
		t.Errorf("refusal = %q", msg)
	}
}

// testPrivateKeyPEM is a throwaway ed25519 key the fake provider hands back from
// Sign, so AuthMethodsFromSign has real material to parse. It authenticates
// nothing — the dialer is a mock.
var testPrivateKeyPEM = func() string {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(block))
}()
