package reaper

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterReaperMetrics_RegistersAndExposes(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)
	if m == nil {
		t.Fatal("RegisterReaperMetrics returned nil")
	}

	// Gauge keeper_reaper_lease_held виден сразу (Counter/Histogram-Vec без
	// первого WithLabelValues не публикуют sample — проверим их после
	// ObserveRule отдельно).
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_reaper_lease_held 0") {
		t.Errorf("metrics output missing lease_held=0 sample; got=\n%s", body)
	}

	// Проверяем, что collectors функциональны — после ObserveRule все
	// MetricFamily становятся видны через Gather().
	m.ObserveRule("purge_audit_old", 1, nil, time.Millisecond)
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_reaper_rule_executions_total",
		"keeper_reaper_rule_purged_total",
		"keeper_reaper_rule_duration_seconds",
		"keeper_reaper_lease_held",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterReaperMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterReaperMetrics(reg)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterReaperMetrics(reg)
}

func TestObserveRule_SuccessIncrementsExecutionsAndPurged(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	m.ObserveRule("purge_audit_old", 42, nil, 100*time.Millisecond)
	m.ObserveRule("purge_audit_old", 7, nil, 50*time.Millisecond)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="purge_audit_old"} 2`) {
		t.Errorf("executions_total mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_reaper_rule_purged_total{rule="purge_audit_old"} 49`) {
		t.Errorf("purged_total mismatch; got=\n%s", body)
	}
	if !strings.Contains(body, `keeper_reaper_rule_duration_seconds_count{rule="purge_audit_old"} 2`) {
		t.Errorf("duration_seconds_count mismatch; got=\n%s", body)
	}
	// При успехе DispatchErrors не растёт. Если бы рос — sample был бы
	// `..._errors_total{rule="purge_audit_old"} > 0`. Sample-а нет — Counter
	// без вызовов WithLabelValues не публикует child-метрику; проверяем
	// негативно — отсутствие label-а `rule="purge_audit_old"` в errors.
	if strings.Contains(body, `keeper_reaper_dispatch_errors_total{rule="purge_audit_old"}`) {
		t.Errorf("dispatch_errors should not have sample for purge_audit_old on success; got=\n%s", body)
	}
}

func TestObserveRule_ErrorIncrementsDispatchErrors(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	m.ObserveRule("purge_souls", 0, errors.New("pg down"), 200*time.Millisecond)
	m.ObserveRule("purge_souls", 0, errors.New("pg down"), 150*time.Millisecond)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_dispatch_errors_total{rule="purge_souls"} 2`) {
		t.Errorf("dispatch_errors mismatch; got=\n%s", body)
	}
	// Executions всё равно растёт — error не должен прятать факт прогона.
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="purge_souls"} 2`) {
		t.Errorf("executions_total on error mismatch; got=\n%s", body)
	}
	// Purged не растёт — affected при error невалиден.
	if strings.Contains(body, `keeper_reaper_rule_purged_total{rule="purge_souls"}`) {
		t.Errorf("purged_total should not have sample on error; got=\n%s", body)
	}
}

func TestObserveRule_ZeroAffectedSuccess_DoesNotEmitPurgedSample(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	// affected=0 — частый случай: правило отработало, но строк под условие нет.
	// Executions/duration обязаны быть, purged — нет (нечего инкрементить).
	m.ObserveRule("mark_disconnected", 0, nil, 10*time.Millisecond)

	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="mark_disconnected"} 1`) {
		t.Errorf("executions_total mismatch; got=\n%s", body)
	}
	if strings.Contains(body, `keeper_reaper_rule_purged_total{rule="mark_disconnected"}`) {
		t.Errorf("purged_total should not have sample when affected=0; got=\n%s", body)
	}
}

func TestSetLeaseHeld_TogglesGauge(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	m.SetLeaseHeld(true)
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_reaper_lease_held 1") {
		t.Errorf("lease_held should be 1 after SetLeaseHeld(true); got=\n%s", body)
	}

	m.SetLeaseHeld(false)
	body = obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_reaper_lease_held 0") {
		t.Errorf("lease_held should be 0 after SetLeaseHeld(false); got=\n%s", body)
	}
}

func TestReaperMetrics_NilReceiver_NoOp(t *testing.T) {
	// caller (Reaper-runner) может быть без obs-стека (тесты/dev-режим).
	// Все методы на nil-получателе должны быть no-op без панéks.
	var m *ReaperMetrics
	m.ObserveRule("any", 5, nil, time.Second)
	m.ObserveRule("any", 0, errors.New("x"), time.Second)
	m.SetLeaseHeld(true)
	m.SetLeaseHeld(false)
}
