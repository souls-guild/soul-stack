package scenario

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// maintenanceAt is the incarnation's maintenance window for the render pass about to
// start (ADR-0093 §8): its windows and freezes read again, with the cluster default,
// and asked about now. The run
// asks before every pass, so a freeze added mid-run reaches the next step. A failed
// read does not abort the run: the pass gets no window, and only a cell that reads it
// fails — a run that reads none is not stopped by a table it never needed.
func maintenanceAt(ctx context.Context, db maintenance.DB, defaults maintenance.DefaultsSource, incarnationID string, log *slog.Logger) *render.MaintenanceWindow {
	c, err := maintenance.Load(ctx, db, incarnationID, defaults)
	if err != nil {
		log.Warn("scenario: maintenance windows not read, the render pass has no window",
			slog.String("incarnation", incarnationID), slog.Any("error", err))
		return nil
	}
	mw := render.MaintenanceAt(c, time.Now())
	return &mw
}

// preflightMaintenance is the window a pre-flight assert reads, asked about the
// request's instant. No row yet (the create path) is no windows of its own, so the
// cluster default alone; [Runner.preflightIncarnation] returns a row only when the
// Runner has a database.
func (r *Runner) preflightMaintenance(ctx context.Context, inc *incarnation.Incarnation) *render.MaintenanceWindow {
	if inc == nil {
		c, err := r.deps.MaintenanceDefaults.Unrecorded().Calculator()
		if err != nil {
			r.logger.Warn("scenario: the default maintenance windows do not compile, the pre-flight has no window",
				slog.Any("error", err))
			return nil
		}
		mw := render.MaintenanceAt(c, time.Now())
		return &mw
	}
	return maintenanceAt(ctx, r.deps.DB, r.deps.MaintenanceDefaults, inc.ID, r.logger)
}

// recipeMaintenance is the pass's window as a planned row carries it to the claim,
// which renders with it instead of asking the clock again (ADR-027 amend (p)).
func recipeMaintenance(mw *render.MaintenanceWindow) *applyrun.RecipeMaintenance {
	if mw == nil {
		return nil
	}
	return &applyrun.RecipeMaintenance{Open: mw.Open, Text: mw.Text, NextStart: mw.NextStart}
}

// maintenanceFromRecipe is [recipeMaintenance] read back. nil stays nil: the run's
// pass had no window, so the claim's has none either.
func maintenanceFromRecipe(rec *applyrun.RecipeMaintenance) *render.MaintenanceWindow {
	if rec == nil {
		return nil
	}
	return &render.MaintenanceWindow{Open: rec.Open, Text: rec.Text, NextStart: rec.NextStart}
}

// maintenanceByStepFromRecipe is the recipe's earlier steps' windows read back.
func maintenanceByStepFromRecipe(rec map[int]*applyrun.RecipeMaintenance) map[int]*render.MaintenanceWindow {
	if rec == nil {
		return nil
	}
	out := make(map[int]*render.MaintenanceWindow, len(rec))
	for step, mw := range rec {
		out[step] = maintenanceFromRecipe(mw)
	}
	return out
}
