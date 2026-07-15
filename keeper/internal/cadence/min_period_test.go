package cadence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// minPeriodRow is a stub row for SELECT MIN(interval_seconds), bool_or(...).
// dest[0] = **int (nullable), dest[1] = *bool (nullable).
type minPeriodRow struct {
	minIv   *int
	hasCron *bool
	err     error
}

func (r minPeriodRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 2 {
		return errors.New("minPeriodRow: expected 2 dest")
	}
	ivp, ok := dest[0].(**int)
	if !ok {
		return errors.New("minPeriodRow: dest[0] not **int")
	}
	bp, ok := dest[1].(**bool)
	if !ok {
		return errors.New("minPeriodRow: dest[1] not **bool")
	}
	*ivp, *bp = r.minIv, r.hasCron
	return nil
}

func TestDerivedMinPeriod(t *testing.T) {
	iv := func(n int) *int { return &n }
	tests := []struct {
		name   string
		mp     MinPeriod
		want   time.Duration
		wantOK bool
	}{
		// "frequent": interval=30 → derived 30 (inside the 30..60 corridor).
		{"interval 30", MinPeriod{MinIntervalSeconds: iv(30)}, 30 * time.Second, true},
		// "rare": interval=3600 → derived 3600 (clamp will later cut it down to ceiling 60).
		{"interval 3600 raw", MinPeriod{MinIntervalSeconds: iv(3600)}, 3600 * time.Second, true},
		// cron-only: interval_seconds is NULL for cron → derived = cronGranularity 60s.
		{"cron only", MinPeriod{HasCron: true}, 60 * time.Second, true},
		// mixed: interval=45 + cron → min(45, 60) = 45.
		{"mixed interval 45 + cron", MinPeriod{MinIntervalSeconds: iv(45), HasCron: true}, 45 * time.Second, true},
		// mixed: interval=120 + cron → min(120, 60) = 60 (cron "wins").
		{"mixed interval 120 + cron", MinPeriod{MinIntervalSeconds: iv(120), HasCron: true}, 60 * time.Second, true},
		// below-floor (interval=10): derived 10 without the floor clamp — the
		// floor is applied by Clamp, a separate reject-validation lives in Pass B.
		{"interval 10 below floor (no floor here)", MinPeriod{MinIntervalSeconds: iv(10)}, 10 * time.Second, true},
		// interval=10 + cron → min(10, 60) = 10 (floor is Pass B/Clamp, not here).
		{"interval 10 + cron", MinPeriod{MinIntervalSeconds: iv(10), HasCron: true}, 10 * time.Second, true},
		// empty: neither interval nor cron → ok=false (the caller falls back to poll_idle).
		{"empty registry → idle signal", MinPeriod{}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.mp.DerivedMinPeriod()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("derived = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMinPeriod_Empty(t *testing.T) {
	iv := func(n int) *int { return &n }
	if !(MinPeriod{}).Empty() {
		t.Error("zero MinPeriod must be Empty")
	}
	if (MinPeriod{MinIntervalSeconds: iv(30)}).Empty() {
		t.Error("interval-only MinPeriod must not be Empty")
	}
	if (MinPeriod{HasCron: true}).Empty() {
		t.Error("cron-only MinPeriod must not be Empty")
	}
}

func TestClamp(t *testing.T) {
	const (
		floor   = 30 * time.Second
		ceiling = 60 * time.Second
	)
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"below floor → floor", 10 * time.Second, floor},
		{"at floor", floor, floor},
		{"inside corridor", 45 * time.Second, 45 * time.Second},
		{"at ceiling", ceiling, ceiling},
		{"above ceiling → ceiling (редкое 1h)", time.Hour, ceiling},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Clamp(tc.in, floor, ceiling); got != tc.want {
				t.Errorf("Clamp(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSelectMinPeriod_ScanShapes(t *testing.T) {
	iv := func(n int) *int { return &n }
	b := func(v bool) *bool { return &v }
	tests := []struct {
		name    string
		row     pgx.Row
		want    MinPeriod
		wantErr bool
	}{
		{"empty registry (both NULL)", minPeriodRow{minIv: nil, hasCron: nil}, MinPeriod{}, false},
		{"interval only", minPeriodRow{minIv: iv(45), hasCron: b(false)}, MinPeriod{MinIntervalSeconds: iv(45)}, false},
		{"cron present", minPeriodRow{minIv: nil, hasCron: b(true)}, MinPeriod{HasCron: true}, false},
		{"mixed", minPeriodRow{minIv: iv(30), hasCron: b(true)}, MinPeriod{MinIntervalSeconds: iv(30), HasCron: true}, false},
		{"scan error", minPeriodRow{err: errors.New("boom")}, MinPeriod{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fdb := &fakeDB{queryRowFunc: func(string) pgx.Row { return tc.row }}
			got, err := SelectMinPeriod(context.Background(), fdb)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.HasCron != tc.want.HasCron {
				t.Errorf("HasCron = %v, want %v", got.HasCron, tc.want.HasCron)
			}
			switch {
			case got.MinIntervalSeconds == nil && tc.want.MinIntervalSeconds != nil:
				t.Errorf("MinIntervalSeconds = nil, want %d", *tc.want.MinIntervalSeconds)
			case got.MinIntervalSeconds != nil && tc.want.MinIntervalSeconds == nil:
				t.Errorf("MinIntervalSeconds = %d, want nil", *got.MinIntervalSeconds)
			case got.MinIntervalSeconds != nil && *got.MinIntervalSeconds != *tc.want.MinIntervalSeconds:
				t.Errorf("MinIntervalSeconds = %d, want %d", *got.MinIntervalSeconds, *tc.want.MinIntervalSeconds)
			}
		})
	}
}
