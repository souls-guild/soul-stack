package main

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/utilization"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func utilPusherWith(sid string, interval time.Duration, ts *telemetryState) utilizationPusher {
	return utilizationPusher{
		collector: utilization.NewCollector(utilization.NewSystemSource()),
		sid:       sid,
		interval:  interval,
		telemetry: ts,
	}
}

// applyTelemetryConfig клампит доставленный interval в [floor 10s, ceiling 3600s]
// (defense-in-depth поверх keeper-клампа) и выставляет delivered/enabled (NIM-87).
func TestApplyTelemetryConfig_IntervalClampAndFlags(t *testing.T) {
	cases := []struct {
		name         string
		intervalSec  int32
		enabled      bool
		wantInterval time.Duration
	}{
		{"in-range 60s", 60, true, 60 * time.Second},
		{"floor: 3s -> 10s", 3, true, 10 * time.Second},
		{"ceiling: 5000s -> 3600s", 5000, true, 3600 * time.Second},
		{"zero -> floor 10s", 0, true, 10 * time.Second},
		{"exactly ceiling 3600s", 3600, false, 3600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := &telemetryState{collectors: allCollectorsSet()}
			ts.applyTelemetryConfig(&keeperv1.TelemetryConfig{Enabled: tc.enabled, IntervalSec: tc.intervalSec})
			if !ts.delivered {
				t.Fatal("delivered must be true after apply")
			}
			if ts.enabled != tc.enabled {
				t.Errorf("enabled=%v want %v", ts.enabled, tc.enabled)
			}
			if ts.interval != tc.wantInterval {
				t.Errorf("interval=%v want %v", ts.interval, tc.wantInterval)
			}
		})
	}
}

// nil-cfg — no-op (defensive): delivered остаётся false.
func TestApplyTelemetryConfig_NilCfgNoop(t *testing.T) {
	ts := &telemetryState{collectors: allCollectorsSet()}
	ts.applyTelemetryConfig(nil)
	if ts.delivered {
		t.Error("nil cfg must not mark delivered")
	}
}

// collectorSetFromNames: пустой список → все 5; список → только валидные;
// неизвестные имена игнорируются (валиден лишь config.KnownCollectors).
func TestCollectorSetFromNames(t *testing.T) {
	all := collectorSetFromNames(nil)
	if len(all) != len(config.KnownCollectors) {
		t.Fatalf("empty -> %d collectors want %d (all)", len(all), len(config.KnownCollectors))
	}
	for _, n := range config.KnownCollectors {
		if !all[n] {
			t.Errorf("empty must enable %q", n)
		}
	}

	one := collectorSetFromNames([]string{"cpu", "bogus", "disk"})
	if !one["cpu"] || !one["disk"] {
		t.Errorf("must enable cpu+disk: %v", one)
	}
	if one["bogus"] {
		t.Error("unknown collector must be ignored")
	}
	if len(one) != 2 {
		t.Errorf("set size=%d want 2 (cpu,disk)", len(one))
	}
}

func TestClampUtilizationInterval(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{3 * time.Second, 10 * time.Second},  // floor
		{10 * time.Second, 10 * time.Second}, // exactly floor
		{45 * time.Second, 45 * time.Second}, // in-range
		{3600 * time.Second, 3600 * time.Second},
		{5000 * time.Second, 3600 * time.Second}, // ceiling
		{0, 10 * time.Second},                    // defensive
	}
	for _, tc := range cases {
		if got := clampUtilizationInterval(tc.in); got != tc.want {
			t.Errorf("clamp(%v)=%v want %v", tc.in, got, tc.want)
		}
	}
}

// pulseEnabled: nil-holder / без директивы → pulse включён; delivered управляет
// enable-тогглом (enabled=false → стоп, повторный enabled=true → возобновление).
func TestPulseEnabled_Toggle(t *testing.T) {
	var nilTs *telemetryState
	if !nilTs.pulseEnabled() {
		t.Error("nil holder must default to pulse enabled")
	}

	ts := &telemetryState{collectors: allCollectorsSet()}
	if !ts.pulseEnabled() {
		t.Error("not-delivered must default to pulse enabled")
	}

	ts.applyTelemetryConfig(&keeperv1.TelemetryConfig{Enabled: false, IntervalSec: 30})
	if ts.pulseEnabled() {
		t.Error("enabled=false must stop pulse")
	}

	ts.applyTelemetryConfig(&keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 30})
	if !ts.pulseEnabled() {
		t.Error("re-enabled must resume pulse")
	}
}

// Durable across reconnect: delivered-конфиг (durable holder) перебивает
// soul-local utilization.interval при старте новой сессии. Симулирует reconnect —
// повторный effectiveStartInterval на том же holder-е (переживает сессию).
func TestEffectiveStartInterval_DeliveredWinsOverSoulLocal(t *testing.T) {
	store, _ := soulFixtureStore(t) // фикстура без блока utilization → soul-local default 30s
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ts := &telemetryState{collectors: allCollectorsSet()}
	// Сессия 1: директивы ещё не было → soul-local (30s).
	if d := effectiveStartInterval(ts, store, logger); d != 30*time.Second {
		t.Fatalf("pre-directive interval=%v want 30s (soul-local)", d)
	}

	// Пришла директива 60s.
	ts.applyTelemetryConfig(&keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 60})

	// Сессия 2 (после «reconnect»): тот же holder → delivered 60s, не soul-local.
	if d := effectiveStartInterval(ts, store, logger); d != 60*time.Second {
		t.Errorf("post-reconnect interval=%v want 60s (delivered wins)", d)
	}
}

// Живое применение директивы: каденс меняется на 60s (holder+payload), gating
// коллекторов уважается — процесс не рестартует (in-place мутация holder-а).
func TestTelemetryDirective_LiveRecadenceAndGating(t *testing.T) {
	ts := &telemetryState{collectors: allCollectorsSet()}
	ts.applyTelemetryConfig(&keeperv1.TelemetryConfig{Enabled: true, IntervalSec: 60, Collectors: []string{"cpu"}})
	if ts.interval != 60*time.Second {
		t.Fatalf("holder interval=%v want 60s (cadence changed)", ts.interval)
	}

	sink := &captureUtilSink{}
	up := utilPusherWith("h", ts.interval, ts)
	if err := up.pushOnce(context.Background(), sink); err != nil {
		t.Fatalf("pushOnce: %v", err)
	}
	rep := sink.reports[0]
	// interval_sec несёт эффективный каденс (Keeper масштабирует TTL).
	if rep.GetIntervalSec() != 60 {
		t.Errorf("payload interval_sec=%d want 60", rep.GetIntervalSec())
	}
	// collectors=[cpu] → mem/load/uptime нулевые, disk пропущен.
	if rep.GetMemTotalMb() != 0 || rep.GetLoad1() != 0 || rep.GetUptimeSec() != 0 || rep.GetDisks() != nil {
		t.Errorf("only cpu expected, got mem/load/uptime/disks: %d/%v/%d/%v",
			rep.GetMemTotalMb(), rep.GetLoad1(), rep.GetUptimeSec(), rep.GetDisks())
	}
}

// pushOnce всегда проставляет interval_sec = эффективный каденс, даже без
// доставленной директивы (soul-local/дефолтный интервал).
func TestUtilPusher_IntervalSecFromInterval(t *testing.T) {
	sink := &captureUtilSink{}
	up := utilPusherWith("h", 25*time.Second, &telemetryState{collectors: allCollectorsSet()})
	if err := up.pushOnce(context.Background(), sink); err != nil {
		t.Fatalf("pushOnce: %v", err)
	}
	if got := sink.reports[0].GetIntervalSec(); got != 25 {
		t.Errorf("interval_sec=%d want 25", got)
	}
}
