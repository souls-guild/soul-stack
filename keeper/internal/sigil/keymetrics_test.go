package sigil

import (
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/souls-guild/soul-stack/shared/obs"
)

func TestRegisterKeyMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterKeyMetrics(reg)
	if m == nil {
		t.Fatal("RegisterKeyMetrics вернул nil")
	}

	// Gauge/Counter публикуют семейство сразу после регистрации (в отличие от
	// Vec); прогоняем Observe-методы для надёжности и сверяем присутствие.
	m.SetActive(2)
	m.ObserveAnchorsRebroadcast(3)

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_sigil_signing_keys_active",
		"keeper_sigil_anchors_rebroadcast_total",
		"keeper_sigil_anchors_last_delivered",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q не зарегистрировано", want)
		}
	}
}

func TestRegisterKeyMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterKeyMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("ожидалась паника на повторной регистрации, её не было")
		}
	}()
	RegisterKeyMetrics(reg)
}

// TestObserveAnchorsRebroadcast — счётчик проходов растёт на каждый вызов, gauge
// delivered отражает последнее значение (наблюдаемость доставки набора якорей,
// ADR-026(h) R3-S7 Retire-инвариант).
func TestObserveAnchorsRebroadcast(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterKeyMetrics(reg)

	m.ObserveAnchorsRebroadcast(5)
	m.ObserveAnchorsRebroadcast(2)

	if got := counterValue(t, reg, "keeper_sigil_anchors_rebroadcast_total"); got != 2 {
		t.Errorf("rebroadcast_total = %v, want 2 (два прохода)", got)
	}
	if got := gaugeValue(t, reg, "keeper_sigil_anchors_last_delivered"); got != 2 {
		t.Errorf("last_delivered = %v, want 2 (последняя раздача)", got)
	}
}

// TestKeyMetrics_NilSafe — методы на nil-получателе не паникуют (daemon вызывает
// ObserveAnchorsRebroadcast до wire-up registry / при выключенной observability).
func TestKeyMetrics_NilSafe(t *testing.T) {
	var m *KeyMetrics
	m.SetActive(1)
	m.ObserveAnchorsRebroadcast(1)
}

func gaugeValue(t testing.TB, reg *obs.Registry, name string) float64 {
	t.Helper()
	return metricValue(t, reg, name, func(m *dto.Metric) float64 { return m.GetGauge().GetValue() })
}

func counterValue(t testing.TB, reg *obs.Registry, name string) float64 {
	t.Helper()
	return metricValue(t, reg, name, func(m *dto.Metric) float64 { return m.GetCounter().GetValue() })
}

func metricValue(t testing.TB, reg *obs.Registry, name string, pick func(*dto.Metric) float64) float64 {
	t.Helper()
	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("%q: ожидалась одна серия, получено %d", name, len(f.GetMetric()))
		}
		return pick(f.GetMetric()[0])
	}
	t.Fatalf("MetricFamily %q не найдено", name)
	return 0
}
