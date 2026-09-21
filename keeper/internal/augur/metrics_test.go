package augur

import (
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/obs"
	"github.com/souls-guild/soul-stack/shared/obs/obstest"
)

func TestRegisterBrokerMetrics_RegistersFamilies(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBrokerMetrics(reg)
	if m == nil {
		t.Fatal("RegisterBrokerMetrics returned nil")
	}

	// Vec/Histogram families are not published without first Observe; run
	// ObserveFetch then check for family presence.
	m.ObserveFetch(string(SourceVault), DecisionOK, time.Millisecond)

	families, err := reg.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	seen := map[string]bool{}
	for _, f := range families {
		seen[f.GetName()] = true
	}
	for _, want := range []string{
		"keeper_augur_fetch_total",
		"keeper_augur_fetch_duration_seconds",
	} {
		if !seen[want] {
			t.Errorf("MetricFamily %q not registered", want)
		}
	}
}

func TestRegisterBrokerMetrics_PanicsOnDoubleRegister(t *testing.T) {
	reg := obs.NewRegistry()
	RegisterBrokerMetrics(reg)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double register, got none")
		}
	}()
	RegisterBrokerMetrics(reg)
}

// TestBrokerMetrics_FetchBySourceDecision tests fetch_total split by source and
// decision with closed-enum values, without omen_name/query/sid in labels.
func TestBrokerMetrics_FetchBySourceDecision(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBrokerMetrics(reg)

	m.ObserveFetch(string(SourceVault), DecisionOK, time.Millisecond)
	m.ObserveFetch(string(SourceVault), DecisionOK, time.Millisecond)
	m.ObserveFetch(string(SourcePrometheus), DecisionDenied, time.Millisecond)
	m.ObserveFetch(string(SourceELK), DecisionError, time.Millisecond)
	m.ObserveFetch(SourceUnknown, DecisionError, 0)

	body := obstest.Scrape(t, reg.Gatherer())
	for _, want := range []string{
		`keeper_augur_fetch_total{decision="ok",source="vault"} 2`,
		`keeper_augur_fetch_total{decision="denied",source="prometheus"} 1`,
		`keeper_augur_fetch_total{decision="error",source="elk"} 1`,
		`keeper_augur_fetch_total{decision="error",source="unknown"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q; got=\n%s", want, body)
		}
	}
}

// TestBrokerMetrics_NoSecretLabels verifies that fetch_total labels are limited to
// closed-enum source/decision; omen_name/query/sid/apply_id are not in exposition.
func TestBrokerMetrics_NoSecretLabels(t *testing.T) {
	reg := obs.NewRegistry()
	m := RegisterBrokerMetrics(reg)
	m.ObserveFetch(string(SourceVault), DecisionOK, time.Millisecond)

	body := obstest.Scrape(t, reg.Gatherer())
	for _, forbidden := range []string{"omen", "query", "sid", "apply_id", "request_id"} {
		if strings.Contains(body, forbidden+"=") {
			t.Errorf("forbidden label %q leaked into augur metrics; got=\n%s", forbidden, body)
		}
	}
}

func TestBrokerMetrics_NilReceiver_NoOp(t *testing.T) {
	// Broker may start without obs-stack (unit tests, builds without Augur wiring).
	// Method on nil receiver is a no-op without panic.
	var m *BrokerMetrics
	m.ObserveFetch(string(SourceVault), DecisionOK, time.Second)
	m.ObserveFetch(SourceUnknown, DecisionError, 0)
}

func TestTracer_NotNil(t *testing.T) {
	// Tracer() returns a package-level tracer for the gRPC handler; when OTel is disabled
	// it is a no-op tracer (not nil), Start/End are free.
	if Tracer() == nil {
		t.Fatal("Tracer() returned nil")
	}
}
