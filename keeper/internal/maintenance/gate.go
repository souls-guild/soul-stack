package maintenance

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// Rules is one incarnation's windows and freezes, and the cluster default, as a
// read finds them. Every answer about when the incarnation may start goes
// through [Rules.Effective]. The zero value is an incarnation with none of the
// three: always open.
type Rules struct {
	Windows []Window
	Freezes []Freeze
	// Defaults is the cluster default at the read (NIM-1138); it counts only
	// while Windows holds no weekly window.
	Defaults []Window
}

// LoadRules reads the windows and freezes of every incarnation in ids, one
// query per table however many ids there are: a Voyage may name thousands. Each
// incarnation gets the default defaults supplies, read once for the whole call.
// An incarnation with no rows is absent from the map while the default is empty,
// and present with the default alone otherwise.
func LoadRules(ctx context.Context, db DB, ids []string, defaults DefaultsSource) (map[string]Rules, error) {
	out := make(map[string]Rules, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if err := loadRows(ctx, db, ids, out); err != nil {
		return nil, err
	}
	if d := defaults.windows(); len(d) > 0 {
		for _, id := range ids {
			r := out[id]
			r.Defaults = d
			out[id] = r
		}
	}
	return out, nil
}

// LoadRulesOf is [LoadRules] for one incarnation: the zero Rules when it has
// neither rows nor a default.
func LoadRulesOf(ctx context.Context, db DB, id string, defaults DefaultsSource) (Rules, error) {
	rules, err := LoadRules(ctx, db, []string{id}, defaults)
	if err != nil {
		return Rules{}, err
	}
	return rules[id], nil
}

func loadRows(ctx context.Context, db DB, ids []string, out map[string]Rules) error {
	rows, err := db.Query(ctx, `SELECT `+windowCols+`
		FROM incarnation_maintenance_windows WHERE incarnation_id = ANY($1::text[])
		ORDER BY created_at, id`, ids)
	if err != nil {
		return fmt.Errorf("maintenance: load windows: %w", err)
	}
	for rows.Next() {
		w, err := scanWindow(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("maintenance: load windows: %w", err)
		}
		r := out[w.IncarnationID]
		r.Windows = append(r.Windows, w)
		out[w.IncarnationID] = r
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("maintenance: load windows: %w", err)
	}

	rows, err = db.Query(ctx, `SELECT `+freezeCols+`
		FROM incarnation_freezes WHERE incarnation_id = ANY($1::text[])
		ORDER BY starts_at, id`, ids)
	if err != nil {
		return fmt.Errorf("maintenance: load freezes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		f, err := scanFreeze(rows)
		if err != nil {
			return fmt.Errorf("maintenance: load freezes: %w", err)
		}
		r := out[f.IncarnationID]
		r.Freezes = append(r.Freezes, f)
		out[f.IncarnationID] = r
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("maintenance: load freezes: %w", err)
	}
	return nil
}

// Gate compiles the calculator the start of one run is held to (ADR-0093 §4,
// §5). A run that asked for a window is held to the windows in effect
// ([Rules.Effective]) and to every freeze: it waits them out, so it needs nobody's consent. A run that did not
// ask is held to no window, and only to the freezes outside consented — the
// ones its operator was never asked about.
func (r Rules) Gate(inWindow bool, consented []string) (*Calculator, error) {
	if inWindow {
		return r.Calculator()
	}
	freezes := make([]Freeze, 0, len(r.Freezes))
	for _, f := range r.Freezes {
		if !slices.Contains(consented, f.ID) {
			freezes = append(freezes, f)
		}
	}
	return NewCalculator(nil, freezes)
}

// FreezesAt returns every freeze covering t, by start then id.
func (c *Calculator) FreezesAt(t time.Time) []Freeze {
	var out []Freeze
	for _, f := range c.freezes {
		if !t.Before(f.StartsAt) && t.Before(f.EndsAt) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b Freeze) int {
		if c := a.StartsAt.Compare(b.StartsAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

const (
	// CommonStartHorizon is how far past the end of the last freeze, or past
	// the latest single start when that is later, [CommonStart] looks for slots
	// that meet. Freezes never exhaust it: the search waits out freezes of any
	// length and any arrangement.
	CommonStartHorizon = 366 * 24 * time.Hour
	// commonStartSteps bounds the search when the calculators' slots do not
	// meet: every step jumps to the next start of one of them, and a run over
	// thousands of incarnations with disjoint windows would otherwise walk the
	// whole horizon one slot at a time, re-asking each calculator at every step.
	commonStartSteps = 1000
)

// lastFreezeEnd returns the end of the calculator's last freeze, zero without one.
func (c *Calculator) lastFreezeEnd() time.Time {
	if n := len(c.frozen); n > 0 {
		return c.frozen[n-1].end
	}
	return time.Time{}
}

// CommonStart returns the first instant at or after t at which every calculator
// is open: the one start a run over several incarnations has. One calculator
// always has one ([Calculator.NextStart]), and so do calculators without
// windows: every step then lands on the end of a freeze, so the search ends.
// False only when slots must meet and do not within [CommonStartHorizon], or
// the search gives up after a thousand steps — the incarnations' windows do not
// meet.
func CommonStart(cals []*Calculator, t time.Time) (time.Time, bool) {
	switch len(cals) {
	case 0:
		return t, true
	case 1:
		return cals[0].NextStart(t), true
	}
	windowed := false
	latest := t
	for _, c := range cals {
		// Asked at t: a calculator without a window that counts at t has none
		// later either.
		windowed = windowed || c.HasWindows(t)
		if s := c.NextStart(t); s.After(latest) {
			latest = s
		}
		if e := c.lastFreezeEnd(); e.After(latest) {
			latest = e
		}
	}
	if !windowed {
		return commonFreezeEnd(cals, t), true
	}
	limit := latest.Add(CommonStartHorizon)
	cand := t
	for range commonStartSteps {
		if cand.After(limit) {
			return time.Time{}, false
		}
		next := cand
		for _, c := range cals {
			if s := c.NextStart(next); s.After(next) {
				next = s
			}
		}
		// Each calculator answered the instant itself, so each is open at it.
		if next.Equal(cand) {
			return cand, true
		}
		cand = next
	}
	return time.Time{}, false
}

// commonFreezeEnd is [CommonStart] over calculators without windows: the first
// instant at or after t that no freeze of any of them covers. Each step moves
// to the end of a freeze strictly later than the last, and freezes are finite,
// so it needs no bound.
func commonFreezeEnd(cals []*Calculator, t time.Time) time.Time {
	cand := t
	for {
		next := cand
		for _, c := range cals {
			if s := c.FreezeEnd(next); s.After(next) {
				next = s
			}
		}
		if next.Equal(cand) {
			return cand
		}
		cand = next
	}
}
