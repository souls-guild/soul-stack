package pushorch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeStore is an in-memory Store substitute via fake-DB. Stores per-applyID
// statuses/summaries, records terminal commit for assertions.
type fakeStore struct {
	mu       sync.Mutex
	inserts  []PushRunRow
	statuses map[string]PushRunStatus
	summary  map[string]map[string]any
	terminal chan struct{}
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		statuses: map[string]PushRunStatus{},
		summary:  map[string]map[string]any{},
		terminal: make(chan struct{}, 1),
	}
}

func (f *fakeStore) Insert(ctx context.Context, row PushRunRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inserts = append(f.inserts, row)
	f.statuses[row.ApplyID] = StatusPending
	return nil
}

func (f *fakeStore) MarkRunning(ctx context.Context, applyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[applyID] = StatusRunning
	return nil
}

func (f *fakeStore) MarkTerminal(ctx context.Context, applyID string, status PushRunStatus, summary map[string]any) error {
	f.mu.Lock()
	f.statuses[applyID] = status
	f.summary[applyID] = summary
	f.mu.Unlock()
	select {
	case f.terminal <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeStore) Get(ctx context.Context, applyID string) (*PushRunRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.inserts {
		if r.ApplyID == applyID {
			r.Status = f.statuses[applyID]
			r.Summary = f.summary[applyID]
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

// fakeTopology is a fixed roster by InventorySIDs.
type fakeTopology struct {
	hosts []*topology.HostFacts
	err   error
}

func (f *fakeTopology) LoadByInventory(_ context.Context, _ []string) ([]*topology.HostFacts, error) {
	return f.hosts, f.err
}

// fakeRender is a fixed plan (one task, target all hosts).
type fakeRender struct {
	plan  []*render.RenderedTask
	plans []render.DispatchPlan
	err   error
}

func (f *fakeRender) Render(_ context.Context, in render.RenderInput) ([]*render.RenderedTask, []render.DispatchPlan, error) {
	return f.plan, f.plans, f.err
}

// fakeDispatcher is a parametrized SendApply: per-SID outcome can be set.
type fakeDispatcher struct {
	mu       sync.Mutex
	calls    int32
	results  map[string]*keeperv1.RunResult
	errs     map[string]error
	delay    time.Duration
	received map[string][]*keeperv1.RenderedTask
}

func (f *fakeDispatcher) SendApply(_ context.Context, sid string, _ string, req *keeperv1.ApplyRequest) (*keeperv1.RunResult, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	if f.received == nil {
		f.received = map[string][]*keeperv1.RenderedTask{}
	}
	f.received[sid] = req.GetTasks()
	f.mu.Unlock()
	if err, ok := f.errs[sid]; ok && err != nil {
		return nil, err
	}
	if rr, ok := f.results[sid]; ok {
		return rr, nil
	}
	return &keeperv1.RunResult{ApplyId: req.GetApplyId(), Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}, nil
}

// fakeRouter is a static provider, returns fixed name for any SID.
// Used in unit tests of NewPushRun validation (Router-deps required).
type fakeRouter struct {
	name   string
	source push.RouteSource
	err    error
}

func (r *fakeRouter) RouteFor(_ context.Context, _ string) (string, push.RouteSource, error) {
	if r.err != nil {
		return "", push.SourceUnknown, r.err
	}
	return r.name, r.source, nil
}

// fakeAudit collects events.
type fakeAudit struct {
	mu     sync.Mutex
	events []*audit.Event
}

func (f *fakeAudit) Write(_ context.Context, ev *audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

// fakeDestinyLoader / stubTemplate are reused from destiny_test.go.

func newTestPushRun(t *testing.T, store *fakeStore, topo *fakeTopology, rend *fakeRender, disp *fakeDispatcher, audWriter *fakeAudit) *PushRun {
	t.Helper()
	loader := &stubLoader{out: &artifact.DestinyArtifact{
		LocalDir: t.TempDir(),
		Manifest: &config.DestinyManifest{Name: "redis-base"},
		Tasks:    []config.Task{},
	}}
	// Real *Store is not needed, we pass a substitute via own interface. PushRun
	// validates non-nil Store, but in the fake we use an interface with the same
	// method names. We lay down real-NewStore with nil-DB; in tests executeAsync
	// only calls store methods we redefine via embed wrapper. To stay within current
	// code without interface refactoring, we assemble PushRun with reference to
	// realStore, but actual calls are intercepted via fakeStore. Since PushRun.deps.Store
	// has concrete type *Store, we make shim: use real NewStore over wrappedDB which
	// calls fakeStore methods. Better: extend Deps to DB-uninvolved test mode:
	// here test bypasses by passing *Store with special fakeDB and embedded behavior.
	// To avoid multiplying shims, we make PushRun with deps.Store = realStore over
	// fakeDB, which PushRun only reads (UpdateStatus calls go through fakeDB.Exec
	// which returns ok).
	//
	// Simplification: executeAsync tests check the full chain via fakeStore statuses.
	// For this to work, we use NewPushRun with deps.Store = real Store over fakeDB
	// imitation. But simpler: instead of Store mocks via interface — embed *Store
	// in PushRun and mix in in-memory PG mock. This exceeds current slice scope
	// (needs testcontainers).
	//
	// So unit tests of executeAsync check deterministic parts (parse, summarize, fanOut)
	// directly, while end-to-end Apply run over PG is deferred to integration_test.go
	// (S4-integration).
	_ = store
	_ = topo
	_ = rend
	_ = disp
	_ = audWriter
	_ = loader
	return nil
}

func TestSummarize_AllSuccess(t *testing.T) {
	results := []hostResult{
		{sid: "a", ok: true, status: "success"},
		{sid: "b", ok: true, status: "success"},
	}
	status, summary := summarize(results)
	if status != StatusSuccess {
		t.Errorf("status = %s, want success", status)
	}
	if got := summary["success_count"]; got != 2 {
		t.Errorf("success_count = %v, want 2", got)
	}
	if got := summary["fail_count"]; got != 0 {
		t.Errorf("fail_count = %v, want 0", got)
	}
	if got := summary["total"]; got != 2 {
		t.Errorf("total = %v, want 2", got)
	}
	hosts, ok := summary["hosts"].([]map[string]any)
	if !ok {
		t.Fatalf("hosts type = %T, want []map[string]any", summary["hosts"])
	}
	if len(hosts) != 2 {
		t.Errorf("len(hosts) = %d, want 2", len(hosts))
	}
}

func TestSummarize_AllFailed(t *testing.T) {
	results := []hostResult{
		{sid: "a", status: "error", errText: "connect refused"},
		{sid: "b", status: "failed", errText: "run_status=failed"},
	}
	status, summary := summarize(results)
	if status != StatusFailed {
		t.Errorf("status = %s, want failed", status)
	}
	if summary["success_count"] != 0 {
		t.Errorf("success_count = %v, want 0", summary["success_count"])
	}
	if summary["fail_count"] != 2 {
		t.Errorf("fail_count = %v, want 2", summary["fail_count"])
	}
	hosts := summary["hosts"].([]map[string]any)
	if hosts[0]["error"] != "connect refused" {
		t.Errorf("hosts[0].error = %v", hosts[0]["error"])
	}
}

func TestSummarize_PartialFailed(t *testing.T) {
	results := []hostResult{
		{sid: "a", ok: true, status: "success"},
		{sid: "b", status: "failed", errText: "run_status=failed"},
		{sid: "c", ok: true, status: "success"},
	}
	status, summary := summarize(results)
	if status != StatusPartialFailed {
		t.Errorf("status = %s, want partial_failed", status)
	}
	if summary["success_count"] != 2 {
		t.Errorf("success_count = %v, want 2", summary["success_count"])
	}
	if summary["fail_count"] != 1 {
		t.Errorf("fail_count = %v, want 1", summary["fail_count"])
	}
}

func TestSummarize_Empty(t *testing.T) {
	// Guard against nil-slice / zero-host: total=0 ⇒ status=success (degenerate
	// case: 0 ok == 0 total).
	status, summary := summarize(nil)
	if status != StatusSuccess {
		// total=0, success=0 -> the success==total formula is triggered.
		t.Errorf("empty summarize: status = %s, want success (0 == 0)", status)
	}
	if summary["total"] != 0 {
		t.Errorf("total = %v, want 0", summary["total"])
	}
}

func TestBuildHostResult_SendApplyError(t *testing.T) {
	err := errors.New("dial timeout")
	got := buildHostResult("host1", "test-provider", nil, err)
	if got.ok {
		t.Error("ok = true, want false on SendApply error")
	}
	if got.status != "error" {
		t.Errorf("status = %q, want error", got.status)
	}
	if got.errText != "dial timeout" {
		t.Errorf("errText = %q, want dial timeout", got.errText)
	}
}

func TestBuildHostResult_RunSuccess(t *testing.T) {
	rr := &keeperv1.RunResult{ApplyId: "x", Status: keeperv1.RunStatus_RUN_STATUS_SUCCESS}
	got := buildHostResult("host1", "test-provider", rr, nil)
	if !got.ok {
		t.Error("ok = false, want true on RUN_STATUS_SUCCESS")
	}
	if got.status != "success" {
		t.Errorf("status = %q, want success", got.status)
	}
}

func TestBuildHostResult_RunFailed(t *testing.T) {
	rr := &keeperv1.RunResult{ApplyId: "x", Status: keeperv1.RunStatus_RUN_STATUS_FAILED}
	got := buildHostResult("host1", "test-provider", rr, nil)
	if got.ok {
		t.Error("ok = true, want false on RUN_STATUS_FAILED")
	}
	if got.status != "failed" {
		t.Errorf("status = %q, want failed", got.status)
	}
}

// TestResolveProviders_AlphaCompatPreset is α-compat: non-empty req.SSHProvider
// → preset applied to ALL SIDs, router NOT called.
func TestResolveProviders_AlphaCompatPreset(t *testing.T) {
	calledRouter := false
	r := &fakeRouter{name: "should-not-be-used", source: push.SourceCluster}
	r.err = nil
	// Wire wrapper to detect router call.
	routerWrap := &trackingRouter{inner: r, called: &calledRouter}

	run := &PushRun{
		deps: Deps{
			Router: routerWrap,
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		},
	}
	sids := []string{"sid-1", "sid-2", "sid-3"}
	sidProv, fails := run.resolveProviders(context.Background(), sids, ApplyRequest{SSHProvider: "preset-provider"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if calledRouter {
		t.Error("router was called with α-compat preset set")
	}
	if len(fails) != 0 {
		t.Errorf("fails = %d, want 0 (preset should not fail)", len(fails))
	}
	for _, sid := range sids {
		if sidProv[sid] != "preset-provider" {
			t.Errorf("sidProv[%s] = %q, want preset-provider", sid, sidProv[sid])
		}
	}
}

// TestResolveProviders_RouterNotRouted_FailPerHost — router returns ErrProviderNotRouted
// → SID ends up in routingResults with error_code.
func TestResolveProviders_RouterNotRouted_FailPerHost(t *testing.T) {
	r := &fakeRouter{err: push.ErrProviderNotRouted}
	run := &PushRun{
		deps: Deps{
			Router: r,
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		},
	}
	sids := []string{"sid-1", "sid-2"}
	sidProv, fails := run.resolveProviders(context.Background(), sids, ApplyRequest{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if len(sidProv) != 0 {
		t.Errorf("sidProv must be empty on all fail, got %d", len(sidProv))
	}
	if len(fails) != 2 {
		t.Fatalf("fails len = %d, want 2", len(fails))
	}
	for _, sid := range sids {
		hr, ok := fails[sid]
		if !ok {
			t.Errorf("no failure entry for %s", sid)
			continue
		}
		if hr.status != "error" {
			t.Errorf("hr[%s].status = %q, want error", sid, hr.status)
		}
		if !strings.HasPrefix(hr.errText, "provider_not_routed") {
			t.Errorf("hr[%s].errText = %q, want provider_not_routed prefix", sid, hr.errText)
		}
	}
}

// TestResolveProviders_RouterHappyPath — router returns different providers for different SIDs.
func TestResolveProviders_RouterHappyPath(t *testing.T) {
	r := &perSIDRouter{
		out: map[string]struct {
			name   string
			source push.RouteSource
		}{
			"sid-1": {"vault", push.SourceSoul},
			"sid-2": {"static", push.SourceCluster},
		},
	}
	run := &PushRun{
		deps: Deps{
			Router: r,
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		},
	}
	sidProv, fails := run.resolveProviders(context.Background(), []string{"sid-1", "sid-2"}, ApplyRequest{}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if len(fails) != 0 {
		t.Fatalf("unexpected fails: %v", fails)
	}
	if sidProv["sid-1"] != "vault" || sidProv["sid-2"] != "static" {
		t.Errorf("sidProv = %v, want sid-1=vault sid-2=static", sidProv)
	}
}

// trackingRouter is a wrapper to verify RouteFor was called.
type trackingRouter struct {
	inner  ProviderRouter
	called *bool
}

func (t *trackingRouter) RouteFor(ctx context.Context, sid string) (string, push.RouteSource, error) {
	*t.called = true
	return t.inner.RouteFor(ctx, sid)
}

// perSIDRouter is a SID -> (provider, source) map.
type perSIDRouter struct {
	out map[string]struct {
		name   string
		source push.RouteSource
	}
}

func (p *perSIDRouter) RouteFor(_ context.Context, sid string) (string, push.RouteSource, error) {
	v, ok := p.out[sid]
	if !ok {
		return "", push.SourceUnknown, push.ErrProviderNotRouted
	}
	return v.name, v.source, nil
}

func TestBuildHostResult_RunErrorLocked(t *testing.T) {
	rr := &keeperv1.RunResult{ApplyId: "x", Status: keeperv1.RunStatus_RUN_STATUS_ERROR_LOCKED}
	got := buildHostResult("host1", "test-provider", rr, nil)
	if got.ok {
		t.Error("ok = true, want false on ERROR_LOCKED")
	}
	if got.status != "error_locked" {
		t.Errorf("status = %q, want error_locked", got.status)
	}
}

func TestUnionTargetSIDs_SortedUnique(t *testing.T) {
	plans := []render.DispatchPlan{
		{TaskIndex: 0, TargetSIDs: []string{"b", "a", "c"}},
		{TaskIndex: 1, TargetSIDs: []string{"a", "d"}},
	}
	got := unionTargetSIDs(plans)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("[%d] = %q, want %q (got %v)", i, got[i], s, got)
		}
	}
}

func TestUnionTargetSIDs_Empty(t *testing.T) {
	got := unionTargetSIDs(nil)
	if got != nil {
		t.Errorf("nil plans → %v, want nil", got)
	}
	got = unionTargetSIDs([]render.DispatchPlan{{TaskIndex: 0, TargetSIDs: nil}})
	if got != nil {
		t.Errorf("empty plan → %v, want nil", got)
	}
}

func TestRunStatusLabel(t *testing.T) {
	cases := map[keeperv1.RunStatus]string{
		keeperv1.RunStatus_RUN_STATUS_SUCCESS:      "success",
		keeperv1.RunStatus_RUN_STATUS_FAILED:       "failed",
		keeperv1.RunStatus_RUN_STATUS_CANCELLED:    "cancelled",
		keeperv1.RunStatus_RUN_STATUS_ERROR_LOCKED: "error_locked",
		keeperv1.RunStatus_RUN_STATUS_UNSPECIFIED:  "unknown",
	}
	for in, want := range cases {
		if got := runStatusLabel(in); got != want {
			t.Errorf("runStatusLabel(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestNewPushRun_RequiredDeps verifies that the constructor rejects nil required
// deps. The concrete list is defense-in-depth: a caller wire-up programming
// error.
func TestNewPushRun_RequiredDeps(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	loader := &stubLoader{}
	template := stubTemplate("git@host/{name}.git")
	disp := &fakeDispatcher{}
	store := &Store{}
	topo := &fakeTopology{}
	rend := &fakeRender{}

	router := &fakeRouter{name: "test-provider", source: push.SourceCluster}

	cases := []struct {
		name string
		mut  func(*Deps)
	}{
		{"store_nil", func(d *Deps) { d.Store = nil }},
		{"topology_nil", func(d *Deps) { d.Topology = nil }},
		{"render_nil", func(d *Deps) { d.Render = nil }},
		{"loader_nil", func(d *Deps) { d.DestinyLoader = nil }},
		{"template_nil", func(d *Deps) { d.Template = nil }},
		{"dispatcher_nil", func(d *Deps) { d.Dispatcher = nil }},
		{"router_nil", func(d *Deps) { d.Router = nil }},
		{"logger_nil", func(d *Deps) { d.Logger = nil }},
		{"kid_empty", func(d *Deps) { d.KID = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{
				Store:         store,
				Topology:      topo,
				Render:        rend,
				DestinyLoader: loader,
				Template:      template,
				Dispatcher:    disp,
				Router:        router,
				Logger:        logger,
				KID:           "kid-test",
			}
			tc.mut(&deps)
			_, err := NewPushRun(deps)
			if err == nil {
				t.Fatalf("expected error for missing %s", tc.name)
			}
		})
	}
}
