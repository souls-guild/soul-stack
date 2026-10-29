package validate

// in_maintenance_window() offline (ADR-0093 §8, NIM-1073). The function answers for
// the moment the Keeper renders a step, so it is refused where an expression is
// evaluated later or by the Soul — the same refusal the Keeper makes at render, here
// before the scenario reaches it:
//
//   - `changed_when:`, `failed_when:` and `retry.until:`, which run after the step's
//     module, retries included;
//   - a capture's `match:`, evaluated per collection element at merge;
//   - every predicate of a destiny's task, which travels whole and is evaluated on
//     the Soul (ADR-0091, decision 6);
//   - a `compute:` entry, `${ }` cell or not: the block resolves once per pass for
//     every step it renders, the steps that already ran included.
//
// A scenario step's `when:` is accepted: under the default linear dispatch the Keeper
// decides it. Under `execution_model: staged` the Keeper refuses it at render, and a
// grouped step's `when:` reading a sibling's register fails its hosts there; neither
// is visible in the file. A service `vars/` definition calling it is reported by the
// vars check, whose environment refuses the function.

import (
	"fmt"
	"strings"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/servicevars"
)

const codeMaintenanceWindowOutOfScope = "maintenance_window_out_of_scope"

const maintenanceWindowHint = "in_maintenance_window() answers for the moment the Keeper renders the step: " +
	"gate the step itself with `when: in_maintenance_window()` in the scenario"

// lateMaintenance reports in_maintenance_window() in expr, a predicate evaluated
// after the step's render; context says when it is evaluated.
func (c *computeChecker) lateMaintenance(where, expr, context string) {
	if d, ok := maintenanceWindowDiag(c.eng, c.path, where, expr, expr, context); ok {
		c.out = append(c.out, d)
	}
}

// lateMaintenanceMatch is [computeChecker.lateMaintenance] for a capture's `match:`,
// whose `${ }` cells the params render substitutes at the step: only the text around
// them is evaluated at merge.
func (c *computeChecker) lateMaintenanceMatch(where, raw string) {
	residual := raw
	if segs, ok := servicevars.SplitInterpolation(raw); ok {
		var b strings.Builder
		for _, s := range segs {
			if s.Expr {
				b.WriteString(" true ")
			} else {
				b.WriteString(s.Text)
			}
		}
		residual = b.String()
	}
	if d, ok := maintenanceWindowDiag(c.eng, c.path, where, residual, raw, maintenanceLateMatch); ok {
		c.out = append(c.out, d)
	}
}

// maintenanceWindowDiag reports a call in check, quoting quote — the author's text.
func maintenanceWindowDiag(eng *cel.Engine, path, where, check, quote, context string) (diag.Diagnostic, bool) {
	if check == "" || !eng.CallsMaintenanceWindow(check) {
		return diag.Diagnostic{}, false
	}
	expr := quote
	return diag.Diagnostic{
		Level:    diag.LevelError,
		Phase:    diag.PhaseSemanticValidate,
		File:     path,
		Code:     codeMaintenanceWindowOutOfScope,
		Message:  fmt.Sprintf("%q calls in_maintenance_window(), but %s", expr, context),
		Hint:     maintenanceWindowHint,
		YAMLPath: where,
	}, true
}

// computeMaintenance reports a `compute:` entry that calls the function or reads
// `incarnation.maintenance_window`.
func (c *computeChecker) computeMaintenance(where, raw string) {
	reads := c.eng.InterpolationReads(raw, []string{"incarnation"})["incarnation"]
	if !c.eng.CallsMaintenanceWindow(raw) && !reads.Fields["maintenance_window"] {
		return
	}
	c.out = append(c.out, diag.Diagnostic{
		Level:    diag.LevelError,
		Phase:    diag.PhaseSemanticValidate,
		File:     c.path,
		Code:     codeMaintenanceWindowOutOfScope,
		Message:  fmt.Sprintf("%q reads the maintenance window, but compute: resolves once per pass for every step, the steps that already ran included", raw),
		Hint:     "read the window in the step that needs it: each step keeps the answer it ran with",
		YAMLPath: where,
	})
}

// Why each predicate is evaluated too late.
const (
	maintenanceLatePostModule = "this predicate runs after the step's module, later than the moment the window was read"
	maintenanceLateMatch      = "a capture's match: is evaluated per collection element at merge, with no window"
	maintenanceLateDestiny    = "a destiny's predicates are evaluated on the Soul, which has no window"
)

// destinyMaintenanceDiagnostics walks a destiny's tasks, blocks included: every
// predicate there is the Soul's.
func destinyMaintenanceDiagnostics(path string, tasks []config.Task) []diag.Diagnostic {
	eng := computeScopeEngine()
	if eng == nil {
		return nil
	}
	var out []diag.Diagnostic
	var walk func(tasks []config.Task, prefix string)
	walk = func(tasks []config.Task, prefix string) {
		for i := range tasks {
			t := &tasks[i]
			where := fmt.Sprintf("%s[%d]", prefix, i)
			preds := map[string]string{"when": t.When, "changed_when": t.ChangedWhen, "failed_when": t.FailedWhen}
			if t.Retry != nil {
				preds["retry.until"] = t.Retry.Until
			}
			for _, key := range []string{"when", "changed_when", "failed_when", "retry.until"} {
				if d, ok := maintenanceWindowDiag(eng, path, where+"."+key, preds[key], preds[key], maintenanceLateDestiny); ok {
					out = append(out, d)
				}
			}
			if t.Block != nil {
				walk(t.Block.Block, where+".block")
			}
		}
	}
	walk(tasks, "$")
	return out
}
