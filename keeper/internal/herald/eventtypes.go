package herald

import (
	"fmt"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// runScopeAreas is the closed list of RUN event areas that Tiding may subscribe
// to in the MVP (ADR-052(b), run events only). area-globs (`scenario_run.*`) and
// exact types (`command_run.invoked`) are validated against this set: host beacon
// events (Portent/Oracle), registry CRUD audit, and other keeper-internal logs are
// out of scope.
//
// This is a closed list, not the full audit.EventType catalog, because ADR-052(b)
// covers run areas rather than any known audit type. Scope expansion requires an
// ADR-052 amendment and updating this set.
var runScopeAreas = map[string]struct{}{
	"scenario_run": {},
	"command_run":  {},
	"voyage":       {},
	"cadence":      {},
}

// runScopePointEvents are exact event types outside area-glob areas that are
// explicitly in run scope (ADR-052(b)). `incarnation.*` as a whole is not in scope
// because the area carries incarnation CRUD/lifecycle events, but selected run
// events are allowed:
//   - drift_checked is a Scry run event (ADR-031);
//   - run_completed is a terminal scenario-run for one incarnation (ADR-052(k),
//     status in {success, failed}): the carrier for T4a/T4b subscriptions (task
//     alerts and scheduled-run notifications). The incarnation.* area remains out
//     of scope as CRUD noise; only this exact type is open.
var runScopePointEvents = map[string]struct{}{
	"incarnation.drift_checked":                {},
	string(audit.EventIncarnationRunCompleted): {},
}

// heraldArea is the area for Herald's own delivery terminals (`herald.delivered` /
// `herald.failed`, ADR-052(d)). Keeping it as a constant gives the dispatcher
// loop guard ([isHeraldOwnEvent]) and future expansion one shared point.
const heraldArea = "herald"

// isHeraldOwnEvent is true when the event belongs to Herald's own delivery area
// (`herald.*`). Loop guard: delivery terminals themselves flow through
// audit-writer -> tap, so subscribing to them would create a notification about a
// notification and could loop forever during a storm.
//
// Defence in depth: subscribing to `herald.*` is already impossible at the CRUD
// layer ([validateEventType] rejects areas outside runScopeAreas), but this guard
// filters the event before rule matching even if the scope expands later.
func isHeraldOwnEvent(et audit.EventType) bool {
	return eventArea(et) == heraldArea
}

// ValidateEventTypes validates a Tiding subscription list (ADR-052(b)). The list
// must be non-empty (mirrors CHECK tidings_event_types_nonempty for defence in
// depth and a friendly pre-DB error). Each element is either an area-glob
// `<area>.*` with a run-scope `<area>`, an exact `<area>.<action>` with that same
// `<area>`, or an explicitly allowed point type (`incarnation.drift_checked`).
// Everything else (bare wildcard `*`, unknown area, point type outside scope) is
// rejected.
func ValidateEventTypes(eventTypes []string) error {
	if len(eventTypes) == 0 {
		return fmt.Errorf("herald: tiding event_types must be non-empty")
	}
	for _, et := range eventTypes {
		if err := validateEventType(et); err != nil {
			return err
		}
	}
	return nil
}

func validateEventType(et string) error {
	if et == "" {
		return fmt.Errorf("herald: empty event_type")
	}
	if et == "*" || strings.HasPrefix(et, "*") {
		return fmt.Errorf("herald: bare wildcard %q not allowed (use area-glob like scenario_run.*)", et)
	}

	dot := strings.IndexByte(et, '.')
	if dot <= 0 {
		return fmt.Errorf("herald: invalid event_type %q (expected <area>.<action> or <area>.*)", et)
	}
	area := et[:dot]
	rest := et[dot+1:]

	// area-glob `<area>.*`: whole area, run scope only.
	if rest == "*" {
		if _, ok := runScopeAreas[area]; !ok {
			return fmt.Errorf("herald: event_type area %q is out of run-scope (allowed: scenario_run/command_run/voyage/cadence)", area)
		}
		return nil
	}
	if strings.Contains(rest, "*") {
		return fmt.Errorf("herald: invalid event_type %q (wildcard only as whole-area suffix `.*`)", et)
	}

	// Exact type: either from a scoped area or an explicitly allowed point type.
	if _, ok := runScopeAreas[area]; ok {
		return nil
	}
	if _, ok := runScopePointEvents[et]; ok {
		return nil
	}
	return fmt.Errorf("herald: event_type %q is out of run-scope (allowed areas: scenario_run/command_run/voyage/cadence; point: %s)", et, pointEventsList())
}

// pointEventsList returns the sorted list of allowed point event types
// (runScopePointEvents) for error text. It is map-driven so the message stays in
// sync with the actual allowed set when it grows.
func pointEventsList() string {
	out := make([]string, 0, len(runScopePointEvents))
	for et := range runScopePointEvents {
		out = append(out, et)
	}
	sort.Strings(out)
	return strings.Join(out, "/")
}

// RunScopeAreas returns sorted run area names (without the `.*` suffix) allowed
// for Tiding area-glob subscriptions (ADR-052(b)). The source of truth for
// `GET /v1/event-types` (EventTypeCatalogHandler) is the same runScopeAreas used
// by CRUD validation, so the catalog and validator cannot diverge.
func RunScopeAreas() []string {
	out := make([]string, 0, len(runScopeAreas))
	for area := range runScopeAreas {
		out = append(out, area)
	}
	sort.Strings(out)
	return out
}

// RunScopePointEvents returns sorted point event types outside area-globs
// (`incarnation.drift_checked`/`incarnation.run_completed`) allowed for whole
// Tiding subscriptions (ADR-052(b)). It is the source of truth for the
// `GET /v1/event-types` catalog.
func RunScopePointEvents() []string {
	out := make([]string, 0, len(runScopePointEvents))
	for et := range runScopePointEvents {
		out = append(out, et)
	}
	sort.Strings(out)
	return out
}
