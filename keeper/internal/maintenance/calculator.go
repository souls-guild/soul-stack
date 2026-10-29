package maintenance

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Calculator answers, for one incarnation's windows and freezes, whether a run
// may start at a given instant and when it next may. It is pure: every method
// takes the instant it is asked about, and nothing reads the clock.
//
// "Open" means inside a slot of some window (or there are no windows) AND
// outside every freeze. A freeze applies with or without windows.
type Calculator struct {
	windows []slotRule
	freezes []Freeze
	// frozen is freezes merged into disjoint intervals, sorted, with touching
	// intervals joined, so the next start after a freeze is the end of the
	// whole run of freezes it belongs to.
	frozen []span
	text   string
}

type slotRule struct {
	days   [7]bool // indexed by time.Weekday
	minute int     // minutes after local midnight
	dur    time.Duration
	loc    *time.Location
}

type span struct{ start, end time.Time }

// Status is what the API shows beside the lists.
type Status struct {
	OpenNow      bool
	NextStart    time.Time
	ActiveFreeze *Freeze
}

// NewCalculator compiles windows and freezes. It fails only on a row that would
// not pass validation now — in practice a zone the embedded tzdata no longer
// knows — and names the window.
func NewCalculator(windows []Window, freezes []Freeze) (*Calculator, error) {
	c := &Calculator{}
	texts := make([]string, 0, len(windows))
	for _, w := range windows {
		spec, err := WindowSpec{
			Days: w.Days, StartTime: w.StartTime, DurationMinutes: w.DurationMinutes, Timezone: w.Timezone,
		}.Normalize()
		if err != nil {
			return nil, fmt.Errorf("maintenance window %s: %w", w.ID, err)
		}
		loc, _ := loadZone(spec.Timezone)
		var r slotRule
		for _, d := range spec.Days {
			r.days[weekdayOf(d)] = true
		}
		var hh, mm int
		_, _ = fmt.Sscanf(spec.StartTime, "%d:%d", &hh, &mm)
		r.minute = hh*60 + mm
		r.dur = time.Duration(spec.DurationMinutes) * time.Minute
		r.loc = loc
		c.windows = append(c.windows, r)
		texts = append(texts, fmt.Sprintf("%s %s for %s (%s)",
			strings.Join(spec.Days, ","), spec.StartTime, formatMinutes(spec.DurationMinutes), spec.Timezone))
	}
	if len(texts) == 0 {
		c.text = "any time"
	} else {
		c.text = strings.Join(texts, "; ")
	}

	c.freezes = slices.Clone(freezes)
	sorted := slices.Clone(freezes)
	slices.SortFunc(sorted, func(a, b Freeze) int { return a.StartsAt.Compare(b.StartsAt) })
	for _, f := range sorted {
		if !f.EndsAt.After(f.StartsAt) {
			continue
		}
		if n := len(c.frozen); n > 0 && !f.StartsAt.After(c.frozen[n-1].end) {
			if f.EndsAt.After(c.frozen[n-1].end) {
				c.frozen[n-1].end = f.EndsAt
			}
			continue
		}
		c.frozen = append(c.frozen, span{f.StartsAt, f.EndsAt})
	}
	return c, nil
}

// OpenAt reports whether a run may start at t.
func (c *Calculator) OpenAt(t time.Time) bool {
	if c.frozenUntil(t) != nil {
		return false
	}
	return len(c.windows) == 0 || c.inSlot(t)
}

// NextStart returns the first instant at or after t at which a run may start:
// t itself when [Calculator.OpenAt] is true. Every window lists at least one
// day and freezes are finite, so such an instant always exists.
func (c *Calculator) NextStart(t time.Time) time.Time {
	cand := t
	for {
		if end := c.frozenUntil(cand); end != nil {
			cand = *end
			continue
		}
		if len(c.windows) == 0 {
			return cand
		}
		next := c.slotFrom(cand)
		// Zero only for a window with no days, which NewCalculator refuses;
		// returning beats looping back to the year 1.
		if next.IsZero() || next.Equal(cand) {
			return next
		}
		cand = next
	}
}

// ActiveFreeze returns the freeze in force at t, or nil. When several overlap
// at t it returns the one that ends last, since that is the one an operator
// waiting for a start needs to see; ties go to the earlier start, then the id.
func (c *Calculator) ActiveFreeze(t time.Time) *Freeze {
	var best *Freeze
	for i := range c.freezes {
		f := &c.freezes[i]
		if t.Before(f.StartsAt) || !t.Before(f.EndsAt) {
			continue
		}
		if best == nil || f.EndsAt.After(best.EndsAt) ||
			(f.EndsAt.Equal(best.EndsAt) && (f.StartsAt.Before(best.StartsAt) ||
				(f.StartsAt.Equal(best.StartsAt) && f.ID < best.ID))) {
			best = f
		}
	}
	if best == nil {
		return nil
	}
	out := *best
	return &out
}

// FreezeEnd returns the first instant at or after t that no freeze covers: t
// itself outside a freeze, else the end of the whole run of touching or
// overlapping freezes t falls in. It ignores the windows, so it is what a run
// that is not held to a window still waits for (a Cadence spawn without
// in_maintenance_window).
func (c *Calculator) FreezeEnd(t time.Time) time.Time {
	if end := c.frozenUntil(t); end != nil {
		return *end
	}
	return t
}

// Text renders the windows for a human or a scenario, e.g.
// `mon,thu 02:00 for 3h (Europe/Moscow); sat 23:00 for 4h30m (UTC)`, in the
// order the windows were given; `any time` when there are none. Freezes are not
// part of it.
func (c *Calculator) Text() string { return c.text }

// Status bundles the three answers for t.
func (c *Calculator) Status(t time.Time) Status {
	return Status{OpenNow: c.OpenAt(t), NextStart: c.NextStart(t), ActiveFreeze: c.ActiveFreeze(t)}
}

// frozenUntil returns the end of the merged freeze interval containing t.
func (c *Calculator) frozenUntil(t time.Time) *time.Time {
	for i := range c.frozen {
		s := &c.frozen[i]
		if t.Before(s.start) {
			return nil
		}
		if t.Before(s.end) {
			return &s.end
		}
	}
	return nil
}

func (c *Calculator) inSlot(t time.Time) bool {
	return c.slotFrom(t).Equal(t)
}

// slotFrom returns t when t is inside a slot, else the earliest slot start
// after t. It looks two local days back: a slot is at most 24h of elapsed
// time, but one starting late on a spring-forward day gains the skipped hour
// on the wall clock and runs into the day after next (sat 23:30 for 24h in
// Berlin ends Monday 00:30). Eight days ahead always reaches every listed
// weekday.
func (c *Calculator) slotFrom(t time.Time) time.Time {
	var next time.Time
	for _, r := range c.windows {
		lt := t.In(r.loc)
		for i := -2; i <= 8; i++ {
			day := time.Date(lt.Year(), lt.Month(), lt.Day()+i, 0, 0, 0, 0, time.UTC)
			if !r.days[day.Weekday()] {
				continue
			}
			start := wallClock(day.Year(), day.Month(), day.Day(), r.minute, r.loc)
			if !t.Before(start) && t.Before(start.Add(r.dur)) {
				return t
			}
			if start.After(t) && (next.IsZero() || start.Before(next)) {
				next = start
			}
		}
	}
	return next
}

// wallClock resolves a wall-clock time on a date in loc to an instant, and
// settles the two cases a DST change creates instead of leaving them to
// time.Date, which documents neither choice:
//   - a time the spring change skips moves forward by the length of the gap
//     (02:30 on a 02:00→03:00 night is 03:30);
//   - a time the autumn change repeats is its FIRST occurrence, so a slot
//     opens as early as its wall clock allows.
//
// The two candidate offsets are taken two days either side, far enough to
// straddle any one transition and too near for two of them.
func wallClock(y int, m time.Month, d, minute int, loc *time.Location) time.Time {
	naive := time.Date(y, m, d, 0, minute, 0, 0, time.UTC)
	_, offBefore := naive.Add(-48 * time.Hour).In(loc).Zone()
	_, offAfter := naive.Add(48 * time.Hour).In(loc).Zone()
	early := naive.Add(-time.Duration(offBefore) * time.Second)
	late := naive.Add(-time.Duration(offAfter) * time.Second)
	if early.After(late) {
		early, late = late, early
	}
	matches := func(c time.Time) bool {
		lc := c.In(loc)
		return lc.Year() == y && lc.Month() == m && lc.Day() == d && lc.Hour()*60+lc.Minute() == minute
	}
	switch {
	case matches(early):
		return early
	case matches(late):
		return late
	default:
		return late
	}
}

func weekdayOf(day string) time.Weekday {
	return time.Weekday((slices.Index(weekdays, day) + 1) % 7)
}

func formatMinutes(m int) string {
	h, mm := m/60, m%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", mm)
	case mm == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, mm)
	}
}
