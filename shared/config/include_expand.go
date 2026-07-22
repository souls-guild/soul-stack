package config

import (
	"fmt"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// IncludeResolver resolves an include target by file name into its contents and
// a canonical display path. The display path is a stable source identifier (used
// in cycle detection and diagnostics), not necessarily a file path: for the
// two-level scenario resolution (orchestration.md §6) it is the resolved path
// "local or service-level", for within-destiny it is a path inside the destiny
// directory.
//
// The resolver encapsulates ALL I/O and all source selection (two-level fallback,
// securejoin clamp). [ExpandIncludes] stays pure over the contents: it parses,
// expands, and detects cycles.
type IncludeResolver func(name string) (data []byte, displayPath string, err error)

// maxIncludeDepth — hard ceiling on include-chain depth. A safety net over cycle
// detection (visited-stack): even without a direct cycle, an uncontrolled deep
// chain is almost always an author error, not legitimate composition.
const maxIncludeDepth = 32

// ExpandIncludes expands include-tasks into a FLAT task list before the render
// phase (orchestration.md §6, destiny/tasks.md §4). Each include-task is replaced
// inline, in place, by the tasks of the included file; nested includes expand
// recursively.
//
// resolve encapsulates source selection (two-level scenario resolution or
// within-destiny) and I/O. The included file is parsed by the same task parser
// ([LoadDestinyTasksFromBytes]) — a top-level YAML task list without a wrapper.
//
// Cycles (a→b→a, direct self-include) are detected by display path via a
// visited-stack: re-entry of a path into the active chain → `include_cycle` error
// (not infinite recursion). Depth is bounded by [maxIncludeDepth].
//
// Return contract:
//   - parse/cycle/depth/resolve errors of included files → error-level
//     diagnostics (the caller rejects via [diag.HasErrors]); tasks is returned as
//     fully expanded as possible (for partial diagnostics).
//   - error != nil — never (reserved for symmetry with other Load*).
//
// Splice semantics (slice B): a plain `include: <file>` (optional `name:`) is
// spliced flat. On an include-task the fields `include:`/`name:` AND `when:`
// (conditional include, ADR-009 amendment) are allowed — a whitelist; any other
// non-empty scope/control modifier is rejected with an
// `include_modifier_unsupported` diagnostic, so scope isn't lost silently.
//
// Conditional include (`when:` on an include-task): the include-when MUST be
// static (input./essence./incarnation./vars. — [IsStaticIncludeWhen]), since
// expansion runs BEFORE the Stratify phase, when register isn't assembled yet and
// the per-host soulprint is unknown. A dynamic when → `include_when_dynamic_unsupported`.
// The static when and the group id are stamped into EVERY spliced task
// (Task.IncludeWhen/IncludeGroupID) — keeper-side render drops the whole group in
// one include-when evaluation (group-drop, a real exclusion from the plan).
//
// Nested conditional include cascade: the effective include-when of a NESTED
// conditional group = conjunction of ancestors `(<ancestor include-when>) && (<inner include-when>)`.
// The accumulated ancestor-when is carried down the expansion recursion; the
// nested group gets its OWN group-id whose include-when encodes the full ancestor
// conjunction. So dropping a parent (e.g. `outer=='no'`) cascades to the child
// naturally: its conjunctive include-when also evaluates to false.
func ExpandIncludes(tasks []Task, resolve IncludeResolver) ([]Task, []diag.Diagnostic) {
	e := &includeExpander{resolve: resolve}
	out := e.expand(tasks, nil, "")
	// Uniqueness of the subscription address space (register ∪ id) over the FLAT
	// run list: per-file validateTaskRefs catches a duplicate within one file, but
	// not between the main file and an expanded include (each file there is
	// validated in its own scope). This check runs on the final flat `[]Task` —
	// one pass after flatten — and catches a cross-include duplicate. There are no
	// line/col coordinates at this level (expansion erased AST positions); the
	// diagnostic addresses by name.
	if !diag.HasErrors(e.diags) {
		e.diags = append(e.diags, validateFlatTaskAddresses(out)...)
	}
	return out, e.diags
}

// validateFlatTaskAddresses checks uniqueness of the `register ∪ id` subscription
// address space (destiny/tasks.md §8) over the flat run task list — after include
// expansion. A duplicate (two registers, two ids, or a register/id intersection)
// → duplicate_task_address. Recurses into nested block: (its addresses live in
// the same flat plan space).
//
// Called at the ExpandIncludes exit only when there are no error-level expansion
// diagnostics: checking addresses on a half-expanded list (parse/cycle/resolve
// failure) is pointless. Per-file duplicates (within one file) are already caught
// by validateTaskRefs at file load — here we catch specifically cross-file
// duplicates, so a single file without includes produces no duplicate diagnostics
// (a valid per-file list is unique here too).
func validateFlatTaskAddresses(tasks []Task) []diag.Diagnostic {
	seen := map[string]bool{}
	var out []diag.Diagnostic
	collectFlatAddresses(tasks, seen, &out)
	return out
}

// collectFlatAddresses fills seen with register/id names from the flat list
// (recursively via block:); a repeated name → duplicate_task_address. Order is
// deterministic by list traversal.
func collectFlatAddresses(tasks []Task, seen map[string]bool, out *[]diag.Diagnostic) {
	for i := range tasks {
		t := &tasks[i]
		for _, addr := range []string{t.Register, t.ID} {
			if addr == "" {
				continue
			}
			if seen[addr] {
				*out = append(*out, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
					Code:    "duplicate_task_address",
					Message: fmt.Sprintf("task address %q (register/id) is declared more than once in this plan after include expansion", addr),
					Hint:    "register and id share one subscription address space across the flattened run (main + included files) — a duplicate makes \"alert on task X\" ambiguous; rename one",
				})
				continue
			}
			seen[addr] = true
		}
		if t.Block != nil {
			collectFlatAddresses(t.Block.Block, seen, out)
		}
	}
}

type includeExpander struct {
	resolve IncludeResolver
	diags   []diag.Diagnostic
	// lastGroupID — id counter for conditional include-groups (carry-through
	// group-drop). Monotonically grows on EVERY include with a non-empty `when:`;
	// 0 is reserved for "outside a conditional include" (Task.IncludeGroupID==0).
	// Nested conditional includes get distinct ids; dropping each is an independent
	// evaluation of its own include-when.
	lastGroupID int
}

// expand recursively expands a task list. stack is the active chain of display
// paths (for cycle detection and depth); nil at the top level. ancestorWhen is
// the accumulated include-when of conditional include ancestors (a conjunction
// for cascading drop); "" at the top level and under unconditional includes.
func (e *includeExpander) expand(tasks []Task, stack []string, ancestorWhen string) []Task {
	out := make([]Task, 0, len(tasks))
	for i := range tasks {
		task := tasks[i]

		// Non-include or block: no splice needed. block is expanded in the render
		// phase (renderBlockTask, like loop) — passthrough here. within-block
		// include isn't supported in pilot C1 (guardPilotBlockChild rejects an
		// include child as ErrUnexpandedInclude) — no block recursion needed here.
		if task.Include == nil {
			out = append(out, task)
			continue
		}

		expanded, ok := e.expandOne(task, stack, ancestorWhen)
		if !ok {
			// Diagnostic already recorded; don't splice the task (nothing to splice).
			continue
		}
		out = append(out, expanded...)
	}
	return out
}

// conjoinIncludeWhen builds the effective include-when of a nested conditional
// group: the conjunction of the accumulated ancestor-when and the own
// include-when. Each operand is parenthesized so that CEL operator precedence
// inside a predicate (e.g. `a || b`) doesn't "leak" across &&. An empty ancestor
// (top level or unconditional parent) → the own when without wrapping (single
// level unchanged).
func conjoinIncludeWhen(ancestorWhen, ownWhen string) string {
	if ancestorWhen == "" {
		return ownWhen
	}
	return fmt.Sprintf("(%s) && (%s)", ancestorWhen, ownWhen)
}

// stampIncludeGroup stamps the include-when and group id into EVERY spliced task
// (recursively via block:, so children of a block-task inside a conditional
// include are also dropped as a whole). A nested conditional include has already
// stamped its OWN (more specific) IncludeGroupID onto its tasks earlier (recursion
// expandOne → expand → stampIncludeGroup), so we do NOT overwrite an
// already-stamped group here. Dropping each level cascades via CONJUNCTION: the
// nested group is already stamped with the effective include-when `(ancestor) &&
// (own)` (conjoinIncludeWhen), so a false ancestor also silences the child even if
// the child's own when is true. Mirrors the block.when-injection idea
// (mergeBlockInheritance), but via a separate carry-through axis, not via an AND in
// the task's own when (render drops the group before emitStaticWhenSkip by its
// IncludeWhen, rather than silencing it with a placeholder).
func stampIncludeGroup(tasks []Task, when string, groupID int) {
	for i := range tasks {
		t := &tasks[i]
		if t.IncludeGroupID != 0 {
			continue // nested conditional include already stamped its own group.
		}
		t.IncludeWhen = when
		t.IncludeGroupID = groupID
		if t.Block != nil {
			stampIncludeGroup(t.Block.Block, when, groupID)
		}
	}
}

// expandOne expands one include-task: checks modifiers, resolves and parses the
// target, detects cycle/depth, and recursively expands its tasks.
//
// ancestorWhen is the accumulated include-when of conditional ancestors. If this
// include has its own `when:`, the effective group include-when = conjunction
// `(ancestorWhen) && (own)` (conjoinIncludeWhen) — that's what's stamped into the
// tasks and carried further down. If `when:` is empty (unconditional include), no
// group is created, but ancestorWhen is carried further UNCHANGED — a conditional
// descendant gets the conjunction with the ancestor through its own expansion.
func (e *includeExpander) expandOne(task Task, stack []string, ancestorWhen string) ([]Task, bool) {
	name := task.Include.Include

	if reason := includeModifierReason(task); reason != "" {
		e.addError("include_modifier_unsupported",
			fmt.Sprintf("include %q carries %s - forwarding scope/control through include is out of slice B; move the modifier onto a module task of the included file", name, reason),
			"")
		return nil, false
	}

	// Conditional include (`when:` on an include): the predicate MUST be static —
	// the include expands BEFORE Stratify, register of prior tasks isn't assembled
	// yet, the per-host soulprint is unknown. A dynamic when (register./soulprint.)
	// → include_when_dynamic_unsupported. An allowed when resolves to a group-id
	// that stampIncludeGroup carries into every spliced task.
	//
	// effectiveWhen — conjunction with the accumulated ancestor-when (cascading
	// drop of nested conditional includes): the nested group is dropped if ANY
	// ancestor OR its own predicate is false. This same effectiveWhen is carried
	// down as ancestorWhen for the next expansion level.
	groupID := 0
	effectiveWhen := ancestorWhen
	if task.When != "" {
		if !IsStaticIncludeWhen(task.When) {
			e.addError("include_when_dynamic_unsupported",
				fmt.Sprintf("include %q carries a dynamic when %q (reference to register./soulprint.) - include expands BEFORE stratification, only a static predicate input./essence./incarnation./vars. is available", name, task.When),
				"replace with a static predicate (input./essence./incarnation.) or move the condition onto a module task of the included file via when:")
			return nil, false
		}
		e.lastGroupID++
		groupID = e.lastGroupID
		effectiveWhen = conjoinIncludeWhen(ancestorWhen, task.When)
	}

	data, display, err := e.resolve(name)
	if err != nil {
		e.addError("include_resolve_failed", fmt.Sprintf("include %q: %v", name, err), "")
		return nil, false
	}

	if depth := len(stack); depth >= maxIncludeDepth {
		e.addError("include_depth_exceeded",
			fmt.Sprintf("include %q: maximum depth %d exceeded (chain: %v)", name, maxIncludeDepth, stack),
			"")
		return nil, false
	}
	for _, prev := range stack {
		if prev == display {
			e.addError("include_cycle",
				fmt.Sprintf("include %q forms a cycle: %s is already in the active chain %v", name, display, stack),
				"break the cyclic include dependency")
			return nil, false
		}
	}

	parsed, diags, _ := LoadDestinyTasksFromBytes(display, data, ValidateOptions{})
	if diag.HasErrors(diags) {
		e.diags = append(e.diags, diags...)
		return nil, false
	}

	expanded := e.expand(parsed, append(append([]string(nil), stack...), display), effectiveWhen)
	if groupID != 0 {
		stampIncludeGroup(expanded, effectiveWhen, groupID)
	}
	return expanded, true
}

// includeModifierReason returns a human-readable reason if an include-task
// carries any field besides `include:`/`name:`/`when:`. These fields are allowed
// on an include-task (`when:` — conditional include, ADR-009 amendment); any other
// non-empty scope/control modifier would be lost silently by the splice — so
// expansion rejects it. A whitelist (not a blacklist) is robust to future Task
// fields: a new field is forbidden on include by default. An empty string means
// the task is clean.
//
// `when:` is NOT in this list (conditional include) — its staticness is checked
// separately by expandOne (IsStaticIncludeWhen → include_when_dynamic_unsupported
// for dynamic). `loop:` stays forbidden: loop on include is not implemented
// (docs↔code drift, docs/destiny/tasks.md §7) → include_modifier_unsupported.
func includeModifierReason(task Task) string {
	switch {
	case task.Loop != nil:
		return "loop: (slice E)"
	case len(task.Vars) > 0:
		return "vars:"
	case task.Parallel:
		return "parallel:"
	case task.Register != "":
		return "register:"
	case len(task.Output) > 0:
		return "output:"
	case task.NoLog:
		return "no_log:"
	case len(task.OnChanges) > 0:
		return "onchanges:"
	case len(task.OnFail) > 0:
		return "onfail:"
	case task.Require != nil:
		return "require:"
	case task.ChangedWhen != "":
		return "changed_when:"
	case task.FailedWhen != "":
		return "failed_when:"
	case task.Retry != nil:
		return "retry:"
	case task.Timeout != "":
		return "timeout:"
	case task.On != nil:
		return "on:"
	case task.Where != "":
		return "where:"
	case task.Serial != nil:
		return "serial:"
	case task.RunOnce:
		return "run_once:"
	}
	return ""
}

// addError records a semantic expansion diagnostic (cycle/depth/modifier/
// resolve): these are cross-field/cross-file invariants, not a node-structure check.
func (e *includeExpander) addError(code, msg, hint string) {
	e.diags = append(e.diags, diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSemanticValidate,
		Code:    code,
		Message: msg,
		Hint:    hint,
	})
}
