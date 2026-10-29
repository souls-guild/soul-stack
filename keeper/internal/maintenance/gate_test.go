package maintenance

import (
	"testing"
	"time"
)

func gate(t *testing.T, r Rules, inWindow bool, consented ...string) *Calculator {
	t.Helper()
	c, err := r.Gate(inWindow, consented)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	return c
}

func assertCommon(t *testing.T, cals []*Calculator, from, want time.Time, wantOK bool) {
	t.Helper()
	got, ok := CommonStart(cals, from)
	if ok != wantOK || (ok && !got.Equal(want)) {
		t.Errorf("CommonStart(%s) = %s,%v, want %s,%v", from.Format(time.RFC3339),
			got.Format(time.RFC3339), ok, want.Format(time.RFC3339), wantOK)
	}
}

func TestCommonStart_NoCalculatorsIsTheInstant(t *testing.T) {
	at := utc(2026, 11, 2, 12, 0)
	assertCommon(t, nil, at, at, true)
}

func TestCommonStart_OverlapOfTwoWindows(t *testing.T) {
	// Mon 01:00-04:00 and Mon 03:00-05:00 UTC meet at Mon 03:00.
	a := calc(t, []Window{win("01:00", 180, "UTC", "mon")}, nil)
	b := calc(t, []Window{win("03:00", 120, "UTC", "mon")}, nil)
	assertCommon(t, []*Calculator{a, b}, utc(2026, 10, 28, 12, 0), utc(2026, 11, 2, 3, 0), true)
	assertCommon(t, []*Calculator{b, a}, utc(2026, 10, 28, 12, 0), utc(2026, 11, 2, 3, 0), true)
}

func TestCommonStart_AnIncarnationWithoutWindowsDoesNotHoldTheOthers(t *testing.T) {
	a := calc(t, []Window{win("02:00", 60, "UTC", "thu")}, nil)
	always := calc(t, nil, nil)
	assertCommon(t, []*Calculator{always, a}, utc(2026, 10, 28, 12, 0), utc(2026, 10, 29, 2, 0), true)
}

func TestCommonStart_DisjointWindowsHaveNone(t *testing.T) {
	a := calc(t, []Window{win("02:00", 60, "UTC", "mon")}, nil)
	b := calc(t, []Window{win("02:00", 60, "UTC", "sat")}, nil)
	assertCommon(t, []*Calculator{a, b}, utc(2026, 10, 28, 12, 0), time.Time{}, false)
}

func TestCommonStart_AFreezeOnOneMovesEveryone(t *testing.T) {
	a := calc(t, nil, []Freeze{{ID: "f", StartsAt: utc(2026, 10, 28, 0, 0), EndsAt: utc(2026, 11, 3, 0, 0)}})
	b := calc(t, []Window{win("02:00", 120, "UTC", "mon", "tue", "wed", "thu", "fri")}, nil)
	assertCommon(t, []*Calculator{a, b}, utc(2026, 10, 28, 12, 0), utc(2026, 11, 3, 2, 0), true)
}

func TestCommonStart_AFreezeOfAnyLengthIsWaitedOut(t *testing.T) {
	from := utc(2026, 10, 28, 12, 0)
	end := from.Add(3 * CommonStartHorizon)
	long := calc(t, nil, []Freeze{{ID: "f", StartsAt: from, EndsAt: end}})
	assertCommon(t, []*Calculator{long}, from, end, true)
	assertCommon(t, []*Calculator{long, calc(t, nil, nil)}, from, end, true)
	// Windows that meet after the freeze are still found: the horizon runs
	// from the latest single start, not from the request.
	windowed := calc(t, []Window{win("02:00", 60, "UTC", "mon")}, nil)
	got, ok := CommonStart([]*Calculator{long, windowed}, from)
	if !ok || got.Before(end) || got.Weekday() != time.Monday || got.Hour() != 2 {
		t.Errorf("CommonStart after a long freeze = %s,%v, want the first Monday 02:00 after %s", got, ok, end)
	}
}

// Staggered freezes on targets with the same window: the freeze of one starts
// after the other's single start and ends past a year. The search must wait
// it out, not spend the horizon on it.
func TestCommonStart_StaggeredFreezesDoNotExhaustTheHorizon(t *testing.T) {
	from := utc(2026, 10, 28, 12, 0)
	mon := []Window{win("02:00", 60, "UTC", "mon")}
	a := calc(t, mon, []Freeze{{ID: "a", StartsAt: from, EndsAt: from.AddDate(0, 0, 30)}})
	b := calc(t, mon, []Freeze{{ID: "b", StartsAt: from.AddDate(0, 0, 20), EndsAt: from.AddDate(0, 0, 400)}})
	got, ok := CommonStart([]*Calculator{a, b}, from)
	if !ok || got.Before(from.AddDate(0, 0, 400)) || got.Weekday() != time.Monday || got.Hour() != 2 {
		t.Errorf("CommonStart = %s,%v, want the first Monday 02:00 after day 400", got, ok)
	}
}

// Many freeze-only targets whose freezes chain one after another: no step
// bound applies, each step lands on a freeze end.
func TestCommonStart_FreezeOnlyChainsBeyondTheStepBound(t *testing.T) {
	from := utc(2026, 10, 28, 12, 0)
	var cals []*Calculator
	n := 3 * commonStartSteps
	for i := n - 1; i >= 0; i-- {
		start := from.Add(time.Duration(i) * time.Hour)
		cals = append(cals, calc(t, nil, []Freeze{{ID: "f", StartsAt: start, EndsAt: start.Add(90 * time.Minute)}}))
	}
	want := from.Add(time.Duration(n-1)*time.Hour + 90*time.Minute)
	assertCommon(t, cals, from, want, true)
}

func TestCommonStart_OneCalculatorAlwaysHasAStart(t *testing.T) {
	from := utc(2026, 10, 28, 12, 0)
	c := calc(t, []Window{win("02:00", 60, "UTC", "mon")},
		[]Freeze{{ID: "f", StartsAt: from, EndsAt: from.Add(2 * CommonStartHorizon)}})
	got, ok := CommonStart([]*Calculator{c}, from)
	if !ok || !got.Equal(c.NextStart(from)) {
		t.Errorf("CommonStart = %s,%v, want NextStart %s", got, ok, c.NextStart(from))
	}
}

func TestGate_WithoutWindowIgnoresWindowsAndConsentedFreezes(t *testing.T) {
	at := utc(2026, 10, 28, 12, 0) // a Wednesday, outside the Monday slot
	r := Rules{
		Windows: []Window{win("02:00", 60, "UTC", "mon")},
		Freezes: []Freeze{
			{ID: "sale", StartsAt: utc(2026, 10, 27, 0, 0), EndsAt: utc(2026, 10, 29, 0, 0)},
			{ID: "incident", StartsAt: utc(2026, 10, 28, 6, 0), EndsAt: utc(2026, 10, 28, 18, 0)},
		},
	}
	assertOpen(t, gate(t, r, false, "sale", "incident"), at, true)
	// The freeze added after consent was given still holds the run.
	c := gate(t, r, false, "sale")
	assertOpen(t, c, at, false)
	assertNext(t, c, at, utc(2026, 10, 28, 18, 0))
	// No consent at all: both freezes, merged.
	assertNext(t, gate(t, r, false), at, utc(2026, 10, 29, 0, 0))
}

func TestGate_InWindowIgnoresConsent(t *testing.T) {
	r := Rules{
		Windows: []Window{win("02:00", 60, "UTC", "thu")},
		Freezes: []Freeze{{ID: "sale", StartsAt: utc(2026, 10, 29, 0, 0), EndsAt: utc(2026, 10, 30, 0, 0)}},
	}
	// Consent is for a run that starts inside the freeze; a window run waits it out.
	c := gate(t, r, true, "sale")
	assertNext(t, c, utc(2026, 10, 28, 12, 0), utc(2026, 11, 5, 2, 0))
}

func TestFreezesAt_EveryCoveringFreezeByStart(t *testing.T) {
	at := utc(2026, 10, 28, 12, 0)
	c := calc(t, nil, []Freeze{
		{ID: "b", StartsAt: utc(2026, 10, 28, 0, 0), EndsAt: utc(2026, 10, 29, 0, 0)},
		{ID: "past", StartsAt: utc(2026, 10, 1, 0, 0), EndsAt: utc(2026, 10, 2, 0, 0)},
		{ID: "a", StartsAt: utc(2026, 10, 28, 0, 0), EndsAt: utc(2026, 10, 28, 13, 0)},
		{ID: "early", StartsAt: utc(2026, 10, 20, 0, 0), EndsAt: utc(2026, 11, 1, 0, 0)},
		{ID: "ends-now", StartsAt: utc(2026, 10, 27, 0, 0), EndsAt: at},
	})
	got := c.FreezesAt(at)
	var ids []string
	for _, f := range got {
		ids = append(ids, f.ID)
	}
	want := []string{"early", "a", "b"}
	if len(ids) != len(want) {
		t.Fatalf("FreezesAt = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("FreezesAt = %v, want %v", ids, want)
		}
	}
}
