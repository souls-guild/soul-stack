package cel

import (
	"errors"
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestMaintenanceWindow_AnswersTheStateItIsGiven(t *testing.T) {
	builders := map[string]func(...Option) (*Engine, error){
		"render":        New,
		"scenario vars": NewScenarioVars,
		"keeper when": func(opts ...Option) (*Engine, error) {
			return NewFlowControl(append(opts, WithMaintenanceWindow())...)
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			e, err := build()
			if err != nil {
				t.Fatal(err)
			}
			for _, open := range []bool{true, false} {
				got, err := e.EvalPredicate("in_maintenance_window()", Vars{InMaintenanceWindow: boolPtr(open)})
				if err != nil {
					t.Fatalf("open=%v: %v", open, err)
				}
				if got != open {
					t.Fatalf("open=%v: got %v", open, got)
				}
			}
		})
	}
}

// A context that carries no state must not read as "no windows": a step gated on
// the window would then run outside it.
func TestMaintenanceWindow_NoStateFailsTheCall(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.EvalPredicate("in_maintenance_window()", Vars{})
	var evalErr *ErrEval
	if !errors.As(err, &evalErr) || !strings.Contains(err.Error(), "no maintenance-window state") {
		t.Fatalf("want an evaluation error naming the missing state, got %v", err)
	}
}

// The compile cache keys on the text, so one program serves every pass: the answer
// must come from the activation, not from the first evaluation.
func TestMaintenanceWindow_CachedProgramReadsEachPassState(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for i, open := range []bool{true, false, true} {
		got, err := e.EvalInterpolation("${ in_maintenance_window() }", Vars{InMaintenanceWindow: boolPtr(open)})
		if err != nil {
			t.Fatal(err)
		}
		if got != open {
			t.Fatalf("pass %d: got %v, want %v", i, got, open)
		}
	}
	if n := e.CachedPrograms(); n != 1 {
		t.Fatalf("want one cached program, got %d", n)
	}
}

func TestMaintenanceWindow_RefusedWhereNotEvaluatedPerStep(t *testing.T) {
	cases := map[string]struct {
		build func(...Option) (*Engine, error)
		want  string
	}{
		"soul flow control": {NewFlowControl, MaintenanceSoulSideFeature},
		"service vars":      {NewServiceVars, maintenanceFeatureServiceVars},
		"stack selector":    {NewServiceVarsSelector, maintenanceFeatureServiceVars},
		"migration":         {NewMigration, maintenanceFeatureMigration},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			for _, expr := range []string{"in_maintenance_window()", "in_maintenance_window( ) && ("} {
				_, err = e.EvalExpression(expr, Vars{InMaintenanceWindow: boolPtr(true)})
				var unsupported *ErrUnsupported
				if !errors.As(err, &unsupported) || unsupported.Feature != tc.want {
					t.Fatalf("%q: want ErrUnsupported %q, got %v", expr, tc.want, err)
				}
			}
		})
	}
}

func TestMaintenanceWindow_OptionRefusedOutsideFlowControl(t *testing.T) {
	for name, build := range map[string]func(...Option) (*Engine, error){
		"migration":    NewMigration,
		"service vars": NewServiceVars,
	} {
		if _, err := build(WithMaintenanceWindow()); err == nil {
			t.Fatalf("%s: WithMaintenanceWindow accepted", name)
		}
	}
}

func TestMaintenanceWindow_ReservedIdentifierNotWritable(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvalPredicate(maintenanceOpenVar, Vars{InMaintenanceWindow: boolPtr(true)}); err == nil {
		t.Fatalf("%s evaluated as an author expression", maintenanceOpenVar)
	}
}

// RenderPredicate emits text for the Soul, so it is judged by the Soul's environment
// alone: the Keeper's when: engine is refused, and the function in a predicate the Soul
// evaluates is refused by name.
func TestMaintenanceWindow_RenderPredicateIsTheSoulsEnvironment(t *testing.T) {
	keeper, err := NewFlowControl(WithMaintenanceWindow())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keeper.RenderPredicate("register.self.changed", Vars{}, []string{RegisterSelf}); err == nil {
		t.Fatal("RenderPredicate accepted the Keeper's when: engine")
	}
	soul := newFlowControlEngine(t)
	_, err = soul.RenderPredicate("register.self.changed && in_maintenance_window()", Vars{InMaintenanceWindow: boolPtr(true)}, []string{RegisterSelf})
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) || unsupported.Feature != MaintenanceSoulSideFeature {
		t.Fatalf("want ErrUnsupported naming the Soul-side predicate, got %v", err)
	}
}
