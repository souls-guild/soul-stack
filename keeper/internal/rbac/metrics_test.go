package rbac

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterRBACMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)
	if m == nil {
		t.Fatal("RegisterRBACMetrics returned nil")
	}

	// Vec/Histogram/Counter без первого Observe/Inc семейство не публикуют —
	// прогоняем все Observe-методы, затем сверяем присутствие семейств.
	m.ObserveRebuildSuccess(2*time.Millisecond, 3, 2)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("deny"))
	m.ObserveInvalidation()

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_rbac_snapshot_rebuild_duration_seconds",
		"keeper_rbac_snapshot_rebuild_errors_total",
		"keeper_rbac_snapshot_last_success_timestamp_seconds",
		"keeper_rbac_snapshot_roles",
		"keeper_rbac_snapshot_operators",
		"keeper_rbac_checks_total",
		"keeper_rbac_invalidations_received_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterRBACMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterRBACMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterRBACMetrics(reg)
}

func TestRBACMetrics_RebuildErrorsByKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorParse)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorParse)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="load"} 1`) {
		t.Errorf("load count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="parse"} 2`) {
		t.Errorf("parse count mismatch; got=\n%s", body)
	}
	// Каждый ObserveRebuildError тоже наблюдает длительность.
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_duration_seconds_count 3`) {
		t.Errorf("rebuild duration count should be 3; got=\n%s", body)
	}
}

func TestRBACMetrics_RebuildSuccessGauges(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveRebuildSuccess(time.Millisecond, 5, 3)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_roles 5`) {
		t.Errorf("roles gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_operators 3`) {
		t.Errorf("operators gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, "keeper_rbac_snapshot_last_success_timestamp_seconds") {
		t.Errorf("last_success_timestamp missing; got=\n%s", body)
	}
}

func TestRBACMetrics_ChecksByResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	m.ObserveCheck(nil)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("deny"))

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_checks_total{result="allow"} 2`) {
		t.Errorf("allow count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_checks_total{result="deny"} 1`) {
		t.Errorf("deny count mismatch; got=\n%s", body)
	}
}

func TestRBACMetrics_NilReceiver_NoOp(t *testing.T) {
	// Holder может подниматься без obs-стека (NewHolder в bootstrap-пути,
	// unit-тесты до wire-up метрик). Метод на nil-получателе — no-op без паники.
	var m *RBACMetrics
	m.ObserveRebuildSuccess(time.Second, 1, 1)
	m.ObserveRebuildError(time.Second, rebuildErrorLoad)
	m.ObserveCheck(nil)
	m.ObserveCheck(errors.New("x"))
	m.ObserveInvalidation()
}

// TestHolder_RefreshRecordsSnapshotMetrics — интеграция Holder.Refresh с
// метриками: counts ролей/операторов берутся из построенного enforcer-а.
// fakeSource/adminSnapshot переиспользуются из holder_test.go (тот же пакет).
func TestHolder_RefreshRecordsSnapshotMetrics(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: &Snapshot{
		Roles: map[string][]string{
			"admin":  {"*"},
			"viewer": {"incarnation.get"},
		},
		Membership: map[string][]string{
			"archon-alice": {"admin"},
			"archon-bob":   {"viewer"},
		},
	}}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)

	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_roles 2`) {
		t.Errorf("roles gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_snapshot_operators 2`) {
		t.Errorf("operators gauge mismatch; got=\n%s", body)
	}
}

// TestHolder_RefreshLoadErrorKind — отказ src.Load маппится в kind=load.
func TestHolder_RefreshLoadErrorKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)
	src.set(nil, errors.New("db down")) // источник падает после первичной загрузки

	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected Refresh error")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_snapshot_rebuild_errors_total{kind="load"} 1`) {
		t.Errorf("load error kind mismatch; got=\n%s", body)
	}
}

// TestHolder_CheckRecordsResult — Holder.Check инкрементирует checks_total
// по реальному исходу enforcer.Check.
func TestHolder_CheckRecordsResult(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)

	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)

	if err := h.Check("archon-alice", "incarnation", "get", nil); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if err := h.Check("archon-nobody", "incarnation", "get", nil); err == nil {
		t.Fatal("expected deny for unknown AID")
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_rbac_checks_total{result="allow"} 1`) {
		t.Errorf("allow count mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_rbac_checks_total{result="deny"} 1`) {
		t.Errorf("deny count mismatch; got=\n%s", body)
	}
}

// TestHolder_SetMetricsRace воспроизводит реальный init-order daemon-а: фоновая
// goroutine Run (запускается в setupRBAC) живёт, когда SetMetrics вызывается
// позже в setupMetricsRegistry. В этом окне Run конкурентно читает поле metrics
// (через Refresh.ObserveRebuild* и Check.ObserveCheck), пока SetMetrics его
// пишет. С обычным полем `metrics *RBACMetrics` это data race — go test -race
// падал бы (write ↔ read одного и того же слова без синхронизации). С
// atomic.Pointer Store/Load атомарны, race-детектор чист.
//
// Зачем interval=мс: тик Run-а реально вызывает Refresh → m.ObserveRebuild*,
// т.е. читателя поля metrics, а не просто крутит select. Параллельный Check —
// второй читатель того же поля. Цикл SetMetrics — писатель. Возврат к обычному
// полю снова уронит этот тест под -race (регрессия ловится).
func TestHolder_SetMetricsRace(t *testing.T) {
	src := &fakeSource{snap: adminSnapshot("archon-alice")}
	h, err := NewHolder(context.Background(), src, time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Read-сторона: фоновый Run (как в setupRBAC) перечитывает снимок и на
	// каждом тике/проверке читает поле metrics.
	go h.Run(ctx)
	checkDone := make(chan struct{})
	go func() {
		defer close(checkDone)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_ = h.Check("archon-alice", "operator", "create", nil)
			}
		}
	}()

	// Write-сторона: SetMetrics конкурентно пишет поле (как в
	// setupMetricsRegistry, позже по времени). Каждый Store на отдельном
	// дескрипторе — нагляднее для гонки.
	reg := obs.NewRegistry()
	m := RegisterRBACMetrics(reg)
	for i := 0; i < 200; i++ {
		h.SetMetrics(m) // повтор одного и того же дескриптора допустим (idempotent Store)
	}

	cancel()
	<-checkDone
}
