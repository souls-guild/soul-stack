// Package maintenance holds an incarnation's maintenance windows and freezes
// ([ADR-0093], epic NIM-1069): the two lists an operator edits, their
// validation, their storage, and the one calculator every consumer asks "may a
// run start now, and if not, when" — the run path, Cadence, scenario CEL and
// the UI read the same answer from [Calculator]; the Reaper's cert rotation
// joins them with epic NIM-1070.
//
// A MaintenanceWindow is a weekly slot set: on each listed day a slot starts at
// a wall-clock time in the window's own timezone and lasts a duration. A window
// gates the START of a run; a run already started is never cut off when the
// slot ends. A Freeze is a one-off interval during which no automatic run
// starts, whatever the windows say. An incarnation with no windows may start a
// run at any time outside a freeze.
//
// [ADR-0093]: ../../../docs/adr/0093-maintenance-window.md
package maintenance

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	// The Keeper must resolve an IANA zone the operator typed even on a host or
	// image that ships no /usr/share/zoneinfo; without the embedded database
	// every zone but UTC would be refused as unknown there.
	_ "time/tzdata"
)

// Window is one MaintenanceWindow of an incarnation.
type Window struct {
	ID            string
	IncarnationID string
	// Days are the weekdays a slot starts on, as `mon`..`sun`, in calendar
	// order. A slot belongs to the day it starts on, even when it crosses
	// midnight.
	Days []string
	// StartTime is the wall-clock start of each slot, `HH:MM`, in Timezone.
	StartTime string
	// DurationMinutes is elapsed time from the slot's start, not wall-clock: a
	// slot over a DST change still lasts exactly this long.
	DurationMinutes int
	Timezone        string
	Comment         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Freeze is one Freeze of an incarnation: the half-open interval
// [StartsAt, EndsAt) during which no automatic run starts.
type Freeze struct {
	ID            string
	IncarnationID string
	StartsAt      time.Time
	EndsAt        time.Time
	Comment       string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// WindowSpec is the operator-supplied part of a window: what create and update
// accept. [WindowSpec.Normalize] validates it.
type WindowSpec struct {
	Days            []string
	StartTime       string
	DurationMinutes int
	Timezone        string
	Comment         string
}

// FreezeSpec is the operator-supplied part of a freeze.
type FreezeSpec struct {
	StartsAt time.Time
	EndsAt   time.Time
	Comment  string
}

const (
	// MaxDurationMinutes bounds a slot to one day, so a slot overlaps at most
	// the next calendar day and a weekly window can never cover a whole week
	// by accident.
	MaxDurationMinutes = 24 * 60
	// MaxCommentLength is in characters, not bytes.
	MaxCommentLength = 500
	// MaxWindowsPerIncarnation and MaxFreezesPerIncarnation keep the
	// calculator, which Cadence (and with epic NIM-1070 the Reaper) runs per
	// incarnation across the fleet, small. Checked at insert, so two
	// concurrent creates may each pass at the last free place.
	MaxWindowsPerIncarnation = 50
	MaxFreezesPerIncarnation = 200
)

// weekdays is the accepted day vocabulary in calendar order (Monday first, the
// order the operator reads a week in); the index is NOT time.Weekday.
var weekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var startTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// IDPattern is the shape of a window or freeze id: the UUID Postgres assigns.
const IDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

var idRe = regexp.MustCompile(IDPattern)

// ValidID reports whether s can name a window or freeze. Checked before a
// lookup, because a malformed value would reach Postgres as a UUID cast error.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidationError is a refused field of a spec. REST answers it 422, MCP
// `validation-failed`.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return "field '" + e.Field + "' " + e.Reason }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Normalize validates s and returns it with Days in calendar order. Duplicate
// days are refused rather than collapsed: a window listing a day twice is more
// likely a typo for another day than a meaningful request.
func (s WindowSpec) Normalize() (WindowSpec, error) {
	if len(s.Days) == 0 {
		return s, invalid("days", "must list at least one day of %v", weekdays)
	}
	seen := make(map[string]bool, len(s.Days))
	for _, d := range s.Days {
		if !slices.Contains(weekdays, d) {
			return s, invalid("days", "entry %q must be one of %v", d, weekdays)
		}
		if seen[d] {
			return s, invalid("days", "lists %q twice", d)
		}
		seen[d] = true
	}
	days := make([]string, 0, len(seen))
	for _, d := range weekdays {
		if seen[d] {
			days = append(days, d)
		}
	}
	s.Days = days

	if !startTimeRe.MatchString(s.StartTime) {
		return s, invalid("start_time", "must be HH:MM (00:00-23:59), got %q", s.StartTime)
	}
	if s.DurationMinutes <= 0 || s.DurationMinutes > MaxDurationMinutes {
		return s, invalid("duration_minutes", "must be between 1 and %d, got %d", MaxDurationMinutes, s.DurationMinutes)
	}
	if _, err := loadZone(s.Timezone); err != nil {
		return s, invalid("timezone", "%v", err)
	}
	if err := checkComment(s.Comment); err != nil {
		return s, err
	}
	return s, nil
}

// Validate checks s. Zero times are refused, so an omitted field is not read as
// the year 1.
func (s FreezeSpec) Validate() error {
	if s.StartsAt.IsZero() {
		return invalid("starts_at", "is required")
	}
	if s.EndsAt.IsZero() {
		return invalid("ends_at", "is required")
	}
	// Compared at the microsecond Postgres stores, so an interval that is empty
	// in the column is refused here rather than by its CHECK as a 500.
	if !s.EndsAt.Truncate(time.Microsecond).After(s.StartsAt.Truncate(time.Microsecond)) {
		return invalid("ends_at", "must be after starts_at")
	}
	return checkComment(s.Comment)
}

func checkComment(c string) error {
	if n := utf8.RuneCountInString(c); n > MaxCommentLength {
		return invalid("comment", "must be at most %d characters, got %d", MaxCommentLength, n)
	}
	if strings.ContainsRune(c, 0) {
		return invalid("comment", "must not contain a NUL character")
	}
	return nil
}

// loadZone resolves an IANA zone name. It refuses every name whose meaning
// depends on the Keeper host, because two instances would then disagree on
// when a window opens: `Local`, and the lower-case entries a system zoneinfo
// directory carries beside the IANA names (`localtime`, `posixrules`,
// `posix/…`, `right/…`). Every IANA zone name starts with a capital letter.
func loadZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, errors.New("is required (an IANA zone such as Europe/Moscow or UTC)")
	}
	if name == "Local" || name[0] < 'A' || name[0] > 'Z' {
		return nil, fmt.Errorf("must name an IANA zone such as Europe/Moscow or UTC; %q depends on the Keeper host", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a known IANA zone", name)
	}
	return loc, nil
}
