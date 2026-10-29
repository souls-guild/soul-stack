package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// RunGate is what a run request asks of the incarnations' maintenance windows
// and freezes (ADR-0093 §4, §5): the two request fields, unchanged.
type RunGate struct {
	InMaintenanceWindow bool
	ConfirmFreeze       bool
}

// AuditPayload adds both fields to a run's audit payload, so a run started
// during a freeze is findable afterwards (ADR-0093 §5).
func (g RunGate) AuditPayload(p map[string]any) {
	p["in_maintenance_window"] = g.InMaintenanceWindow
	p["confirm_freeze"] = g.ConfirmFreeze
}

// RunStart is the request-time answer for a run over some incarnations.
type RunStart struct {
	// DeferTo — non-zero when the run asked for a window and cannot start at
	// the requested moment: the run waits as a scheduled Voyage until then.
	DeferTo time.Time
	// ConfirmedFreezeIDs — the freezes covering the start that the operator
	// consented to; the Voyage records them so the claim tells them apart from
	// a freeze added later.
	ConfirmedFreezeIDs []string
}

// FrozenIncarnation is one target whose start falls into a freeze.
type FrozenIncarnation struct {
	ID string
	// Freeze is the covering freeze that ends last — the one an operator
	// waiting for the start needs to see.
	Freeze maintenance.Freeze
	// FreezeIDs are every freeze covering the start.
	FreezeIDs []string
}

// FreezeActiveError refuses a request whose start falls into a freeze it did
// not consent to. REST answers it 409 freeze-active, MCP `freeze-active`.
type FreezeActiveError struct {
	Frozen []FrozenIncarnation
	// Act names an act that is never deferred (rerun-last, the upgrade, the
	// destroy): its remedy is consent alone, since it has no
	// in_maintenance_window to wait with. Empty for a run.
	Act string
}

func (e *FreezeActiveError) Error() string { return freezeActiveDetail(e.Frozen, e.Act) }

// NoCommonWindowError refuses a window run whose targets' windows and freezes
// never let all of them start at once. REST answers it 422, MCP
// `validation-failed`.
type NoCommonWindowError struct{ Targets int }

func (e *NoCommonWindowError) Error() string {
	return fmt.Sprintf("in_maintenance_window: the maintenance windows of the %d target incarnations never meet: "+
		"no moment within %d days after their last freeze ends at which all of them may start; run them in separate requests",
		e.Targets, int(maintenance.CommonStartHorizon/(24*time.Hour)))
}

// PlanRunStart applies the start gate to a run request over ids, starting at
// `at` (now, or the request's own schedule_at), before anything is written.
//
// A window run is deferred to the first moment all targets are open, and needs
// no consent: it waits every freeze out. Any other run starts at `at`; when a
// freeze covers `at` on any target it needs confirm_freeze, else the whole
// request is refused naming every frozen target.
func PlanRunStart(ctx context.Context, db maintenance.DB, ids []string, at time.Time, g RunGate) (RunStart, error) {
	rules, err := maintenance.LoadRules(ctx, db, ids)
	if err != nil {
		return RunStart{}, err
	}
	if g.InMaintenanceWindow {
		cals := make([]*maintenance.Calculator, 0, len(rules))
		for _, id := range ids {
			r, ok := rules[id]
			if !ok {
				continue
			}
			c, err := r.Gate(true, nil)
			if err != nil {
				return RunStart{}, fmt.Errorf("maintenance windows of %s: %w", id, err)
			}
			cals = append(cals, c)
		}
		start, ok := maintenance.CommonStart(cals, at)
		if !ok {
			return RunStart{}, &NoCommonWindowError{Targets: len(ids)}
		}
		if start.After(at) {
			return RunStart{DeferTo: start}, nil
		}
		return RunStart{}, nil
	}

	frozen, err := frozenAt(rules, ids, at)
	if err != nil {
		return RunStart{}, err
	}
	if len(frozen) == 0 {
		return RunStart{}, nil
	}
	if !g.ConfirmFreeze {
		return RunStart{}, &FreezeActiveError{Frozen: frozen}
	}
	var consented []string
	for _, f := range frozen {
		consented = append(consented, f.FreezeIDs...)
	}
	slices.Sort(consented)
	return RunStart{ConfirmedFreezeIDs: slices.Compact(consented)}, nil
}

// The acts [CheckFreezeConsent] guards, as its refusal names them.
const (
	FreezeActRerun   = "rerun"
	FreezeActUpgrade = "start the upgrade scenario"
	FreezeActDestroy = "destroy"
)

// CheckFreezeConsent refuses an act on one incarnation that starts now inside
// a freeze without confirm_freeze: rerun-last, the upgrade's scenario and the
// destroy, none of which is deferred (ADR-0093 §4). act words the remedy
// ("rerun", "destroy", …). Called before the act commits anything.
func CheckFreezeConsent(ctx context.Context, db maintenance.DB, id string, now time.Time, confirm bool, act string) error {
	if confirm {
		return nil
	}
	rules, err := maintenance.LoadRules(ctx, db, []string{id})
	if err != nil {
		return err
	}
	frozen, err := frozenAt(rules, []string{id}, now)
	if err != nil {
		return err
	}
	if len(frozen) > 0 {
		return &FreezeActiveError{Frozen: frozen, Act: act}
	}
	return nil
}

func frozenAt(rules map[string]maintenance.Rules, ids []string, at time.Time) ([]FrozenIncarnation, error) {
	var out []FrozenIncarnation
	for _, id := range ids {
		r, ok := rules[id]
		if !ok || len(r.Freezes) == 0 {
			continue
		}
		c, err := r.Gate(false, nil)
		if err != nil {
			return nil, fmt.Errorf("freezes of %s: %w", id, err)
		}
		covering := c.FreezesAt(at)
		if len(covering) == 0 {
			continue
		}
		fids := make([]string, len(covering))
		for i, f := range covering {
			fids[i] = f.ID
		}
		out = append(out, FrozenIncarnation{ID: id, Freeze: *c.ActiveFreeze(at), FreezeIDs: fids})
	}
	return out, nil
}

// DeferredRun is a run of one incarnation that waits for its maintenance
// window as a scheduled Voyage (ADR-0093 §3).
type DeferredRun struct {
	VoyageID   string
	ScheduleAt time.Time
}

// InsertDeferredRun writes the scheduled Voyage a deferred run becomes: one
// target, one Leg, in_maintenance_window set so the claim asks the gate again
// when scheduleAt comes. It is listed, read and cancelled like any Voyage.
func InsertDeferredRun(ctx context.Context, db incarnation.TxBeginner, incID, scenarioName string, input map[string]any, aid string, scheduleAt time.Time) (DeferredRun, error) {
	var inputJSON []byte
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return DeferredRun{}, fmt.Errorf("deferred run: marshal input: %w", err)
		}
		inputJSON = b
	}
	resolved, _ := json.Marshal([]string{incID})
	origin, _ := json.Marshal(voyageTargetRequest{Incarnations: []string{incID}})
	at := scheduleAt.UTC()
	one := 1
	row := &voyage.Voyage{
		VoyageID:            audit.NewULID(),
		Kind:                voyage.KindScenario,
		ScenarioName:        &scenarioName,
		Input:               inputJSON,
		TargetResolved:      resolved,
		TargetOrigin:        origin,
		Concurrency:         &one,
		ScheduleAt:          &at,
		TotalBatches:        1,
		Status:              voyage.StatusScheduled,
		StartedByAID:        aid,
		InMaintenanceWindow: true,
	}

	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return DeferredRun{}, fmt.Errorf("deferred run: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := voyage.Insert(ctx, tx, row); err != nil {
		return DeferredRun{}, fmt.Errorf("deferred run: %w", err)
	}
	if err := voyage.InsertTargets(ctx, tx, row.VoyageID, []voyage.VoyageTarget{{
		TargetKind: voyage.TargetKindIncarnation,
		TargetID:   incID,
		Status:     voyage.TargetStatusAwaiting,
	}}); err != nil {
		return DeferredRun{}, fmt.Errorf("deferred run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return DeferredRun{}, fmt.Errorf("deferred run: commit: %w", err)
	}
	return DeferredRun{VoyageID: row.VoyageID, ScheduleAt: at}, nil
}

// maxFrozenNamed caps the incarnations a freeze-active refusal names per
// freeze, and maxFreezesNamed the freezes it lists: a broad target can resolve
// to the whole max_scope, each incarnation with a freeze of its own.
const (
	maxFrozenNamed  = 10
	maxFreezesNamed = 10
)

// freezeActiveDetail groups the frozen targets by their freeze's end and
// comment — a freeze set on many incarnations at once is one line, not one per
// incarnation.
func freezeActiveDetail(frozen []FrozenIncarnation, act string) string {
	type key struct {
		end     time.Time
		comment string
	}
	groups := map[key][]string{}
	var keys []key
	for _, f := range frozen {
		k := key{f.Freeze.EndsAt.UTC(), f.Freeze.Comment}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], f.ID)
	}
	slices.SortFunc(keys, func(a, b key) int {
		if c := a.end.Compare(b.end); c != 0 {
			return c
		}
		return strings.Compare(a.comment, b.comment)
	})
	parts := make([]string, 0, min(len(keys), maxFreezesNamed)+1)
	for i, k := range keys {
		if i == maxFreezesNamed {
			hidden := 0
			for _, rest := range keys[i:] {
				hidden += len(groups[rest])
			}
			parts = append(parts, "and "+strconv.Itoa(hidden)+" more under "+strconv.Itoa(len(keys)-i)+" more freezes")
			break
		}
		names := groups[k]
		slices.Sort(names)
		shown := names
		if len(shown) > maxFrozenNamed {
			shown = shown[:maxFrozenNamed]
		}
		s := strings.Join(shown, ", ")
		if rest := len(names) - len(shown); rest > 0 {
			s += " and " + strconv.Itoa(rest) + " more"
		}
		s += " frozen until " + k.end.Format(time.RFC3339)
		if k.comment != "" {
			s += " (" + strconv.Quote(k.comment) + ")"
		}
		parts = append(parts, s)
	}
	subject := "incarnation"
	if len(frozen) > 1 {
		subject = strconv.Itoa(len(frozen)) + " target incarnations"
	}
	remedy := "send confirm_freeze: true to run during the freeze, or in_maintenance_window: true to wait it out"
	if act != "" {
		remedy = "send confirm_freeze: true to " + act + " during the freeze"
	}
	return "freeze-active: " + subject + " " + strings.Join(parts, "; ") + "; " + remedy
}

// runGateProblem maps the two refusals of [PlanRunStart] and
// [CheckFreezeConsent] to their problem; ok=false for any other error.
func runGateProblem(err error) (error, bool) {
	var fe *FreezeActiveError
	if errors.As(err, &fe) {
		return incProblem(problem.TypeFreezeActive, fe.Error()), true
	}
	var ne *NoCommonWindowError
	if errors.As(err, &ne) {
		return incProblem(problem.TypeValidationFailed, ne.Error()), true
	}
	return nil, false
}
