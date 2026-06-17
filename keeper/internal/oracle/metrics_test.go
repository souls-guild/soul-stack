package oracle

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterOracleMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterOracleMetrics(reg)
	if m == nil {
		t.Fatal("RegisterOracleMetrics returned nil")
	}

	// Counter без первого Inc семейство не публикует — прогоняем каждый Observe,
	// затем сверяем присутствие семейств.
	m.ObservePortentReceived()
	m.ObserveDecreeMatched()
	m.ObserveScenarioEnqueued()
	m.ObserveCooldownBlocked()
	m.ObserveCircuitTripped()

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_oracle_portents_received_total",
		"keeper_oracle_decrees_matched_total",
		"keeper_oracle_scenarios_enqueued_total",
		"keeper_oracle_cooldown_blocked_total",
		"keeper_oracle_circuit_tripped_total",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterOracleMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterOracleMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterOracleMetrics(reg)
}

// TestOracleMetrics_Increments — каждый Observe инкрементирует свой счётчик на
// нужную величину.
func TestOracleMetrics_Increments(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterOracleMetrics(reg)

	m.ObservePortentReceived()
	m.ObservePortentReceived()
	m.ObserveDecreeMatched()
	m.ObserveScenarioEnqueued()
	m.ObserveCooldownBlocked()
	m.ObserveCooldownBlocked()
	m.ObserveCooldownBlocked()
	m.ObserveCircuitTripped()

	body := obstest.Scrape(t, reg.Gatherer())
	for _, want := range []string{
		"keeper_oracle_portents_received_total 2",
		"keeper_oracle_decrees_matched_total 1",
		"keeper_oracle_scenarios_enqueued_total 1",
		"keeper_oracle_cooldown_blocked_total 3",
		"keeper_oracle_circuit_tripped_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; got=\n%s", want, body)
		}
	}
}

// TestOracleMetrics_NoHighCardinalityLabels — метрики Oracle без label-ов:
// decree/sid/apply_id/beacon в exposition отсутствуют (cardinality + недоверенный
// вход, ADR-024 §2.2).
func TestOracleMetrics_NoHighCardinalityLabels(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterOracleMetrics(reg)
	m.ObservePortentReceived()
	m.ObserveCircuitTripped()

	body := obstest.Scrape(t, reg.Gatherer())
	for _, forbidden := range []string{"decree=", "sid=", "apply_id=", "beacon=", "subject="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("forbidden label %q leaked into oracle metrics; got=\n%s", forbidden, body)
		}
	}
}

func TestOracleMetrics_NilReceiver_NoOp(t *testing.T) {
	// Oracle-handler может подниматься без obs-стека (unit-тесты, сборки без
	// wire-up). Методы на nil-получателе — no-op без паники.
	var m *OracleMetrics
	m.ObservePortentReceived()
	m.ObserveDecreeMatched()
	m.ObserveScenarioEnqueued()
	m.ObserveCooldownBlocked()
	m.ObserveCircuitTripped()
}
