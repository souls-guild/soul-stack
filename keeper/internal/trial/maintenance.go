package trial

import (
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// trialInstant is the moment every L0 render describes. Fixed, so a case reading
// `incarnation.maintenance_window.next_start` with no window renders the same text on
// every run; it is not meant to look like a real date.
var trialInstant = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// maintenanceWindow is `fixtures.maintenance_window` as the render reads it, or no
// windows when the case sets none.
func (f Fixtures) maintenanceWindow() (*render.MaintenanceWindow, error) {
	mw := render.MaintenanceAt(nil, trialInstant)
	fx := f.MaintenanceWindow
	if fx == nil {
		return &mw, nil
	}
	if fx.Open == nil {
		return nil, fmt.Errorf("fixtures.maintenance_window.open: required (true - inside the window, false - outside it)")
	}
	mw.Open, mw.Text = *fx.Open, fx.Text
	if fx.NextStart != "" {
		next, err := time.Parse(time.RFC3339, fx.NextStart)
		if err != nil {
			return nil, fmt.Errorf("fixtures.maintenance_window.next_start: %w", err)
		}
		mw.NextStart = next.UTC()
	}
	switch {
	case mw.Open && !mw.NextStart.Equal(trialInstant):
		return nil, fmt.Errorf("fixtures.maintenance_window: an open window starts now - next_start is the L0 instant %s; leave it out", trialInstant.Format(time.RFC3339))
	case !mw.Open && !mw.NextStart.After(trialInstant):
		return nil, fmt.Errorf("fixtures.maintenance_window: a closed window starts later - next_start must be after the L0 instant %s", trialInstant.Format(time.RFC3339))
	}
	return &mw, nil
}
