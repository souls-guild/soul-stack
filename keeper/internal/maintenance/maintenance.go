// Package maintenance holds an incarnation's maintenance windows and freezes
// ([ADR-0093], epic NIM-1069): the two lists an operator edits, their
// validation, their storage, and the one calculator every consumer asks "may a
// run start now, and if not, when" — the run path, Cadence, scenario CEL and
// the UI read the same answer from [Calculator]; the Reaper's cert rotation
// joins them with epic NIM-1070.
//
// A MaintenanceWindow is of one of two kinds. A `weekly` window is a slot set:
// on each listed day a slot starts at a wall-clock time in the window's own
// timezone and lasts a duration. A `once` window is one interval (NIM-1137). A
// window gates the START of a run; a run already started is never cut off when
// the slot ends. A Freeze is a one-off interval during which no automatic run
// starts, whatever the windows say. An incarnation with no windows — a `once`
// window that has ended is none — may start a run at any time outside a freeze,
// unless the cluster default applies (NIM-1138): an incarnation with no weekly
// window of its own is held to the weekly windows of
// `maintenance.default_windows`, to which its own once windows add
// ([Rules.Effective]).
//
// [ADR-0093]: ../../../docs/adr/0093-maintenance-window.md
package maintenance

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"

	// The Keeper must resolve an IANA zone the operator typed even on a host or
	// image that ships no /usr/share/zoneinfo; without the embedded database
	// every zone but UTC would be refused as unknown there.
	_ "time/tzdata"
)

// The two kinds of window. Each carries only its own fields: Days, StartTime,
// DurationMinutes and Timezone a weekly one, StartsAt and EndsAt a once one.
const (
	KindWeekly = "weekly"
	KindOnce   = "once"
)

// Window is one MaintenanceWindow of an incarnation.
type Window struct {
	ID            string
	IncarnationID string
	Kind          string
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
	// StartsAt and EndsAt are a once window's half-open interval
	// [StartsAt, EndsAt).
	StartsAt  time.Time
	EndsAt    time.Time
	Comment   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Spec is the operator-supplied part of w.
func (w Window) Spec() WindowSpec {
	return WindowSpec{
		Kind: w.Kind, Days: w.Days, StartTime: w.StartTime, DurationMinutes: w.DurationMinutes,
		Timezone: w.Timezone, StartsAt: w.StartsAt, EndsAt: w.EndsAt, Comment: w.Comment,
	}
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
// accept. [WindowSpec.Normalize] validates it. An empty Kind is weekly, so a
// request written before the kinds existed means what it meant. A zero field is
// an absent one.
type WindowSpec struct {
	Kind            string
	Days            []string
	StartTime       string
	DurationMinutes int
	Timezone        string
	StartsAt        time.Time
	EndsAt          time.Time
	Comment         string
}

// FreezeSpec is the operator-supplied part of a freeze.
type FreezeSpec struct {
	StartsAt time.Time
	EndsAt   time.Time
	Comment  string
}

// The weekly window rules are shared/config's, which checks the cluster default
// against the same ones.
const (
	MaxDurationMinutes = config.MaintenanceMaxDurationMinutes
	MaxCommentLength   = config.MaintenanceMaxCommentLength
	// MaxWindowsPerIncarnation and MaxFreezesPerIncarnation keep the
	// calculator, which Cadence (and with epic NIM-1070 the Reaper) runs per
	// incarnation across the fleet, small. Checked at insert, so two
	// concurrent creates may each pass at the last free place.
	MaxWindowsPerIncarnation = config.MaintenanceMaxWindows
	MaxFreezesPerIncarnation = 200
)

var weekdays = config.MaintenanceWeekdays

// IDPattern is the shape of a window or freeze id: the UUID Postgres assigns.
const IDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

var idRe = regexp.MustCompile(IDPattern)

// ValidID reports whether s can name a window or freeze. Checked before a
// lookup, because a malformed value would reach Postgres as a UUID cast error.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidationError is a refused field of a spec. REST answers it 422, MCP
// `validation-failed`.
type ValidationError = config.MaintenanceFieldError

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Normalize validates s and returns it with its kind spelled out, the days of a
// weekly window in calendar order and the bounds of a once window in UTC, cut
// to the second: the render asks the calculator at a whole second, and a bound
// between two seconds would let the claim start a run the render then sees
// outside the window. A field of the other kind is refused by name rather than
// ignored: the operator who sent it meant something by it.
func (s WindowSpec) Normalize() (WindowSpec, error) {
	if err := CheckKindFields(s.Kind, s.setFields()); err != nil {
		return s, err
	}
	switch s.Kind {
	case "", KindWeekly:
		s.Kind = KindWeekly
		return s.normalizeWeekly()
	case KindOnce:
		s.StartsAt, s.EndsAt = s.StartsAt.UTC().Truncate(time.Second), s.EndsAt.UTC().Truncate(time.Second)
		if err := checkInterval(s.StartsAt, s.EndsAt); err != nil {
			return s, err
		}
		return s, checkComment(s.Comment)
	default:
		return s, invalid("kind", "must be %s or %s, got %q", KindWeekly, KindOnce, s.Kind)
	}
}

var (
	weeklyFields = []string{"days", "start_time", "duration_minutes", "timezone"}
	onceFields   = []string{"starts_at", "ends_at"}
)

// CheckKindFields refuses a field among fields that belongs to the other kind
// than kind (empty is weekly). [WindowSpec.Normalize] asks it about the fields
// that are not zero; a caller that sees which fields a request sent asks it
// about those, so one sent empty is refused like one with a value. An unknown
// kind passes here and is refused by Normalize.
func CheckKindFields(kind string, fields []string) error {
	other, reason := onceFields, "belongs to a once window; a weekly window takes days, start_time, duration_minutes and timezone"
	switch kind {
	case "", KindWeekly:
	case KindOnce:
		other, reason = weeklyFields, "belongs to a weekly window; a once window takes starts_at and ends_at"
	default:
		return nil
	}
	for _, f := range other {
		if slices.Contains(fields, f) {
			return invalid(f, "%s", reason)
		}
	}
	return nil
}

// setFields names the kind-specific fields of s that are not zero.
func (s WindowSpec) setFields() []string {
	var out []string
	for f, set := range map[string]bool{
		"days": len(s.Days) > 0, "start_time": s.StartTime != "", "duration_minutes": s.DurationMinutes != 0,
		"timezone": s.Timezone != "", "starts_at": !s.StartsAt.IsZero(), "ends_at": !s.EndsAt.IsZero(),
	} {
		if set {
			out = append(out, f)
		}
	}
	return out
}

// normalizeWeekly puts the days in calendar order
// ([config.NormalizeWeeklyMaintenanceWindow]).
func (s WindowSpec) normalizeWeekly() (WindowSpec, error) {
	days, err := config.NormalizeWeeklyMaintenanceWindow(s.Days, s.StartTime, s.DurationMinutes, s.Timezone, s.Comment)
	if err != nil {
		return s, err
	}
	s.Days = days
	return s, nil
}

// Validate checks s. Zero times are refused, so an omitted field is not read as
// the year 1.
func (s FreezeSpec) Validate() error {
	if err := checkInterval(s.StartsAt, s.EndsAt); err != nil {
		return err
	}
	return checkComment(s.Comment)
}

// checkInterval checks the bounds of a freeze or a once window.
func checkInterval(start, end time.Time) error {
	if start.IsZero() {
		return invalid("starts_at", "is required")
	}
	if end.IsZero() {
		return invalid("ends_at", "is required")
	}
	// Compared at the microsecond Postgres stores, so an interval that is empty
	// in the column is refused here rather than by its CHECK as a 500.
	if !end.Truncate(time.Microsecond).After(start.Truncate(time.Microsecond)) {
		return invalid("ends_at", "must be after starts_at")
	}
	return nil
}

func checkComment(c string) error { return config.CheckMaintenanceComment(c) }

func loadZone(name string) (*time.Location, error) { return config.LoadMaintenanceZone(name) }
