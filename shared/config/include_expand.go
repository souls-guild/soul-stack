package config

import (
	"fmt"
	"strconv"
	"strings"

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
// Within-block include: a `block:` is not an expansion boundary — its children
// are walked with the same visited-stack and ancestor-when, so an include among
// them splices into the block's children at any nesting depth (block → include →
// block → include). The block node itself survives expansion (render merges its
// when:/requisites into every child, spliced ones included).
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
// static (input./vars./incarnation. — [IsStaticIncludeWhen]), since
// expansion runs BEFORE the Stratify phase, when register isn't assembled yet and
// the per-host soulprint is unknown. A dynamic when → `include_when_dynamic_unsupported`.
// The static when and the group id are stamped into EVERY spliced task
// (Task.IncludeWhen/IncludeGroupID) — keeper-side render drops the whole group in
// one include-when evaluation (group-drop, a real exclusion from the plan).
//
// Register scope across the include boundary ([ADR-0083] §4): an included body may
// reference a register the INCLUDER declares (`register.<name>` in params/vars/
// apply.input/…). Expansion collects the declared register names of each level and
// threads them down as ValidateOptions.OuterRegisters, so the body's per-file
// validateTaskRefs sees them. The reverse direction stays rejected — see that
// field's doc for why the asymmetry is load-bearing for group-drop.
//
// Nested conditional include cascade: the effective include-when of a NESTED
// conditional group = conjunction of ancestors `(<ancestor include-when>) && (<inner include-when>)`.
// The accumulated ancestor-when is carried down the expansion recursion; the
// nested group gets its OWN group-id whose include-when encodes the full ancestor
// conjunction. So dropping a parent (e.g. `outer=='no'`) cascades to the child
// naturally: its conjunctive include-when also evaluates to false.
func ExpandIncludes(tasks []Task, resolve IncludeResolver) ([]Task, []diag.Diagnostic) {
	return expandIncludes(tasks, resolve, false, nil)
}

// ExpandIncludesWithModules is [ExpandIncludes] for a caller that can resolve
// plugin manifests, so an included body's `params:` are checked against the same
// contract the including file's are ([ValidateOptions.ModuleManifests]).
//
// Without it the body is loaded with no resolver, which is not silence — every
// plugin module in it yields [DiagPluginParamsUnchecked] — but it is a gap the
// caller could have closed. soul-lint is the one that can: the author bound the
// manifests on the command line, and the whole point of `--modules` is that the
// four checks then run offline (NIM-779).
//
// A separate entry point rather than a parameter on [ExpandIncludes] so the keeper
// call sites stay as they are. Be precise about what that costs, because the
// tempting sentence here — "not adopting it is safe, the default is loud" — is
// false as an operator-facing claim: [ExpandIncludes] RETURNS the hint, and every
// keeper call site filters the slice to errors before it formats anything, so
// nothing prints it. The honest statement is narrower. The gap is in the returned
// diagnostics rather than nowhere, so a caller that decides to surface hints needs
// no change here to start seeing it; whether one does is that caller's own choice,
// and today only soul-lint's stage pass makes it.
//
// The keeper is deliberately not adopting this in NIM-779. `unknown_param` is an
// ERROR, and the keeper aborts a run on any error out of expansion, so wiring the
// resolver there would turn scenarios that render and dispatch today into hard
// aborts — a policy change about when a cluster refuses work, which wants its own
// decision rather than riding along with a linter fix. That decision is NIM-785.
func ExpandIncludesWithModules(tasks []Task, resolve IncludeResolver, modules ModuleManifestResolver) ([]Task, []diag.Diagnostic) {
	return expandIncludes(tasks, resolve, false, modules)
}

// ExpandIncludesInDestiny is [ExpandIncludes] for a DESTINY's own `tasks/` tree,
// where an included body is subject to the one rule a scenario's is not: a
// destiny is Soul-side by construction, so a keeper-side module address anywhere
// in it can never execute (`keeper_module_in_destiny`, NIM-749).
//
// A separate entry point rather than a parameter on [ExpandIncludes] because the
// seven scenario callers must never pass it, and a bool at every call site is a
// bool somebody eventually passes the wrong way round. The flag reaches the
// included body's own [LoadDestinyTasksFromBytes] — without it the check stops at
// the destiny's `tasks/main.yml` and a capture one `include:` deep is linted
// clean, folded by L0, and dies at dispatch: the exact false-green this rule
// exists to close.
func ExpandIncludesInDestiny(tasks []Task, resolve IncludeResolver) ([]Task, []diag.Diagnostic) {
	return expandIncludes(tasks, resolve, true, nil)
}

func expandIncludes(tasks []Task, resolve IncludeResolver, destinyTasks bool, modules ModuleManifestResolver) ([]Task, []diag.Diagnostic) {
	e := &includeExpander{
		resolve:      resolve,
		destinyTasks: destinyTasks,
		modules:      modules,
		seenBodyDiag: map[string]bool{},
	}
	out := e.expand(tasks, nil, "", nil)
	// Uniqueness of the subscription address space (register ∪ id) over the FLAT
	// run list: per-file validateTaskRefs catches a duplicate within one file, but
	// not between the main file and an expanded include (each file there is
	// validated in its own scope). This check runs on the final flat `[]Task` —
	// one pass after flatten — and catches a cross-include duplicate. There are no
	// line/col coordinates at this level (expansion erased AST positions); the
	// diagnostic addresses by name.
	if !diag.HasErrors(e.diags) {
		e.diags = append(e.diags, validateFlatTaskAddresses(out)...)
		e.diags = append(e.diags, validateFlatBlockKeeperSide(out)...)
	}
	return out, e.diags
}

// validateFlatBlockKeeperSide raises `block_on_keeper_invalid` for a keeper-side
// task sitting inside a `block:` in the FLATTENED plan.
//
// The per-file check ([validateBlockChildOnKeeper]) cannot see this one. An
// `include:` written as a block CHILD is a block child to the expander and a
// TOP-LEVEL task to the included file's own validation, so a capture arriving
// that way is judged by neither: the block-child pass looks at an `include:` node
// with no module, and the included file's pass sees a task that is perfectly
// legal where it is written. Only the spliced result is both.
//
// It is not a formality. `Render` consults [render.IsKeeperTask] on top-level
// tasks ONLY, so a keeper-side module reached through a block is fanned out over
// the roster and dispatched to hosts that have no such module. And the L0 trial
// folds a capture by its module address alone, so the case predicts `state_after`
// exactly as a routed one does and goes GREEN on a plan the run cannot execute —
// the same false green the retired `state_capture_not_on_keeper` used to close
// here by being unconditional.
//
// Runs after expansion, so there are no line/col coordinates left (as with
// [validateFlatTaskAddresses]); the diagnostic addresses by name.
func validateFlatBlockKeeperSide(tasks []Task) []diag.Diagnostic {
	var out []diag.Diagnostic
	for i := range tasks {
		if tasks[i].Block == nil {
			continue
		}
		collectBlockKeeperSide(tasks[i].Block.Block, &out)
	}
	return out
}

// collectBlockKeeperSide walks a block's children (and nested blocks) for
// keeper-side tasks.
func collectBlockKeeperSide(children []Task, out *[]diag.Diagnostic) {
	for i := range children {
		t := &children[i]
		if IsKeeperSideTask(*t) {
			addr := ""
			if t.Module != nil {
				addr = t.Module.Module
			}
			*out = append(*out, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code: "block_on_keeper_invalid",
				Message: fmt.Sprintf("task %q inside a block: is keeper-side (%s) — a block fans its children out over the run's hosts, and the keeper is not one of them",
					t.Name, keeperSideBecause(addr)),
				Hint: "write the keeper-side task flat, in the scenario's own task list, outside the block — including when it reaches the block through an include:",
			})
		}
		if t.Block != nil {
			collectBlockKeeperSide(t.Block.Block, out)
		}
	}
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

// registerScope returns the register names visible to an included body: the
// inherited outer set plus every `register:` declared at this level (recursively
// through block:, which shares the plan's flat address space). The result is a
// fresh map — sibling branches of the expansion must not see each other's
// additions.
//
// Only the includer→included direction is widened. The reverse stays a
// per-file error: see ValidateOptions.OuterRegisters.
func registerScope(outer map[string]bool, tasks []Task) map[string]bool {
	scope := make(map[string]bool, len(outer)+len(tasks))
	for name := range outer {
		scope[name] = true
	}
	collectDeclaredRegisters(tasks, scope)
	return scope
}

// collectDeclaredRegisters fills out with the `register:` names of tasks,
// recursing into block: children.
func collectDeclaredRegisters(tasks []Task, out map[string]bool) {
	for i := range tasks {
		if tasks[i].Register != "" {
			out[tasks[i].Register] = true
		}
		if tasks[i].Block != nil {
			collectDeclaredRegisters(tasks[i].Block.Block, out)
		}
	}
}

type includeExpander struct {
	resolve IncludeResolver
	diags   []diag.Diagnostic
	// destinyTasks — this expansion is inside a DESTINY's `tasks/` tree, so each
	// included body is loaded under [ValidateOptions.DestinyTasks]. Set only by
	// [ExpandIncludesInDestiny]; a scenario's included body must NOT carry it,
	// since a keeper-side address there is correct and ordinary.
	destinyTasks bool
	// modules — the plugin-manifest resolver handed to each included body, so its
	// `params:` are checked the way the including file's were. nil is legal and
	// not silent: the body's own post-pass then reports every plugin module as
	// [DiagPluginParamsUnchecked]. Set by [ExpandIncludesWithModules].
	modules ModuleManifestResolver
	// lastGroupID — id counter for conditional include-groups (carry-through
	// group-drop). Monotonically grows on EVERY include with a non-empty `when:`;
	// 0 is reserved for "outside a conditional include" (Task.IncludeGroupID==0).
	// Nested conditional includes get distinct ids; dropping each is an independent
	// evaluation of its own include-when.
	lastGroupID int
	// seenBodyDiag dedupes a body's OWN diagnostics across include sites. One file
	// included twice — the dispatcher pattern, two branches pulling one provision
	// body — is read and validated twice, and reporting the same defect at the same
	// coordinates twice teaches the reader to skip diagnostics, which is the
	// argument [pluginParamWalk.reported] already makes within a single document.
	//
	// Keyed on the whole diagnostic, not on the body's path: the two sites differ in
	// [ValidateOptions.OuterRegisters], so the same file legitimately produces a
	// DIFFERENT unknown_register_reference at one site and not the other. Collapsing
	// by path would hide that; collapsing byte-identical findings cannot.
	seenBodyDiag map[string]bool
}

// recordBodyDiags appends the diagnostics of one included body, dropping any that
// an earlier include site of the same file already produced verbatim.
func (e *includeExpander) recordBodyDiags(diags []diag.Diagnostic) {
	for _, d := range diags {
		key := strings.Join([]string{
			d.File, strconv.Itoa(d.Line), strconv.Itoa(d.Column),
			string(d.Level), d.Code, d.YAMLPath, d.Message,
		}, "\x00")
		if e.seenBodyDiag[key] {
			continue
		}
		e.seenBodyDiag[key] = true
		e.diags = append(e.diags, d)
	}
}

// expand recursively expands a task list. stack is the active chain of display
// paths (for cycle detection and depth); nil at the top level. ancestorWhen is
// the accumulated include-when of conditional include ancestors (a conjunction
// for cascading drop); "" at the top level and under unconditional includes.
func (e *includeExpander) expand(tasks []Task, stack []string, ancestorWhen string, outer map[string]bool) []Task {
	// scope — the registers an included body may reference: everything declared at
	// THIS level plus everything inherited from the includer chain. Collected over
	// the whole level before the splice loop, so an include placed before the task
	// that declares the register it reads is still legal (resolution goes by name,
	// not by position — see validateTaskRefs).
	scope := registerScope(outer, tasks)

	out := make([]Task, 0, len(tasks))
	for i := range tasks {
		task := tasks[i]

		// Non-include: no splice at this node. A block is expanded in the render
		// phase (renderBlockTask, like loop), but its children are a task list of
		// the same shape — recurse so a within-block include is spliced too. The
		// stack/ancestorWhen are passed UNCHANGED (a block is not an include
		// boundary): cycle/depth detection and the conditional-include cascade
		// behave identically at any nesting depth. BlockTask is copied — the
		// caller's manifest outlives expansion (Trial re-renders it per case) and
		// must not be spliced in place.
		if task.Include == nil {
			if task.Block != nil {
				block := *task.Block
				block.Block = e.expand(task.Block.Block, stack, ancestorWhen, scope)
				task.Block = &block
			}
			out = append(out, task)
			continue
		}

		expanded, ok := e.expandOne(task, stack, ancestorWhen, scope)
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
func (e *includeExpander) expandOne(task Task, stack []string, ancestorWhen string, outer map[string]bool) ([]Task, bool) {
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
				fmt.Sprintf("include %q carries a dynamic when %q (reference to register./soulprint.) - include expands BEFORE stratification, only a static predicate input./vars./incarnation. is available", name, task.When),
				"replace with a static predicate (input./vars./incarnation.) or move the condition onto a module task of the included file via when:")
			return nil, false
		}
		e.lastGroupID++
		groupID = e.lastGroupID
		effectiveWhen = conjoinIncludeWhen(ancestorWhen, task.When)
	}

	data, display, err := e.resolve(name)
	if err != nil {
		e.addError(CodeIncludeResolveFailed, fmt.Sprintf("include %q: %v", name, err), "")
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

	parsed, diags, _ := LoadDestinyTasksFromBytes(display, data, ValidateOptions{
		OuterRegisters:  outer,
		DestinyTasks:    e.destinyTasks,
		ModuleManifests: e.modules,
	})
	// EVERY diagnostic of the body is kept, not only the errors. The error-only
	// filter this replaces was a silent pass over the whole non-fatal half of a
	// body's validation — [DiagPluginParamsUnchecked] and `deprecated_param` are
	// the two that reach it — and silence is indistinguishable from "checked and
	// clean", which is the defect the hint exists to prevent (NIM-779; NIM-778
	// travelled this exact path).
	e.recordBodyDiags(diags)
	if diag.HasErrors(diags) {
		return nil, false
	}

	expanded := e.expand(parsed, append(append([]string(nil), stack...), display), effectiveWhen, outer)
	if groupID != 0 {
		stampIncludeGroup(expanded, effectiveWhen, groupID)
	}
	return expanded, true
}

// includeModifierReason returns a human-readable reason if an include-task
// carries any field besides `include:`/`name:`/`when:`. These fields are allowed
// on an include-task (`when:` — conditional include, ADR-009 amendment); any other
// non-empty scope/control modifier would be lost silently by the splice — so
// expansion rejects it. An empty string means the task is clean.
//
// ⚠ The switch enumerates the FORBIDDEN fields, so a new [Task] field is
// PERMITTED on an include task until someone adds it here — the opposite of the
// "forbidden by default" this comment used to claim. Adding a scope/control field
// to Task means adding a case here in the same commit.
//
// `when:` is NOT in this list (conditional include) — its staticness is checked
// separately by expandOne (IsStaticIncludeWhen → include_when_dynamic_unsupported
// for dynamic). `loop:` stays forbidden: loop on include is not implemented
// (docs↔code drift, docs/destiny/tasks.md §7) → include_modifier_unsupported.
//
// ★ `output:` left the switch in NIM-334. The key is refused on EVERY task kind
// at validation (`output_unsupported`), which runs before expansion, so this case
// could only ever fire second — and its advice ("move the modifier onto a module
// task of the included file") became false the moment a module task stopped
// accepting the key too. One key, one diagnostic, and the surviving one is the
// true one. It comes back here when the output-contract slice makes `output:`
// legal on a module task, because forwarding it through include stays unbuilt.
func includeModifierReason(task Task) string {
	switch {
	case task.Loop != nil:
		return "loop: (slice E)"
	case len(task.Vars) > 0:
		return "vars:"
	case task.Async:
		return "async:"
	case task.Register != "":
		return "register:"
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

// CodeIncludeResolveFailed is raised when an include's TARGET cannot be found —
// the one expansion failure that depends on which levels the caller can see. It
// is named here, beside the call that raises it, because a consumer that has to
// tell "the target was not found" from "the target was found and its body is
// wrong" must ask the producer rather than keep its own copy of the list.
const CodeIncludeResolveFailed = "include_resolve_failed"

// IsIncludeResolveDiag reports whether a diagnostic code out of [ExpandIncludes]
// is about RESOLVING an include target, as opposed to a defect of the include
// node itself or of a body that resolved and was read.
//
// The distinction has exactly one consumer and one purpose: an offline linter
// standing outside a service tree cannot perform the service-level half of the
// resolve, so a target it fails to find may be perfectly findable at the keeper —
// that failure, and only that one, is a deferral rather than a defect. Every other
// expansion error is judged by code that does not care where the file sits:
//
//   - include_modifier_unsupported and include_when_dynamic_unsupported are
//     properties of the include NODE, true wherever it is read;
//   - include_cycle and include_depth_exceeded are properties of files that DID
//     resolve, and outside a service tree strictly FEWER files resolve — so a
//     cycle or an overlong chain found there is a subset of the real one, never
//     an artefact of the missing level;
//   - anything else in the slice came out of parsing a body that was read, at
//     that body's own coordinates.
//
// Treating those as deferrals is [NIM-716]: a real error reported as "does not
// resolve offline" — text that is false, since the include resolved and the file
// was read — with exit 0 behind it.
func IsIncludeResolveDiag(code string) bool {
	return code == CodeIncludeResolveFailed
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
