package validate

// Offline refusal of an indexed register name (NIM-909): `register["addr"]` /
// `register.hosts["addr"]` instead of `register.addr` / `register.hosts.addr`.
//
// The rule itself lives in shared/cel (register_index.go) and is enforced at
// COMPILE, so a run refuses the expression whatever soul-lint says. What the offline
// half buys is the moment: the author learns at `soul-lint validate-service`, with
// the YAML path to edit, instead of mid-run on the host where the task happened to
// land. Both halves ask the same [cel.Engine] the same question about the same AST —
// including the same [cel.Engine.rewriteHostsWhere] first, without which a
// `.where("register['x'] …")` predicate is a string literal to the walk and the two
// halves disagree on exactly the spelling that hides a register from every other
// reader (the defect an independent review of this ticket found). Where they still
// differ is the message, not the verdict: offline assumes the host accessor is open,
// so on a host task the run answers with the `register.hosts` isolation error first.
//
// ★ It hangs off [computeChecker] deliberately. The rule needs exactly what that
// walk already has — every expression-bearing cell of the scenario, each tagged
// whole-string vs interpolated — and the walk is the part that rots: it must track
// the key set (`on:`/`loop.*`/the four flow-control keys/`where:`/`assert.that[]`/
// `vars:`/`params:`/`apply.input:`/`block:` children/a capture's `match:`) as the DSL
// grows. A second copy of it would be one DSL addition away from covering a
// different set of keys than the first.
//
// Unlike the compute rules the FINDING is scope-independent — the compile-time refusal
// is unconditional, so no cell gets a pass here — and unlike them it reads the scope
// anyway, for the hint alone ([registerIndexHint]).
//
// Not covered offline, same two holes as the compute rules and for the same reasons:
// the destiny entry point lints `destiny.yml`, whose tasks are in a file this rule
// never receives, and tasks spliced in by `include:` are not in
// ScenarioManifest.Tasks. Both are refused by the guard at run time.

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// registerIndex reports one cell if it chooses a register by index. whole says the
// cell is a top-level expression key (the whole string is CEL); otherwise it is an
// interpolated string and only its `${ … }` blocks are expressions.
//
// scope is read for the HINT only — the finding itself is scope-independent. In one
// context the ordinary advice would be unfollowable, and saying it there would repeat
// NIM-619's mistake in a new rule: see [registerIndexHint].
func (c *computeChecker) registerIndex(where, raw string, whole bool, scope cel.ComputeScope) {
	indexed := c.eng.InterpolationIndexesRegisterName(raw)
	if whole {
		indexed = c.eng.ExpressionIndexesRegisterName(raw)
	}
	if !indexed {
		return
	}
	c.out = append(c.out, diag.Diagnostic{
		Level: diag.LevelError,
		Phase: diag.PhaseSemanticValidate,
		File:  c.path,
		Code:  "register_index_form",
		Message: fmt.Sprintf(
			"%q chooses a register by index -- write register.<name> (register.hosts.<name> for the per-host map)",
			raw),
		Hint:     registerIndexHint(scope),
		YAMLPath: where,
	})
}

// registerIndexHint — the fix, and the one context where the fix is different.
//
// A capture's `match:`/`patch:` predicate is evaluated AGAIN per collection element at
// merge time, against `elem`/`value` and nothing else (`render.Pipeline.StateOpEvaluators`
// builds `cel.Vars` with no Register at all — the only such context this walk visits).
// There `register.<name>` is no better than `register["name"]`: the root is empty either
// way, and telling the author to rewrite the spelling would send them to a second
// no-such-key. The route that works is a `${ … }` cell, which the params render
// substitutes with the register in scope before merge ever reads the text.
func registerIndexHint(scope cel.ComputeScope) string {
	if scope == cel.ComputeOutOfScopeStateMatch {
		return "a BARE predicate here is re-evaluated per collection element against elem/value " +
			"only -- there is no register root, whichever spelling you use. Put the read in a " +
			"${ register.<name>.<field> } cell instead: the params render substitutes it before " +
			"merge reads the predicate"
	}
	return "the index form is invisible to reference extraction, so nothing declares the " +
		"dependency on the producing task: it neither orders the run nor is checked for " +
		"existence. An index INTO a register is fine -- register.<name>[\"key\"] and " +
		"register.hosts.<name>[\"<sid>\"] both name the register first"
}
