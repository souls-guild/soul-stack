package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Guard tests for intra-host task concurrency (ADR-0075): `async: true` tasks
// really overlap, every barrier collects what it names, a failure inside a flow
// reaches RunResult instead of being swallowed, and a plan without `async:`
// behaves exactly as it did before — including one shipped by a Keeper that
// never heard of the fields (all three arrive zero-valued).

// registerIndexFrom seeds a register index from a literal by-index map — the
// shape the requisite helpers took before ADR-0075 put the accumulator behind a
// mutex.
func registerIndexFrom(byIdx map[int32]*structpb.Struct) *registerIndex {
	x := newRegisterIndex(len(byIdx))
	for idx, data := range byIdx {
		x.record(idx, "", data)
	}
	return x
}

// gateModule is a module whose Apply blocks until released, so a test can hold a
// flow open and observe what overlaps with it. Each task addresses one gate by
// the `name` param.
type gateModule struct {
	mu      sync.Mutex
	gates   map[string]chan struct{}
	started map[string]chan struct{}
	fail    map[string]bool
	changed map[string]bool

	// live/peak track how many Applies are in flight at once — the measurement
	// behind "concurrent, not sequential".
	live atomic.Int32
	peak atomic.Int32
}

func newGateModule(names ...string) *gateModule {
	m := &gateModule{
		gates:   map[string]chan struct{}{},
		started: map[string]chan struct{}{},
		fail:    map[string]bool{},
		changed: map[string]bool{},
	}
	for _, n := range names {
		m.gates[n] = make(chan struct{})
		m.started[n] = make(chan struct{})
	}
	return m
}

func (m *gateModule) Name() string { return "core.gate" }
func (m *gateModule) Validate(context.Context, *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	return &pluginv1.ValidateReply{Ok: true}, nil
}

func (m *gateModule) Plan(*pluginv1.PlanRequest, grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *gateModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	name := req.GetParams().GetFields()["name"].GetStringValue()
	m.mu.Lock()
	gate, started := m.gates[name], m.started[name]
	fail, changed := m.fail[name], m.changed[name]
	m.mu.Unlock()

	n := m.live.Add(1)
	for {
		peak := m.peak.Load()
		if n <= peak || m.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	defer m.live.Add(-1)

	if started != nil {
		close(started)
	}
	if gate != nil {
		select {
		case <-gate:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	switch {
	case fail:
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: name + " failed"})
	case changed:
		return stream.Send(&pluginv1.ApplyEvent{Changed: true})
	}
	return stream.Send(&pluginv1.ApplyEvent{})
}

// open releases one gate.
func (m *gateModule) open(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if g, ok := m.gates[name]; ok {
		close(g)
		delete(m.gates, name)
	}
}

// awaitStart blocks until the named task's Apply is running.
func (m *gateModule) awaitStart(t *testing.T, name string) {
	t.Helper()
	m.mu.Lock()
	ch := m.started[name]
	m.mu.Unlock()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("task %q never started", name)
	}
}

// hasStarted reports whether the named task's Apply has begun, without waiting.
func (m *gateModule) hasStarted(name string) bool {
	m.mu.Lock()
	ch := m.started[name]
	m.mu.Unlock()
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// startedNames returns which of the named tasks are already running. Used where
// the scheduler picks the winners and the test must not assume which.
func (m *gateModule) startedNames(names ...string) []string {
	var out []string
	for _, n := range names {
		if m.hasStarted(n) {
			out = append(out, n)
		}
	}
	return out
}

// awaitStartedCount blocks until at least n of names have started.
func (m *gateModule) awaitStartedCount(t *testing.T, n int, names ...string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if started := m.startedNames(names...); len(started) >= n {
			return started
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %v of %v started, want %d", m.startedNames(names...), names, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func gateTask(name string, async bool) *keeperv1.RenderedTask {
	return &keeperv1.RenderedTask{
		Name:     name,
		Module:   "core.gate.run",
		Register: name,
		Async:    async,
		Params: &structpb.Struct{Fields: map[string]*structpb.Value{
			"name": structpb.NewStringValue(name),
		}},
	}
}

// safeSink is a recording sink usable from several flows at once. The runner
// serializes its own writes (lockedSink), but a test asserting on the recorded
// slice reads it from the test goroutine.
type safeSink struct {
	mu     sync.Mutex
	events []*keeperv1.TaskEvent
	result *keeperv1.RunResult
	err    error
}

func (s *safeSink) SendTaskEvent(ev *keeperv1.TaskEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return s.err
}

func (s *safeSink) SendRunResult(rr *keeperv1.RunResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = rr
	return s.err
}

func (s *safeSink) byIdx(idx int32) *keeperv1.TaskEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.GetTaskIdx() == idx {
			return ev
		}
	}
	return nil
}

func (s *safeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func gateRunner(m *gateModule) *ApplyRunner {
	return NewApplyRunner(mapRegistry{"core.gate": m}, nil)
}

// runAsync runs req on its own goroutine and returns a channel carrying Run's
// error, so the test can drive gates while the run is in flight.
func runAsync(r *ApplyRunner, ctx context.Context, req *keeperv1.ApplyRequest, sink EventSink) <-chan error {
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, req, sink) }()
	return done
}

func awaitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish — a barrier is hanging")
		return nil
	}
}

// TestAsync_TasksRunConcurrently — the core claim: two async: tasks are in
// flight at the same time, and the main flow reaches the tasks after them
// without waiting. Sequential execution would deadlock this test (the first
// gate is only opened once BOTH have started), which is the point: it cannot
// pass by accident.
func TestAsync_TasksRunConcurrently(t *testing.T) {
	t.Parallel()
	m := newGateModule("a", "b", "tail")
	sink := &safeSink{}

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-concurrent",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			gateTask("b", true),
			gateTask("tail", false),
		},
	}, sink)

	// Both flows are in Apply at once, and the ordinary task after them started
	// without waiting for either — fire-and-forget, not a group with a join.
	m.awaitStart(t, "a")
	m.awaitStart(t, "b")
	m.awaitStart(t, "tail")
	if got := m.peak.Load(); got < 3 {
		t.Errorf("peak concurrent Applies = %d, want >= 3 (two async flows + the main flow)", got)
	}

	m.open("a")
	m.open("b")
	m.open("tail")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", sink.result.GetStatus())
	}
	if sink.count() != 3 {
		t.Errorf("TaskEvents = %d, want 3 (one per task, whichever flow ran it)", sink.count())
	}
}

// TestAsync_RequireBarrierWaits — an explicit require: does not let the task
// start until every source it names is finished, and only those: an async
// sibling nobody named keeps running across the barrier.
func TestAsync_RequireBarrierWaits(t *testing.T) {
	t.Parallel()
	m := newGateModule("a", "b", "barrier")
	sink := &safeSink{}

	barrier := gateTask("barrier", false)
	barrier.RequireIdx = []int32{0, -1} // -1: a source filtered out by where: — nothing to wait for

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-require",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			gateTask("b", true),
			barrier,
		},
	}, sink)

	m.awaitStart(t, "a")
	m.awaitStart(t, "b")
	// `a` is still in Apply, so the barrier must not have started.
	if m.hasStarted("barrier") {
		t.Fatal("require: [a] started before a finished")
	}

	m.open("a")
	m.awaitStart(t, "barrier")
	// The unnamed sibling was NOT collected by this barrier — it is still running.
	if m.live.Load() < 2 {
		t.Error("barrier appears to have waited for b as well; require: is a local barrier")
	}

	m.open("b")
	m.open("barrier")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", sink.result.GetStatus())
	}
}

// TestAsync_RequireAllBarrier — `require: all` waits for every async task
// started earlier in the run.
func TestAsync_RequireAllBarrier(t *testing.T) {
	t.Parallel()
	m := newGateModule("a", "b", "barrier")
	sink := &safeSink{}

	barrier := gateTask("barrier", false)
	barrier.RequireAll = true

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-require-all",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			gateTask("b", true),
			barrier,
		},
	}, sink)

	m.awaitStart(t, "a")
	m.awaitStart(t, "b")
	m.open("a")
	// One of the two is still in flight → the all-barrier is still closed.
	if m.hasStarted("barrier") {
		t.Fatal("require: all started while b was still running")
	}
	m.open("b")
	m.awaitStart(t, "barrier")
	m.open("barrier")

	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestAsync_RequireOnAsyncTaskDoesNotBlockPlan — `require:` on an async task
// orders the TASK, it does not hold up the plan: the flow waits for its source
// inside itself while the main loop walks on.
func TestAsync_RequireOnAsyncTaskDoesNotBlockPlan(t *testing.T) {
	t.Parallel()
	m := newGateModule("a", "dependent", "tail")
	sink := &safeSink{}

	dependent := gateTask("dependent", true)
	dependent.RequireIdx = []int32{0}

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-require-async",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			dependent,
			gateTask("tail", false),
		},
	}, sink)

	// The main flow reached `tail` even though `dependent` is parked on its
	// barrier behind `a`.
	m.awaitStart(t, "a")
	m.awaitStart(t, "tail")
	if m.hasStarted("dependent") {
		t.Fatal("an async task with require: started before its source finished")
	}

	m.open("a")
	m.awaitStart(t, "dependent")
	m.open("dependent")
	m.open("tail")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestAsync_ImplicitBarrierOnRegisterReference — a Soul-side reference to an
// async task's register.<name> is itself a barrier (ADR-0075(b.2)): without it
// the predicate would hit a missing register and fail the task.
func TestAsync_ImplicitBarrierOnRegisterReference(t *testing.T) {
	t.Parallel()
	m := newGateModule("probe", "consumer")
	m.mu.Lock()
	m.changed["probe"] = true
	m.mu.Unlock()
	sink := &safeSink{}

	consumer := gateTask("consumer", false)
	consumer.When = "register.probe.changed"

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-implicit",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("probe", true),
			consumer,
		},
	}, sink)

	m.awaitStart(t, "probe")
	if m.hasStarted("consumer") {
		t.Fatal("when: register.probe.* evaluated before probe finished")
	}
	m.open("probe")
	m.awaitStart(t, "consumer")
	m.open("consumer")

	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := sink.byIdx(1).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("consumer status = %v, want OK — the barrier must make register.probe readable", got)
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", sink.result.GetStatus())
	}
}

// TestAsync_OnchangesOnAsyncSource — the index-shaped implicit barrier: an
// onchanges: naming an async source waits for it and then gates on its real
// outcome.
func TestAsync_OnchangesOnAsyncSource(t *testing.T) {
	t.Parallel()
	m := newGateModule("probe", "consumer")
	sink := &safeSink{} // probe reports unchanged → consumer must skip

	consumer := gateTask("consumer", false)
	consumer.OnchangesIdx = []int32{0}

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-onchanges",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("probe", true),
			consumer,
		},
	}, sink)

	m.awaitStart(t, "probe")
	m.open("probe")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.hasStarted("consumer") {
		t.Error("consumer ran although its async source reported unchanged")
	}
	if got := sink.byIdx(1).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
		t.Errorf("consumer status = %v, want SKIPPED", got)
	}
}

// TestAsync_FailureReachesRunResult — a failure inside a flow that nobody
// awaits is still the run's failure: the final barrier is where it surfaces
// (ADR-0075(b.3)). The event carries the module's error, not a generic one.
func TestAsync_FailureReachesRunResult(t *testing.T) {
	t.Parallel()
	m := newGateModule("boom", "tail")
	m.mu.Lock()
	m.fail["boom"] = true
	m.mu.Unlock()
	sink := &safeSink{}

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-fail",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("boom", true),
			gateTask("tail", false),
		},
	}, sink)

	// The ordinary task after the failing flow runs to completion: a failure
	// does NOT break the main flow at the moment it happens (ADR-0075(c)).
	m.awaitStart(t, "tail")
	m.open("boom")
	m.open("tail")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED — an unawaited async failure must not be lost",
			sink.result.GetStatus())
	}
	ev := sink.byIdx(0)
	if ev.GetStatus() != keeperv1.TaskStatus_TASK_STATUS_FAILED {
		t.Errorf("async TaskEvent status = %v, want FAILED", ev.GetStatus())
	}
	if ev.GetError().GetCode() != "module.failed" {
		t.Errorf("error code = %q, want module.failed", ev.GetError().GetCode())
	}
	if got := sink.byIdx(1).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("tail status = %v, want OK — siblings are not cancelled and the flow is not fail-stopped", got)
	}
}

// TestAsync_FailStopEngagesAtBarrier — the same failure, this time awaited:
// fail-stop engages at the barrier that reached for it, so the awaiting task and
// everything after it skip, and only the onfail: rescue tail runs.
func TestAsync_FailStopEngagesAtBarrier(t *testing.T) {
	t.Parallel()
	m := newGateModule("boom", "consumer", "after", "rescue")
	m.mu.Lock()
	m.fail["boom"] = true
	m.mu.Unlock()
	sink := &safeSink{}

	consumer := gateTask("consumer", false)
	consumer.RequireIdx = []int32{0}
	// The combination §8 documents: "wait for the source, run only if it failed."
	rescue := gateTask("rescue", false)
	rescue.RequireIdx = []int32{0}
	rescue.OnfailIdx = []int32{0}

	done := runAsync(gateRunner(m), context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-failstop",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("boom", true),
			consumer,
			gateTask("after", false),
			rescue,
		},
	}, sink)

	m.open("boom")
	m.open("rescue")
	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", sink.result.GetStatus())
	}
	for _, tc := range []struct {
		idx  int32
		name string
	}{{1, "consumer"}, {2, "after"}} {
		if got := sink.byIdx(tc.idx).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_SKIPPED {
			t.Errorf("%s status = %v, want SKIPPED (fail-stop engaged at the barrier)", tc.name, got)
		}
	}
	if m.hasStarted("consumer") {
		t.Error("the task awaiting the failed flow executed its module")
	}
	if got := sink.byIdx(3).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_OK {
		t.Errorf("rescue status = %v, want OK — onfail: on a failed async source must run", got)
	}
}

// TestAsync_NoBarrierDoesNotHang — a plan whose async tasks nobody awaits still
// terminates, and only after every flow is finalized: the final barrier is
// required behaviour, not an optimization. A RunResult sent before the last
// TaskEvent would be a torn report.
func TestAsync_NoBarrierDoesNotHang(t *testing.T) {
	t.Parallel()
	m := newGateModule()
	sink := &safeSink{}

	tasks := make([]*keeperv1.RenderedTask, 0, 8)
	for i := range 8 {
		tasks = append(tasks, gateTask(fmt.Sprintf("t%d", i), true))
	}

	if err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-no-barrier",
		Tasks:   tasks,
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.count() != len(tasks) {
		t.Errorf("TaskEvents = %d, want %d — Run returned before every flow finalized",
			sink.count(), len(tasks))
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", sink.result.GetStatus())
	}
}

// TestAsync_MaxConcurrentCeiling — the host-side ceiling is honoured, and a task
// above it waits for a slot rather than being dropped or failed.
func TestAsync_MaxConcurrentCeiling(t *testing.T) {
	t.Parallel()
	m := newGateModule("a", "b", "c")
	sink := &safeSink{}
	r := gateRunner(m)
	r.SetAsyncLimit(2)

	done := runAsync(r, context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-ceiling",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			gateTask("b", true),
			gateTask("c", true),
		},
	}, sink)

	// Two of the three get a slot; which two is the scheduler's business. The
	// third is queued, not dropped: opening a gate frees a slot and it starts.
	running := m.awaitStartedCount(t, 2, "a", "b", "c")
	m.open(running[0])
	m.awaitStartedCount(t, 3, "a", "b", "c")
	for _, n := range []string{"a", "b", "c"} {
		m.open(n)
	}

	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// peak is monotone: a third concurrent Apply at any moment would show here.
	if got := m.peak.Load(); got > 2 {
		t.Errorf("peak concurrent Applies = %d, want <= 2 (max_concurrent: 2)", got)
	}
	if sink.count() != 3 {
		t.Errorf("TaskEvents = %d, want 3 — a queued flow must still finalize", sink.count())
	}
}

// TestAsync_CancelReachesFlows — CancelApply reaches in-flight flows, not only
// the main loop (ADR-0075(h)), and Run does not return until they are done.
func TestAsync_CancelReachesFlows(t *testing.T) {
	t.Parallel()
	m := newGateModule("slow", "tail")
	sink := &safeSink{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runAsync(gateRunner(m), ctx, &keeperv1.ApplyRequest{
		ApplyId: "async-cancel",
		Tasks: []*keeperv1.RenderedTask{
			gateTask("slow", true),
			gateTask("tail", false),
		},
	}, sink)

	m.awaitStart(t, "slow")
	m.awaitStart(t, "tail")
	cancel() // the gated module returns ctx.Err() on both

	if err := awaitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_CANCELLED {
		t.Errorf("status = %v, want CANCELLED", sink.result.GetStatus())
	}
	if got := sink.byIdx(0).GetStatus(); got != keeperv1.TaskStatus_TASK_STATUS_CANCELLED {
		t.Errorf("async flow status = %v, want CANCELLED — cancel must reach the flow", got)
	}
}

// TestAsync_SinkErrorInFlowSurfaces — a broken stream observed off the main
// goroutine is still Run's error.
func TestAsync_SinkErrorInFlowSurfaces(t *testing.T) {
	t.Parallel()
	m := newGateModule()
	sink := &safeSink{err: fmt.Errorf("stream broken")}

	err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-sink-err",
		Tasks:   []*keeperv1.RenderedTask{gateTask("a", true)},
	}, sink)
	if err == nil {
		t.Fatal("Run = nil, want the sink error a flow hit")
	}
}

// TestAsync_ZeroValueIsSequential — the forward-compat contract of NIM-150: a
// plan whose tasks carry all three fields zero-valued (an older Keeper, or any
// plan not using the construct) runs strictly in plan order, one at a time.
func TestAsync_ZeroValueIsSequential(t *testing.T) {
	t.Parallel()
	m := newGateModule() // no gates: nothing blocks
	sink := &safeSink{}

	tasks := make([]*keeperv1.RenderedTask, 0, 4)
	for i := range 4 {
		task := gateTask(fmt.Sprintf("t%d", i), false)
		task.RequireIdx = nil
		task.RequireAll = false
		tasks = append(tasks, task)
	}

	if err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-zero",
		Tasks:   tasks,
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := m.peak.Load(); got != 1 {
		t.Errorf("peak concurrent Applies = %d, want 1 — a plan without async: must stay sequential", got)
	}
	for i, ev := range sink.events {
		if ev.GetTaskIdx() != int32(i) {
			t.Fatalf("TaskEvent[%d].task_idx = %d — plan order must be preserved without async:", i, ev.GetTaskIdx())
		}
	}
}

// TestAsync_DryRunIgnoresAsync — dry_run (Scry, ADR-031) plans in order: a Plan
// touches nothing, so concurrency would buy only a scrambled report.
func TestAsync_DryRunIgnoresAsync(t *testing.T) {
	t.Parallel()
	m := newGateModule()
	sink := &safeSink{}

	if err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-dry",
		DryRun:  true,
		Tasks: []*keeperv1.RenderedTask{
			gateTask("a", true),
			gateTask("b", true),
		},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, ev := range sink.events {
		if ev.GetTaskIdx() != int32(i) {
			t.Errorf("TaskEvent[%d].task_idx = %d, want %d", i, ev.GetTaskIdx(), i)
		}
	}
}

// TestAsync_ForwardRequireIsNoOp — a `require:` naming a task LATER in the plan
// cannot deadlock the run: barrier targets are resolved at the awaiting task's
// plan position, so a flow not launched yet is simply not in the set. Rejecting
// the authoring mistake statically is NIM-152's.
func TestAsync_ForwardRequireIsNoOp(t *testing.T) {
	t.Parallel()
	m := newGateModule()
	sink := &safeSink{}

	head := gateTask("head", false)
	head.RequireIdx = []int32{2} // a task two positions ahead

	if err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-forward-require",
		Tasks: []*keeperv1.RenderedTask{
			head,
			gateTask("mid", true),
			gateTask("late", true),
		},
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.count() != 3 {
		t.Errorf("TaskEvents = %d, want 3", sink.count())
	}
}

// TestAsync_RegisterIndexRace — the register accumulator under concurrent
// writers: with -race this is the guard on registerByIdx/registerByName, which
// were plain maps before ADR-0075.
func TestAsync_RegisterIndexRace(t *testing.T) {
	t.Parallel()
	m := newGateModule()
	sink := &safeSink{}

	// Every flow registers a name, and every other task reads all of them
	// through a require: all barrier plus a when: over the register.
	tasks := make([]*keeperv1.RenderedTask, 0, 24)
	for i := range 12 {
		tasks = append(tasks, gateTask(fmt.Sprintf("w%d", i), true))
		reader := gateTask(fmt.Sprintf("r%d", i), false)
		reader.RequireAll = true
		reader.When = fmt.Sprintf("!register.w%d.failed", i)
		tasks = append(tasks, reader)
	}

	if err := gateRunner(m).Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "async-race",
		Tasks:   tasks,
	}, sink); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sink.count() != len(tasks) {
		t.Errorf("TaskEvents = %d, want %d", sink.count(), len(tasks))
	}
	if sink.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		t.Errorf("status = %v, want SUCCESS", sink.result.GetStatus())
	}
}

var _ module.SoulModule = (*gateModule)(nil)
