package serviceregistry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterRegistryMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)
	if m == nil {
		t.Fatal("RegisterRegistryMetrics returned nil")
	}

	// Vec/Histogram/Counter без первого Observe/Inc семейство не публикуют —
	// прогоняем все Observe-методы, затем сверяем присутствие семейств.
	m.ObserveRebuildSuccess(2*time.Millisecond, 3)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
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
		"keeper_serviceregistry_snapshot_rebuild_duration_seconds",
		"keeper_serviceregistry_snapshot_rebuild_errors_total",
		"keeper_serviceregistry_snapshot_last_success_timestamp_seconds",
		"keeper_serviceregistry_snapshot_services",
		"keeper_serviceregistry_invalidations_received_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterRegistryMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterRegistryMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterRegistryMetrics(reg)
}

func TestRegistryMetrics_RebuildSuccessGauges(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)

	m.ObserveRebuildSuccess(time.Millisecond, 7)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_serviceregistry_snapshot_services 7`) {
		t.Errorf("services gauge mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, "keeper_serviceregistry_snapshot_last_success_timestamp_seconds") {
		t.Errorf("last_success_timestamp missing; got=\n%s", body)
	}
}

func TestRegistryMetrics_RebuildErrorKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)

	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)
	m.ObserveRebuildError(time.Millisecond, rebuildErrorLoad)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_serviceregistry_snapshot_rebuild_errors_total{kind="load"} 2`) {
		t.Errorf("load error count mismatch; got=\n%s", body)
	}
	// Каждый ObserveRebuildError тоже наблюдает длительность.
	if !strings.Contains(body, `keeper_serviceregistry_snapshot_rebuild_duration_seconds_count 2`) {
		t.Errorf("rebuild duration count should be 2; got=\n%s", body)
	}
}

func TestRegistryMetrics_NilReceiver_NoOp(t *testing.T) {
	// Holder может подниматься без obs-стека (NewHolder в bootstrap-пути,
	// unit-тесты до wire-up метрик). Метод на nil-получателе — no-op без паники.
	var m *RegistryMetrics
	m.ObserveRebuildSuccess(time.Second, 1)
	m.ObserveRebuildError(time.Second, rebuildErrorLoad)
	m.ObserveInvalidation()
}

// TestHolder_RefreshRecordsSnapshotMetrics — интеграция Holder.Refresh с
// метриками: число Service-ов берётся из построенного снимка. fakeSnapSource/
// snapWith переиспользуются из holder_test.go (тот же пакет).
func TestHolder_RefreshRecordsSnapshotMetrics(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)

	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web", "db")}
	h, err := NewHolder(context.Background(), src, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	h.SetMetrics(m)

	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_serviceregistry_snapshot_services 2`) {
		t.Errorf("services gauge mismatch; got=\n%s", body)
	}
}

// TestHolder_RefreshLoadErrorKind — отказ src.Load маппится в kind=load.
func TestHolder_RefreshLoadErrorKind(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)

	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
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
	if !strings.Contains(body, `keeper_serviceregistry_snapshot_rebuild_errors_total{kind="load"} 1`) {
		t.Errorf("load error kind mismatch; got=\n%s", body)
	}
}

// TestHolder_SetMetricsRace воспроизводит init-order daemon-а: фоновый Run
// (запускается в setupServiceRegistry) живёт, когда SetMetrics вызывается позже в
// setupMetricsRegistry. В этом окне Run конкурентно читает поле metrics (через
// Refresh.ObserveRebuild*), пока SetMetrics его пишет. С atomic.Pointer Store/Load
// атомарны, race-детектор чист (паттерн rbac.Holder).
func TestHolder_SetMetricsRace(t *testing.T) {
	src := &fakeSnapSource{snap: snapWith("git@x:destiny.git", "web")}
	h, err := NewHolder(context.Background(), src, time.Millisecond, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.Run(ctx)

	reg := obs.NewRegistry()
	m := RegisterRegistryMetrics(reg)
	for i := 0; i < 200; i++ {
		h.SetMetrics(m)
	}

	cancel()
}
