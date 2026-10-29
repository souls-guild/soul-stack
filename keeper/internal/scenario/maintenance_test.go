package scenario

import (
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// A claim renders a step with the window the run's pass had (ADR-0093 §8), carried
// through the row's recipe: asking again could tell it the window has since closed,
// and it would send what the run did not decide.
//
// Mutation: drop `rec.Maintenance = recipeMaintenance(in.Maintenance)` from stepRecipe
// → the claim reads no window.
func TestMaintenance_StepRecipeCarriesThePassWindow(t *testing.T) {
	mw := render.MaintenanceWindow{Open: false, Text: "mon 02:00 for 3h (Europe/Moscow)", NextStart: time.Date(2026, time.November, 8, 23, 0, 0, 0, time.UTC)}
	b, err := applyrun.MarshalRecipe(stepRecipe(RunSpec{}, "abc", render.RenderInput{Maintenance: &mw}, nil))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := applyrun.UnmarshalRecipe(b)
	if err != nil {
		t.Fatal(err)
	}
	got := maintenanceFromRecipe(rec.Maintenance)
	if got == nil || *got != mw {
		t.Fatalf("claim window = %+v, want %+v", got, mw)
	}

	b, err = applyrun.MarshalRecipe(stepRecipe(RunSpec{}, "abc", render.RenderInput{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if rec, err = applyrun.UnmarshalRecipe(b); err != nil || maintenanceFromRecipe(rec.Maintenance) != nil {
		t.Fatalf("a pass without a window: claim window = %+v (err %v), want none", rec.Maintenance, err)
	}
}

// The claim of step 1 re-renders step 0 too, with step 0's window: the recipe carries
// the earlier steps' answers, and only those.
//
// Mutation: drop the MaintenanceByStep copy from stepRecipe.
func TestMaintenance_StepRecipeCarriesEarlierStepsWindows(t *testing.T) {
	earlier := render.MaintenanceWindow{Open: true, NextStart: time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC)}
	now := render.MaintenanceWindow{Open: false, NextStart: time.Date(2026, time.November, 8, 23, 0, 0, 0, time.UTC)}
	in := render.RenderInput{Maintenance: &now, MaintenanceByStep: map[int]*render.MaintenanceWindow{0: &earlier, 1: &now}, ActivePassage: 1}
	b, err := applyrun.MarshalRecipe(stepRecipe(RunSpec{}, "abc", in, nil))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := applyrun.UnmarshalRecipe(b)
	if err != nil {
		t.Fatal(err)
	}
	got := maintenanceByStepFromRecipe(rec.MaintenanceByStep)
	if len(got) != 1 || got[0] == nil || *got[0] != earlier {
		t.Fatalf("claim's earlier windows = %+v, want only step 0's", got)
	}
}
