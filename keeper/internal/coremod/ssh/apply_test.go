package ssh

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
)

// --- doubles for core.ssh.apply -------------------------------------------

// agentRun is what one host's `soul apply` answers: the task events and the
// final status, and an optional hook that runs before it answers.
type agentRun struct {
	events []*keeperv1.TaskEvent
	status keeperv1.RunStatus
	before func()
}

// agentHost records what was done to one host, in order.
type agentHost struct {
	mu  sync.Mutex
	log []string
	req *keeperv1.ApplyRequest
}

func (h *agentHost) add(s string) {
	h.mu.Lock()
	h.log = append(h.log, s)
	h.mu.Unlock()
}

type agentSession struct {
	host *agentHost
	run  agentRun
}

func (s *agentSession) Run(_ context.Context, cmd string, stdin []byte) (string, error) {
	s.host.add("exec " + cmd)
	if cmd != push.HostSoulBinaryPath+" apply" {
		return "", nil
	}
	req := &keeperv1.ApplyRequest{}
	if err := protojson.Unmarshal(stdin, req); err != nil {
		return "", fmt.Errorf("stdin is not an ApplyRequest: %w", err)
	}
	s.host.mu.Lock()
	s.host.req = req
	s.host.mu.Unlock()
	if s.run.before != nil {
		s.run.before()
	}
	var b strings.Builder
	for _, ev := range s.run.events {
		line, _ := protojson.Marshal(ev)
		b.Write(line)
		b.WriteByte('\n')
	}
	line, _ := protojson.Marshal(&keeperv1.RunResult{ApplyId: req.GetApplyId(), Status: s.run.status})
	b.Write(line)
	b.WriteByte('\n')
	return b.String(), nil
}

func (s *agentSession) Close() error { return nil }

// sessionDeliverer records delivery into the host's log, so the order against
// the exec is observable.
type sessionDeliverer struct{}

func (sessionDeliverer) Deliver(_ context.Context, sess push.Session, spec push.SoulSpec) error {
	if as, ok := sess.(*agentSession); ok {
		as.host.add("deliver " + spec.SoulBinaryPath)
	}
	return nil
}

// applyHarness is a teleport-transport Module over doubles: dialed hosts by SID,
// what each answers, and the render it was handed.
type applyHarness struct {
	mod      *Module
	mu       sync.Mutex
	hosts    map[string]*agentHost
	answers  map[string]agentRun
	dials    atomic.Int32
	rendered []string
	inputs   map[string]map[string]any
	audits   []*audit.Event
}

var twoTasks = []*keeperv1.RenderedTask{
	{Name: "Lay out /etc/soul", Module: "core.directory.present"},
	{Name: "Redeem the bootstrap token", Module: "core.exec.run"},
}

func okRun() agentRun {
	return agentRun{
		events: []*keeperv1.TaskEvent{
			{TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED},
			{TaskIdx: 1, Status: keeperv1.TaskStatus_TASK_STATUS_OK},
		},
		status: keeperv1.RunStatus_RUN_STATUS_SUCCESS,
	}
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	h := &applyHarness{
		hosts:   map[string]*agentHost{},
		answers: map[string]agentRun{},
		inputs:  map[string]map[string]any{},
	}
	h.mod = &Module{
		Transport:   TransportTeleport,
		Deliverer:   sessionDeliverer{},
		SoulSpec:    push.SoulSpec{SoulBinaryPath: "/opt/keeper/soul"},
		RetryBase:   time.Millisecond,
		RetryJitter: time.Millisecond,
		Dial: func(_ context.Context, cfg push.DialConfig) (push.Session, error) {
			h.dials.Add(1)
			h.mu.Lock()
			defer h.mu.Unlock()
			host := h.hosts[cfg.Host]
			if host == nil {
				host = &agentHost{}
				h.hosts[cfg.Host] = host
			}
			run, ok := h.answers[cfg.Host]
			if !ok {
				run = okRun()
			}
			return &agentSession{host: host, run: run}, nil
		},
		Audit: auditFunc(func(_ context.Context, ev *audit.Event) error {
			h.mu.Lock()
			h.audits = append(h.audits, ev)
			h.mu.Unlock()
			return nil
		}),
	}
	return h
}

func (h *applyHarness) render(_ context.Context, name, sid string, input map[string]any) (*util.RenderedDestiny, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rendered = append(h.rendered, sid)
	h.inputs[sid] = input
	return &util.RenderedDestiny{Ref: "v1.2.0", Tasks: twoTasks, Secrets: []string{"render-sealed-value"}}, nil
}

func (h *applyHarness) ctx() context.Context {
	ctx := util.WithDestinyRenderer(context.Background(), h.render)
	return util.WithRunScope(ctx, util.RunScope{ApplyID: "apply-905"})
}

func runApply(t *testing.T, m *Module, ctx context.Context, p *structpb.Struct) *internaltest.ApplyStream {
	t.Helper()
	st := internaltest.NewApplyStreamCtx(ctx)
	if err := m.Apply(&pluginv1.ApplyRequest{State: StateApply, Params: p}, st); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	if st.Last() == nil {
		t.Fatal("Apply sent no final event")
	}
	return st
}

func applyParams(t *testing.T, hosts ...map[string]any) *structpb.Struct {
	t.Helper()
	list := make([]any, 0, len(hosts))
	for _, h := range hosts {
		list = append(list, h)
	}
	return params(t, map[string]any{
		"ssh_provider": "teleport",
		"destiny":      "soul",
		"hosts":        list,
		"input":        map[string]any{"keeper_host": "keeper.example"},
		"input_from":   map[string]any{"bootstrap_token": "bootstrap_token"},
	})
}

func vm(sid string) map[string]any {
	return map[string]any{"sid": sid, "bootstrap_token": testToken + "-" + sid}
}

// --- the invariants --------------------------------------------------------

// ★ GUARD (NIM-905, decision 7). Every host is rendered before any is dialed: a
// host whose entry lacks the field `input_from:` names stops the step with
// nothing executed on ANY host. core.ssh.run resolves `stdin_from:` inside its
// dial loop and so runs earlier hosts' commands before refusing a later one.
//
// Mutation: resolve input_from inside applyHost (after the dial) and this
// reddens — vm-1 is dialed and installed before vm-2 is refused.
func TestSSHApply_RendersEveryHostBeforeDialingAny(t *testing.T) {
	h := newApplyHarness(t)
	noToken := map[string]any{"sid": "vm-2.example"}
	msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), noToken)))

	if !strings.Contains(msg, `"vm-2.example"`) || !strings.Contains(msg, "bootstrap_token") {
		t.Errorf("refusal = %q, want the host and the missing field named", msg)
	}
	if n := h.dials.Load(); n != 0 {
		t.Errorf("dialed %d host(s) before refusing — a refusal must leave every host untouched", n)
	}
}

// A render error is the same: refused before the first dial.
func TestSSHApply_RenderErrorDialsNothing(t *testing.T) {
	h := newApplyHarness(t)
	ctx := util.WithDestinyRenderer(context.Background(), func(_ context.Context, _, sid string, _ map[string]any) (*util.RenderedDestiny, error) {
		if sid == "vm-2.example" {
			return nil, errors.New("no such key: os")
		}
		return &util.RenderedDestiny{Tasks: twoTasks}, nil
	})
	msg := mustFail(t, runApply(t, h.mod, ctx, applyParams(t, vm("vm-1.example"), vm("vm-2.example"))))
	if !strings.Contains(msg, "no such key: os") {
		t.Errorf("refusal = %q, want the render error", msg)
	}
	if n := h.dials.Load(); n != 0 {
		t.Errorf("dialed %d host(s) before a render error", n)
	}
}

// ★ GUARD (decisions 2, 7). An `onboarded: true` entry carries no token and is
// skipped BEFORE its fields are read: a repeat run over a half-onboarded batch is
// not refused for the token the converged host does not carry.
//
// Mutation: drop the `if h.onboarded` skip in applyDestiny and this reddens.
func TestSSHApply_OnboardedHostIsSkippedNotRenderedNorDialed(t *testing.T) {
	h := newApplyHarness(t)
	st := runApply(t, h.mod, h.ctx(), applyParams(t, converged("vm-0.example"), vm("vm-1.example")))
	out := mustSucceed(t, st).GetOutput().AsMap()

	if got := out["skipped"]; got != float64(1) {
		t.Errorf("skipped = %v, want 1", got)
	}
	if _, dialed := h.hosts["vm-0.example"]; dialed {
		t.Error("an onboarded host was dialed")
	}
	for _, sid := range h.rendered {
		if sid == "vm-0.example" {
			t.Error("an onboarded host was rendered for")
		}
	}
}

// ★ GUARD (decisions 3, 8). The list branch runs the shared executor: the agent
// is delivered first, then the DELIVERED path is exec'd with the rendered tasks
// on stdin — a bare host has no other `soul`.
//
// Mutation: exec anything but push.HostSoulBinaryPath, or skip the deliverer,
// and this reddens.
func TestSSHApply_DeliversThenExecsTheDeliveredAgent(t *testing.T) {
	h := newApplyHarness(t)
	mustSucceed(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"))))

	host := h.hosts["vm-1.example"]
	want := []string{"deliver /opt/keeper/soul", "exec " + push.HostSoulBinaryPath + " apply"}
	if strings.Join(host.log, "|") != strings.Join(want, "|") {
		t.Errorf("host saw %q, want %q", host.log, want)
	}
	if host.req.GetApplyId() != "apply-905" || len(host.req.GetTasks()) != len(twoTasks) {
		t.Errorf("ApplyRequest = %v, want the run's apply_id and the rendered tasks", host.req)
	}
}

// ★ GUARD (decision 2). input_from gives each host ITS OWN value, input the same
// one to all, and the destiny input carries both.
func TestSSHApply_InputFromIsPerHost(t *testing.T) {
	h := newApplyHarness(t)
	mustSucceed(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), vm("vm-2.example"))))

	for _, sid := range []string{"vm-1.example", "vm-2.example"} {
		in := h.inputs[sid]
		if in["bootstrap_token"] != testToken+"-"+sid {
			t.Errorf("%s got bootstrap_token %v, want its own", sid, in["bootstrap_token"])
		}
		if in["keeper_host"] != "keeper.example" {
			t.Errorf("%s lost the shared input: %v", sid, in)
		}
	}
}

// ★ GUARD (decision 8). A bare host has no agent: with delivery off the step is
// refused by name before anything is dialed, never exec'd blind.
func TestSSHApply_NoDeliveryIsRefusedBeforeAnyDial(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		h := newApplyHarness(t)
		h.mod.Deliverer = nil
		msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"))))
		if !strings.Contains(msg, "push.soul_binary_path") {
			t.Errorf("refusal = %q, want the unset key named", msg)
		}
		if h.dials.Load() != 0 {
			t.Error("dialed without an agent to deliver")
		}
	})
	t.Run("unreadable", func(t *testing.T) {
		h := newApplyHarness(t)
		h.mod.Deliverer = nil
		h.mod.DeliveryErr = errors.New(`push.soul_binary_path "/opt/soul": permission denied`)
		msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"))))
		if !strings.Contains(msg, "permission denied") {
			t.Errorf("refusal = %q, want the startup error carried", msg)
		}
	})
}

// ★ GUARD (decision 11). Hosts run at once: each host's `soul apply` waits until
// all three have reached it, which only a concurrent fan-out can satisfy.
//
// Mutation: run applyHost in the loop instead of a goroutine and this reddens —
// the first host waits out the barrier alone.
func TestSSHApply_HostsRunConcurrently(t *testing.T) {
	h := newApplyHarness(t)
	var arrived atomic.Int32
	all := make(chan struct{})
	var once sync.Once
	barrier := func() {
		if arrived.Add(1) == 3 {
			once.Do(func() { close(all) })
		}
		select {
		case <-all:
		case <-time.After(2 * time.Second):
		}
	}
	for _, sid := range []string{"vm-1.example", "vm-2.example", "vm-3.example"} {
		run := okRun()
		run.before = barrier
		h.answers[sid] = run
	}
	start := time.Now()
	mustSucceed(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), vm("vm-2.example"), vm("vm-3.example"))))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("three hosts took %v — they ran one after another", elapsed)
	}
}

// ★ GUARD (decision 11). Every host runs to its end, and the step fails naming
// every failed host with its failed task and reason, the token masked.
//
// Mutation: return on the first failed outcome and this reddens — vm-3 is never
// reached, or vm-3's failure goes unnamed.
func TestSSHApply_EveryHostRunsAndEveryFailureIsNamed(t *testing.T) {
	h := newApplyHarness(t)
	failing := func(sid string) agentRun {
		return agentRun{
			events: []*keeperv1.TaskEvent{
				{TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_OK},
				{TaskIdx: 1, Status: keeperv1.TaskStatus_TASK_STATUS_FAILED, Error: &keeperv1.TaskError{Message: "soul init: token " + testToken + "-" + sid + " rejected"}},
			},
			status: keeperv1.RunStatus_RUN_STATUS_FAILED,
		}
	}
	h.answers["vm-1.example"] = failing("vm-1.example")
	h.answers["vm-3.example"] = failing("vm-3.example")

	msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), vm("vm-2.example"), vm("vm-3.example"))))

	for _, sid := range []string{"vm-1.example", "vm-2.example", "vm-3.example"} {
		if h.hosts[sid] == nil || h.hosts[sid].req == nil {
			t.Errorf("%s never ran — one host's failure must not stop the others", sid)
		}
	}
	for _, want := range []string{`"vm-1.example"`, `"vm-3.example"`, "Redeem the bootstrap token", "2 of 3"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure = %q, want %q in it", msg, want)
		}
	}
	if strings.Contains(msg, `"vm-2.example"`) {
		t.Errorf("failure = %q names a host that succeeded", msg)
	}
	if strings.Contains(msg, testToken) {
		t.Errorf("failure = %q carries a host's bootstrap token", msg)
	}
}

// ★ GUARD (decision 12). The audit records what ran — the destiny at its ref and
// each host's tasks with their status — under `ssh.run` with `action: apply`,
// correlated to the run, and nothing a token could hide in. It is written when
// the step fails too.
func TestSSHApply_AuditRecordsWhatRan(t *testing.T) {
	h := newApplyHarness(t)
	h.answers["vm-2.example"] = agentRun{
		events: []*keeperv1.TaskEvent{{TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_FAILED, Error: &keeperv1.TaskError{Message: "boom " + testToken + "-vm-2.example"}}},
		status: keeperv1.RunStatus_RUN_STATUS_FAILED,
	}
	mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), vm("vm-2.example"))))

	if len(h.audits) != 1 {
		t.Fatalf("audit events = %d, want one, written on failure too", len(h.audits))
	}
	ev := h.audits[0]
	if ev.EventType != audit.EventSSHRun || ev.Payload["action"] != StateApply || ev.CorrelationID != "apply-905" {
		t.Errorf("event = %s action=%v correlation=%q", ev.EventType, ev.Payload["action"], ev.CorrelationID)
	}
	if ev.Payload["destiny"] != "soul@v1.2.0" {
		t.Errorf("destiny = %v, want name@ref", ev.Payload["destiny"])
	}
	hosts, _ := ev.Payload["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts = %v", ev.Payload["hosts"])
	}
	first, _ := hosts[0].(map[string]any)
	tasks, _ := first["tasks"].([]any)
	if first["status"] != "success" || len(tasks) != 2 {
		t.Errorf("vm-1 = %v, want success with both tasks", first)
	}
	task0, _ := tasks[0].(map[string]any)
	if task0["task"] != "Lay out /etc/soul" || task0["module"] != "core.directory.present" || task0["status"] != "changed" {
		t.Errorf("task record = %v", task0)
	}
	if second, _ := hosts[1].(map[string]any); second["status"] != "failed" {
		t.Errorf("vm-2 = %v, want failed", second)
	}
	if leak := findString(ev.Payload, testToken); leak != "" {
		t.Errorf("audit payload carries a token at %s", leak)
	}
}

type failingDeliverer struct{}

func (failingDeliverer) Deliver(context.Context, push.Session, push.SoulSpec) error {
	return errors.New("mkdir /var/lib/soul-stack/bin: permission denied")
}

// A host whose delivery failed ran no task, and its audit entry says so by
// carrying none — not the destiny's whole task list marked not_run, which would
// read as a destiny that started.
//
// Mutation: record taskRecords unconditionally in applyHost and this reddens.
func TestSSHApply_FailedDeliveryAuditsNoTasks(t *testing.T) {
	h := newApplyHarness(t)
	h.mod.Deliverer = failingDeliverer{}
	msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"))))
	if !strings.Contains(msg, "permission denied") {
		t.Errorf("failure = %q, want the delivery error", msg)
	}
	hosts, _ := h.audits[0].Payload["hosts"].([]any)
	entry, _ := hosts[0].(map[string]any)
	if entry["status"] != "failed" {
		t.Errorf("entry = %v, want failed", entry)
	}
	if _, has := entry["tasks"]; has {
		t.Errorf("entry = %v carries tasks for a host where nothing ran", entry)
	}
}

// ★ GUARD (decision 10). On teleport the join wait retries every error, so a
// host-key refusal has to stop it by type — on BOTH states, since the dial is
// shared.
//
// Mutation: drop the push.IsHostKeyError check in dialWithJoinRetry and this
// reddens — the refusal is dialed until the budget runs out.
func TestSSH_TeleportDoesNotRetryAHostKeyRefusal(t *testing.T) {
	refusal := func(_ context.Context, cfg push.DialConfig) (push.Session, error) {
		return nil, fmt.Errorf("push: teleport SSH handshake with %s: %w", cfg.Host, &push.HostKeyError{Err: errors.New("ssh: no authorities for hostname")})
	}
	for _, state := range []string{StateRun, StateApply} {
		t.Run(state, func(t *testing.T) {
			var attempts atomic.Int32
			h := newApplyHarness(t)
			h.mod.Dial = func(ctx context.Context, cfg push.DialConfig) (push.Session, error) {
				attempts.Add(1)
				return refusal(ctx, cfg)
			}
			p := applyParams(t, vm("vm-1.example"))
			if state == StateRun {
				p = params(t, map[string]any{"ssh_provider": "teleport", "hosts": []any{vm("vm-1.example")}, "steps": writeTokenSteps()})
			}
			p.Fields["join_wait_timeout"] = structpb.NewStringValue("200ms")
			st := internaltest.NewApplyStreamCtx(h.ctx())
			if err := h.mod.Apply(&pluginv1.ApplyRequest{State: state, Params: p}, st); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			msg := mustFail(t, st)
			if attempts.Load() != 1 {
				t.Errorf("dialed %d time(s), want one — a refused key is refused again", attempts.Load())
			}
			if strings.Contains(msg, "join_wait_timeout") {
				t.Errorf("refusal = %q reads as a wait that ran out", msg)
			}
		})
	}
}

// ★ GUARD (decision 10). On direct, an Authorize deny fails the host at once and
// names it; nothing is dialed.
func TestSSHApply_DirectAuthorizeDenyNamesTheHost(t *testing.T) {
	h := newApplyHarness(t)
	h.mod.Transport = TransportDirect
	prov := &fakeProvider{allow: false, reason: "not in the allowed CIDR", authorized: new(int), signed: new(int)}
	h.mod.Providers = func() map[string]SshProviderHost { return map[string]SshProviderHost{"vault-ssh": prov} }
	h.mod.HostCAs = fakeCA
	p := applyParams(t, map[string]any{"sid": "vm-1.example", "primary_ip": "10.0.0.7", "bootstrap_token": testToken})
	p.Fields["ssh_provider"] = structpb.NewStringValue("vault-ssh")

	msg := mustFail(t, runApply(t, h.mod, h.ctx(), p))
	if !strings.Contains(msg, `"vm-1.example"`) || !strings.Contains(msg, "not in the allowed CIDR") {
		t.Errorf("refusal = %q, want the host and the deny reason", msg)
	}
	if h.dials.Load() != 0 {
		t.Error("dialed a host the provider denied")
	}
}

// The output is the roster and nothing the destiny printed.
func TestSSHApply_OutputIsTheRoster(t *testing.T) {
	h := newApplyHarness(t)
	ev := mustSucceed(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), converged("vm-2.example"))))
	if !ev.GetChanged() {
		t.Error("a host changed something, the step reports no change")
	}
	out := ev.GetOutput().AsMap()
	hosts, _ := out["hosts"].([]any)
	if len(hosts) != 2 || out["count"] != float64(2) || out["changed"] != float64(1) {
		t.Fatalf("output = %v", out)
	}
	first, _ := hosts[0].(map[string]any)
	if first["sid"] != "vm-1.example" || first["ran"] != true || first["changed"] != true || first["skipped"] != false {
		t.Errorf("hosts[0] = %v", first)
	}
	if leak := findString(out, testToken); leak != "" {
		t.Errorf("output carries a token at %s", leak)
	}
}

func TestSSHApply_RefusesWhatCannotRun(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*structpb.Struct)
		ctx    func(*applyHarness) context.Context
		want   string
	}{
		"input and input_from name the same input": {
			mutate: func(p *structpb.Struct) {
				p.Fields["input"].GetStructValue().Fields["bootstrap_token"] = structpb.NewStringValue("x")
			},
			want: "both set bootstrap_token",
		},
		"no destiny": {
			mutate: func(p *structpb.Struct) { delete(p.Fields, "destiny") },
			want:   `"destiny"`,
		},
		"outside a scenario run": {
			ctx:  func(*applyHarness) context.Context { return context.Background() },
			want: "no destiny render",
		},
		"token_held entry": {
			mutate: func(p *structpb.Struct) {
				held, _ := structpb.NewStruct(map[string]any{"sid": "vm-9.example", "token_held": true})
				p.Fields["hosts"].GetListValue().Values = append(p.Fields["hosts"].GetListValue().Values, structpb.NewStructValue(held))
			},
			want: "token_held",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newApplyHarness(t)
			p := applyParams(t, vm("vm-1.example"))
			if tc.mutate != nil {
				tc.mutate(p)
			}
			ctx := h.ctx()
			if tc.ctx != nil {
				ctx = tc.ctx(h)
			}
			if msg := mustFail(t, runApply(t, h.mod, ctx, p)); !strings.Contains(msg, tc.want) {
				t.Errorf("refusal = %q, want %q", msg, tc.want)
			}
			if h.dials.Load() != 0 {
				t.Error("dialed before refusing")
			}
		})
	}
}

func TestValidate_Apply(t *testing.T) {
	m := &Module{}
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: StateApply, Params: applyParams(t, vm("vm-1.example"))})
	if err != nil || !reply.GetOk() {
		t.Fatalf("valid apply params refused: %v %v", err, reply.GetErrors())
	}
	p := applyParams(t, vm("vm-1.example"))
	delete(p.Fields, "destiny")
	if reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: StateApply, Params: p}); reply.GetOk() {
		t.Error("apply without a destiny passed validation")
	}
}

// ★ GUARD (decision 5). This module neither reads nor writes the registry: its
// own sources import no registry store and no Postgres driver, and its Deps carry
// none. The scope is this package's surface — `push`, which it imports, holds the
// pgx-backed readers of the REGISTRY branch, and this module never constructs one.
//
// Mutation: import keeper/internal/soul (or pushorch, or pgx) into a non-test
// file of this package and this reddens.
func TestSSHModule_ImportsNoRegistry(t *testing.T) {
	forbidden := []string{
		"github.com/souls-guild/soul-stack/keeper/internal/soul",
		"github.com/souls-guild/soul-stack/keeper/internal/pushorch",
		"github.com/jackc/pgx",
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, bad := range forbidden {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %s — core.ssh must not touch the registry", name, path)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no sources checked")
	}
}

// ★ GUARD (decision 11, validator finding). A value the run sealed in this
// step's OWN params — a `${ vault(…) }` handed to a destiny input the destiny does
// not declare secret — is masked in the audit record's per-host error, which
// nothing downstream masks for it.
//
// Mutation: drop the SealedPathsFrom term from `secrets` in applyDestiny and this
// reddens.
func TestSSHApply_StepSealIsMaskedInTheAudit(t *testing.T) {
	const adminPW = "vault-admin-pw-91c2"
	h := newApplyHarness(t)
	h.answers["vm-1.example"] = agentRun{
		events: []*keeperv1.TaskEvent{{TaskIdx: 0, Status: keeperv1.TaskStatus_TASK_STATUS_FAILED, Error: &keeperv1.TaskError{Message: "login with " + adminPW + " refused"}}},
		status: keeperv1.RunStatus_RUN_STATUS_FAILED,
	}
	p := applyParams(t, vm("vm-1.example"))
	p.Fields["input"].GetStructValue().Fields["admin_pw"] = structpb.NewStringValue(adminPW)
	ctx := util.WithSealedPaths(h.ctx(), map[string]bool{"input.admin_pw": true})

	msg := mustFail(t, runApply(t, h.mod, ctx, p))
	if strings.Contains(msg, adminPW) {
		t.Errorf("step message carries the sealed value: %q", msg)
	}
	if leak := findString(h.audits[0].Payload, adminPW); leak != "" {
		t.Errorf("audit payload carries the sealed value at %s", leak)
	}
}

// Hosts run at once, so a SID listed twice would race two installs on one
// machine and redeem one token twice: refused before anything is dialed.
//
// Mutation: drop the duplicate-SID check in applyDestiny and this reddens.
func TestSSHApply_ADuplicateHostIsRefused(t *testing.T) {
	h := newApplyHarness(t)
	msg := mustFail(t, runApply(t, h.mod, h.ctx(), applyParams(t, vm("vm-1.example"), vm("vm-2.example"), vm("vm-1.example"))))
	if !strings.Contains(msg, `"vm-1.example"`) || !strings.Contains(msg, "hosts[0]") || !strings.Contains(msg, "hosts[2]") {
		t.Errorf("refusal = %q, want both positions and the SID", msg)
	}
	if h.dials.Load() != 0 {
		t.Error("dialed before refusing a duplicate")
	}
}

// ★ GUARD (decision 12). The audit is written on a context detached from the
// run's: a run being cut off is when the record of what ran matters most.
//
// Mutation: write the audit on the stream's ctx and this reddens.
func TestSSHApply_AuditIsWrittenWhenTheRunIsCancelled(t *testing.T) {
	h := newApplyHarness(t)
	var writeErr error
	h.mod.Audit = auditFunc(func(ctx context.Context, ev *audit.Event) error {
		writeErr = ctx.Err()
		h.audits = append(h.audits, ev)
		return nil
	})
	ctx, cancel := context.WithCancel(h.ctx())
	cancel()
	runApply(t, h.mod, ctx, applyParams(t, vm("vm-1.example")))
	if len(h.audits) != 1 {
		t.Fatalf("audit events = %d, want one", len(h.audits))
	}
	if writeErr != nil {
		t.Errorf("the audit was written on a cancelled context (%v) — a real writer would drop it", writeErr)
	}
}

// Validate reports every bad connection param, not only the first.
func TestValidate_ReportsEveryBadConnectionParam(t *testing.T) {
	m := &Module{}
	for _, state := range []string{StateRun, StateApply} {
		p := applyParams(t, vm("vm-1.example"))
		if state == StateRun {
			p = params(t, map[string]any{"ssh_provider": "vault-ssh", "hosts": []any{vm("vm-1.example")}, "steps": writeTokenSteps()})
		}
		p.Fields["ssh_port"] = structpb.NewNumberValue(70000)
		p.Fields["join_wait_timeout"] = structpb.NewStringValue("soon")
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{State: state, Params: p})
		if reply.GetOk() || len(reply.GetErrors()) < 2 {
			t.Errorf("%s: errors = %v, want both the port and the wait reported", state, reply.GetErrors())
		}
	}
}
