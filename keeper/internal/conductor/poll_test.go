package conductor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
)

type fakeFetcher struct {
	mp  cadence.MinPeriod
	err error
}

func (f fakeFetcher) SelectMinPeriod(context.Context) (cadence.MinPeriod, error) {
	return f.mp, f.err
}

func iv(n int) *int { return &n }

func calmCorridor() PollCorridor {
	return PollCorridor{
		Floor:   30 * time.Second,
		Ceiling: 60 * time.Second,
		Idle:    120 * time.Second,
	}
}

// TestAdaptivePollInterval_Sets — clamp(derivedMinPeriod, floor, ceiling) для
// наборов реестра в профиле «Спокойный» (ADR-048 «Adaptive interval»).
func TestAdaptivePollInterval_Sets(t *testing.T) {
	tests := []struct {
		name string
		mp   cadence.MinPeriod
		want time.Duration
	}{
		// «частое» (interval=30) → floor-bound 30s (derived 30 внутри коридора).
		{"frequent interval 30 → floor-bound", cadence.MinPeriod{MinIntervalSeconds: iv(30)}, 30 * time.Second},
		// частее floor (interval=10) → floor-clamp 30s (reject — Pass B).
		{"interval 10 below floor → floor 30", cadence.MinPeriod{MinIntervalSeconds: iv(10)}, 30 * time.Second},
		// «редкое» (interval=1h) → ceiling-cap 60s.
		{"rare interval 1h → ceiling-cap 60", cadence.MinPeriod{MinIntervalSeconds: iv(3600)}, 60 * time.Second},
		// cron-only → derived 60s = ceiling.
		{"cron only → 60", cadence.MinPeriod{HasCron: true}, 60 * time.Second},
		// смешанное (interval=45 + cron) → 45s (внутри коридора).
		{"mixed 45 + cron → 45", cadence.MinPeriod{MinIntervalSeconds: iv(45), HasCron: true}, 45 * time.Second},
		// смешанное (interval=120 + cron) → min(120,60)=60 = ceiling.
		{"mixed 120 + cron → 60", cadence.MinPeriod{MinIntervalSeconds: iv(120), HasCron: true}, 60 * time.Second},
		// пусто → idle 120s.
		{"empty registry → idle 120", cadence.MinPeriod{}, 120 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AdaptivePollInterval(context.Background(), calmCorridor, fakeFetcher{mp: tc.mp}, nil)
			if got != tc.want {
				t.Errorf("AdaptivePollInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdaptivePollInterval_FetchError — ошибка SelectMinPeriod → fallback на
// ceiling (нечастый край, не floor), лидер не падает.
func TestAdaptivePollInterval_FetchError(t *testing.T) {
	got := AdaptivePollInterval(
		context.Background(),
		calmCorridor,
		fakeFetcher{err: errors.New("pg glitch")},
		nil,
	)
	if got != 60*time.Second {
		t.Errorf("on fetch error = %v, want ceiling 60s", got)
	}
}

// TestAdaptivePollInterval_HotReload — смена коридора в config-снимке (closure)
// видна на следующем resolve без пересоздания IntervalFn. Реестр тот же
// (interval=45), но смена ceiling/floor двигает результат.
func TestAdaptivePollInterval_HotReload(t *testing.T) {
	cur := calmCorridor()
	corridor := func() PollCorridor { return cur }
	fetcher := fakeFetcher{mp: cadence.MinPeriod{MinIntervalSeconds: iv(45)}}

	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 45*time.Second {
		t.Fatalf("before reload = %v, want 45s", got)
	}

	// Hot-reload: сузили ceiling до 40s — derived 45 теперь > ceiling → cap 40.
	cur = PollCorridor{Floor: 30 * time.Second, Ceiling: 40 * time.Second, Idle: 120 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 40*time.Second {
		t.Errorf("after ceiling reload = %v, want 40s (cap)", got)
	}

	// Hot-reload: подняли floor до 50s — derived 45 теперь < floor → floor 50.
	cur = PollCorridor{Floor: 50 * time.Second, Ceiling: 60 * time.Second, Idle: 120 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 50*time.Second {
		t.Errorf("after floor reload = %v, want 50s (floor)", got)
	}

	// Hot-reload: idle 90s, реестр пуст → idle 90 (новое значение снимка).
	cur = PollCorridor{Floor: 30 * time.Second, Ceiling: 60 * time.Second, Idle: 90 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fakeFetcher{}, nil); got != 90*time.Second {
		t.Errorf("after idle reload (empty registry) = %v, want 90s", got)
	}
}

// TestAdaptivePollInterval_FailoverStateless — два независимых вызова над тем же
// реестром дают идентичный шаг: IntervalFn stateless, новый лидер после failover
// пересчитывает из PG, не неся in-memory состояния опроса.
func TestAdaptivePollInterval_FailoverStateless(t *testing.T) {
	fetcher := fakeFetcher{mp: cadence.MinPeriod{MinIntervalSeconds: iv(45), HasCron: true}}
	// «Старый лидер».
	a := AdaptivePollInterval(context.Background(), calmCorridor, fetcher, nil)
	// «Новый лидер» после failover — тот же реестр, тот же config.
	b := AdaptivePollInterval(context.Background(), calmCorridor, fetcher, nil)
	if a != b {
		t.Errorf("stateless нарушен: leader1=%v leader2=%v", a, b)
	}
	if a != 45*time.Second {
		t.Errorf("derived(min(45,60)) clamp = %v, want 45s", a)
	}
}
