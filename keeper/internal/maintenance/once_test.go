package maintenance

import (
	"errors"
	"testing"
	"time"
)

func once(id string, start, end time.Time) Window {
	return Window{ID: id, Kind: KindOnce, StartsAt: start, EndsAt: end}
}

// TestPastOnceWindowDoesNotCount is the guard of NIM-1137: an incarnation whose
// only window is a once window that has ended has no windows, so a run may
// start at any time outside a freeze — and a freeze still closes it.
func TestPastOnceWindowDoesNotCount(t *testing.T) {
	now := utc(2026, 11, 2, 12, 0)
	c := calc(t, []Window{once("o1", now.Add(-3*time.Hour), now.Add(-time.Hour))}, nil)
	if c.HasWindows(now) {
		t.Error("HasWindows(now) = true for a once window that ended an hour ago, want false")
	}
	assertOpen(t, c, now, true)
	assertNext(t, c, now, now)
	if got := c.Text(now); got != "any time" {
		t.Errorf("Text(now) = %q, want %q", got, "any time")
	}

	frozen := calc(t, []Window{once("o1", now.Add(-3*time.Hour), now.Add(-time.Hour))},
		[]Freeze{{ID: "f1", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)}})
	assertOpen(t, frozen, now, false)
	assertNext(t, frozen, now, now.Add(time.Hour))
}

// TestOnceWindowIsHalfOpen: open from starts_at inclusive, closed at ends_at.
// The weekly window keeps the incarnation windowed at ends_at, so a closed
// interval would show as open there rather than as "no windows left".
func TestOnceWindowIsHalfOpen(t *testing.T) {
	start, end := utc(2026, 11, 4, 10, 0), utc(2026, 11, 4, 11, 0) // a Wednesday
	c := calc(t, []Window{once("o1", start, end), win("02:00", 60, "UTC", "mon")}, nil)
	assertOpen(t, c, start.Add(-time.Nanosecond), false)
	assertOpen(t, c, start, true)
	assertOpen(t, c, end.Add(-time.Nanosecond), true)
	assertOpen(t, c, end, false)
	assertNext(t, c, end, utc(2026, 11, 9, 2, 0))
}

func TestOnceWindowAloneDefersToItsStart(t *testing.T) {
	now := utc(2026, 11, 2, 12, 0)
	start := now.Add(26 * time.Hour)
	c := calc(t, []Window{once("o1", start, start.Add(2*time.Hour))}, nil)
	assertOpen(t, c, now, false)
	assertNext(t, c, now, start)
	assertOpen(t, c, start.Add(time.Hour), true)
	// From its end on the incarnation has no windows left.
	assertOpen(t, c, start.Add(2*time.Hour), true)
}

func TestOnceWindowAddsToWeeklySlots(t *testing.T) {
	// Monday 2026-11-02: the weekly slot is 02:00-03:00; the once window adds
	// Tuesday 14:00-15:00.
	tue := utc(2026, 11, 3, 14, 0)
	c := calc(t, []Window{win("02:00", 60, "UTC", "mon"), once("o1", tue, tue.Add(time.Hour))}, nil)
	assertNext(t, c, utc(2026, 11, 2, 4, 0), tue)
	assertOpen(t, c, tue.Add(30*time.Minute), true)
	assertNext(t, c, tue.Add(time.Hour), utc(2026, 11, 9, 2, 0))
}

func TestFreezeCutsAOnceWindow(t *testing.T) {
	start := utc(2026, 11, 3, 10, 0)
	w := once("o1", start, start.Add(4*time.Hour))
	f := Freeze{ID: "f1", StartsAt: start.Add(time.Hour), EndsAt: start.Add(2 * time.Hour)}
	c := calc(t, []Window{w, win("02:00", 60, "UTC", "mon")}, []Freeze{f})
	assertOpen(t, c, start.Add(90*time.Minute), false)
	assertNext(t, c, start.Add(90*time.Minute), start.Add(2*time.Hour))

	// A freeze over the whole rest of the once windows: at its end they have
	// ended, so with nothing else the incarnation is open there.
	all := Freeze{ID: "f2", StartsAt: start.Add(-time.Hour), EndsAt: start.Add(5 * time.Hour)}
	alone := calc(t, []Window{w}, []Freeze{all})
	assertNext(t, alone, start, start.Add(5*time.Hour))
}

func TestTextNamesOnceWindowsUntilTheyEnd(t *testing.T) {
	start := time.Date(2026, 11, 5, 22, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	c := calc(t, []Window{
		win("02:00", 180, "Europe/Moscow", "mon"),
		once("o1", start, start.Add(6*time.Hour)),
		once("o2", start.Add(30*time.Second), start.Add(90*time.Second)),
	}, nil)
	before := start.Add(-time.Hour)
	want := "mon 02:00 for 3h (Europe/Moscow); once 2026-11-05T19:00:00Z for 6h; once 2026-11-05T19:00:30Z for 1m"
	if got := c.Text(before); got != want {
		t.Errorf("Text before = %q, want %q", got, want)
	}
	if got := c.Text(start.Add(6 * time.Hour)); got != "mon 02:00 for 3h (Europe/Moscow)" {
		t.Errorf("Text after both ended = %q, want the weekly window alone", got)
	}
}

func TestCommonStartWithAOnceWindow(t *testing.T) {
	now := utc(2026, 11, 2, 12, 0)
	start := now.Add(3 * time.Hour)
	a := calc(t, []Window{once("o1", start, start.Add(time.Hour))}, nil)
	b := calc(t, nil, []Freeze{{ID: "f1", StartsAt: now, EndsAt: start.Add(30 * time.Minute)}})
	got, ok := CommonStart([]*Calculator{a, b}, now)
	if !ok || !got.Equal(start.Add(30*time.Minute)) {
		t.Errorf("CommonStart = %s, %v; want the freeze's end inside the once window", got, ok)
	}
	past := calc(t, []Window{once("o2", now.Add(-2*time.Hour), now.Add(-time.Hour))}, nil)
	if got, ok := CommonStart([]*Calculator{past, b}, now); !ok || !got.Equal(start.Add(30*time.Minute)) {
		t.Errorf("CommonStart with a past once window = %s, %v; want the freeze's end", got, ok)
	}
}

func TestOnceWindowSpec(t *testing.T) {
	start := time.Date(2026, 11, 5, 22, 0, 0, 0, time.FixedZone("MSK", 3*3600))
	s, err := WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour), Comment: "fix"}.Normalize()
	if err != nil || s.StartsAt.Location() != time.UTC || !s.StartsAt.Equal(start) {
		t.Fatalf("Normalize = %+v, %v; want the bounds in UTC", s, err)
	}
	if s, err := (WindowSpec{Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC"}).Normalize(); err != nil || s.Kind != KindWeekly {
		t.Errorf("a spec without kind = %+v, %v; want weekly", s, err)
	}
	// Cut to the second, the instant the render asks at; an interval inside one
	// second is then empty.
	frac := utc(2026, 11, 5, 22, 0).Add(500 * time.Millisecond)
	if s, err := (WindowSpec{Kind: KindOnce, StartsAt: frac, EndsAt: frac.Add(time.Hour)}).Normalize(); err != nil ||
		!s.StartsAt.Equal(utc(2026, 11, 5, 22, 0)) || !s.EndsAt.Equal(utc(2026, 11, 5, 23, 0)) {
		t.Errorf("sub-second bounds = %+v, %v; want them cut to the second", s, err)
	}
	if _, err := (WindowSpec{Kind: KindOnce, StartsAt: frac, EndsAt: frac.Add(200 * time.Millisecond)}).Normalize(); err == nil {
		t.Error("an interval inside one second was accepted")
	}
	for name, tc := range map[string]struct {
		spec  WindowSpec
		field string
	}{
		"empty interval": {WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start}, "ends_at"},
		"no start":       {WindowSpec{Kind: KindOnce, EndsAt: start}, "starts_at"},
		"unknown kind":   {WindowSpec{Kind: "daily"}, "kind"},
	} {
		var ve *ValidationError
		if _, err := tc.spec.Normalize(); !errors.As(err, &ve) || ve.Field != tc.field {
			t.Errorf("%s: err = %v, want a refusal of %q", name, err, tc.field)
		}
	}
}

// TestKindsCannotBeMixed is the guard of the one refusal REST answers 422 and
// MCP validation-failed: a field of the other kind is named, never ignored.
func TestKindsCannotBeMixed(t *testing.T) {
	start := utc(2026, 11, 5, 22, 0)
	weekly := WindowSpec{Days: []string{"mon"}, StartTime: "02:00", DurationMinutes: 60, Timezone: "UTC"}
	for name, tc := range map[string]struct {
		spec  WindowSpec
		field string
	}{
		"weekly with starts_at":         {withBounds(weekly, start, time.Time{}), "starts_at"},
		"weekly with ends_at":           {withBounds(weekly, time.Time{}, start), "ends_at"},
		"kind omitted with both bounds": {WindowSpec{StartsAt: start, EndsAt: start.Add(time.Hour)}, "starts_at"},
		"once with days":                {WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour), Days: []string{"mon"}}, "days"},
		"once with start_time":          {WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour), StartTime: "02:00"}, "start_time"},
		"once with duration_minutes":    {WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour), DurationMinutes: 60}, "duration_minutes"},
		"once with timezone":            {WindowSpec{Kind: KindOnce, StartsAt: start, EndsAt: start.Add(time.Hour), Timezone: "UTC"}, "timezone"},
	} {
		var ve *ValidationError
		if _, err := tc.spec.Normalize(); !errors.As(err, &ve) || ve.Field != tc.field {
			t.Errorf("%s: err = %v, want a refusal naming %q", name, err, tc.field)
		}
	}
}

func withBounds(s WindowSpec, start, end time.Time) WindowSpec {
	s.StartsAt, s.EndsAt = start, end
	return s
}

func TestOnceWindowAuditPayloadCarriesOnlyItsFields(t *testing.T) {
	start := utc(2026, 11, 5, 22, 0)
	p := WindowAuditPayload(Window{ID: "o1", IncarnationID: "redis-prod", Kind: KindOnce,
		StartsAt: start, EndsAt: start.Add(time.Hour), Comment: "fix"}, nil)
	if p["kind"] != KindOnce || p["starts_at"] != "2026-11-05T22:00:00Z" || p["ends_at"] != "2026-11-05T23:00:00Z" {
		t.Errorf("payload = %v", p)
	}
	for _, k := range []string{"days", "start_time", "duration_minutes", "timezone"} {
		if _, ok := p[k]; ok {
			t.Errorf("payload carries the weekly field %q: %v", k, p)
		}
	}
}
