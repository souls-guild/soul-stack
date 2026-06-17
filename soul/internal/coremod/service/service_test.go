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

// errSpawn — имитация «процесс не удалось запустить» (fork failed / binary not
// found): Runner возвращает Result с непустым Err, не ненулевым ExitCode.
var errSpawn = errors.New("fork/exec: permission denied")

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// systemdDetected — runner с проинициализированной systemd-веткой detection.
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

	// enabled — валидный bool-param для running.
	okEnabled, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis", "enabled": true}),
	})
	if !okEnabled.Ok {
		t.Fatalf("Validate running+enabled:true: not ok: %v", okEnabled.Errors)
	}

	// enabled не-bool → отклоняется на Validate, не молча проглатывается.
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

// running + enabled:true на inactive+disabled юните → старт И enable, changed.
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

// running + enabled:true когда уже active И уже enabled → no-op, changed=false,
// никаких start/enable не вызывается (идемпотентность обоих измерений).
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

// running + enabled:true когда active но disabled → enable выполняется, старт
// нет; changed=true только из-за enable-измерения.
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

// running + enabled:false на active+enabled юните → disable выполняется (старт
// нет, уже active), changed=true.
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

// running без param enabled → autostart НЕ трогается: is-enabled/enable/disable
// не вызываются вовсе (управляем только активностью).
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

// TestApply_InitFromFact_NoDetect — BUG-B: soulprint-факт init_system=openrc
// primary, даже когда `rc-service --version` отсутствует (детект провалился бы и
// модуль упал бы «no supported init system» — реальный alpine-баг). С фактом
// модуль идёт прямо в openrc-ветку.
func TestApply_InitFromFact_NoDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // ВСЕ detection-команды отсутствуют
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
		t.Fatalf("failed=true: факт init_system=openrc должен миновать детект, msg=%q", stream.Last().Message)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false: остановленный сервис должен стартовать")
	}
	if hasCall(r, "rc-service --version") {
		t.Fatalf("факт primary: detection не должен вызываться, calls=%v", r.Calls)
	}
}

// TestApply_FactEmpty_FallbackDetect — пустой факт → runtime-детект (обратная
// совместимость factless-хоста: push-режим, старый Keeper до soulprint).
func TestApply_FactEmpty_FallbackDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0}) // детект → openrc
	r.On("rc-service redis status", util.Result{ExitCode: 3})
	r.On("rc-service redis start", util.Result{ExitCode: 0})
	m := &service.Module{Runner: r}
	// SetHostFacts не вызываем — facts пуст.

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "running",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: fallback-детект openrc должен сработать, msg=%q", stream.Last().Message)
	}
	if !hasCall(r, "rc-service --version") {
		t.Fatalf("пустой факт: ожидался fallback-детект, calls=%v", r.Calls)
	}
}

// openrcDetected — runner с systemd absent, openrc present.
func openrcDetected() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("rc-service --version", util.Result{ExitCode: 0})
	return r
}

// sysvDetected — runner с systemd+openrc absent, sysv present.
func sysvDetected() *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("service --version", util.Result{ExitCode: 0})
	return r
}

// hasCall — был ли вызов команды (точное совпадение ключа).
func hasCall(r *internaltest.Runner, cmd string) bool {
	for _, c := range r.Calls {
		if c == cmd {
			return true
		}
	}
	return false
}

// --- Apply: ранние границы (отсутствие name, неизвестное состояние) ---

// name обязателен: без него Apply падает на StringParam ДО детекта init
// (init-проверки не должны выполняться).
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

// state, прошедший Validate, но не разобранный switch-ем Apply, → failed с
// сообщением про unknown state. (Защита от рассинхрона manifest↔switch.)
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

// running + non-bool enabled доходит до Apply (Validate отдельно) → failed на
// TriBoolParam внутри applyRunning, без мутаций.
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

// TestPlan_Running_Active_Clean — Plan(running) для уже активного сервиса без
// управления autostart: changed=false, без мутаций (start/enable/restart).
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

// TestPlan_Running_Inactive_Drift — Plan(running) для остановленного сервиса:
// changed=true (Apply запустил бы), без мутаций.
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

// TestPlan_Running_EnabledDrift — Plan(running, enabled=true) для активного, но
// НЕ автозапускаемого сервиса: changed=true (Apply сделал бы enable).
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

// TestPlan_Restarted_AlwaysDrift — Plan(restarted) всегда changed=true (restart
// безусловно мутирует), без мутаций самого Plan.
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
		t.Fatalf("changed=false, want true (restarted всегда drift)")
	}
	assertNoMutatingSvcCalls(t, r)
}

// planStream — fake stream для Plan, захватывает события.
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

// assertNoMutatingSvcCalls — фейлит, если runner получил start/stop/restart/
// enable/disable (Plan обязан быть pure-read, ADR-031). Допускает читающие
// is-active/is-enabled/--version.
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
				t.Fatalf("Plan вызвал мутирующую команду %q (должен быть pure-read)", c)
			}
		}
	}
}

// --- stopped: путь реального stop + error на проверке активности ---

// stopped на активном сервисе → stop вызывается, changed=true, active:false.
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

// stopped: stop-команда упала (exit≠0) → failed.
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

// stopped: is-active не смогла запуститься (Err) → failed (не falsely-inactive).
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

// --- running: error-пути start и enable ---

// running: start-команда упала → failed.
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

// running + enabled:true: сервис стартовал, но enable упал → failed (ошибка
// enabled-измерения не глотается).
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

// running: is-active процесс не запустился (Err) → failed.
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

// running + enabled:true: is-enabled процесс не запустился (Err) → failed.
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

// --- restarted / enabled: error-пути ---

// restarted: restart-команда упала → failed (несмотря на «всегда changed»).
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

// enabled: is-enabled процесс не запустился (Err) → failed.
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

// enabled: enable-команда упала → failed.
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

// --- OpenRC backend: активность, идемпотентность, enable через rc-update ---

// OpenRC: rc-service status вернул "started" (exit 0) → active, running no-op.
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

// OpenRC: status exit 0, но stdout без "started" (например "crashed") → НЕ
// active, сервис стартует. Проверяет, что бэкенд смотрит на текст, а не только
// на exit-код.
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

// OpenRC: rc-service status процесс не запустился (Err) → failed.
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

// OpenRC: stopped на активном → rc-service stop, changed.
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

// OpenRC: restarted → rc-service restart, всегда changed.
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

// OpenRC: enabled когда юнит НЕ в runlevel default → rc-update add, changed.
// isEnabled парсит вывод `rc-update show default` построчно.
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

// OpenRC: enabled когда юнит УЖЕ в runlevel default (первое поле строки) →
// no-op, changed=false. Проверяет, что парсинг матчит имя как первое поле,
// а не подстроку.
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

// OpenRC: running + enabled:false на enabled юните → rc-update del, changed.
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

// OpenRC: rc-update show процесс не запустился (Err) → failed.
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

// --- SysV backend: активность, enabled через chkconfig ---

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

// SysV: service status exit≠0 → inactive, стартует через service start.
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

// SysV: service status процесс не запустился (Err) → failed.
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

// SysV: stopped на активном → service stop, changed.
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

// SysV: enabled — chkconfig --list exit 0 (зарегистрирован) → no-op.
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

// SysV: enabled — chkconfig --list exit≠0 (не зарегистрирован) → chkconfig on.
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

// SysV: running + enabled:false на enabled юните → chkconfig off, changed.
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

// SysV: chkconfig --list процесс не запустился (Err) → failed.
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

// --- must: ветка Err (процесс не запустился) отдельно от ненулевого exit ---

// Если сама команда действия не запустилась (Err), а не вернула ненулевой код —
// тоже failed.
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
