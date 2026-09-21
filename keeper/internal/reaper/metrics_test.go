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

	// Gauge keeper_reaper_lease_held is visible immediately. Counter/Histogram
	// Vecs do not publish a sample before the first WithLabelValues, so verify
	// them separately after ObserveRule.
	body := obstest.Scrape(t, reg.Gatherer())
	if !strings.Contains(body, "keeper_reaper_lease_held 0") {
		t.Errorf("metrics output missing lease_held=0 sample; got=\n%s", body)
	}

	// Verify collectors are functional: after ObserveRule all MetricFamilies
	// become visible through Gather().
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
	// DispatchErrors does not grow on success. If it did, the sample would be
	// `..._errors_total{rule="purge_audit_old"} > 0`. There is no sample because
	// Counter without WithLabelValues calls does not publish a child metric; check
	// negatively by requiring no `rule="purge_audit_old"` label in errors.
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
	// Executions still grows; an error must not hide the run.
	if !strings.Contains(body, `keeper_reaper_rule_executions_total{rule="purge_souls"} 2`) {
		t.Errorf("executions_total on error mismatch; got=\n%s", body)
	}
	// Purged does not grow because affected is invalid on error.
	if strings.Contains(body, `keeper_reaper_rule_purged_total{rule="purge_souls"}`) {
		t.Errorf("purged_total should not have sample on error; got=\n%s", body)
	}
}

func TestObserveRule_ZeroAffectedSuccess_DoesNotEmitPurgedSample(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterReaperMetrics(reg)

	// affected=0 is common: the rule ran, but no rows matched the condition.
	// Executions/duration must exist, purged must not because there is nothing
	// to increment.
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
	// caller (Reaper runner) may not have an obs stack in tests or dev mode.
	// All methods on a nil receiver must be no-ops without panics.
	var m *ReaperMetrics
	m.ObserveRule("any", 5, nil, time.Second)
	m.ObserveRule("any", 0, errors.New("x"), time.Second)
	m.SetLeaseHeld(true)
	m.SetLeaseHeld(false)
}
