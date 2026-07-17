package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/utilization"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// captureUtilSink — fake utilizationReportSink: запоминает отправленные снимки.
type captureUtilSink struct {
	reports []*keeperv1.HostUtilization
	err     error
}

func (s *captureUtilSink) SendHostUtilization(u *keeperv1.HostUtilization) error {
	if s.err != nil {
		return s.err
	}
	s.reports = append(s.reports, u)
	return nil
}

func newTestUtilPusher(sid string) utilizationPusher {
	return utilizationPusher{
		collector: utilization.NewCollector(utilization.NewSystemSource()),
		sid:       sid,
		interval:  time.Hour,
	}
}

func TestUtilizationPusher_PushOnce(t *testing.T) {
	sink := &captureUtilSink{}
	up := newTestUtilPusher("redis1.example")

	if err := up.pushOnce(context.Background(), sink); err != nil {
		t.Fatalf("pushOnce: %v", err)
	}
	if len(sink.reports) != 1 {
		t.Fatalf("reports=%d want 1", len(sink.reports))
	}
	if sink.reports[0].GetCollectedAt() == nil {
		t.Error("collected_at must be set")
	}
}

func TestUtilizationPusher_PushOnce_SinkError(t *testing.T) {
	sink := &captureUtilSink{err: errors.New("stream broken")}
	up := newTestUtilPusher("h")
	if err := up.pushOnce(context.Background(), sink); err == nil {
		t.Fatal("pushOnce must propagate sink error")
	}
}

func TestUtilizationPusher_StartTicker(t *testing.T) {
	up := utilizationPusher{interval: 5 * time.Millisecond}
	tick := make(chan struct{}, 1)
	stop := up.startTicker(context.Background(), tick)
	defer stop()

	select {
	case <-tick:
	case <-time.After(time.Second):
		t.Fatal("expected a tick within 1s")
	}
}

// Floor 10s + ceiling 30s + default 30s (ADR-072): значение зажимается в
// [10s,30s], отсутствие блока → дефолт, кривая строка → error.
func TestLoadUtilizationInterval(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.SoulConfig
		want    time.Duration
		wantErr bool
	}{
		{"nil block default 30s", &config.SoulConfig{}, 30 * time.Second, false},
		{"empty value default", &config.SoulConfig{Utilization: &config.SoulUtilization{}}, 30 * time.Second, false},
		{"in-range 20s passes", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "20s"}}, 20 * time.Second, false},
		{"floor: 3s -> 10s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "3s"}}, 10 * time.Second, false},
		{"floor: 9s -> 10s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "9s"}}, 10 * time.Second, false},
		{"exactly floor 10s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "10s"}}, 10 * time.Second, false},
		{"just above floor 11s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "11s"}}, 11 * time.Second, false},
		{"exactly ceiling 30s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "30s"}}, 30 * time.Second, false},
		{"ceiling: 1m -> 30s", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "1m"}}, 30 * time.Second, false},
		{"invalid", &config.SoulConfig{Utilization: &config.SoulUtilization{Interval: "bogus"}}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadUtilizationInterval(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("interval=%v want %v", got, tc.want)
			}
		})
	}
}

// resolveUtilizationInterval: фикстура без блока → default 30s; после инъекции
// utilization.interval=3s + Reload → floor 10s (hot-reload, ADR-021 + floor).
func TestResolveUtilizationInterval_DefaultAndFloorReload(t *testing.T) {
	t.Parallel()
	store, path := soulFixtureStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if d := resolveUtilizationInterval(store, logger); d != 30*time.Second {
		t.Fatalf("no-block interval = %s, want 30s (default)", d)
	}

	src, _ := os.ReadFile(path)
	edited := append(src, []byte("\nutilization:\n  interval: 3s\n")...)
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edit: %v", err)
	}
	if res := store.Reload(context.Background(), config.ReloadSourceSignal); !res.Swapped {
		t.Fatalf("Swapped=false on valid edit: %+v", res.Diagnostics)
	}

	if d := resolveUtilizationInterval(store, logger); d != 10*time.Second {
		t.Errorf("after reload interval = %s, want 10s (floor)", d)
	}
}
