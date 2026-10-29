package maintenance

import (
	"testing"
	"time"
)

// The Cadence spawn builds the calculator list from its resolved targets, in
// whatever order they come; CommonStart must not depend on it. Each case below
// is one where a single pass over one of the orders stops on a moment an
// earlier calculator no longer accepts.

func mustCalc(t *testing.T, windows []Window, freezes []Freeze) *Calculator {
	t.Helper()
	c, err := NewCalculator(windows, freezes)
	if err != nil {
		t.Fatalf("NewCalculator: %v", err)
	}
	return c
}

func utcWindow0(id string, days []string, start string, minutes int) Window {
	return Window{ID: id, Days: days, StartTime: start, DurationMinutes: minutes, Timezone: "UTC"}
}

var everyDay = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// 2026-11-02 is a Monday.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, time.UTC)
}

// a's 02:00–03:00 slot is frozen until 02:30 and then open; b's 02:00 slot
// ends at 02:20. In the order [b, a] a pass stops at 02:30 only because a
// moved it, and the next pass must find b closed there.
func TestCommonStart_ConvergesInEitherOrder(t *testing.T) {
	a := mustCalc(t, []Window{utcWindow0("a", everyDay, "02:00", 60), utcWindow0("a2", everyDay, "10:00", 60)},
		[]Freeze{{ID: "f", StartsAt: at(3, 1, 0), EndsAt: at(3, 2, 30)}})
	b := mustCalc(t, []Window{utcWindow0("b", everyDay, "02:00", 20), utcWindow0("b2", everyDay, "10:00", 60)}, nil)
	for _, order := range [][]*Calculator{{a, b}, {b, a}} {
		got, ok := CommonStart(order, at(2, 14, 0))
		if !ok || !got.Equal(at(3, 10, 0)) {
			t.Fatalf("CommonStart = %v, %v; want %v (b is closed again at 02:30)", got, ok, at(3, 10, 0))
		}
	}
}

// Without windows the start is the end of the freezes, chained across
// incarnations: b is open at 11:00, a then moves the candidate into b's freeze.
func TestCommonStart_FreezesChainAcrossIncarnationsInEitherOrder(t *testing.T) {
	a := mustCalc(t, nil, []Freeze{{ID: "a", StartsAt: at(2, 10, 0), EndsAt: at(2, 12, 0)}})
	b := mustCalc(t, nil, []Freeze{{ID: "b", StartsAt: at(2, 11, 30), EndsAt: at(2, 14, 0)}})
	for _, order := range [][]*Calculator{{a, b}, {b, a}} {
		if got, ok := CommonStart(order, at(2, 11, 0)); !ok || !got.Equal(at(2, 14, 0)) {
			t.Fatalf("CommonStart = %v, %v; want %v", got, ok, at(2, 14, 0))
		}
	}
}
