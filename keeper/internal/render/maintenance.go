package render

import (
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// MaintenanceCalculator is the incarnation's maintenance windows and freezes as render
// reads them — [maintenance.Calculator] satisfies it ([ADR-0093] §8). Pure: every
// method takes the instant it is asked about.
//
// [maintenance.Calculator]: ../maintenance/calculator.go
// [ADR-0093]: ../../../docs/adr/0093-maintenance-window.md
type MaintenanceCalculator interface {
	OpenAt(t time.Time) bool
	NextStart(t time.Time) time.Time
	Text(t time.Time) string
	HasWindows(t time.Time) bool
}

// MaintenanceWindow is the incarnation's maintenance window as one render pass reads
// it: `in_maintenance_window()`, `incarnation.maintenance_window.text` and `.next_start`.
// It is asked once, for one instant, by whoever starts the pass, so every host and
// every task of the pass — the destiny passes it opens included — gets one answer, and
// nothing in the render reads a clock. A pass is one step of a run, which is the
// moment the ADR's "now" names; a claim that renders the step again is handed the same
// value through its recipe rather than asking again.
type MaintenanceWindow struct {
	Open bool
	// Text — the windows that count at the pass's instant for a human, "" when
	// there are none.
	Text string
	// NextStart — the first instant at or after the pass's at which a run may start:
	// that instant itself when Open.
	NextStart time.Time
}

// MaintenanceAt is c's answer at at, cut to the second so `next_start` of an open
// window is exactly the instant it was asked about. nil c — no windows and no freezes:
// open, no text, next start now.
func MaintenanceAt(c MaintenanceCalculator, at time.Time) MaintenanceWindow {
	at = at.UTC().Truncate(time.Second)
	if c == nil {
		return MaintenanceWindow{Open: true, NextStart: at}
	}
	mw := MaintenanceWindow{Open: c.OpenAt(at), NextStart: c.NextStart(at).UTC()}
	if c.HasWindows(at) {
		mw.Text = c.Text(at)
	}
	return mw
}

// maintenanceFor is the window a task of step renders with — a top-level task's, or a
// default block's child, which is a step of its own: the one it was rendered with when
// it ran, if it ran before this pass, else the pass's.
func (in RenderInput) maintenanceFor(step int) *MaintenanceWindow {
	if in.TaskPassage != nil && step < in.ActivePassage {
		if mw, ok := in.MaintenanceByStep[step]; ok {
			return mw
		}
	}
	return in.passMaintenance
}

// refuseLateMaintenanceReads refuses in_maintenance_window() in a predicate evaluated
// after the step's render: `changed_when:`, `failed_when:` and `retry.until:` run after
// the module, retries included, and a `when:` the Soul evaluates (soulWhen; never a
// keeper task's, an assert's or an applier's, which the Keeper decides and sends no
// Soul) is not the Keeper's to answer. The answer is the render's, so read
// there it would describe a moment already gone. It reads the plan's own text, blocks
// included, so a step not yet rendered — a placeholder carries no predicate — is
// judged too, and the refusal comes before the first step is sent. A grouped step's
// `when:` reading a sibling's register is the one Soul-side case the text alone does
// not show; it fails its hosts by name where the Keeper renders it ([settleHostFlow]).
func (p *Pipeline) refuseLateMaintenanceReads(tasks []config.Task, soulWhen bool) error {
	for _, t := range tasks {
		preds := []struct{ kind, expr string }{
			{"changed_when", t.ChangedWhen}, {"failed_when", t.FailedWhen},
		}
		if t.Retry != nil {
			preds = append(preds, struct{ kind, expr string }{"until", t.Retry.Until})
		}
		if soulWhen && !IsKeeperTask(t) && !IsAssertTask(t) && t.Apply == nil {
			preds = append(preds, struct{ kind, expr string }{predicateWhen, t.When})
		}
		for _, pr := range preds {
			if pr.expr != "" && p.cel.CallsMaintenanceWindow(pr.expr) {
				return fmt.Errorf("render: task %q: %s: %w", t.Name, pr.kind,
					&cel.ErrUnsupported{Expr: pr.expr, Feature: cel.MaintenanceSoulSideFeature})
			}
		}
		if t.Block != nil {
			if err := p.refuseLateMaintenanceReads(t.Block.Block, soulWhen); err != nil {
				return err
			}
		}
	}
	return nil
}

// computeMaintenanceFeature is why `compute:` has no window.
const computeMaintenanceFeature = "in_maintenance_window() / incarnation.maintenance_window (compute: resolves once per pass for every step it renders, the steps that already ran included, while each step keeps its own window: read the window in the step)"

// readsMaintenanceWindow reports whether an interpolated cell calls the function or
// names `incarnation.maintenance_window`.
func (p *Pipeline) readsMaintenanceWindow(raw string) bool {
	if p.cel.CallsMaintenanceWindow(raw) {
		return true
	}
	return p.cel.InterpolationReads(raw, []string{"incarnation"})["incarnation"].Fields[maintenanceWindowKey]
}

// predicateReadsMaintenanceWindow is [Pipeline.readsMaintenanceWindow] for a bare
// predicate: what the step decides with its own window, and so nothing to decide ahead
// of it (the up-front capability survey).
func (p *Pipeline) predicateReadsMaintenanceWindow(expr string) bool {
	if expr == "" {
		return false
	}
	if p.cel.CallsMaintenanceWindow(expr) {
		return true
	}
	return p.cel.PredicateReads(expr, []string{"incarnation"})["incarnation"].Fields[maintenanceWindowKey]
}

// maintenanceWindowKey is the `incarnation.maintenance_window` field.
const maintenanceWindowKey = "maintenance_window"

// inMaintenanceWindow is the pass's answer for [cel.Vars.InMaintenanceWindow]; nil
// when the pass has none, which makes a call fail rather than answer.
func (in RenderInput) inMaintenanceWindow() *bool {
	if in.Maintenance == nil {
		return nil
	}
	open := in.Maintenance.Open
	return &open
}

// withMaintenanceWindow adds `incarnation.maintenance_window` to an incarnation map
// built for a CEL context: plain strings, so it travels in flow_context and into a
// rendered predicate like any other incarnation field. Absent when the pass has none,
// like the function.
func (in RenderInput) withMaintenanceWindow(inc map[string]any) map[string]any {
	if in.Maintenance != nil {
		inc[maintenanceWindowKey] = map[string]any{
			"text":       in.Maintenance.Text,
			"next_start": in.Maintenance.NextStart.UTC().Format(time.RFC3339),
		}
	}
	return inc
}
