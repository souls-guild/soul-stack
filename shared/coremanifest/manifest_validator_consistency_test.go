// Invariant P5 — consistency between a core module's embedded manifest and what
// the task `params:` validator actually checks (shared/config).
//
// Why a separate `coremanifest_test` package rather than an internal test:
// production code `shared/config` imports `shared/coremanifest`
// (config/module_params.go pulls the schema via coremanifest.Default()). An
// internal `package coremanifest` test cannot import config — import cycle. An
// external test package is not part of the production import graph, so here both
// coremanifest and config may be imported at once. The file lives physically in
// shared/coremanifest/; production code is untouched.
//
// Architecture note (key to what the test guards): the `params:` validator has NO
// own source of truth for the param set/required/types — validateModuleParams
// reads def.Input straight from coremanifest.State. So "param names in the manifest
// == names known to the validator" currently holds BY CONSTRUCTION (one source).
// TestP5_* pin this as a regression guard: if someone gives the validator a second
// params source (hardcoded list, separate allow-table, codegen), the behavioural
// checks below start diverging from the manifest and the test fails.
//
// The substantive (non-tautological) part is TYPES: the manifest parser
// (shared/plugin.validInputTypes) accepts a wider set of type than the params
// validator can interpret at the literal level. If the manifest gains a type the
// validator silently swallows (the default branch in astMatchesType — "unknown
// type, skip type-check"), that is a silent hole: param_type_mismatch never fires
// for such a param. TestP5_DeclaredTypesAreEnforceable catches this behaviourally,
// via the public config.LoadDestinyTasksFromBytes path.
package coremanifest_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// keeperSideModules — core modules dispatched `on: keeper` (ADR-017). They have no
// Soul-side destiny form ("task in tasks/main.yml"), but the `params:` validator
// still resolves them through the same coremanifest.State (config.module_params),
// so the synthetic probe task below is valid for them too.
//
// Listed here only so test messages can tag such modules, not to skip them: the
// check is identical for both sides.
var keeperSideModules = map[string]bool{
	"core.soul":  true,
	"core.cloud": true,
	"core.vault": true,
	"core.choir": true,
}

// allRegisteredModules — deterministic list of all registered core modules. Taken
// from the registry itself (Names() is sorted), not a hardcoded table, so a
// "forgot the new module" case cannot bypass P5.
func allRegisteredModules() []string {
	return coremanifest.Default().Names()
}

// mismatchLiteralFor picks a YAML literal that, for EACH manifest type,
// deliberately does NOT match that type structurally — to provoke
// param_type_mismatch from the validator if it interprets the type. The validator
// accepts almost anything for string, so a string is opposed with a list literal
// and vice versa. Returning "" means "no meaningful check can be built for this
// type" (type-check skipped, but the fact is logged).
func mismatchLiteralFor(declaredType string) (literal string, ok bool) {
	switch declaredType {
	case "string":
		return "[a, b]", true // list instead of string
	case "int", "integer", "number":
		return "[a, b]", true // list instead of a number
	case "bool", "boolean":
		return "[a, b]", true // list instead of bool
	case "list", "array":
		return "\"scalar\"", true // string instead of list
	case "map", "object":
		return "\"scalar\"", true // string instead of map
	default:
		// Type unknown to the mismatch-literal builder — the silent-hole candidate;
		// signal the caller via ok=false.
		return "", false
	}
}

// firstStateWithRequiredOrAny returns the name of any module state in deterministic
// order. Used to build the core.<mod>.<state> address in the synthetic task.
func firstStateWithRequiredOrAny(m *plugin.Manifest) (state string, def plugin.StateDef, ok bool) {
	names := make([]string, 0, len(m.Spec.States))
	for s := range m.Spec.States {
		names = append(names, s)
	}
	// Lexicographic order for reproducibility.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	if len(names) == 0 {
		return "", plugin.StateDef{}, false
	}
	return names[0], m.Spec.States[names[0]], true
}

// TestP5_AllModulesResolvableByValidator — every registered core module and each of
// its states resolves through the params validator via the same registry that serves
// the manifest. Base invariant: validator and manifest "see" the same set of
// modules/states. A failure here = validator and manifest diverged on source.
func TestP5_AllModulesResolvableByValidator(t *testing.T) {
	reg := coremanifest.Default()
	for _, mod := range allRegisteredModules() {
		m, ok := reg.Lookup(mod)
		if !ok {
			t.Errorf("%s: есть в Names(), но Lookup вернул ok=false", mod)
			continue
		}
		if len(m.Spec.States) == 0 {
			t.Errorf("%s: манифест без states", mod)
			continue
		}
		for state := range m.Spec.States {
			def, ok := reg.State(mod, state)
			if !ok {
				t.Errorf("%s.%s: State() не нашёл задекларированный state", mod, state)
				continue
			}
			// The def the validator reads (config.module_params) is exactly the
			// same object from Spec.States; check the contract is non-empty.
			if def.Input == nil && len(m.Spec.States[state].Input) != 0 {
				t.Errorf("%s.%s: State().Input расходится с Spec.States", mod, state)
			}
		}
	}
}

// TestP5_DeclaredTypesAreEnforceable — for every param of EVERY state of EVERY
// module: if the manifest declared a `type`, the params validator must actually
// interpret it. Behavioural check via the public config path
// (LoadDestinyTasksFromBytes), without touching private config helpers.
//
// Method: feed the param a literal of a deliberately DIFFERENT type and fill every
// other required field with valid values (so missing_required_param adds no noise).
// Expect param_type_mismatch on exactly the probed param. If absent, the manifest
// type is unenforced (validator ignores it): either a silent hole or a
// CEL-wrapper/special type. All such cases are collected into one report.
func TestP5_DeclaredTypesAreEnforceable(t *testing.T) {
	reg := coremanifest.Default()

	type miss struct {
		addr string // core.<mod>.<state>.<param>
		typ  string
		why  string
	}
	var unenforced []miss

	for _, mod := range allRegisteredModules() {
		m, _ := reg.Lookup(mod)
		for state, def := range m.Spec.States {
			for pname, p := range def.Input {
				if p.Type == "" {
					continue // type not declared — nothing to check.
				}
				mismatch, buildable := mismatchLiteralFor(p.Type)
				if !buildable {
					// A type we can't build a mismatch literal for. Signals a new
					// manifest type covered neither by the test nor (possibly) the
					// validator. Record as a finding, not a pass.
					unenforced = append(unenforced, miss{
						addr: fmt.Sprintf("core.%s.%s.%s", strings.TrimPrefix(mod, "core."), state, pname),
						typ:  p.Type,
						why:  "тест не умеет строить мисматч-литерал для этого типа (новый тип?)",
					})
					continue
				}

				src := buildTaskProbing(mod, state, def, pname, mismatch)
				_, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{})

				if !hasTypeMismatchFor(diags, pname) {
					unenforced = append(unenforced, miss{
						addr: fmt.Sprintf("core.%s.%s.%s", strings.TrimPrefix(mod, "core."), state, pname),
						typ:  p.Type,
						why:  fmt.Sprintf("валидатор НЕ дал param_type_mismatch на литерал %q; diags=%v", mismatch, codes(diags)),
					})
				}
			}
		}
	}

	if len(unenforced) > 0 {
		t.Errorf("найдено %d param-ов, чей manifest-type валидатор не проверяет (дрейф manifest↔валидатор):", len(unenforced))
		for _, u := range unenforced {
			side := "soul-side"
			modName := "core." + strings.SplitN(strings.TrimPrefix(u.addr, "core."), ".", 2)[0]
			if keeperSideModules[modName] {
				side = "keeper-side"
			}
			t.Errorf("  - %s (%s, type=%s): %s", u.addr, side, u.typ, u.why)
		}
	}
}

// TestP5_RequiredEnforcedByValidator — for each state with at least one required
// param: a task WITHOUT params: must yield missing_required_param for every such
// field. Guards the "manifest marked required, validator doesn't require it"
// divergence. Source of required is the manifest; we check the validator honours it.
func TestP5_RequiredEnforcedByValidator(t *testing.T) {
	reg := coremanifest.Default()
	for _, mod := range allRegisteredModules() {
		m, _ := reg.Lookup(mod)
		for state, def := range m.Spec.States {
			req := requiredNames(def)
			if len(req) == 0 {
				continue
			}
			// Address without params: — all required are absent by definition.
			src := fmt.Sprintf("- name: probe\n  module: %s.%s\n", mod, state)
			_, diags, _ := config.LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), config.ValidateOptions{})
			for _, name := range req {
				if !hasMissingRequiredFor(diags, name) {
					t.Errorf("%s.%s: manifest пометил %q required, но валидатор не дал missing_required_param; diags=%v",
						mod, state, name, codes(diags))
				}
			}
		}
	}
}

// buildTaskProbing builds a synthetic destiny task: it feeds the probed param a
// mismatch literal and other required fields valid stubs of their type, so
// missing_required_param doesn't mask the expected param_type_mismatch.
func buildTaskProbing(mod, state string, def plugin.StateDef, probeParam, probeLiteral string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- name: probe\n  module: %s.%s\n  params:\n", mod, state)
	fmt.Fprintf(&b, "    %s: %s\n", probeParam, probeLiteral)
	// Fill other required fields with valid values of their type.
	for _, name := range requiredNames(def) {
		if name == probeParam {
			continue
		}
		fmt.Fprintf(&b, "    %s: %s\n", name, validLiteralFor(def.Input[name].Type))
	}
	return b.String()
}

// validLiteralFor — a valid YAML literal for a type (to fill sibling required
// fields in the probing task).
func validLiteralFor(t string) string {
	switch t {
	case "int", "integer", "number":
		return "1"
	case "bool", "boolean":
		return "true"
	case "list", "array":
		return "[x]"
	case "map", "object":
		return "{k: v}"
	default: // string and unknown — a string is safe.
		return "\"x\""
	}
}

func requiredNames(def plugin.StateDef) []string {
	var out []string
	for name, p := range def.Input {
		if p.Required {
			out = append(out, name)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func hasTypeMismatchFor(diags []diag.Diagnostic, param string) bool {
	suffix := ".params." + param
	for _, d := range diags {
		if d.Code == "param_type_mismatch" && strings.HasSuffix(d.YAMLPath, suffix) {
			return true
		}
	}
	return false
}

func hasMissingRequiredFor(diags []diag.Diagnostic, param string) bool {
	suffix := ".params." + param
	for _, d := range diags {
		if d.Code == "missing_required_param" && strings.HasSuffix(d.YAMLPath, suffix) {
			return true
		}
	}
	return false
}

func codes(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
