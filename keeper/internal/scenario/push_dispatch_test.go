package scenario

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// --- fixtures ---------------------------------------------------------

// recordingDB records every statement the dispatcher issues. The push branch's
// whole contract with the barrier is a pair of writes — the register
// accumulator and the terminal — so the statements ARE the subject here, not a
// side effect of it.
type recordingDB struct {
	mu   sync.Mutex
	sqls []string
	args [][]any
}

// Exec HONOURS the context, unlike most fakes: a real pool refuses a cancelled
// one, and the push branch's terminal write is specifically supposed to survive
// a cancelled run. A recorder that ignored ctx would record the statement either
// way and make that property untestable.
func (d *recordingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := ctx.Err(); err != nil {
		return pgconn.CommandTag{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sqls = append(d.sqls, sql)
	d.args = append(d.args, args)
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (d *recordingDB) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{err: pgx.ErrNoRows}
}

func (d *recordingDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

// statementsMatching returns the recorded statements containing frag, with the
// args they carried. `SET status` distinguishes the terminal write from
// RecordTaskFailure, which also UPDATEs apply_runs but never touches status.
func (d *recordingDB) statementsMatching(frag string) [][]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out [][]any
	for i, s := range d.sqls {
		if strings.Contains(s, frag) {
			out = append(out, d.args[i])
		}
	}
	return out
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakePushDispatcher plays the host: it emits the TaskEvents it was given and
// returns the RunResult (or the error) it was configured with.
type fakePushDispatcher struct {
	events []*keeperv1.TaskEvent
	result *keeperv1.RunResult
	err    error

	mu        sync.Mutex
	gotRoute  push.Route
	gotSID    string
	gotHandle bool
}

func (f *fakePushDispatcher) SendApply(_ context.Context, sid string, route push.Route, _ *keeperv1.ApplyRequest, onEvent push.EventHandler) (*keeperv1.RunResult, error) {
	f.mu.Lock()
	f.gotSID, f.gotRoute, f.gotHandle = sid, route, onEvent != nil
	f.mu.Unlock()
	for _, ev := range f.events {
		if onEvent != nil {
			onEvent(ev)
		}
	}
	return f.result, f.err
}

type fixedRouter struct {
	name   string
	source push.RouteSource
	err    error
}

func (r fixedRouter) RouteFor(context.Context, string) (string, push.RouteSource, error) {
	return r.name, r.source, r.err
}

func hostFacts(sid, transport string) *topology.HostFacts {
	return &topology.HostFacts{SID: sid, Transport: transport}
}

func onePlan(sid string, transport any) (map[string][]*render.RenderedTask, []render.DispatchPlan) {
	perHost := map[string][]*render.RenderedTask{sid: {{Index: 0, Module: "core.exec.run"}}}
	plan := render.DispatchPlan{TaskIndex: 0, TargetSIDs: []string{sid}}
	if transport != nil {
		name, params, ok := config.TransportSpecOf(transport)
		if !ok {
			panic("onePlan: test fixture wrote an undecodable transport")
		}
		plan.TransportName, plan.TransportParams = name, params
	}
	return perHost, []render.DispatchPlan{plan}
}

// --- transport resolution ---------------------------------------------

// The effective transport is the HOST's; the task's key may not contradict it.
// It still beats the registry on the three fields it actually carries
// (ssh_provider/user/port) — that is what TestResolveHostDispatch_OverrideRides
// pins.
func TestResolveHostDispatch_RegistryPicksTheBranch(t *testing.T) {
	tests := []struct {
		name      string
		registry  string
		transport any
		wantPush  bool
	}{
		{"no key, agent host -> stream", config.TransportAgent, nil, false},
		{"no key, ssh host -> push", config.TransportSSH, nil, true},
		{"transport: ssh on an ssh host -> push", config.TransportSSH, config.TransportSSH, true},
		{"transport: agent on an agent host -> stream", config.TransportAgent, config.TransportAgent, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perHost, plans := onePlan("h1.example.com", tt.transport)
			got, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", tt.registry)})
			if len(refusals) != 0 {
				t.Fatalf("unexpected refusal: %v", refusals)
			}
			if got["h1.example.com"].isPush() != tt.wantPush {
				t.Errorf("isPush = %v, want %v (effective %q)", got["h1.example.com"].isPush(), tt.wantPush, got["h1.example.com"].transport)
			}
		})
	}
}

// A key naming a transport the host's registry row does not carry is REFUSED,
// not ignored. Ignoring it would make the key decorative, which is the one
// outcome ADR-0088 exists to prevent; and in the ssh-on-agent direction the
// push dispatcher refuses the row anyway, three layers later and with a worse
// sentence.
func TestResolveHostDispatch_MismatchIsRefused(t *testing.T) {
	tests := []struct {
		name      string
		registry  string
		transport any
	}{
		{"ssh named on an agent host", config.TransportAgent, config.TransportSSH},
		{"agent named on an ssh host", config.TransportSSH, config.TransportAgent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perHost, plans := onePlan("h1.example.com", tt.transport)
			got, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", tt.registry)})
			if len(got) != 0 {
				t.Fatalf("host was dispatched anyway: %+v", got)
			}
			if refusals["h1.example.com"].reason != reasonTransportMismatch {
				t.Fatalf("reason = %q, want %q", refusals["h1.example.com"].reason, reasonTransportMismatch)
			}
		})
	}
}

// Two tasks of one Passage targeting one host with different transports: a host
// receives ONE ApplyRequest per Passage, so there is nothing to split and
// first-wins would dispatch one of them down a line its own file does not name.
func TestResolveHostDispatch_DisagreementIsRefused(t *testing.T) {
	perHost := map[string][]*render.RenderedTask{
		"h1.example.com": {{Index: 0}, {Index: 1}},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h1.example.com"}, TransportName: config.TransportSSH},
		{TaskIndex: 1, TargetSIDs: []string{"h1.example.com"}, TransportName: config.TransportAgent},
	}
	_, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", config.TransportSSH)})
	if refusals["h1.example.com"].reason != reasonTransportDisagreement {
		t.Fatalf("reason = %q, want %q", refusals["h1.example.com"].reason, reasonTransportDisagreement)
	}
}

// ★ Two tasks naming the SAME transport with DIFFERENT params disagree just as
// much as two naming different transports: `transport: ssh` beside
// `transport: { ssh: { user: deploy } }` is one transport and two connections,
// and last-wins would drop a Level 0 override with no diagnostic. The message
// has to say which shape of disagreement it is, because the fixes are opposite.
func TestResolveHostDispatch_DisagreeingParamsAreRefused(t *testing.T) {
	perHost := map[string][]*render.RenderedTask{
		"h1.example.com": {{Index: 0}, {Index: 1}},
	}
	plans := []render.DispatchPlan{
		{
			TaskIndex: 0, TargetSIDs: []string{"h1.example.com"},
			TransportName:   config.TransportSSH,
			TransportParams: map[string]any{config.TransportParamUser: "deploy"},
		},
		{TaskIndex: 1, TargetSIDs: []string{"h1.example.com"}, TransportName: config.TransportSSH},
	}
	got, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", config.TransportSSH)})
	if len(got) != 0 {
		t.Fatalf("host was dispatched anyway with override %+v - the other task's override was dropped silently", got["h1.example.com"].override)
	}
	if refusals["h1.example.com"].reason != reasonTransportDisagreement {
		t.Fatalf("reason = %q, want %q", refusals["h1.example.com"].reason, reasonTransportDisagreement)
	}
	if msg := refusals["h1.example.com"].err.Error(); !strings.Contains(msg, "parameter sets") {
		t.Errorf("message = %q, want it to name the PARAMS as what disagrees - naming the transport would send the author to the wrong line", msg)
	}
}

// A task the cross-passage gate dropped for a host runs nowhere, so it does not
// get a vote on that host's transport. `plans` is the Passage's whole list while
// `perHost` has already been narrowed by the gate — reading the former alone
// would abort a run over a line that is not being dispatched.
func TestResolveHostDispatch_AGatedOutTaskDoesNotVote(t *testing.T) {
	// The gate kept task 1 only; task 0's plan still names the other transport.
	perHost := map[string][]*render.RenderedTask{
		"h1.example.com": {{Index: 1}},
	}
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"h1.example.com"}, TransportName: config.TransportAgent},
		{TaskIndex: 1, TargetSIDs: []string{"h1.example.com"}, TransportName: config.TransportSSH},
	}
	got, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", config.TransportSSH)})
	if len(refusals) != 0 {
		t.Fatalf("refused over a task the gate dropped: %v", refusals)
	}
	if !got["h1.example.com"].isPush() {
		t.Errorf("effective transport = %q, want the surviving task's ssh", got["h1.example.com"].transport)
	}
}

// A param that does not decode fails the hosts it targets rather than falling
// back to the registry the key exists to beat.
func TestResolveHostDispatch_UndecodableParamIsRefused(t *testing.T) {
	perHost, plans := onePlan("h1.example.com", map[string]any{
		config.TransportSSH: map[string]any{config.TransportParamPort: "2222"},
	})
	_, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", config.TransportSSH)})
	if refusals["h1.example.com"].reason != reasonTransportMismatch {
		t.Fatalf("reason = %q, want %q (a string port must not be read as the registry's answer)",
			refusals["h1.example.com"].reason, reasonTransportMismatch)
	}
}

// A targeted host with no roster entry cannot happen (both come from one
// resolve) and must not be defaulted to `agent`: that would dispatch over a
// stream nobody confirmed.
func TestResolveHostDispatch_HostOutsideTheRosterIsRefused(t *testing.T) {
	perHost, plans := onePlan("ghost.example.com", nil)
	_, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("other.example.com", config.TransportAgent)})
	if refusals["ghost.example.com"].reason != reasonTransportMismatch {
		t.Fatalf("refusals = %+v, want a refusal for the unrostered host", refusals)
	}
}

// The task's ssh_provider/user/port still beat souls.ssh_target and the
// keeper.yml defaults — the Level 0 of ADR-0088. Registry-picks-the-branch is
// about the BRANCH, not about these three fields.
func TestResolveHostDispatch_OverrideRides(t *testing.T) {
	perHost, plans := onePlan("h1.example.com", map[string]any{
		config.TransportSSH: map[string]any{
			config.TransportParamSSHProvider: "vault-bastion",
			config.TransportParamUser:        "deploy",
			config.TransportParamPort:        uint64(2222),
		},
	})
	got, refusals := resolveHostDispatch(perHost, plans, []*topology.HostFacts{hostFacts("h1.example.com", config.TransportSSH)})
	if len(refusals) != 0 {
		t.Fatalf("unexpected refusal: %v", refusals)
	}
	hd := got["h1.example.com"]
	if hd.override.Provider != "vault-bastion" || hd.override.User != "deploy" || hd.override.Port != 2222 {
		t.Fatalf("override = %+v, want the task's three fields", hd.override)
	}
	if hd.named != config.TransportSSH {
		t.Errorf("named = %q, want the transport the task wrote", hd.named)
	}
}

// --- the run on the host ----------------------------------------------

// ★ The acceptance of NIM-880 in unit form: a task executed over push fills
// `register:` through the same accumulator a streamed one does. The FK on
// apply_task_register targets the apply_runs row this dispatch path already
// mints, so the write lands — which is what makes a `require:` over a push
// register release instead of waiting out the run timeout.
func TestDispatchPushHost_FillsRegisterAndWritesSuccess(t *testing.T) {
	db := &recordingDB{}
	rd, err := structpb.NewStruct(map[string]any{"stdout": "PONG"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{{
			ApplyId: "ap-1", TaskIdx: 0, PlanIndex: 0,
			Status: keeperv1.TaskStatus_TASK_STATUS_CHANGED, RegisterData: rd,
		}},
		result: &keeperv1.RunResult{ApplyId: "ap-1", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-1"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-1"})

	if !disp.gotHandle {
		t.Fatal("SendApply was given a nil EventHandler - a push run would then keep no register at all")
	}
	regs := db.statementsMatching("INSERT INTO apply_task_register")
	if len(regs) != 1 {
		t.Fatalf("apply_task_register writes = %d, want 1", len(regs))
	}
	if regs[0][1] != "h1.example.com" {
		t.Errorf("register row sid = %v, want the push host", regs[0][1])
	}
	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want exactly 1", len(terminals))
	}
	if terminals[0][2] != "success" {
		t.Errorf("terminal status = %v, want success", terminals[0][2])
	}
}

// A transport failure never produces a RunResult. Without a written terminal
// the barrier would wait for this host until the run timeout, so the failure
// path owes the row exactly as much as the success path does.
func TestDispatchPushHost_TransportFailureStillWritesATerminal(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{err: errors.New("Authorize refused for deploy@h1")}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-2"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-2"})

	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1 - a barrier with no terminal hangs until the run timeout", len(terminals))
	}
	if terminals[0][2] != "failed" {
		t.Fatalf("status = %v, want failed", terminals[0][2])
	}
	summary, _ := terminals[0][3].(string)
	if summary != reasonPushTransportFailed {
		t.Errorf("error_summary = %q, want exactly %q", summary, reasonPushTransportFailed)
	}
	// ★ The point of the constant: `error_summary` is read back through GET
	// /v1/incarnations/<name> WITHOUT masking, and a SendApply error can echo the
	// request that produced it. Asserting only that the code is PRESENT would stay
	// green for `push_transport_failed: Authorize refused for deploy@h1`.
	if strings.Contains(summary, "Authorize refused") {
		t.Errorf("error_summary = %q - the raw transport error reached an unmasked operator-visible column", summary)
	}
}

// ★ …and the constant must NOT be written when a task already recorded its own
// reason: UpdateStatus COALESCEs, so a non-nil summary WINS, and the transport
// sentence would replace the failure an operator can actually act on.
func TestDispatchPushHost_TransportFailureKeepsTheTaskReason(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{{
			ApplyId: "ap-10", TaskIdx: 3,
			Status: keeperv1.TaskStatus_TASK_STATUS_FAILED,
			Error:  &keeperv1.TaskError{Module: "core.pkg.installed", Message: "no such package"},
		}},
		err: errors.New("push: run h1 without RunResult"),
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-10"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-10"})

	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(terminals))
	}
	if terminals[0][3] != nil {
		t.Errorf("error_summary = %v, want nil so COALESCE keeps `task 3 core.pkg.installed: …`", terminals[0][3])
	}
}

// ★ The RunResult carries a Passage too, and `run.completed` is stamped from it.
// An unchecked echo would file the terminal's audit row under a Passage the run
// was never in — the same defect as the TaskEvent one, on the message that
// closes the run.
func TestDispatchPushHost_WrongPassageOnRunResultFailsTheHost(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		// The TaskEvent echo is CORRECT, so the run reaches the RunResult check.
		events: []*keeperv1.TaskEvent{{ApplyId: "ap-11", Passage: 1}},
		result: &keeperv1.RunResult{ApplyId: "ap-11", Passage: 0, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-11"}, discardLog(), 1,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-11", Passage: 1})

	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(terminals))
	}
	if terminals[0][2] != "failed" {
		t.Errorf("status = %v, want failed - a RunResult from the wrong Passage is not a successful run", terminals[0][2])
	}
	summary, _ := terminals[0][3].(string)
	if !strings.Contains(summary, reasonSoulPassageUnsupported) {
		t.Errorf("error_summary = %q, want it to name %s", summary, reasonSoulPassageUnsupported)
	}
}

// A nil event must not crash the run goroutine. applysink tolerates one and the
// [PushApplyDispatcher] contract does not forbid one, so the stamp on the
// mismatch path must not be the thing that dereferences it.
//
// Passage 1 ON PURPOSE: a nil event reports passage 0, so this is the only way
// to route it through stampPassage — at passage 0 the mismatch branch is never
// taken and the guard would go untested.
func TestDispatchPushHost_NilEventDoesNotPanic(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{nil},
		result: &keeperv1.RunResult{ApplyId: "ap-12", Passage: 1, Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-12"}, discardLog(), 1,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-12", Passage: 1})

	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(terminals))
	}
	// WHICH terminal matters: a nil event reports passage 0, so it takes the
	// mismatch branch and fails the host. Asserting only "one row was written"
	// would stay green if a dispatcher started emitting nils and every staged
	// push run began failing for a reason that is not true of it.
	if terminals[0][2] != "failed" {
		t.Errorf("status = %v, want failed - a nil event reads as passage 0 against this run's passage 1", terminals[0][2])
	}
}

// The terminal is written on a context detached from the run's. Cancelling a
// run tears down the SSH session; a terminal that then failed to write would
// turn a cancel into a hang.
func TestTerminatePushHost_WritesOnACancelledRunContext(t *testing.T) {
	db := &recordingDB{}
	r := &Runner{applyDB: db}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.terminatePushHost(ctx, RunSpec{ApplyID: "ap-3"}, discardLog(), 0, "h1.example.com", "failed", "cancelled")

	if got := len(db.statementsMatching("SET status")); got != 1 {
		t.Fatalf("terminal writes on a cancelled run ctx = %d, want 1", got)
	}
}

// A run that reaches a RunResult with a non-success status keeps the per-task
// reason the sink already recorded: UpdateStatus COALESCEs, and a generic
// sentence passed here would win against nothing and say less.
func TestDispatchPushHost_FailedRunLeavesTheSummaryToTheSink(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{{
			ApplyId: "ap-4", TaskIdx: 0,
			Status: keeperv1.TaskStatus_TASK_STATUS_FAILED,
			Error:  &keeperv1.TaskError{Module: "core.pkg.installed", Message: "no such package"},
		}},
		result: &keeperv1.RunResult{ApplyId: "ap-4", Status: keeperv1.RunStatus_RUN_STATUS_FAILED},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-4"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-4"})

	failures := db.statementsMatching("failed_plan_index")
	if len(failures) == 0 {
		t.Fatal("the per-task failure reason was not recorded - the barrier would report a bare `failed`")
	}
	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(terminals))
	}
	if terminals[0][3] != nil {
		t.Errorf("terminal error_summary = %v, want nil so COALESCE keeps the per-task reason", terminals[0][3])
	}
}

// ★ A push host announces no capabilities, so the ADR-056 §S5 staged gate cannot
// ask it whether it understands `passage`. The echo answers instead: a binary
// that does not know the field reports `passage: 0` on every event, and since
// the sink keys the register and the failure reason on the ECHO while the
// terminal is keyed on the Keeper's own number, a Passage-1 failure would land
// its reason in the Passage-0 row and Passage 1 would go terminal with a bare
// `failed`. Silent, wrong data.
//
// The answer is to CORRECT the echo and still fail the run — not to drop the
// events, which would close the corruption and lose the only record of work the
// host really did.
func TestDispatchPushHost_WrongPassageEchoFailsTheHost(t *testing.T) {
	db := &recordingDB{}
	rd, err := structpb.NewStruct(map[string]any{"stdout": "PONG"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	disp := &fakePushDispatcher{
		// The Keeper sent passage 1; an old binary echoes 0.
		events: []*keeperv1.TaskEvent{{ApplyId: "ap-8", Passage: 0, RegisterData: rd}},
		result: &keeperv1.RunResult{ApplyId: "ap-8", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-8"}, discardLog(), 1,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-8", Passage: 1})

	// ★ The event IS recorded — and under the Keeper's Passage, not the echoed
	// one. Dropping it would close the corruption by throwing away the only
	// record of work that actually ran on the host: SendApply is not aborted, so
	// that ApplyRequest installed packages and wrote files either way.
	regs := db.statementsMatching("INSERT INTO apply_task_register")
	if len(regs) != 1 {
		t.Fatalf("register rows written = %d, want 1 - the run must keep the record of what the host actually did", len(regs))
	}
	if regs[0][5] != 1 {
		t.Errorf("register row passage = %v, want 1 - the Keeper's number is authoritative, the echo is not", regs[0][5])
	}
	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 {
		t.Fatalf("terminal writes = %d, want 1", len(terminals))
	}
	if terminals[0][2] != "failed" {
		t.Errorf("status = %v, want failed - a wrong echo is not a successful run", terminals[0][2])
	}
	summary, _ := terminals[0][3].(string)
	if !strings.Contains(summary, reasonSoulPassageUnsupported) {
		t.Errorf("error_summary = %q, want it to name %s", summary, reasonSoulPassageUnsupported)
	}
}

// The same guard costs the common case nothing: a non-staged run sends passage 0
// and every binary, old or new, echoes 0.
func TestDispatchPushHost_MatchingPassageEchoIsNotRefused(t *testing.T) {
	db := &recordingDB{}
	disp := &fakePushDispatcher{
		events: []*keeperv1.TaskEvent{{ApplyId: "ap-9", Passage: 0}},
		result: &keeperv1.RunResult{ApplyId: "ap-9", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS},
	}
	r := &Runner{deps: Deps{PushApply: disp}, applyDB: db}

	r.dispatchPushHost(context.Background(), RunSpec{ApplyID: "ap-9"}, discardLog(), 0,
		"h1.example.com", hostDispatch{transport: config.TransportSSH},
		push.Route{Provider: "static"}, &keeperv1.ApplyRequest{ApplyId: "ap-9"})

	terminals := db.statementsMatching("SET status")
	if len(terminals) != 1 || terminals[0][2] != "success" {
		t.Fatalf("terminal = %v, want one success", terminals)
	}
}

// --- routing ----------------------------------------------------------

// Level 0: the task's own ssh_provider short-circuits the router entirely —
// its cheapest level is a PG read per SID and there is nothing left to decide.
func TestResolvePushRoute_TaskProviderBeatsTheRouter(t *testing.T) {
	r := &Runner{deps: Deps{PushRouter: fixedRouter{name: "cluster-default", source: push.SourceCluster}}}
	route, source, err := r.resolvePushRoute(context.Background(), "h1.example.com",
		hostDispatch{override: push.TransportOverride{Provider: "vault-bastion"}})
	if err != nil {
		t.Fatalf("resolvePushRoute: %v", err)
	}
	if route.Provider != "vault-bastion" || source != push.SourceTask {
		t.Fatalf("route = %q/%v, want vault-bastion/task", route.Provider, source)
	}
}

// Without a Level 0 the router answers, and the task's user/port ride the route
// anyway: picking a provider and overriding the connection fields are
// independent axes.
func TestResolvePushRoute_OverrideRidesTheRoutersAnswer(t *testing.T) {
	r := &Runner{deps: Deps{PushRouter: fixedRouter{name: "static", source: push.SourceCoven}}}
	route, source, err := r.resolvePushRoute(context.Background(), "h1.example.com",
		hostDispatch{override: push.TransportOverride{User: "deploy", Port: 2222}})
	if err != nil {
		t.Fatalf("resolvePushRoute: %v", err)
	}
	if route.Provider != "static" || source != push.SourceCoven {
		t.Fatalf("route = %q/%v, want static/coven", route.Provider, source)
	}
	if route.Override.User != "deploy" || route.Override.Port != 2222 {
		t.Fatalf("override lost on the router's answer: %+v", route.Override)
	}
}

// --- the paths that cannot carry a push host --------------------------

// The Acolyte claims an assignment and dispatches it over the EventStream. A
// planned row for a push host would be claimed and then fail
// soul_not_connected, so the refusal comes BEFORE the first Insert — a
// half-written planned set leaves the barrier waiting on hosts nothing closes.
func TestDispatchPlanned_RefusesAPushHost(t *testing.T) {
	// deps.DB is left nil ON PURPOSE. dispatchPlanned inserts planned rows
	// through it, so reaching the first Insert would panic on the nil pool —
	// which makes "this test returned an error instead of panicking" the proof
	// that the refusal fires BEFORE any row is written. A recorder here would
	// prove nothing: the code under test never reads applyDB.
	r := &Runner{}
	err := r.dispatchPlanned(context.Background(), RunSpec{ApplyID: "ap-5"}, discardLog(), []*topology.HostFacts{
		hostFacts("agent.example.com", config.TransportAgent),
		hostFacts("push.example.com", config.TransportSSH),
	})
	if err == nil {
		t.Fatal("the work-queue path accepted a transport=ssh host")
	}
	if !strings.Contains(err.Error(), "push.example.com") {
		t.Errorf("error does not name the host: %v", err)
	}
}

// ★ A run needing the push branch on a Keeper that has none is refused BEFORE
// the first wave, not when the push host's turn comes. Discovering it mid-wave
// would leave the fleet half-applied: the agent hosts sorted ahead of it would
// already have executed their ApplyRequests, and a second push host would
// already have opened and closed an SSH session.
func TestResolvePushRoutes_RefusesWithoutAPushDispatcher(t *testing.T) {
	r := &Runner{}
	routes, refusals := r.resolvePushRoutes(context.Background(), map[string]hostDispatch{
		"a-agent.example.com": {transport: config.TransportAgent},
		"z-push.example.com":  {transport: config.TransportSSH},
	})
	if len(routes) != 0 {
		t.Fatalf("routes = %+v, want none", routes)
	}
	if refusals["z-push.example.com"].reason != reasonPushNotConfigured {
		t.Fatalf("reason = %q, want %q", refusals["z-push.example.com"].reason, reasonPushNotConfigured)
	}
	if _, present := refusals["a-agent.example.com"]; present {
		t.Error("the agent host was refused too - only push hosts need a push dispatcher")
	}
}

// A router that names no provider is the same class of failure and gets the
// same treatment: refused up front, before anything is applied anywhere.
func TestResolvePushRoutes_RefusesAnUnroutableHost(t *testing.T) {
	r := &Runner{deps: Deps{
		PushApply:  &fakePushDispatcher{},
		PushRouter: fixedRouter{err: push.ErrProviderNotRouted},
	}}
	_, refusals := r.resolvePushRoutes(context.Background(), map[string]hostDispatch{
		"h1.example.com": {transport: config.TransportSSH},
	})
	if refusals["h1.example.com"].reason != reasonProviderNotRouted {
		t.Fatalf("reason = %q, want %q", refusals["h1.example.com"].reason, reasonProviderNotRouted)
	}
}

// ★ The hoist itself: a routing miss must be refused by dispatchPassage BEFORE
// the first wave. Driven through dispatchPassage rather than through
// resolvePushRoutes, because the defect this exists for is "the refusal lives at
// the host's turn" — a test that called the resolver directly would stay green
// if the call were moved back down into startPushHost.
//
// `deps.DB` is left nil ON PURPOSE, and that is what makes the assertions bite.
// dispatchWave inserts the `apply_runs` row through it, so if the refusal were
// removed the agent host (sorted first) would reach `applyrun.Insert` and panic
// on the nil pool. "Returned an error instead of panicking, and Outbound was
// never called" is therefore the proof that nothing was dispatched and nothing
// was written — a recorder could not give it, because the insert path never
// touches `applyDB`.
func TestDispatchPassage_RefusesBeforeDispatchingAnyHost(t *testing.T) {
	disp := &fakePushDispatcher{result: &keeperv1.RunResult{Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}}
	out := &countingOutbound{}
	r := &Runner{
		deps: Deps{
			PushApply: disp,
			// The router refuses, so the push host cannot be routed. The agent host
			// sorts FIRST, so a refusal raised at the push host's turn would leave
			// it already dispatched.
			PushRouter: fixedRouter{err: push.ErrProviderNotRouted},
			Outbound:   out,
		},
		logger: discardLog(),
	}
	tasks := []*render.RenderedTask{{Index: 0, Module: "core.exec.run"}}
	plans := []render.DispatchPlan{{TaskIndex: 0, TargetSIDs: []string{"a-agent.example.com", "z-push.example.com"}}}
	hosts := []*topology.HostFacts{
		hostFacts("a-agent.example.com", config.TransportAgent),
		hostFacts("z-push.example.com", config.TransportSSH),
	}
	spec := RunSpec{ApplyID: "ap-7", IncarnationName: "redis-prod", ScenarioName: "live"}

	err := r.dispatchPassage(context.Background(), spec, discardLog(), 0, tasks, plans, nil, hosts)
	if err == nil {
		t.Fatal("dispatchPassage accepted a run with an unroutable push host")
	}
	if !strings.Contains(err.Error(), reasonProviderNotRouted) {
		t.Errorf("err = %v, want it to name %s", err, reasonProviderNotRouted)
	}
	if out.calls() != 0 {
		t.Errorf("the agent host was dispatched %d time(s) before the refusal - the fleet is half-applied", out.calls())
	}
}

// countingOutbound counts stream dispatches, so a test can assert that NOTHING
// was applied before a refusal.
type countingOutbound struct {
	mu sync.Mutex
	n  int
}

func (o *countingOutbound) SendApply(context.Context, string, *keeperv1.ApplyRequest) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.n++
	return nil
}

func (o *countingOutbound) calls() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n
}

// ★ The run-wide push-wiring guard, which is what an operator on a pull-only
// Keeper actually hits. It runs before the first Passage AND before the
// keeper-side tasks, so a run that could never be applied does not first
// provision the cloud VMs it would have applied to.
func TestGuardPushConfigured(t *testing.T) {
	roster := []*topology.HostFacts{
		hostFacts("a-agent.example.com", config.TransportAgent),
		hostFacts("z-push.example.com", config.TransportSSH),
	}

	pullOnly := &Runner{}
	err := pullOnly.guardPushConfigured("redis-prod", "create", roster)
	if err == nil {
		t.Fatal("a pull-only Keeper accepted a roster with a transport=ssh host")
	}
	if !strings.Contains(err.Error(), "z-push.example.com") {
		t.Errorf("error does not name the host: %v", err)
	}

	if err := pullOnly.guardPushConfigured("redis-prod", "create", roster[:1]); err != nil {
		t.Errorf("an all-agent roster was refused on a pull-only Keeper: %v", err)
	}

	wired := &Runner{deps: Deps{PushApply: &fakePushDispatcher{}}}
	if err := wired.guardPushConfigured("redis-prod", "create", roster); err != nil {
		t.Errorf("a configured Keeper refused a push host: %v", err)
	}
}

// --- the announcement gates -------------------------------------------

// A push host announces nothing: no lease, no Hello, and the binary that runs
// there is the one the Keeper delivers. Asking the presence source about it
// returns "lacking every capability", which would reject every run that touched
// one — so it is left out of the question rather than answered wrongly.
func TestStreamedSIDs_LeavesOutPushHosts(t *testing.T) {
	got := streamedSIDs([]*topology.HostFacts{
		hostFacts("agent.example.com", config.TransportAgent),
		hostFacts("push.example.com", config.TransportSSH),
		nil,
	})
	if len(got) != 1 || got[0] != "agent.example.com" {
		t.Fatalf("streamedSIDs = %v, want only the streamed host", got)
	}
}

func TestExemptPushHosts_DropsTheirRequirements(t *testing.T) {
	required := map[string][]string{
		"agent.example.com": {"module.core.exec"},
		"push.example.com":  {"module.core.exec"},
	}
	got := exemptPushHosts(required, []*topology.HostFacts{
		hostFacts("agent.example.com", config.TransportAgent),
		hostFacts("push.example.com", config.TransportSSH),
	})
	if _, present := got["push.example.com"]; present {
		t.Error("the push host is still gated on a capability set it never announces")
	}
	if _, present := got["agent.example.com"]; !present {
		t.Error("the streamed host lost its requirement")
	}
}
