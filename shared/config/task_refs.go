package config

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// validateTaskRefs runs cross-task checks over a plan's task list (scenario/main.yml
// `tasks[]` or destiny `tasks/main.yml`) that a per-task validation
// (validateTaskNode) cannot raise:
//
//  1. duplicate_task_address — two+ tasks share one name in the subscription
//     address space `register ∪ id` (two registers, two ids, or one task's
//     register == another's id).
//  2. unknown_register_reference — a reference to a register name `register.<name>.*`
//     that no task in the plan declares, in any register-reading field:
//     `onchanges:`/`onfail:`/`require:` (list form) OR a CEL predicate
//     (`when:`/`changed_when:`/`failed_when:`/`until:`/`where:`/`loop.when:`) OR
//     interpolation `${ register.<name>.* }` in a source field (`vars:`/`output:`/
//     `params:`/`apply.input:`/`loop.items:`). The last class is closed by ADR-056 S2 —
//     before it, an unknown register in interpolation survived to the runtime
//     stratifier; the field set here must match the stratifier's passage-defining
//     sources (see collectRefs).
//
// Why an ERROR, not a warning, for a duplicate address:
// register names resolve to task indices via a flat name→index map over the whole
// plan (keeper/internal/render registerIndex) — last-wins. With two tasks
// `register: X`, a dependency (`onchanges:[X]`/`onfail:[X]`/`when: register.X.*`)
// silently binds to the LAST X only; a change/failure of the first X silently does
// not activate the dependency. This is a silent source of "rescue did not fire" /
// "onchanges skipped" — the linter must catch it statically, not leave it to
// runtime debugging. Symmetrically `id:` is a task address for alert subscriptions
// "task X changed" (ADR-052 §h); register and id live in ONE address space
// (destiny/tasks.md §8), so a duplicate id or a register/id overlap would match the
// "alert on task X" subscription to the wrong task (or several). A duplicate is
// almost always a bug (copy-paste without renaming), with no legitimate case for one
// name on two tasks — hence an error, not a warning.
//
// Names are walked over the plan's flat space, including nested `block:`
// (recursively), because the keeper-side registerIndex is flat too: a register on a
// block task is addressable from anywhere in the plan. Order does not matter: for a
// duplicate, because a duplicate is a duplicate; for cross-ref, because resolution
// goes by index and an unknown name is a bug regardless of position (a forward-ref
// to an existing name is a separate, legitimate case, not penalized here).
//
// Cross-file uniqueness through an expanded include (a duplicate address between the
// main file and an included one) is caught separately — on the flat `[]Task` after
// ExpandIncludes (validateFlatTaskAddresses). Here — per-file AST level with precise
// line/col.
//
// Cross-ref for CEL predicates (`when:`/`changed_when:`/`failed_when:`/`until:`/
// `where:`/`loop.when:`, where the reference is written as `register.<name>.*`) is
// covered by textual extraction: string-literal contents are stripped (as in the
// shared/cel guards), then the regex `register.<name>` collects names (see
// ExtractRegisterRefs). Dynamic access (`register["..."]`) and `register.self` (the
// current task) are deliberately not flagged. A full CEL-AST parse is unnecessary —
// the reference form is fixed by the grammar (`register.<name>`).
//
// tasksSeq — the `tasks:` AST node (scenario) or the root sequence (destiny).
// A nil node → nil (an empty/invalid list is already diagnosed above).
func validateTaskRefs(tasksSeq *ast.SequenceNode, pathPrefix string) []diag.Diagnostic {
	if tasksSeq == nil {
		return nil
	}

	// addrs — the full subscription address space (register ∪ id) for the
	// uniqueness check. registers — only register names for cross-ref
	// (unknown_register_reference): an id addresses an alert subscription but does
	// NOT create `register.<name>` — an id task cannot be referenced in
	// onchanges/onfail/when.
	addrs := map[string]bool{}
	registers := map[string]bool{}
	var dupDiags []diag.Diagnostic
	collectAddresses(tasksSeq, pathPrefix, addrs, registers, &dupDiags)

	var out []diag.Diagnostic
	out = append(out, dupDiags...)
	out = append(out, collectRefs(tasksSeq, pathPrefix, registers)...)
	return out
}

// collectAddresses walks tasks (recursively through block:) and fills two sets:
// addrs — the full subscription address space `register ∪ id` (destiny/tasks.md §8);
// registers — only register names (for cross-ref). A repeated name in addrs (a
// duplicate register, a duplicate id, or a register/id overlap) →
// duplicate_task_address on EVERY repeat declaration (the first is treated as the
// "primary" and is not diagnosed — symmetric to task_discriminator_multiple).
//
// addrs carries both facets of one space, so register "X" and id "X" on different
// tasks collide: an "alert on task X" subscription must not silently match two
// different tasks. registers is filled only from `register:` — an id task creates no
// register.<name> reference to itself.
func collectAddresses(seq *ast.SequenceNode, pathPrefix string, addrs, registers map[string]bool, dups *[]diag.Diagnostic) {
	for i, item := range seq.Values {
		taskPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "register", "id":
				sn, isStr := kv.Value.(*ast.StringNode)
				if !isStr || sn.Value == "" {
					continue // type/format already diagnosed by validateRegisterField/validateIDField.
				}
				if tok.Value == "register" {
					registers[sn.Value] = true
				}
				if addrs[sn.Value] {
					rt := sn.GetToken()
					*dups = append(*dups, diagAt(rt.Position.Line, rt.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
						Code:     "duplicate_task_address",
						Message:  fmt.Sprintf("task address %q (register/id) is declared more than once in this plan", sn.Value),
						Hint:     "register and id share one subscription address space — a duplicate makes \"alert on task X\" match the wrong task and silently breaks onchanges/onfail/when wiring; rename one",
						YAMLPath: taskPath + "." + tok.Value,
					}))
					continue
				}
				addrs[sn.Value] = true
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					collectAddresses(bseq, taskPath+".block", addrs, registers, dups)
				}
			}
		}
	}
}

// collectRefs walks tasks (recursively through block:) and checks EVERY
// register-reading field of a task against known; an unknown name →
// unknown_register_reference. Fields split into three classes:
//
//   - requisite lists (onchanges/onfail/require) — the task name is a bare list
//     element (checkRefList);
//   - CEL predicates (when/changed_when/failed_when/where + nested retry.until/
//     loop.when) — a reference as `register.<name>.*` in the expression
//     (checkPredicateRefs);
//   - interpolation fields (vars/output/params/apply.input/loop.items) — a reference
//     as `${ register.<name>.* }` in string literals, recursively over map/seq
//     (checkInterpRefs).
//
// This set must match the stratifier's passage-defining sources 1:1
// (keeper/internal/render.collectTaskReads, the ADR-056 registry), so the linter's
// register-reference graph and the stratifier's graph do not diverge: a hole in the
// validator would mean an unknown register in output/vars/params/apply.input
// survives to runtime and fails StratifyUnknownRegister online instead of being
// caught offline by the linter. Guard invariant — reads==refs consistency
// (passage_test.go).
func collectRefs(seq *ast.SequenceNode, pathPrefix string, known map[string]bool) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i, item := range seq.Values {
		taskPath := fmt.Sprintf("%s[%d]", pathPrefix, i)
		mm, ok := item.(*ast.MappingNode)
		if !ok {
			continue
		}
		for _, kv := range mm.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			switch tok.Value {
			case "onchanges", "onfail", "require":
				out = append(out, checkRefList(tok.Value, kv.Value, known, taskPath)...)
			case "when", "changed_when", "failed_when", "where":
				out = append(out, checkPredicateRefs(tok.Value, kv.Value, known, taskPath)...)
				out = append(out, checkSoulprintRefs(tok.Value, kv.Value, taskPath)...)
				out = append(out, checkStateRefs(tok.Value, kv.Value, taskPath)...)
			case "assert":
				// assert.that[] — a list of CEL bool predicates (render-time precondition,
				// ADR-009 amendment). Each predicate is checked like `where:`:
				// register references against known + soulprint/state canon. assert is
				// evaluated Keeper-side (like where:) — it supports cross-passage register
				// (re-render with accumulated register), so reading register in that is legal.
				if amm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range amm.Values {
						st := sub.Key.GetToken()
						if st == nil || st.Value != "that" {
							continue
						}
						if seq, isSeq := sub.Value.(*ast.SequenceNode); isSeq {
							for j, item := range seq.Values {
								kind := fmt.Sprintf("assert.that[%d]", j)
								out = append(out, checkPredicateRefs(kind, item, known, taskPath)...)
								out = append(out, checkSoulprintRefs(kind, item, taskPath)...)
								out = append(out, checkStateRefs(kind, item, taskPath)...)
							}
						}
					}
				}
			case "vars", "output", "params":
				// Interpolation source fields: ${ register.X } in string literals,
				// recursively over nested map/seq.
				out = append(out, checkInterpRefs(kv.Value, known, taskPath, tok.Value)...)
				out = append(out, checkInterpStateRefs(kv.Value, taskPath, tok.Value)...)
			case "apply":
				// applier task: register is read in apply.input (a nested map).
				if amm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range amm.Values {
						st := sub.Key.GetToken()
						if st != nil && st.Value == "input" {
							out = append(out, checkInterpRefs(sub.Value, known, taskPath, "apply.input")...)
							out = append(out, checkInterpStateRefs(sub.Value, taskPath, "apply.input")...)
						}
					}
				}
			case "retry":
				// `until:` — a CEL predicate inside the retry mapping.
				if rmm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range rmm.Values {
						st := sub.Key.GetToken()
						if st != nil && st.Value == "until" {
							out = append(out, checkPredicateRefs("retry.until", sub.Value, known, taskPath)...)
							out = append(out, checkSoulprintRefs("retry.until", sub.Value, taskPath)...)
							out = append(out, checkStateRefs("retry.until", sub.Value, taskPath)...)
						}
					}
				}
			case "loop":
				// `when:` — a CEL predicate; `items:` — an interpolation source
				// (${ register.X } in a scalar/list/map).
				if lmm, isMap := kv.Value.(*ast.MappingNode); isMap {
					for _, sub := range lmm.Values {
						st := sub.Key.GetToken()
						if st == nil {
							continue
						}
						switch st.Value {
						case "when":
							out = append(out, checkPredicateRefs("loop.when", sub.Value, known, taskPath)...)
							out = append(out, checkSoulprintRefs("loop.when", sub.Value, taskPath)...)
							out = append(out, checkStateRefs("loop.when", sub.Value, taskPath)...)
						case "items":
							out = append(out, checkInterpRefs(sub.Value, known, taskPath, "loop.items")...)
							out = append(out, checkInterpStateRefs(sub.Value, taskPath, "loop.items")...)
						}
					}
				}
			case "block":
				if bseq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
					out = append(out, collectRefs(bseq, taskPath+".block", known)...)
				}
			}
		}
	}
	return out
}

// checkInterpRefs recursively walks the AST node of an interpolation field (vars /
// output / params / apply.input / loop.items) and checks register names found inside
// `${ register.<name>.* }` string literals against known. This closes a hole in the
// cross-ref validator: before ADR-056 S2 an unknown register in interpolation
// (rather than in a predicate/requisite) was not caught offline and failed only in
// the runtime stratifier (StratifyUnknownRegister).
//
// Extraction is done by ExtractRegisterRefs — the same canonical `register.<name>`
// parser as the stratifier and checkPredicateRefs (no duplicate regex). Non-string
// nodes (int/bool/null) carry no references; recursion descends only through
// map/seq. The diagnostic is at the position of the string node where the reference
// was found.
func checkInterpRefs(node ast.Node, known map[string]bool, taskPath, kind string) []diag.Diagnostic {
	switch n := node.(type) {
	case *ast.StringNode:
		var out []diag.Diagnostic
		rt := n.GetToken()
		line, col := 0, 0
		if rt != nil {
			line, col = rt.Position.Line, rt.Position.Column
		}
		for _, name := range ExtractRegisterRefs(n.Value) {
			if known[name] {
				continue
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "unknown_register_reference",
				Message:  fmt.Sprintf("%s interpolates register %q, which no task declares", kind, name),
				Hint:     "interpolation reads ${ register.<name>.* }; check for a typo or a missing register: on the producing task (register.self is the current task and is not checked)",
				YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
			}))
		}
		return out
	case *ast.MappingNode:
		var out []diag.Diagnostic
		for _, kv := range n.Values {
			out = append(out, checkInterpRefs(kv.Value, known, taskPath, kind)...)
		}
		return out
	case *ast.MappingValueNode:
		return checkInterpRefs(n.Value, known, taskPath, kind)
	case *ast.SequenceNode:
		var out []diag.Diagnostic
		for _, v := range n.Values {
			out = append(out, checkInterpRefs(v, known, taskPath, kind)...)
		}
		return out
	default:
		return nil
	}
}

// checkRefList checks the elements of a list requisite field (onchanges/onfail/
// require) against known. `require:` allows the scalar form `"all"` (not a register
// list) — it is skipped. Non-string / CEL-wrapped elements are skipped (the name is
// statically unknown). Empty/invalid list — no type check here (that is
// validateTaskNode's job), silently skipped.
func checkRefList(kind string, value ast.Node, known map[string]bool, taskPath string) []diag.Diagnostic {
	seq, ok := value.(*ast.SequenceNode)
	if !ok {
		return nil // require: "all" (scalar) or an invalid form — not our concern.
	}
	var out []diag.Diagnostic
	for j, item := range seq.Values {
		sn, isStr := item.(*ast.StringNode)
		if !isStr || isCELWrapped(sn.Value) {
			continue
		}
		if known[sn.Value] {
			continue
		}
		rt := sn.GetToken()
		out = append(out, diagAt(rt.Position.Line, rt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "unknown_register_reference",
			Message:  fmt.Sprintf("%s[%d] references register %q, which no task declares", kind, j, sn.Value),
			Hint:     "requisite lists name a task by its register: value; check for a typo or a missing register: on the producing task",
			YAMLPath: fmt.Sprintf("%s.%s[%d]", taskPath, kind, j),
		}))
	}
	return out
}

// reRegisterCELRef extracts `register.<name>` from CEL text. The left boundary is
// the start of the string OR a character that is not part of an identifier and not
// `.` (so `myregister.x` and `foo.register.y` do NOT match: `register` must be a
// root identifier, as in the CEL context grammar). The name is a snake_case register
// id (matches reRegisterID). Dynamic access `register["x"]` is not covered by this
// form — there is no dot, the regex does not match (a safe skip).
var reRegisterCELRef = regexp.MustCompile(`(^|[^A-Za-z0-9_.])register\.([a-z][a-z0-9_]*)`)

// celStringLiteral — a CEL string literal (single/double quotes). Mirrors
// shared/cel.stringLiteralRe; we strip the contents before the textual identifier
// search so that `register.x` inside a data literal does not yield a false
// unknown_register_reference.
var celStringLiteral = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// reSoulprintRef catches a soulprint reference in CEL text (the host-variant layer
// soulprint.self). Mirrors keeper/internal/render.reFlowControlSoulprint (`\bsoulprint\b`)
// — one source of truth for the "host-variant predicate" grammar.
var reSoulprintRef = regexp.MustCompile(`\bsoulprint\b`)

// IsStaticIncludeWhen reports whether a conditional include's `when:` predicate is
// static — i.e. computable BEFORE the Stratify/render phase (conditional-include
// group-drop, ADR-009 amendment). An include expands into the flat list BEFORE
// stratification, when previous tasks' register is not yet collected and per-host
// soulprint is unknown; so only a static predicate is allowed
// (input./essence./incarnation./vars.). The same two criteria as keeper-side
// isStaticWhen (register-/soulprint-independence):
//   - no cross-task register reference (ExtractRegisterRefs — the same canonical parser);
//   - no soulprint reference (the host-variant layer).
//
// An empty when → true (an unconditional include — the group is always spliced in;
// the drop is not activated when IncludeGroupID==0). A dynamic when → false: the
// caller (ExpandIncludes/soul-lint) raises include_when_dynamic_unsupported.
func IsStaticIncludeWhen(when string) bool {
	if when == "" {
		return true
	}
	if len(ExtractRegisterRefs(when)) != 0 {
		return false
	}
	return !reSoulprintRef.MatchString(when)
}

// ExtractRegisterRefs returns a sorted set of unique names from `register.<name>` in
// a CEL string (after stripping string literals). `self` is excluded — it is the
// current task (register.self.*), whose existence cross-ref does not check. Sorting
// makes diagnostics deterministic when a predicate has several unknown references.
//
// Exported as the canonical parser of cross-task register references: stage-render
// stratification ([ADR-056], keeper/internal/render) reuses it to build the
// register-dependency graph over the same `register.<name>` grammar as the cross-ref
// validator — no duplicate regex. `register.self.*` (the Soul-side own result of the
// same task) is NOT counted as a reference here or in stratification (it is not a
// cross-task edge).
func ExtractRegisterRefs(expr string) []string {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	seen := map[string]struct{}{}
	for _, m := range reRegisterCELRef.FindAllStringSubmatch(stripped, -1) {
		name := m[2]
		if name == "self" {
			continue
		}
		seen[name] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkPredicateRefs checks register names in a CEL predicate (when/changed_when/
// failed_when/until/where/loop.when) against known. A bool literal (force-shortcut
// changed_when/failed_when) and null are not a CEL string and are skipped. A
// forward-ref to an existing name is legitimate (known is flat over the whole plan);
// only a missing name is penalized. The diagnostic is at the position of the
// predicate value node (the exact offset within the string is not extracted — this
// is textual, not AST, analysis).
func checkPredicateRefs(kind string, value ast.Node, known map[string]bool, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok {
		return nil
	}
	var out []diag.Diagnostic
	rt := sn.GetToken()
	line, col := 0, 0
	if rt != nil {
		line, col = rt.Position.Line, rt.Position.Column
	}
	for _, name := range ExtractRegisterRefs(sn.Value) {
		if known[name] {
			continue
		}
		out = append(out, diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "unknown_register_reference",
			Message:  fmt.Sprintf("%s references register %q, which no task declares", kind, name),
			Hint:     "CEL predicate reads register.<name>.*; check for a typo or a missing register: on the producing task (register.self is the current task and is not checked)",
			YAMLPath: fmt.Sprintf("%s.%s", taskPath, kind),
		}))
	}
	return out
}
