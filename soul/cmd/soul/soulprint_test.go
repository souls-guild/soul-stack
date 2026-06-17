package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/soulprint"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// captureSink — fake soulprintReportSink: запоминает отправленные отчёты.
type captureSink struct {
	reports []*keeperv1.SoulprintReport
	err     error
}

func (s *captureSink) SendSoulprintReport(r *keeperv1.SoulprintReport) error {
	if s.err != nil {
		return s.err
	}
	s.reports = append(s.reports, r)
	return nil
}

func newTestPusher(sid string) soulprintPusher {
	return soulprintPusher{
		collector: soulprint.NewCollector(soulprint.NewSystemSource(), nil),
		sid:       sid,
		interval:  time.Hour,
	}
}

func TestSoulprintPusher_PushOnce(t *testing.T) {
	sink := &captureSink{}
	sp := newTestPusher("redis1.example")

	if err := sp.pushOnce(context.Background(), sink); err != nil {
		t.Fatalf("pushOnce: %v", err)
	}
	if len(sink.reports) != 1 {
		t.Fatalf("reports=%d want 1", len(sink.reports))
	}
	rep := sink.reports[0]
	if rep.GetCollectedAt() == nil {
		t.Error("collected_at must be set")
	}
	if rep.GetTypedFacts().GetSid() != "redis1.example" {
		t.Errorf("sid=%q want redis1.example", rep.GetTypedFacts().GetSid())
	}
}

func TestSoulprintPusher_PushOnce_SinkError(t *testing.T) {
	sink := &captureSink{err: errors.New("stream broken")}
	sp := newTestPusher("h")
	if err := sp.pushOnce(context.Background(), sink); err == nil {
		t.Fatal("pushOnce must propagate sink error")
	}
}

// startTicker должен слать сигнал по interval; coalescing — не более одного
// отложенного сигнала при незанятом приёмнике.
func TestSoulprintPusher_StartTicker(t *testing.T) {
	sp := soulprintPusher{interval: 5 * time.Millisecond}
	tick := make(chan struct{}, 1)
	stop := sp.startTicker(context.Background(), tick)
	defer stop()

	select {
	case <-tick:
	case <-time.After(time.Second):
		t.Fatal("expected a tick within 1s")
	}
}

func TestSoulprintPusher_StartTicker_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sp := soulprintPusher{interval: time.Millisecond}
	tick := make(chan struct{}, 1)
	stop := sp.startTicker(ctx, tick)
	cancel()
	stop()
	// drain любой in-flight тик; после cancel новых быть не должно (best-effort
	// проверка, что горутина завершается без deadlock).
	select {
	case <-tick:
	default:
	}
}

func TestLoadSoulprintInterval(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.SoulConfig
		want    time.Duration
		wantErr bool
	}{
		{"nil block default 5m", &config.SoulConfig{}, 5 * time.Minute, false},
		{"empty value default", &config.SoulConfig{Soulprint: &config.SoulSoulprint{}}, 5 * time.Minute, false},
		{"explicit 30s", &config.SoulConfig{Soulprint: &config.SoulSoulprint{RefreshInterval: "30s"}}, 30 * time.Second, false},
		{"invalid", &config.SoulConfig{Soulprint: &config.SoulSoulprint{RefreshInterval: "bogus"}}, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadSoulprintInterval(tc.cfg)
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
