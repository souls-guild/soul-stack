package conductor

import (
	"context"
	"slices"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// noWindowListMax caps the incarnations one cadence.skipped_no_window names.
const noWindowListMax = 20

// noWindow is why an in_maintenance_window spawn has no start: how many
// incarnations it targets, and for the first [noWindowListMax] of them by name
// when each opens on its own, or why its windows no longer compile.
type noWindow struct {
	incarnations int
	rows         []map[string]any
}

// spawnStart is when a scenario Cadence's child may start (ADR-0093 §4), asked
// of the gate the claim asks of the same row (voyage.MaintenanceGate), so the
// tick and the claim cannot disagree. With in_maintenance_window that is the
// first moment every target is open; without, the first moment no freeze of
// any target covers: the row records no consent, and a Cadence has no way to
// give one. A zero time is now. A non-nil noWindow means the targets share no
// moment at all, which the claim would answer by failing the row.
//
// incarnations are row's targets (cadence.BuildVoyage marshals them into
// TargetResolved), so the rules are loaded once for the gate and the skip.
func spawnStart(ctx context.Context, db voyage.ExecQueryRower, row *voyage.Voyage, incarnations []string, now time.Time) (time.Time, *noWindow, error) {
	rules, err := maintenance.LoadRules(ctx, db, incarnations)
	if err != nil {
		return time.Time{}, nil, err
	}
	verdict := voyage.MaintenanceGate{}.DecideRules(rules, incarnations, row, now)
	if !verdict.NoStart {
		return verdict.DeferTo, nil, nil
	}
	return time.Time{}, describeNoWindow(rules, len(incarnations), now), nil
}

// describeNoWindow lists, per incarnation, when it opens on its own. Only
// incarnations with windows or freezes are listed: one with neither is always
// open and never the reason. Broken ones come first: one of them alone explains
// the skip, and the cap must not hide it behind twenty that are merely closed.
func describeNoWindow(rules map[string]maintenance.Rules, incarnations int, now time.Time) *noWindow {
	names := make([]string, 0, len(rules))
	for inc := range rules {
		names = append(names, inc)
	}
	slices.Sort(names)
	calcs := make([]*maintenance.Calculator, len(names))
	errs := make([]error, len(names))
	for i, inc := range names {
		calcs[i], errs[i] = rules[inc].Gate(true, nil)
	}
	nw := &noWindow{incarnations: incarnations}
	for _, wantBroken := range []bool{true, false} {
		for i, inc := range names {
			if len(nw.rows) == noWindowListMax {
				return nw
			}
			if (errs[i] != nil) != wantBroken {
				continue
			}
			row := map[string]any{"incarnation": inc}
			if errs[i] != nil {
				row["error"] = errs[i].Error()
			} else {
				row["next_start"] = calcs[i].NextStart(now)
			}
			nw.rows = append(nw.rows, row)
		}
	}
	return nw
}
