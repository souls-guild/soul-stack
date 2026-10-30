package maintenance

import (
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Where the windows in effect for an incarnation come from (NIM-1138), as the
// status reports it.
const (
	// SourceOwn — the incarnation's own windows: a weekly one, or with no
	// default a once window that has not ended.
	SourceOwn = "own"
	// SourceDefault — the cluster default's weekly windows, with the
	// incarnation's own once windows added.
	SourceDefault = "default"
	// SourceNone — nothing counts: a run may start at any time outside a
	// freeze.
	SourceNone = "none"
)

// DefaultsSource supplies the cluster default, `maintenance.default_windows`
// (NIM-1138), as it stands at the moment of a read, so a change of the setting
// reaches the next read on every instance without a restart. nil supplies none.
type DefaultsSource func() []Window

// Unrecorded is the rules of an incarnation with no row yet — one being created:
// the default alone.
func (s DefaultsSource) Unrecorded() Rules { return Rules{Defaults: s.windows()} }

func (s DefaultsSource) windows() []Window {
	if s == nil {
		return nil
	}
	return s()
}

// ConfigDefaults reads the default out of the live config on every call: the
// settings overlay swaps the config on each instance when the setting changes.
func ConfigDefaults(get func() *config.KeeperConfig) DefaultsSource {
	return func() []Window {
		cfg := get()
		if cfg == nil {
			return nil
		}
		return DefaultWindows(cfg.Maintenance.DefaultWindowList())
	}
}

// DefaultWindows turns the configured default into weekly windows, the days in
// calendar order as an incarnation's are stored, whatever order keeper.yml gave.
// The config loader has checked each on the weekly window rules. The id only
// names the entry in a compile error; no answer carries it.
func DefaultWindows(list []config.KeeperMaintenanceWindow) []Window {
	if len(list) == 0 {
		return nil
	}
	out := make([]Window, len(list))
	for i, d := range list {
		out[i] = Window{
			ID: fmt.Sprintf("maintenance.default_windows[%d]", i), Kind: KindWeekly,
			Days: config.OrderedMaintenanceDays(d.Days), StartTime: d.StartTime, DurationMinutes: d.DurationMinutes, Timezone: d.Timezone,
			Comment: d.Comment,
		}
	}
	return out
}

// Effective is the one answer to which windows count for the incarnation, and
// every calculator a reader asks is compiled from it: the incarnation's own
// windows when one of them is weekly, which replace the default entirely; else
// the default's weekly windows followed by the incarnation's own once windows,
// which add to them. Without a default it is the incarnation's own windows, as
// before the default existed.
func (r Rules) Effective() []Window {
	if !r.defaultApplies() {
		return r.Windows
	}
	out := make([]Window, 0, len(r.Defaults)+len(r.Windows))
	out = append(out, r.Defaults...)
	return append(out, r.Windows...)
}

// DefaultsInEffect returns the default's windows when they count for the
// incarnation, nil otherwise.
func (r Rules) DefaultsInEffect() []Window {
	if !r.defaultApplies() {
		return nil
	}
	return r.Defaults
}

// Source names where the windows counting at t come from.
func (r Rules) Source(t time.Time) string {
	switch {
	case r.hasOwnWeekly():
		return SourceOwn
	case len(r.Defaults) > 0:
		return SourceDefault
	}
	for _, w := range r.Windows {
		if w.EndsAt.After(t) {
			return SourceOwn
		}
	}
	return SourceNone
}

// Calculator compiles the windows in effect with the freezes.
func (r Rules) Calculator() (*Calculator, error) {
	return NewCalculator(r.Effective(), r.Freezes)
}

// Status is the calculator's [Calculator.Status] at t with its [Rules.Source].
func (r Rules) Status(t time.Time) (Status, error) {
	c, err := r.Calculator()
	if err != nil {
		return Status{}, err
	}
	s := c.Status(t)
	s.Source = r.Source(t)
	return s, nil
}

func (r Rules) defaultApplies() bool {
	return len(r.Defaults) > 0 && !r.hasOwnWeekly()
}

// hasOwnWeekly reports whether the incarnation has a weekly window of its own; an
// empty kind is weekly, as a spec without one is.
func (r Rules) hasOwnWeekly() bool {
	for _, w := range r.Windows {
		if w.Kind != KindOnce {
			return true
		}
	}
	return false
}
