package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/service"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// errSpawn simulates "process failed to launch" (fork failed / binary not
// found): Runner returns a Result with a non-empty Err, not a non-zero ExitCode.
var errSpawn = errors.New("fork/exec: permission denied")

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// systemdDetected — runner with the systemd detection branch pre-seeded.
func systemdDetected() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("systemctl --version", util.Result{ExitCode: 0})
	return r
}

func TestValidate(t *testing.T) {
	m := service.New()
	for _, st := range []string{"running", "stopped", "restarted", "enabled"} {
		reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
			State:  st,
			Params: mustStruct(t, map[string]any{"name": "redis"}),
		})
		if !reply.Ok {
			t.Fatalf("Validate(%q): not ok: %v", st, reply.Errors)
		}
	}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	})
	if reply.Ok {
		t.Fatal("Validate bad state: ok=true")
	}

	// enabled is a valid bool param for running.
	okEnabled, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	})
	if !okEnabled.Ok {
		t.Fatalf("Validate running+enabled:true: not ok: %v", okEnabled.Errors)
	}

	// non-bool enabled is rejected by Validate, not silently swallowed.
	badEnabled, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": "yes"}),
	})
	if badEnabled.Ok {
		t.Fatal("Validate running+enabled non-bool: ok=true")
	}
}

func TestApply_Running_AlreadyActive(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true for already-active")
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "systemctl start") {
			t.Fatalf("unexpected start: %q", c)
		}
	}
}

func TestApply_Running_StartsWhenInactive(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	r.On("systemctl start redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on start")
	}
}

// running + enabled:true on an inactive+disabled unit → starts AND enables, changed.
func TestApply_Running_EnabledTrue_StartsAndEnables(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	r.On("systemctl start redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on start+enable")
	}
	gotStart, gotEnable := false, false
	for _, c := range r.Calls {
		switch c {
		case "systemctl start redis":
			gotStart = true
		case "systemctl enable redis":
			gotEnable = true
		}
	}
	if !gotStart || !gotEnable {
		t.Fatalf("expected start+enable, got start=%v enable=%v calls=%v", gotStart, gotEnable, r.Calls)
	}
}

// running + enabled:true when already active AND already enabled → no-op,
// changed=false, no start/enable calls (idempotent on both dimensions).
func TestApply_Running_EnabledTrue_Idempotent(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true for already-active+enabled")
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "systemctl start") || strings.HasPrefix(c, "systemctl enable") {
			t.Fatalf("unexpected mutation: %q", c)
		}
	}
}

// running + enabled:true when active but disabled → enable runs, no start;
// changed=true only because of the enable dimension.
func TestApply_Running_EnabledTrue_ActiveButDisabled(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false despite enable")
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "systemctl start") {
			t.Fatalf("unexpected start for already-active: %q", c)
		}
	}
}

// running + enabled:false on an active+enabled unit → disable runs (no start,
// already active), changed=true.
func TestApply_Running_EnabledFalse_Disables(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl disable redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": false}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false despite disable")
	}
	gotDisable := false
	for _, c := range r.Calls {
		if c == "systemctl disable redis" {
			gotDisable = true
		}
		if strings.HasPrefix(c, "systemctl enable") {
			t.Fatalf("unexpected enable for enabled:false: %q", c)
		}
	}
	if !gotDisable {
		t.Fatalf("expected disable, calls=%v", r.Calls)
	}
}

// running without the enabled param → autostart is NOT touched: is-enabled/
// enable/disable are never called (only activity is managed).
func TestApply_Running_EnabledOmitted_DoesNotTouchAutostart(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	r.On("systemctl start redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false on start")
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "is-enabled") || strings.HasPrefix(c, "systemctl enable") || strings.HasPrefix(c, "systemctl disable") {
			t.Fatalf("autostart touched when enabled omitted: %q", c)
		}
	}
}

func TestApply_Stopped_AlreadyInactive(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-stopped")
	}
}

func TestApply_Restarted_AlwaysChanges(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl restart redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false for restart")
	}
}

func TestApply_Enabled_AlreadyEnabled(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-enabled")
	}
}

func TestApply_Enabled_EnablesWhenDisabled(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on enable")
	}
}

func TestApply_OpenRC_Running(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	// systemd absent, openrc present.
	r.On("rc-service --version", util.Result{ExitCode: 0})
	// rc-service redis status → exit 3, stopped.
	r.On("rc-service redis status", util.Result{ExitCode: 3})
	r.On("rc-service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on openrc start")
	}
}

func TestApply_NoInitDetected_Fails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	m := &service.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when no init system")
	}
}

// TestApply_InitFromFact_NoDetect — BUG-B: soulprint fact init_system=openrc
// takes priority even when `rc-service --version` is absent (detection would
// fail and the module would error with "no supported init system" — a real
// Alpine bug). With the fact set, the module goes straight to the openrc branch.
func TestApply_InitFromFact_NoDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // all detection commands absent
	r.On("rc-service redis status", util.Result{ExitCode: 3})
	r.On("rc-service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}
	m.SetHostFacts(util.HostFacts{InitSystem: util.InitSystemOpenRC})

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: fact init_system=openrc must bypass detection, msg=%q", stream.Last().Message)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false: a stopped service must start")
	}
	if hasCall(r, "rc-service --version") {
		t.Fatalf("primary fact: detection must not be called, calls=%v", r.Calls)
	}
}

// TestApply_FactEmpty_FallbackDetect — empty fact → runtime detection (backward
// compat for factless hosts: push mode, old Keeper predating soulprint).
func TestApply_FactEmpty_FallbackDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0}) // detects → openrc
	r.On("rc-service redis status", util.Result{ExitCode: 3})
	r.On("rc-service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}
	// SetHostFacts is not called — facts is empty.

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: fallback detection of openrc must trigger, msg=%q", stream.Last().Message)
	}
	if !hasCall(r, "rc-service --version") {
		t.Fatalf("empty fact: expected fallback detection, calls=%v", r.Calls)
	}
}

// openrcDetected — runner with systemd absent, openrc present.
func openrcDetected() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0})
	return r
}

// sysvDetected — runner with systemd+openrc absent, sysv present.
func sysvDetected() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("service --version", util.Result{ExitCode: 0})
	return r
}

// hasCall reports whether the command was called (exact key match).
func hasCall(r *internaltest.Runner, cmd string) bool {
	for _, c := range r.Calls {
		if c == cmd {
			return true
		}
	}
	return false
}

// --- Apply: early boundaries (missing name, unknown state) ---

// name is required: without it Apply fails on StringParam BEFORE init
// detection (init checks must not run).
func TestApply_MissingName_Fails(t *testing.T) {
	r := systemdDetected()
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when name missing")
	}
	if hasCall(r, "systemctl --version") {
		t.Fatalf("init detection ran before name-check: %v", r.Calls)
	}
}

// A state that passed Validate but isn't handled by Apply's switch → failed
// with an unknown-state message. (Guards against manifest↔switch drift.)
func TestApply_UnknownState_Fails(t *testing.T) {
	r := systemdDetected()
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	last := stream.Last()
	if !last.Failed {
		t.Fatal("failed=false for unknown state")
	}
	if !strings.Contains(last.Message, "unknown state") {
		t.Fatalf("message %q lacks 'unknown state'", last.Message)
	}
}

// running + non-bool enabled reaches Apply (Validate is separate) → failed on
// TriBoolParam inside applyRunning, no mutations.
func TestApply_Running_EnabledNonBool_Fails(t *testing.T) {
	r := systemdDetected()
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": "yes"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false for non-bool enabled in Apply")
	}
}

// --- Plan: pure-read dry-run (ADR-031 Scry) ---

// TestPlan_Running_Active_Clean — Plan(running) for an already-active service
// without autostart management: changed=false, no mutations (start/enable/restart).
func TestPlan_Running_Active_Clean(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingSvcCalls(t, r)
}

// TestPlan_Running_Inactive_Drift — Plan(running) for a stopped service:
// changed=true (Apply would have started it), no mutations.
func TestPlan_Running_Inactive_Drift(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	m := &service.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingSvcCalls(t, r)
}

// TestPlan_Running_EnabledDrift — Plan(running, enabled=true) for an active but
// NOT autostart-enabled service: changed=true (Apply would have enabled it).
func TestPlan_Running_EnabledDrift(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	m := &service.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (enabled-drift)")
	}
	assertNoMutatingSvcCalls(t, r)
}

// TestPlan_Restarted_AlwaysDrift — Plan(restarted) is always changed=true
// (restart unconditionally mutates), no mutations from Plan itself.
func TestPlan_Restarted_AlwaysDrift(t *testing.T) {
	r := systemdDetected()
	m := &service.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := stream.last(); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (restarted is always drift)")
	}
	assertNoMutatingSvcCalls(t, r)
}

// planStream — fake stream for Plan, captures events.
type planStream struct {
	grpc.ServerStreamingServer[pluginv1.PlanEvent]
	events []*pluginv1.PlanEvent
}

func (s *planStream) Send(e *pluginv1.PlanEvent) error { s.events = append(s.events, e); return nil }
func (s *planStream) Context() context.Context         { return context.Background() }
func (s *planStream) last() *pluginv1.PlanEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}

// assertNoMutatingSvcCalls fails if the runner received start/stop/restart/
// enable/disable (Plan must be pure-read, ADR-031). Allows read-only
// is-active/is-enabled/--version calls.
func assertNoMutatingSvcCalls(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	for _, c := range r.Calls {
		for _, bad := range []string{
			"systemctl start", "systemctl stop", "systemctl restart",
			"systemctl enable", "systemctl disable",
			"rc-service", "rc-update add", "rc-update del",
			"service redis", "chkconfig redis",
		} {
			if strings.HasPrefix(c, bad) {
				t.Fatalf("Plan called a mutating command %q (must be pure-read)", c)
			}
		}
	}
}

// --- stopped: real stop path + error on the activity check ---

// stopped on an active service → stop is called, changed=true, active:false.
func TestApply_Stopped_StopsWhenActive(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl stop redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false when stopping active service")
	}
	if !hasCall(r, "systemctl stop redis") {
		t.Fatalf("stop not called, calls=%v", r.Calls)
	}
}

// stopped: stop command failed (exit≠0) → failed.
func TestApply_Stopped_StopFails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl stop redis", util.Result{ExitCode: 1, Stderr: "boom"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when stop exits non-zero")
	}
}

// stopped: is-active failed to launch (Err) → failed (not falsely-inactive).
func TestApply_Stopped_IsActiveProcessError_Fails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when is-active process errored")
	}
}

// --- running: start and enable error paths ---

// running: start command failed → failed.
func TestApply_Running_StartFails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	r.On("systemctl start redis", util.Result{ExitCode: 1, Stderr: "nope"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when start exits non-zero")
	}
}

// running + enabled:true: service started but enable failed → failed (the
// enabled-dimension error isn't swallowed).
func TestApply_Running_EnableFails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 1, Stderr: "fail"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when enable exits non-zero")
	}
}

// running: is-active process failed to launch (Err) → failed.
func TestApply_Running_IsActiveProcessError_Fails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when is-active process errored")
	}
}

// running + enabled:true: is-enabled process failed to launch (Err) → failed.
func TestApply_Running_IsEnabledProcessError_Fails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when is-enabled process errored")
	}
}

// --- restarted / enabled: error paths ---

// restarted: restart command failed → failed (despite "always changed").
func TestApply_Restarted_RestartFails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl restart redis", util.Result{ExitCode: 1, Stderr: "x"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when restart exits non-zero")
	}
}

// enabled: is-enabled process failed to launch (Err) → failed.
func TestApply_Enabled_IsEnabledProcessError_Fails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-enabled --quiet redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when is-enabled process errored")
	}
}

// enabled: enable command failed → failed.
func TestApply_Enabled_EnableFails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 1, Stderr: "x"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when enable exits non-zero")
	}
}

// --- OpenRC backend: activity, idempotency, enable via rc-update ---

// OpenRC: rc-service status returned "started" (exit 0) → active, running no-op.
func TestApply_OpenRC_Running_AlreadyStarted(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: "status: started"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-started openrc service")
	}
	if hasCall(r, "rc-service redis start") {
		t.Fatalf("unexpected start, calls=%v", r.Calls)
	}
}

// OpenRC: status exit 0 but stdout without "started" (e.g. "crashed") → NOT
// active, service starts. Verifies the backend looks at the text, not just the
// exit code.
func TestApply_OpenRC_Running_ExitZeroButNotStarted(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: "status: crashed"})
	r.On("rc-service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false: crashed openrc service must be (re)started")
	}
	if !hasCall(r, "rc-service redis start") {
		t.Fatalf("start not called for crashed service, calls=%v", r.Calls)
	}
}

// OpenRC: rc-service status process failed to launch (Err) → failed.
func TestApply_OpenRC_StatusProcessError_Fails(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis status", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when rc-service status process errored")
	}
}

// OpenRC: stopped on active → rc-service stop, changed.
func TestApply_OpenRC_StopsWhenActive(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: "started"})
	r.On("rc-service redis stop", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false stopping started openrc service")
	}
	if !hasCall(r, "rc-service redis stop") {
		t.Fatalf("rc-service stop not called, calls=%v", r.Calls)
	}
}

// OpenRC: restarted → rc-service restart, always changed.
func TestApply_OpenRC_Restarted(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis restart", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false for openrc restart")
	}
	if !hasCall(r, "rc-service redis restart") {
		t.Fatalf("rc-service restart not called, calls=%v", r.Calls)
	}
}

// OpenRC: enabled when the unit is NOT in runlevel default → rc-update add,
// changed. isEnabled parses `rc-update show default` output line by line.
func TestApply_OpenRC_EnablesWhenNotInRunlevel(t *testing.T) {
	r := openrcDetected()
	r.On("rc-update show default", util.Result{ExitCode: 0, Stdout: "sshd | default\nchronyd | default"})
	r.On("rc-update add redis default", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false: redis absent from runlevel must be added")
	}
	if !hasCall(r, "rc-update add redis default") {
		t.Fatalf("rc-update add not called, calls=%v", r.Calls)
	}
}

// OpenRC: enabled when the unit is ALREADY in runlevel default (first field of
// the line) → no-op, changed=false. Verifies parsing matches the name as the
// first field, not a substring.
func TestApply_OpenRC_Enabled_Idempotent(t *testing.T) {
	r := openrcDetected()
	r.On("rc-update show default", util.Result{ExitCode: 0, Stdout: "sshd | default\nredis | default"})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-enabled openrc service")
	}
	if hasCall(r, "rc-update add redis default") {
		t.Fatalf("unexpected rc-update add, calls=%v", r.Calls)
	}
}

// OpenRC: running + enabled:false on an enabled unit → rc-update del, changed.
func TestApply_OpenRC_Running_EnabledFalse_Deletes(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis status", util.Result{ExitCode: 0, Stdout: "started"})
	r.On("rc-update show default", util.Result{ExitCode: 0, Stdout: "redis | default"})
	r.On("rc-update del redis default", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": false}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false despite rc-update del")
	}
	if !hasCall(r, "rc-update del redis default") {
		t.Fatalf("rc-update del not called, calls=%v", r.Calls)
	}
}

// OpenRC: rc-update show process failed to launch (Err) → failed.
func TestApply_OpenRC_IsEnabledProcessError_Fails(t *testing.T) {
	r := openrcDetected()
	r.On("rc-update show default", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when rc-update show process errored")
	}
}

// --- SysV backend: activity, enabled via chkconfig ---

// SysV: service status exit 0 → active, running no-op.
func TestApply_SysV_Running_AlreadyActive(t *testing.T) {
	r := sysvDetected()
	r.On("service redis status", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for active sysv service")
	}
}

// SysV: service status exit≠0 → inactive, starts via service start.
func TestApply_SysV_StartsWhenInactive(t *testing.T) {
	r := sysvDetected()
	r.On("service redis status", util.Result{ExitCode: 3})
	r.On("service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on sysv start")
	}
	if !hasCall(r, "service redis start") {
		t.Fatalf("service start not called, calls=%v", r.Calls)
	}
}

// SysV: service status process failed to launch (Err) → failed.
func TestApply_SysV_StatusProcessError_Fails(t *testing.T) {
	r := sysvDetected()
	r.On("service redis status", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when service status process errored")
	}
}

// SysV: stopped on active → service stop, changed.
func TestApply_SysV_StopsWhenActive(t *testing.T) {
	r := sysvDetected()
	r.On("service redis status", util.Result{ExitCode: 0})
	r.On("service redis stop", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false stopping active sysv service")
	}
	if !hasCall(r, "service redis stop") {
		t.Fatalf("service stop not called, calls=%v", r.Calls)
	}
}

// SysV: enabled — chkconfig --list exit 0 (registered) → no-op.
func TestApply_SysV_Enabled_Idempotent(t *testing.T) {
	r := sysvDetected()
	r.On("chkconfig --list redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if stream.Last().Changed {
		t.Fatal("changed=true for already-enabled sysv service")
	}
	if hasCall(r, "chkconfig redis on") {
		t.Fatalf("unexpected chkconfig on, calls=%v", r.Calls)
	}
}

// SysV: enabled — chkconfig --list exit≠0 (not registered) → chkconfig on.
func TestApply_SysV_EnablesWhenDisabled(t *testing.T) {
	r := sysvDetected()
	r.On("chkconfig --list redis", util.Result{ExitCode: 1})
	r.On("chkconfig redis on", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false on sysv enable")
	}
	if !hasCall(r, "chkconfig redis on") {
		t.Fatalf("chkconfig on not called, calls=%v", r.Calls)
	}
}

// SysV: running + enabled:false on an enabled unit → chkconfig off, changed.
func TestApply_SysV_Running_EnabledFalse_Disables(t *testing.T) {
	r := sysvDetected()
	r.On("service redis status", util.Result{ExitCode: 0})
	r.On("chkconfig --list redis", util.Result{ExitCode: 0})
	r.On("chkconfig redis off", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": false}),
	}, stream)
	if !stream.Last().Changed {
		t.Fatal("changed=false despite chkconfig off")
	}
	if !hasCall(r, "chkconfig redis off") {
		t.Fatalf("chkconfig off not called, calls=%v", r.Calls)
	}
}

// SysV: chkconfig --list process failed to launch (Err) → failed.
func TestApply_SysV_IsEnabledProcessError_Fails(t *testing.T) {
	r := sysvDetected()
	r.On("chkconfig --list redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when chkconfig --list process errored")
	}
}

// --- must: Err branch (process failed to launch) separate from non-zero exit ---

// If the action command itself fails to launch (Err) rather than returning a
// non-zero code — also failed.
func TestApply_ActionProcessError_Fails(t *testing.T) {
	r := systemdDetected()
	r.On("systemctl restart redis", util.Result{Err: errSpawn})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false when action process errored (Err set)")
	}
}

// ─── daemon_reload (ADR-015 amendment): centralized systemctl daemon-reload
// before mutating actions. Guard cases: auto/always/never modes, reload→action
// ordering, changed cleanliness, non-systemd no-op.

const (
	cmdShowNeedReload = "systemctl show redis --property=NeedDaemonReload --value"
	cmdDaemonReload   = "systemctl daemon-reload"
	cmdRestart        = "systemctl restart redis"
)

// indexOf — position of the first call to cmd in r.Calls (-1 if none). Used to
// verify reload→action ordering (reload index < action index).
func indexOf(calls []string, cmd string) int {
	for i, c := range calls {
		if c == cmd {
			return i
		}
	}
	return -1
}

func contains(calls []string, cmd string) bool { return indexOf(calls, cmd) >= 0 }

// auto + NeedDaemonReload=yes → daemon-reload is called BEFORE restart (ordering).
func TestApply_Restarted_DaemonReloadAuto_NeedYes_ReloadsBeforeRestart(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %s", stream.Last().Message)
	}
	ri, xi := indexOf(r.Calls, cmdDaemonReload), indexOf(r.Calls, cmdRestart)
	if ri < 0 || xi < 0 {
		t.Fatalf("expected daemon-reload and restart, calls=%v", r.Calls)
	}
	if ri >= xi {
		t.Fatalf("daemon-reload must come BEFORE restart: reload@%d restart@%d calls=%v", ri, xi, r.Calls)
	}
	// reload doesn't mark the step changed beyond the usual restarted=true (contract).
	if !stream.Last().Changed {
		t.Fatal("restarted: changed=false")
	}
	// diagnostic: reloaded=true in output.
	if v, ok := stream.Last().GetOutput().AsMap()["reloaded"]; !ok || v != true {
		t.Fatalf("output[reloaded] != true: %v", stream.Last().GetOutput().AsMap())
	}
}

// auto + NeedDaemonReload=no → reload is NOT called, restart is called.
func TestApply_Restarted_DaemonReloadAuto_NeedNo_NoReload(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "no\n"})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("daemon-reload must not be called when NeedDaemonReload=no, calls=%v", r.Calls)
	}
	if !contains(r.Calls, cmdRestart) {
		t.Fatalf("restart not called, calls=%v", r.Calls)
	}
	// reloaded is absent from output when reload wasn't performed.
	if _, ok := stream.Last().GetOutput().AsMap()["reloaded"]; ok {
		t.Fatalf("output[reloaded] must not be present without reload: %v", stream.Last().GetOutput().AsMap())
	}
}

// always → daemon-reload is called unconditionally, even when
// NeedDaemonReload=no (systemctl show isn't called at all in this mode).
func TestApply_Restarted_DaemonReloadAlways_AlwaysReloads(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "no\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis", "daemon_reload": "always"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(r.Calls, cmdShowNeedReload) {
		t.Fatalf("always: systemctl show NeedDaemonReload must not be called, calls=%v", r.Calls)
	}
	ri, xi := indexOf(r.Calls, cmdDaemonReload), indexOf(r.Calls, cmdRestart)
	if ri < 0 || ri >= xi {
		t.Fatalf("always: daemon-reload before restart, reload@%d restart@%d calls=%v", ri, xi, r.Calls)
	}
}

// never → daemon-reload is NEVER called (and show isn't called either), restart is called.
func TestApply_Restarted_DaemonReloadNever_NoReload(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"}) // even when yes
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis", "daemon_reload": "never"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("never: daemon-reload must not be called, calls=%v", r.Calls)
	}
	if contains(r.Calls, cmdShowNeedReload) {
		t.Fatalf("never: systemctl show must not be called, calls=%v", r.Calls)
	}
	if !contains(r.Calls, cmdRestart) {
		t.Fatalf("never: restart not called, calls=%v", r.Calls)
	}
}

// running: auto + NeedDaemonReload=yes → reload BEFORE start, changed only from start.
func TestApply_Running_DaemonReloadAuto_NeedYes_ReloadsBeforeStart(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 3})
	r.On("systemctl start redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ri, si := indexOf(r.Calls, cmdDaemonReload), indexOf(r.Calls, "systemctl start redis")
	if ri < 0 || si < 0 || ri >= si {
		t.Fatalf("running: daemon-reload before start, reload@%d start@%d calls=%v", ri, si, r.Calls)
	}
	if !stream.Last().Changed {
		t.Fatal("running: changed=false on start")
	}
}

// running: auto + NeedDaemonReload=yes on an ALREADY active service → reload
// runs, but changed=false (reload doesn't mark the step changed; start isn't needed).
func TestApply_Running_DaemonReloadAuto_ReloadDoesNotMarkChanged(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0}) // already active
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("reload should have executed, calls=%v", r.Calls)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true due to reload on an already-active service - reload must not affect changed")
	}
	if v, ok := stream.Last().GetOutput().AsMap()["reloaded"]; !ok || v != true {
		t.Fatalf("output[reloaded] != true: %v", stream.Last().GetOutput().AsMap())
	}
}

// enabled: auto + NeedDaemonReload=yes → reload BEFORE enable.
func TestApply_Enabled_DaemonReloadAuto_NeedYes_ReloadsBeforeEnable(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On("systemctl is-enabled --quiet redis", util.Result{ExitCode: 1})
	r.On("systemctl enable redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "enabled",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ri, ei := indexOf(r.Calls, cmdDaemonReload), indexOf(r.Calls, "systemctl enable redis")
	if ri < 0 || ei < 0 || ri >= ei {
		t.Fatalf("enabled: daemon-reload before enable, reload@%d enable@%d calls=%v", ri, ei, r.Calls)
	}
}

// non-systemd init (openrc) → daemon-reload no-op (not called even in always mode).
func TestApply_OpenRC_DaemonReloadAlways_NoOp(t *testing.T) {
	r := openrcDetected()
	r.On("rc-service redis restart", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis", "daemon_reload": "always"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("openrc: daemon-reload must not be called (no-op), calls=%v", r.Calls)
	}
	if !contains(r.Calls, "rc-service redis restart") {
		t.Fatalf("openrc restart not called, calls=%v", r.Calls)
	}
}

// stopped is untouched by daemon_reload: the manifest doesn't declare it there,
// Apply doesn't call EnsureDaemonReloaded (reload isn't called even when
// NeedDaemonReload=yes).
func TestApply_Stopped_DaemonReload_NotInvoked(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 0})
	r.On("systemctl is-active --quiet redis", util.Result{ExitCode: 0})
	r.On("systemctl stop redis", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "stopped",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if contains(r.Calls, cmdDaemonReload) || contains(r.Calls, cmdShowNeedReload) {
		t.Fatalf("stopped must not touch daemon-reload, calls=%v", r.Calls)
	}
}

// daemon_reload with an unknown value → validation error (not silent).
func TestValidate_DaemonReload_UnknownValue_Fails(t *testing.T) {
	m := service.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis", "daemon_reload": "reload-pls"}),
	})
	if reply.Ok {
		t.Fatal("Validate daemon_reload=reload-pls: ok=true (should be rejected)")
	}
}

// daemon_reload with valid values passes Validate on all mutating states.
func TestValidate_DaemonReload_ValidValues(t *testing.T) {
	m := service.New()
	for _, st := range []string{"running", "restarted", "enabled"} {
		for _, mode := range []string{"auto", "always", "never"} {
			reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  st,
				Params: mustStruct(t, map[string]any{"name": "redis", "daemon_reload": mode}),
			})
			if !reply.Ok {
				t.Fatalf("Validate %s+daemon_reload=%s: not ok: %v", st, mode, reply.Errors)
			}
		}
	}
}

// ─── Guard cases (QA, low-risk gaps): pin current invariants so the next
// helper edit doesn't change them silently.

// GUARD-1: auto + `systemctl show NeedDaemonReload` returned exit≠0 (or empty
// stdout) — current behavior: reload is NOT performed (indicator != "yes"), but
// the step does NOT fail (no-op). Pinned: exit≠0 from `show` is interpreted as
// "reload not needed", not a step error. Changing this meaning requires architect sign-off.
func TestApply_Restarted_DaemonReloadAuto_ShowNonZeroExit_NoReload_NoFail(t *testing.T) {
	r := systemdDetected()
	// show returned a non-zero code + empty stdout (no Err: the process launched).
	r.On(cmdShowNeedReload, util.Result{ExitCode: 1, Stdout: ""})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The step does NOT fail: non-zero exit from show means "reload not needed", not an error.
	if stream.Last().Failed {
		t.Fatalf("show exit!=0 must not fail the step (current behavior): %s", stream.Last().Message)
	}
	// reload wasn't performed → restart still happened (restarted is always changed).
	if contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("reload must not happen when show!=\"yes\", calls=%v", r.Calls)
	}
	if !contains(r.Calls, cmdRestart) {
		t.Fatalf("restart not called, calls=%v", r.Calls)
	}
	if !stream.Last().Changed {
		t.Fatal("restarted: changed=false")
	}
}

// GUARD-2: daemon_reload as a non-string (number) → Validate Ok=false AND Apply
// Failed (single daemonReloadMode parser, no mutations). Symmetric to
// EnabledNonBool: OptStringParam rejects non-string before the value switch.
func TestDaemonReload_NonString_RejectedBothPaths(t *testing.T) {
	params := mustStruct(t, map[string]any{"name": "redis", "daemon_reload": 7})

	// Validate: Ok=false (daemonReloadMode returns a parser error).
	m := service.New()
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "restarted",
		Params: params,
	})
	if reply.Ok {
		t.Fatal("Validate daemon_reload=7 (number): ok=true (should be rejected)")
	}

	// Apply: Failed on the same parser, no mutations (restart is NOT called).
	r := systemdDetected()
	r.On(cmdRestart, util.Result{ExitCode: 0})
	mm := &service.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	_ = mm.Apply(&pluginv1.ApplyRequest{State: "restarted", Params: params}, stream)
	if !stream.Last().Failed {
		t.Fatal("Apply daemon_reload=7: failed=false (should fail at the parser)")
	}
	if contains(r.Calls, cmdRestart) || contains(r.Calls, cmdDaemonReload) {
		t.Fatalf("non-string daemon_reload must not reach mutations, calls=%v", r.Calls)
	}
}

// GUARD-3: `systemctl daemon-reload` returned exit≠0 → the step fails BEFORE
// the action (restart is NOT called with the stale unit). This is the point of
// the feature: a reload failure must stop the apply, not silently restart with
// a stale definition.
func TestApply_Restarted_DaemonReloadCommandFails_NoRestart(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{ExitCode: 1, Stderr: "reload boom"})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("daemon-reload exit!=0 should fail the step")
	}
	if contains(r.Calls, cmdRestart) {
		t.Fatalf("restart must NOT be called after a daemon-reload failure (otherwise restart with a stale unit), calls=%v", r.Calls)
	}
}

// GUARD-3b: `systemctl daemon-reload` failed to launch (Err) → also failed
// before the action (distinct from non-zero exit, symmetric with other Err branches).
func TestApply_Restarted_DaemonReloadCommandProcessError_NoRestart(t *testing.T) {
	r := systemdDetected()
	r.On(cmdShowNeedReload, util.Result{ExitCode: 0, Stdout: "yes\n"})
	r.On(cmdDaemonReload, util.Result{Err: errSpawn})
	r.On(cmdRestart, util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "restarted",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("daemon-reload process-error should fail the step")
	}
	if contains(r.Calls, cmdRestart) {
		t.Fatalf("restart must NOT be called after Err daemon-reload, calls=%v", r.Calls)
	}
}
