package cel

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/parser"
)

// in_maintenance_window() ([ADR-0093] §8, NIM-1073) answers whether the incarnation's
// maintenance window is open at the instant the render pass describes: inside a slot
// of some window, or there are no windows, AND outside every freeze. It is the first
// time-dependent function of render CEL, and it is kept deterministic by never reading
// a clock: the caller samples the instant once per pass, asks the calculator, and hands
// the answer in as [Vars.InMaintenanceWindow]. The function only reads it back, so the
// same Vars give the same answer however often a pass evaluates them.
//
// A macro, like vault(): the author's zero-argument call becomes the reserved
// identifier [maintenanceOpenVar], which the author cannot write (the `__` guard runs
// before expansion).
//
// [ADR-0093]: docs/adr/0093-maintenance-window.md

// MaintenanceWindowFunc is the function's name as an author writes it.
const MaintenanceWindowFunc = "in_maintenance_window"

// maintenanceOpenVar is the activation variable the macro expands to.
const maintenanceOpenVar = "__in_maintenance_window"

// Why each environment without the function refuses it. Each names where the function
// does exist, since that is the author's way out.
const (
	// MaintenanceSoulSideFeature — a predicate evaluated after the step's render, or on
	// the Soul: the [ErrUnsupported] feature of the Soul's flow-control Engine, and the
	// one the Keeper refuses such a predicate with before sending it.
	MaintenanceSoulSideFeature    = "in_maintenance_window() (not in a predicate evaluated after the step's render or on the Soul - changed_when:, failed_when:, retry.until:, a bare capture match:, a destiny's predicates, a grouped step's when: reading a sibling's register, a host task's when: under execution_model: staged; it answers for the moment the Keeper renders the step: params, where:, vars:, apply: input:, loop:, assert, and a when: the Keeper decides)"
	maintenanceFeatureServiceVars = "in_maintenance_window() (service vars/ resolve once per run, and the window is read per step: move the definition to the scenario's vars: or to the step)"
	maintenanceFeatureMigration   = "in_maintenance_window() (migration-CEL is a pure function of state)"
)

// CallsMaintenanceWindow reports whether expr calls in_maintenance_window(), in its
// `.where` predicates included, by the parsed tree: a string literal holding the name
// is data. Text that does not parse is read by the pattern.
func (e *Engine) CallsMaintenanceWindow(expr string) bool {
	called, ok := e.calledFunctions(normalize(expr))
	if !ok {
		return maintenanceGuard.MatchString(expr)
	}
	return called[MaintenanceWindowFunc]
}

// WithMaintenanceWindow registers in_maintenance_window() in a flow-control Engine
// ([NewFlowControl]). For the Keeper's when: engine only: a when: the Keeper decides
// may call it, while the Soul's engine is built without it, so a predicate that reaches
// a Soul and calls it is refused there at compile rather than answered. The ordinary
// and scenario-vars Engines have it unconditionally; migration and service-vars
// Engines refuse the option.
func WithMaintenanceWindow() Option {
	return func(c *engineConfig) { c.maintenance = true }
}

func maintenanceEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Variable(maintenanceOpenVar, cel.BoolType),
		cel.Macros(parser.NewGlobalMacro(MaintenanceWindowFunc, 0, expandMaintenanceMacro)),
	}
}

func expandMaintenanceMacro(mef parser.ExprHelper, _ ast.Expr, _ []ast.Expr) (ast.Expr, *common.Error) {
	return mef.NewIdent(maintenanceOpenVar), nil
}

// maintenanceOpenValue is what the macro's identifier resolves to. A context that
// carries no answer fails the call rather than answering true: "no windows" is an
// answer of its own (open), and a context that forgot to pass the state must not read
// as one — a disruptive step gated on the window would run outside it.
func maintenanceOpenValue(open *bool) ref.Val {
	if open == nil {
		return types.NewErr("in_maintenance_window(): no maintenance-window state here (the context does not read the window, or the windows could not be read for this pass)")
	}
	return types.Bool(*open)
}
