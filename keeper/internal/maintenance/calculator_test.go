package maintenance

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func utc(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

func win(start string, minutes int, tz string, days ...string) Window {
	return Window{ID: "w-" + start, Days: days, StartTime: start, DurationMinutes: minutes, Timezone: tz}
}

func calc(t *testing.T, windows []Window, freezes []Freeze) *Calculator {
	t.Helper()
	c, err := NewCalculator(windows, freezes)
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	return c
}

func assertOpen(t *testing.T, c *Calculator, at time.Time, want bool) {
	t.Helper()
	if got := c.OpenAt(at); got != want {
		t.Errorf("OpenAt(%s) = %v, want %v", at.UTC().Format(time.RFC3339), got, want)
	}
}

func assertNext(t *testing.T, c *Calculator, from, want time.Time) {
	t.Helper()
	if got := c.NextStart(from); !got.Equal(want) {
		t.Errorf("NextStart(%s) = %s, want %s", from.UTC().Format(time.RFC3339),
			got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
	}
}

var allDays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

func TestNoWindowsIsAlwaysOpen(t *testing.T) {
	c := calc(t, nil, nil)
	for _, at := range []time.Time{utc(2026, 1, 1, 0, 0), utc(2026, 10, 25, 1, 30), utc(2031, 6, 15, 23, 59)} {
		assertOpen(t, c, at, true)
		assertNext(t, c, at, at)
		if f := c.ActiveFreeze(at); f != nil {
			t.Errorf("ActiveFreeze(%s) = %+v, want nil", at, f)
		}
	}
	if got := c.Text(); got != "any time" {
		t.Errorf("Text() = %q, want %q", got, "any time")
	}
}

func TestNextStartWhileOpenIsTheInstantItself(t *testing.T) {
	c := calc(t, []Window{win("01:00", 120, "UTC", "mon")}, nil)
	at := time.Date(2026, 11, 2, 1, 30, 15, 123456789, time.UTC) // a Monday, sub-second
	got := c.NextStart(at)
	if !got.Equal(at) || got.Nanosecond() != at.Nanosecond() {
		t.Errorf("NextStart(%v) = %v, want the instant itself", at, got)
	}
}

func TestSlotCrossingMidnight(t *testing.T) {
	// Saturday 23:00 for 4h runs into Sunday 03:00; the slot belongs to Saturday.
	c := calc(t, []Window{win("23:00", 240, "UTC", "sat")}, nil)
	assertOpen(t, c, utc(2026, 10, 31, 22, 59), false)
	assertOpen(t, c, utc(2026, 10, 31, 23, 0), true)
	assertOpen(t, c, utc(2026, 11, 1, 1, 0), true) // Sunday, inside Saturday's slot
	assertOpen(t, c, utc(2026, 11, 1, 2, 59), true)
	assertOpen(t, c, utc(2026, 11, 1, 3, 0), false) // the end is exclusive
	assertNext(t, c, utc(2026, 11, 1, 3, 0), utc(2026, 11, 7, 23, 0))
	assertNext(t, c, utc(2026, 10, 30, 12, 0), utc(2026, 10, 31, 23, 0))
}

func TestOverlappingSlots(t *testing.T) {
	c := calc(t, []Window{
		win("01:00", 120, "UTC", "mon"),
		win("02:00", 120, "UTC", "mon"),
		// The same Monday 05:00 UTC written as Moscow wall clock (UTC+3, no DST).
		win("08:00", 60, "Europe/Moscow", "mon"),
	}, nil)
	assertNext(t, c, utc(2026, 11, 2, 0, 0), utc(2026, 11, 2, 1, 0))
	for _, at := range []time.Time{utc(2026, 11, 2, 1, 30), utc(2026, 11, 2, 2, 30), utc(2026, 11, 2, 3, 30)} {
		assertOpen(t, c, at, true)
		assertNext(t, c, at, at)
	}
	assertOpen(t, c, utc(2026, 11, 2, 4, 0), false)
	assertNext(t, c, utc(2026, 11, 2, 4, 0), utc(2026, 11, 2, 5, 0))
	assertOpen(t, c, utc(2026, 11, 2, 5, 59), true)
	assertNext(t, c, utc(2026, 11, 2, 6, 0), utc(2026, 11, 9, 1, 0))
}

// Europe/Berlin moves to CEST on Sunday 2026-03-29 (02:00 -> 03:00) and back to
// CET on Sunday 2026-10-25 (03:00 -> 02:00). Moscow has no DST, so it cannot
// stand in for this test.
func TestDSTEuropeBerlin(t *testing.T) {
	t.Run("a weekly slot keeps its wall clock across the change", func(t *testing.T) {
		c := calc(t, []Window{win("02:00", 60, "Europe/Berlin", "mon")}, nil)
		// Monday 2026-03-23, CET (+1): 02:00 local is 01:00 UTC.
		assertOpen(t, c, utc(2026, 3, 23, 0, 30), false)
		assertOpen(t, c, utc(2026, 3, 23, 1, 0), true)
		// Monday 2026-03-30, CEST (+2): 02:00 local is 00:00 UTC.
		assertNext(t, c, utc(2026, 3, 23, 5, 0), utc(2026, 3, 30, 0, 0))
		assertOpen(t, c, utc(2026, 3, 30, 1, 0), false)
	})
	t.Run("a start the spring change skips moves forward by the gap", func(t *testing.T) {
		c := calc(t, []Window{win("02:30", 60, "Europe/Berlin", "sun")}, nil)
		// 02:30 does not exist on 2026-03-29; the slot starts at 03:30 CEST = 01:30 UTC.
		assertNext(t, c, utc(2026, 3, 28, 12, 0), utc(2026, 3, 29, 1, 30))
		assertOpen(t, c, utc(2026, 3, 29, 2, 29), true)
		assertOpen(t, c, utc(2026, 3, 29, 2, 30), false)
	})
	t.Run("a start the autumn change repeats is its first occurrence", func(t *testing.T) {
		c := calc(t, []Window{win("02:30", 60, "Europe/Berlin", "sun")}, nil)
		// 02:30 happens twice on 2026-10-25: 00:30 UTC (CEST) and 01:30 UTC (CET).
		assertNext(t, c, utc(2026, 10, 24, 12, 0), utc(2026, 10, 25, 0, 30))
		assertOpen(t, c, utc(2026, 10, 25, 1, 29), true)
		assertOpen(t, c, utc(2026, 10, 25, 1, 30), false)
	})
	t.Run("the duration is elapsed time, not wall clock", func(t *testing.T) {
		c := calc(t, []Window{win("01:00", 180, "Europe/Berlin", "sun")}, nil)
		// 01:00 CEST = 23:00 UTC Saturday; three real hours end at 02:00 UTC,
		// which the wall clock reads as 03:00 CET after repeating an hour.
		assertNext(t, c, utc(2026, 10, 24, 12, 0), utc(2026, 10, 24, 23, 0))
		assertOpen(t, c, utc(2026, 10, 25, 1, 59), true)
		assertOpen(t, c, utc(2026, 10, 25, 2, 0), false)
	})
}

func TestFreezeCutsASlot(t *testing.T) {
	freeze := Freeze{ID: "f1", StartsAt: utc(2026, 11, 2, 0, 0), EndsAt: utc(2026, 11, 2, 2, 0), Comment: "release"}
	c := calc(t, []Window{win("01:00", 240, "UTC", allDays...)}, []Freeze{freeze})
	at := utc(2026, 11, 2, 1, 30)
	assertOpen(t, c, at, false)
	assertNext(t, c, at, utc(2026, 11, 2, 2, 0))
	if f := c.ActiveFreeze(at); f == nil || f.ID != "f1" {
		t.Errorf("ActiveFreeze(%s) = %+v, want f1", at, f)
	}
	assertOpen(t, c, utc(2026, 11, 2, 2, 0), true) // the freeze end is exclusive
	if f := c.ActiveFreeze(utc(2026, 11, 2, 2, 0)); f != nil {
		t.Errorf("ActiveFreeze at the freeze end = %+v, want nil", f)
	}
}

func TestFreezeCoversSeveralWindows(t *testing.T) {
	freeze := Freeze{ID: "f1", StartsAt: utc(2026, 11, 2, 0, 0), EndsAt: utc(2026, 11, 5, 12, 0)}
	c := calc(t, []Window{win("02:00", 60, "UTC", allDays...)}, []Freeze{freeze})
	for d := 2; d <= 5; d++ {
		assertOpen(t, c, utc(2026, 11, d, 2, 30), false)
	}
	assertNext(t, c, utc(2026, 11, 2, 0, 0), utc(2026, 11, 6, 2, 0))
}

func TestFreezeWithoutWindows(t *testing.T) {
	// Touching and overlapping freezes form one closed stretch: 10:00 to 14:00.
	// b precedes c in the list, so "the first one containing t" would answer b.
	freezes := []Freeze{
		{ID: "b", StartsAt: utc(2026, 11, 2, 12, 0), EndsAt: utc(2026, 11, 2, 13, 0)},
		{ID: "c", StartsAt: utc(2026, 11, 2, 12, 30), EndsAt: utc(2026, 11, 2, 14, 0)},
		{ID: "a", StartsAt: utc(2026, 11, 2, 10, 0), EndsAt: utc(2026, 11, 2, 12, 0)},
	}
	c := calc(t, nil, freezes)
	assertOpen(t, c, utc(2026, 11, 2, 9, 59), true)
	assertOpen(t, c, utc(2026, 11, 2, 11, 0), false)
	assertNext(t, c, utc(2026, 11, 2, 11, 0), utc(2026, 11, 2, 14, 0))
	if f := c.ActiveFreeze(utc(2026, 11, 2, 12, 45)); f == nil || f.ID != "c" {
		t.Errorf("ActiveFreeze(12:45) = %+v, want c, the one that ends last", f)
	}
}

func TestFreezeEndIgnoresWindowsAndJoinsTouchingFreezes(t *testing.T) {
	freezes := []Freeze{
		{ID: "a", StartsAt: utc(2026, 11, 2, 10, 0), EndsAt: utc(2026, 11, 2, 12, 0)},
		{ID: "b", StartsAt: utc(2026, 11, 2, 12, 0), EndsAt: utc(2026, 11, 2, 13, 0)},
	}
	// A slot only on Sundays: FreezeEnd must not wait for it.
	c := calc(t, []Window{win("02:00", 60, "UTC", "sun")}, freezes)
	if got := c.FreezeEnd(utc(2026, 11, 2, 11, 0)); !got.Equal(utc(2026, 11, 2, 13, 0)) {
		t.Errorf("FreezeEnd(11:00) = %s, want 13:00, the end of both touching freezes", got)
	}
	at := utc(2026, 11, 2, 14, 0)
	if got := c.FreezeEnd(at); !got.Equal(at) {
		t.Errorf("FreezeEnd outside a freeze = %s, want the instant itself", got)
	}
}

// referenceOpen is a brute-force answer to OpenAt that shares nothing with the
// calculator but wallClock, which TestDSTEuropeBerlin pins on its own: every
// slot instance starting from four local days before t to t is tried, so no
// look-back bound can hide a slot that is still running.
func referenceOpen(windows []Window, freezes []Freeze, t time.Time) bool {
	for _, f := range freezes {
		if !t.Before(f.StartsAt) && t.Before(f.EndsAt) {
			return false
		}
	}
	if len(windows) == 0 {
		return true
	}
	for _, w := range windows {
		loc, _ := time.LoadLocation(w.Timezone)
		var hh, mm int
		_, _ = fmt.Sscanf(w.StartTime, "%d:%d", &hh, &mm)
		lt := t.In(loc)
		for back := 4; back >= 0; back-- {
			day := time.Date(lt.Year(), lt.Month(), lt.Day()-back, 0, 0, 0, 0, time.UTC)
			if !slices.Contains(w.Days, weekdays[(int(day.Weekday())+6)%7]) {
				continue
			}
			s := wallClock(day.Year(), day.Month(), day.Day(), hh*60+mm, loc)
			if !t.Before(s) && t.Before(s.Add(time.Duration(w.DurationMinutes)*time.Minute)) {
				return true
			}
		}
	}
	return false
}

// TestCalculatorAgreesWithTheReference walks a minute grid and checks OpenAt
// against [referenceOpen] at every point, and NextStart against the first
// open grid point at or after it. Every boundary is on the grid, so that first
// point is exactly what NextStart must return.
//
// The autumn grid crosses Berlin 2026-10-25 and New York 2026-11-01, with
// Kolkata's +05:30 putting slots off the hour and freezes cutting and covering
// them. The spring grid holds a 24-hour slot starting at 23:30 on Berlin's
// spring-forward Saturday night, which runs into the day after next on the
// wall clock.
func TestCalculatorAgreesWithTheReference(t *testing.T) {
	cases := []struct {
		name     string
		windows  []Window
		freezes  []Freeze
		from, to time.Time
	}{
		{
			name: "autumn",
			windows: []Window{
				win("02:30", 90, "Europe/Berlin", "sun", "wed"),
				win("23:15", 200, "America/New_York", "sat"),
				win("00:00", 45, "Asia/Kolkata", "mon", "tue", "fri"),
			},
			freezes: []Freeze{
				{ID: "f1", StartsAt: utc(2026, 10, 25, 0, 50), EndsAt: utc(2026, 10, 25, 1, 10)},
				{ID: "f2", StartsAt: utc(2026, 10, 27, 0, 0), EndsAt: utc(2026, 10, 30, 0, 0)},
				{ID: "f3", StartsAt: utc(2026, 11, 1, 4, 0), EndsAt: utc(2026, 11, 1, 5, 0)},
			},
			from: utc(2026, 10, 20, 0, 0), to: utc(2026, 11, 10, 0, 0),
		},
		{
			name: "spring, a full-day slot across the gap",
			windows: []Window{
				win("23:30", MaxDurationMinutes, "Europe/Berlin", "sat"),
				win("12:00", 30, "UTC", "wed"),
			},
			from: utc(2026, 3, 25, 0, 0), to: utc(2026, 4, 9, 0, 0),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := calc(t, tc.windows, tc.freezes)
			var grid []time.Time
			for at := tc.from; at.Before(tc.to); at = at.Add(time.Minute) {
				grid = append(grid, at)
			}
			open := make([]bool, len(grid))
			opens, mismatches := 0, 0
			report := func(format string, args ...any) {
				if mismatches++; mismatches <= 5 {
					t.Errorf(format, args...)
				}
			}
			for i, at := range grid {
				open[i] = referenceOpen(tc.windows, tc.freezes, at)
				if open[i] {
					opens++
				}
				if got := c.OpenAt(at); got != open[i] {
					report("OpenAt(%s) = %v, the reference says %v", at.Format(time.RFC3339), got, open[i])
				}
			}
			if opens == 0 || opens == len(grid) {
				t.Fatalf("%d of %d grid points are open; the test would check nothing", opens, len(grid))
			}
			var upcoming time.Time
			for i := len(grid) - 1; i >= 0; i-- {
				if open[i] {
					upcoming = grid[i]
				}
				if upcoming.IsZero() {
					continue
				}
				if got := c.NextStart(grid[i]); !got.Equal(upcoming) {
					report("NextStart(%s) = %s, the first open minute is %s",
						grid[i].Format(time.RFC3339), got.UTC().Format(time.RFC3339), upcoming.Format(time.RFC3339))
				}
			}
			if mismatches > 5 {
				t.Errorf("... %d mismatches in total", mismatches)
			}
		})
	}
}

// TestFullDaySlotAcrossTheSpringGap pins the validator's counterexample: the
// slot starting Saturday 2026-03-28 23:30 CET lasts 24 real hours and ends
// Monday 00:30 CEST, two local days after it started.
func TestFullDaySlotAcrossTheSpringGap(t *testing.T) {
	c := calc(t, []Window{win("23:30", MaxDurationMinutes, "Europe/Berlin", "sat")}, nil)
	at := utc(2026, 3, 29, 22, 15) // Monday 00:15 CEST
	assertOpen(t, c, at, true)
	assertNext(t, c, at, at)
	assertOpen(t, c, utc(2026, 3, 29, 22, 30), false)
}

func TestText(t *testing.T) {
	c := calc(t, []Window{
		win("02:00", 180, "Europe/Moscow", "thu", "mon"),
		win("23:00", 270, "UTC", "sat"),
		win("12:00", 45, "UTC", "sun"),
	}, nil)
	want := "mon,thu 02:00 for 3h (Europe/Moscow); sat 23:00 for 4h30m (UTC); sun 12:00 for 45m (UTC)"
	if got := c.Text(); got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}
}

func TestNewCalculatorRefusesAnUnknownZone(t *testing.T) {
	_, err := NewCalculator([]Window{win("02:00", 60, "Mars/Olympus", "mon")}, nil)
	if err == nil || !strings.Contains(err.Error(), "w-02:00") {
		t.Errorf("NewCalculator with an unknown zone: err = %v, want one naming the window", err)
	}
}

func TestStatus(t *testing.T) {
	freeze := Freeze{ID: "f1", StartsAt: utc(2026, 11, 2, 0, 0), EndsAt: utc(2026, 11, 2, 2, 0)}
	c := calc(t, []Window{win("01:00", 240, "UTC", allDays...)}, []Freeze{freeze})
	s := c.Status(utc(2026, 11, 2, 1, 0))
	if s.OpenNow || !s.NextStart.Equal(utc(2026, 11, 2, 2, 0)) || s.ActiveFreeze == nil || s.ActiveFreeze.ID != "f1" {
		t.Errorf("Status = %+v", s)
	}
}
