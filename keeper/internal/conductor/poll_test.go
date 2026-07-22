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

// TestAdaptivePollInterval_Sets — clamp(derivedMinPeriod, floor, ceiling) for
// registry sets in the "Calm" profile (ADR-048 "Adaptive interval").
func TestAdaptivePollInterval_Sets(t *testing.T) {
	tests := []struct {
		name string
		mp   cadence.MinPeriod
		want time.Duration
	}{
		// "frequent" (interval=30) → floor-bound 30s (derived 30 within the corridor).
		{"frequent interval 30 → floor-bound", cadence.MinPeriod{MinIntervalSeconds: iv(30)}, 30 * time.Second},
		// more frequent than floor (interval=10) → floor-clamp 30s (reject — Pass B).
		{"interval 10 below floor → floor 30", cadence.MinPeriod{MinIntervalSeconds: iv(10)}, 30 * time.Second},
		// "rare" (interval=1h) → ceiling-cap 60s.
		{"rare interval 1h → ceiling-cap 60", cadence.MinPeriod{MinIntervalSeconds: iv(3600)}, 60 * time.Second},
		// cron-only → derived 60s = ceiling.
		{"cron only → 60", cadence.MinPeriod{HasCron: true}, 60 * time.Second},
		// mixed (interval=45 + cron) → 45s (within the corridor).
		{"mixed 45 + cron → 45", cadence.MinPeriod{MinIntervalSeconds: iv(45), HasCron: true}, 45 * time.Second},
		// mixed (interval=120 + cron) → min(120,60)=60 = ceiling.
		{"mixed 120 + cron → 60", cadence.MinPeriod{MinIntervalSeconds: iv(120), HasCron: true}, 60 * time.Second},
		// empty → idle 120s.
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

// TestAdaptivePollInterval_FetchError — SelectMinPeriod error → fallback to
// ceiling (the infrequent edge, not floor), the leader doesn't crash.
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

// TestAdaptivePollInterval_HotReload — a corridor change in the config
// snapshot (closure) is visible on the next resolve without recreating
// IntervalFn. Same registry (interval=45), but the ceiling/floor change
// moves the result.
func TestAdaptivePollInterval_HotReload(t *testing.T) {
	cur := calmCorridor()
	corridor := func() PollCorridor { return cur }
	fetcher := fakeFetcher{mp: cadence.MinPeriod{MinIntervalSeconds: iv(45)}}

	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 45*time.Second {
		t.Fatalf("before reload = %v, want 45s", got)
	}

	// Hot-reload: narrowed ceiling to 40s — derived 45 is now > ceiling → cap 40.
	cur = PollCorridor{Floor: 30 * time.Second, Ceiling: 40 * time.Second, Idle: 120 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 40*time.Second {
		t.Errorf("after ceiling reload = %v, want 40s (cap)", got)
	}

	// Hot-reload: raised floor to 50s — derived 45 is now < floor → floor 50.
	cur = PollCorridor{Floor: 50 * time.Second, Ceiling: 60 * time.Second, Idle: 120 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fetcher, nil); got != 50*time.Second {
		t.Errorf("after floor reload = %v, want 50s (floor)", got)
	}

	// Hot-reload: idle 90s, empty registry → idle 90 (new snapshot value).
	cur = PollCorridor{Floor: 30 * time.Second, Ceiling: 60 * time.Second, Idle: 90 * time.Second}
	if got := AdaptivePollInterval(context.Background(), corridor, fakeFetcher{}, nil); got != 90*time.Second {
		t.Errorf("after idle reload (empty registry) = %v, want 90s", got)
	}
}

// TestAdaptivePollInterval_FailoverStateless — two independent calls over the
// same registry give an identical step: IntervalFn is stateless, the new
// leader after failover recomputes from PG, carrying no in-memory poll state.
func TestAdaptivePollInterval_FailoverStateless(t *testing.T) {
	fetcher := fakeFetcher{mp: cadence.MinPeriod{MinIntervalSeconds: iv(45), HasCron: true}}
	// "Old leader".
	a := AdaptivePollInterval(context.Background(), calmCorridor, fetcher, nil)
	// "New leader" after failover — same registry, same config.
	b := AdaptivePollInterval(context.Background(), calmCorridor, fetcher, nil)
	if a != b {
		t.Errorf("stateless violated: leader1=%v leader2=%v", a, b)
	}
	if a != 45*time.Second {
		t.Errorf("derived(min(45,60)) clamp = %v, want 45s", a)
	}
}
