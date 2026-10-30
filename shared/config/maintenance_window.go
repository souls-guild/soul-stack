package config

// The rules of a weekly maintenance window (ADR-0093). They live here because two
// things hold such a window: an incarnation, whose windows keeper/internal/maintenance
// validates through these functions, and the cluster default
// `maintenance.default_windows` (NIM-1138), which the keeper config validation checks
// whether it comes from keeper.yml or from the settings overlay. One implementation,
// so the default cannot hold a window an incarnation would be refused.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaintenanceMaxDurationMinutes bounds a slot to one day, so a slot overlaps at
	// most the next calendar day and a weekly window can never cover a whole week
	// by accident.
	MaintenanceMaxDurationMinutes = 24 * 60
	// MaintenanceMaxCommentLength is in characters, not bytes.
	MaintenanceMaxCommentLength = 500
	// MaintenanceMaxWindows caps an incarnation's own windows and, separately,
	// the cluster default; a calculator under the default compiles at most
	// both, the default and the own once windows.
	MaintenanceMaxWindows = 50
)

// MaintenanceWeekdays is the accepted day vocabulary in calendar order (Monday
// first, the order the operator reads a week in); the index is NOT time.Weekday.
var MaintenanceWeekdays = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var maintenanceStartTimeRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// MaintenanceFieldError is a refused field of a maintenance window or freeze. The
// REST API answers it 422, MCP `validation-failed`.
type MaintenanceFieldError struct {
	Field  string
	Reason string
}

func (e *MaintenanceFieldError) Error() string { return "field '" + e.Field + "' " + e.Reason }

func maintenanceInvalid(field, format string, args ...any) error {
	return &MaintenanceFieldError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// NormalizeWeeklyMaintenanceWindow validates one weekly window and returns its days
// in calendar order. Duplicate days are refused rather than collapsed: a window
// listing a day twice is more likely a typo for another day than a meaningful
// request. A refusal is a [*MaintenanceFieldError].
func NormalizeWeeklyMaintenanceWindow(days []string, startTime string, durationMinutes int, timezone, comment string) ([]string, error) {
	if len(days) == 0 {
		return nil, maintenanceInvalid("days", "must list at least one day of %v", MaintenanceWeekdays)
	}
	seen := make(map[string]bool, len(days))
	for _, d := range days {
		if !slices.Contains(MaintenanceWeekdays, d) {
			return nil, maintenanceInvalid("days", "entry %q must be one of %v", d, MaintenanceWeekdays)
		}
		if seen[d] {
			return nil, maintenanceInvalid("days", "lists %q twice", d)
		}
		seen[d] = true
	}
	ordered := OrderedMaintenanceDays(days)

	if !maintenanceStartTimeRe.MatchString(startTime) {
		return nil, maintenanceInvalid("start_time", "must be HH:MM (00:00-23:59), got %q", startTime)
	}
	if durationMinutes <= 0 || durationMinutes > MaintenanceMaxDurationMinutes {
		return nil, maintenanceInvalid("duration_minutes", "must be between 1 and %d, got %d", MaintenanceMaxDurationMinutes, durationMinutes)
	}
	if _, err := LoadMaintenanceZone(timezone); err != nil {
		return nil, maintenanceInvalid("timezone", "%v", err)
	}
	if err := CheckMaintenanceComment(comment); err != nil {
		return nil, err
	}
	return ordered, nil
}

// OrderedMaintenanceDays returns the known days of days in calendar order, each
// once — the order a window's days are stored and shown in.
func OrderedMaintenanceDays(days []string) []string {
	out := make([]string, 0, len(days))
	for _, d := range MaintenanceWeekdays {
		if slices.Contains(days, d) {
			out = append(out, d)
		}
	}
	return out
}

// CheckMaintenanceComment checks the comment of a window or a freeze.
func CheckMaintenanceComment(c string) error {
	if n := utf8.RuneCountInString(c); n > MaintenanceMaxCommentLength {
		return maintenanceInvalid("comment", "must be at most %d characters, got %d", MaintenanceMaxCommentLength, n)
	}
	if strings.ContainsRune(c, 0) {
		return maintenanceInvalid("comment", "must not contain a NUL character")
	}
	return nil
}

// LoadMaintenanceZone resolves an IANA zone name. It refuses every name whose
// meaning depends on the Keeper host, because two instances would then disagree on
// when a window opens: `Local`, and the lower-case entries a system zoneinfo
// directory carries beside the IANA names (`localtime`, `posixrules`, `posix/…`,
// `right/…`). Every IANA zone name starts with a capital letter.
//
// The Keeper and soul-lint link the embedded zone database; a binary that does
// not resolves zones from its host's.
//
// A loaded zone is kept for [maintenanceZoneTTL]: time.LoadLocation reads and
// parses the zone on every call, and a calculator resolves each window's zone on
// every compile — under the cluster default that is every window of the default
// for every incarnation a run or a Cadence tick names. The TTL lets a tzdata update
// on the host reach a running Keeper within the hour, as it reached the next
// compile before. Only a name of IANA shape is kept, so the cache holds at most one
// entry per zone file: a refused name, or a spelling the filesystem also resolves
// (`Europe/./Moscow`), is loaded every time instead.
func LoadMaintenanceZone(name string) (*time.Location, error) {
	if name == "" {
		return nil, errors.New("is required (an IANA zone such as Europe/Moscow or UTC)")
	}
	if name == "Local" || name[0] < 'A' || name[0] > 'Z' {
		return nil, fmt.Errorf("must name an IANA zone such as Europe/Moscow or UTC; %q depends on the Keeper host", name)
	}
	cacheable := ianaZoneShape.MatchString(name)
	if cacheable {
		if z, ok := maintenanceZones.Load(name); ok && time.Since(z.(cachedZone).at) < maintenanceZoneTTL {
			return z.(cachedZone).loc, nil
		}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%q is not a known IANA zone", name)
	}
	if cacheable {
		maintenanceZones.Store(name, cachedZone{loc: loc, at: time.Now()})
	}
	return loc, nil
}

const maintenanceZoneTTL = time.Hour

// ianaZoneShape is the spelling of an IANA zone name: segments of letters, digits,
// `_`, `+` and `-` joined by single slashes, so no `.` or empty segment.
var ianaZoneShape = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

type cachedZone struct {
	loc *time.Location
	at  time.Time
}

var maintenanceZones sync.Map
