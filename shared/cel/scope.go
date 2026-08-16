package cel

import (
	"errors"
	"fmt"

	"github.com/google/cel-go/common/ast"
)

// Namespace scoping — telling "this namespace does not exist HERE" apart from
// "this namespace has no such key" (NIM-619).
//
// They are different mistakes with different fixes, and cel-go cannot tell them
// apart on its own: a context that omits a namespace still hands the evaluator an
// empty map ([Vars.activation]), so `compute.topology_node_count` in an
// `on: keeper` task used to fail with `no such key: topology_node_count` — naming
// a key that is not the problem and sending the author hunting for a typo in a
// name that is spelled correctly. It would have failed identically on ANY name.
// Worse, the guard forms were silent: `has(compute.x)` evaluated false and
// `size(compute)` returned 0, with no error at all.
//
// The fix follows the two precedents already in this package rather than adding a
// third mechanism:
//
//   - `soulprint.hosts` outside the scenario pass — [Vars.AllowHosts], cut at
//     compile (hosts.go) with an explicit "scenario-only" feature string;
//   - `register`/`soulprint`/`input` in migration-CEL, `compute` in a
//     `vars/_stack.yaml` step — simply NOT declared in the env ([migrationVars],
//     [serviceVarsVars]), so cel-go itself reports an undeclared reference.
//
// `compute` cannot use non-declaration: one env ([contextVars]) serves both the
// contexts that have the namespace (a Soul-side task's params:/where:/apply.input,
// state_changes) and the ones that do not (an `on: keeper` task, the loop axis,
// `on: [covens]`, the isolated destiny pass). So each context declares its own
// stance in [Vars.ComputeScope], and the guard runs at COMPILE — before eval,
// before any vault() side effect, and on every reference position (`compute.x`,
// `compute['x']`, `has(compute.x)`, `size(compute)`), including branches an
// eval-time sentinel value would never reach.

// computeRoot is the CEL identifier of the scenario-level `compute:` namespace
// (ADR-009 amendment 2026-06-23). Also a reserved `loop.as:`/`loop.index_as:` name
// (shared/config.loopReservedNames) — a loop variable may not shadow it.
const computeRoot = "compute"

// ComputeScope is a context's stance on the `compute` namespace: either it exists
// here (the zero value) or it does not, and then WHICH context the author is
// standing in — the part the old `no such key` message could not say.
//
// The zero value is [ComputeAvailable] deliberately. The alternative polarity
// (default "absent", opt in where present) would rewrite the honest
// undeclared-reference errors that the restricted engines ([NewMigration],
// [NewFlowControl], [NewServiceVars]) already produce by non-declaration, and
// every ad-hoc Vars literal outside the render package would have to opt in to
// keep working. Instead the four contexts that lack the namespace name themselves,
// and keeper/internal/render carries a guard test (compute_scope_guard_test.go)
// asserting that EVERY context builder in the package declares a stance — so a new
// builder cannot silently inherit "available" the way the ones in this ticket
// silently inherited an empty map.
type ComputeScope uint8

const (
	// ComputeAvailable — `compute.<name>` resolves against [Vars.Compute] and a
	// missing name is an ordinary no-such-key (the author really did mistype, or
	// the `compute:` block really has no such entry).
	ComputeAvailable ComputeScope = iota

	// ComputeOutOfScopeKeeperTask — an `on: keeper` task: its params: and its own
	// vars: render in the run-level keeper context (render.keeperVars).
	ComputeOutOfScopeKeeperTask

	// ComputeOutOfScopeLoopAxis — `loop.items:` / `loop.when:`, the host-invariant
	// loop axis (render.loopInvariantVars).
	ComputeOutOfScopeLoopAxis

	// ComputeOutOfScopeCovenList — `on: [covens]`, resolved once per run
	// (render.resolveCovenList).
	ComputeOutOfScopeCovenList

	// ComputeOutOfScopeDestiny — the isolated destiny pass (ADR-009 V2): a destiny
	// receives run-level values only through `apply: input:`.
	ComputeOutOfScopeDestiny

	// ComputeOutOfScopeStateMatch — `state_changes: add: match:`, where identity is
	// a pure function of the two bound elements and NO scenario context is available
	// (render.EvalStateMatch, the same stance ADR-019 takes for migration-CEL).
	ComputeOutOfScopeStateMatch
)

// contextName describes the context in the words the author used to get here
// (the YAML key, not the Go builder). Empty for [ComputeAvailable].
func (s ComputeScope) contextName() string {
	switch s {
	case ComputeOutOfScopeKeeperTask:
		return "an on: keeper task (its params: and vars: render in the run-level keeper context)"
	case ComputeOutOfScopeLoopAxis:
		return "loop.items:/loop.when: (the host-invariant loop axis)"
	case ComputeOutOfScopeCovenList:
		return "on: [covens] (resolved once per run, not per host)"
	case ComputeOutOfScopeDestiny:
		return "the isolated destiny pass"
	case ComputeOutOfScopeStateMatch:
		return "state_changes add: match: (identity is a pure function of elem and value)"
	default:
		return ""
	}
}

// hint is the way out of this particular context — what to write instead.
func (s ComputeScope) hint() string {
	switch s {
	case ComputeOutOfScopeKeeperTask:
		return "compute: is in scope for the Soul-side contexts only (a task's params:/where:/apply.input and state_changes) -- build the value here from input.*/vars.*/register.*, or move the step Soul-side"
	case ComputeOutOfScopeLoopAxis:
		return "the loop axis sees input.*/vars.*/register.*/incarnation.* and soulprint.hosts -- drive the loop from one of those, or build the list inside loop.items: itself"
	case ComputeOutOfScopeCovenList:
		return "build the coven label from input.*/vars.*/incarnation.*"
	case ComputeOutOfScopeDestiny:
		return "a destiny sees run-level computed values only through apply: input: (ADR-009 V2 isolation) -- pass the value in explicitly"
	case ComputeOutOfScopeStateMatch:
		return "match: compares the element already in state (elem) with the one being added (value) and sees nothing else -- fold the computed part into the value itself, then match on the rendered result"
	default:
		return ""
	}
}

// Describe exposes the two human-facing halves of a scope for a caller that is
// building its own diagnostic rather than raising [ErrOutOfScope] — soul-lint's
// offline `compute_out_of_scope`, which has a YAML path to add and no expression
// to quote. Sharing them keeps the offline rule and the runtime guard saying the
// same thing about the same context; two hand-written copies would drift on the
// first wording change. [ComputeAvailable] returns two empty strings.
func (s ComputeScope) Describe() (context, hint string) {
	return s.contextName(), s.hint()
}

// cacheTag discriminates the compile-cache key ([Engine.compile]). ONE tag for all
// out-of-scope contexts, not one per context: for an expression that does NOT
// touch the namespace the compiled program is identical everywhere (the guard only
// ever rejects, never rewrites), so per-context tags would fragment the cache of
// every ordinary keeper-side expression for nothing. The tag is nevertheless
// REQUIRED: without it `compute.x`, compiled and cached in a context that has the
// namespace, would be served straight from the cache in one that does not — the
// lookup happens before any guard, so the check would be bypassed entirely.
func (s ComputeScope) cacheTag() string {
	if s == ComputeAvailable {
		return ""
	}
	return "\x03"
}

// ErrNamespaceOutOfScope — sentinel for errors.Is on [ErrOutOfScope]. Lets a
// caller (and a guard test) tell "the namespace is absent here" from an ordinary
// [ErrEval] no-such-key without matching message text.
var ErrNamespaceOutOfScope = errors.New("context namespace is not in scope here")

// ErrOutOfScope — the expression references a context namespace that this
// evaluation context does not have AT ALL. Distinct from [ErrEval] (the namespace
// is here, the key is not) and from [ErrCompile] (cel-go does not know the name in
// any context of this engine). Raised at compile, so it fires on every reference
// position and on branches that would never be evaluated.
type ErrOutOfScope struct {
	Expr      string
	Namespace string
	Context   string
	Hint      string
}

func (e *ErrOutOfScope) Error() string {
	return fmt.Sprintf(
		"CEL out of scope %q: the %s namespace does not exist in %s -- the whole namespace is absent here, not just this name; %s",
		e.Expr, e.Namespace, e.Context, e.Hint,
	)
}

func (e *ErrOutOfScope) Unwrap() error { return ErrNamespaceOutOfScope }

// guardComputeScope rejects a `compute` reference in a context that has no such
// namespace ([ComputeScope]). Called from [Engine.compile] after the cache lookup
// (the scope is part of the key, see [ComputeScope.cacheTag]) and after
// guardUnsupported.
//
// Cheap text pre-filter first, then the AST — `compute` inside a CEL string
// literal (`soulprint.hosts.where("compute == 4")`, where it is a host field, not
// this namespace) must not trigger it, and neither must the word appearing in
// literal text around a `${ … }` block (which never reaches here anyway: each
// block is compiled on its own).
//
// A loop variable named `compute` shadows the namespace in the activation
// ([Vars.activation] merges Loop last), so a declared loop name suppresses the
// guard. shared/config forbids that name for `loop.as:`/`loop.index_as:`, so this
// is unreachable from YAML — it keeps a programmatic caller honest.
//
// Known coarseness: an identifier `compute` bound by a comprehension inside the
// expression (`[1].map(compute, compute)`) is read as the namespace and rejected.
// Naming an iteration variable after a context namespace is already forbidden for
// `loop:`; the diagnostic is loud rather than silent, which is the direction this
// guard exists to move in.
func (e *Engine) guardComputeScope(expr string, scope ComputeScope, loopNames []string) error {
	if scope == ComputeAvailable || !containsIdentText(expr, computeRoot) {
		return nil
	}
	for _, n := range loopNames {
		if n == computeRoot {
			return nil
		}
	}
	parsed, perr := e.parseNoMacro(expr)
	if perr != nil {
		// A syntax error is not ours to report: env.Compile says it better, with a
		// position. Mirrors VarRefs/DetectSealed, which also skip unparseable text.
		return nil
	}
	if !referencesIdent(parsed.Expr(), computeRoot) {
		return nil
	}
	return &ErrOutOfScope{
		Expr:      expr,
		Namespace: computeRoot,
		Context:   scope.contextName(),
		Hint:      scope.hint(),
	}
}

// ExpressionReferencesCompute reports whether a WHOLE-string CEL expression (an
// expression key: `loop.when:`, `where:`, `when:`) reads the `compute` namespace.
// Exported for the offline half of this rule (soul-lint's compute_out_of_scope),
// so the linter and [Engine.guardComputeScope] answer the same question through
// the same AST walk — a divergence between them would be a rule that flags what
// the runtime allows, or stays quiet on what it rejects.
//
// Unparseable text → false: a syntax error belongs to the compiler, which reports
// it with a position (mirrors [Engine.VarRefs] / DetectSealed).
func (e *Engine) ExpressionReferencesCompute(expr string) bool {
	if !containsIdentText(expr, computeRoot) {
		return false
	}
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return false
	}
	return referencesIdent(parsed.Expr(), computeRoot)
}

// InterpolationReferencesCompute reports whether an interpolated string
// (`params:` values, `on:` elements, `loop.items:`, `vars:` values) reads the
// `compute` namespace inside a `${ … }` block. The AST walk is what makes the
// answer honest: the word in literal text around a block ("compute the digest")
// and inside a CEL string literal (`soulprint.hosts.where("compute == 4")`, where
// it names a host field) are both correctly NOT references.
//
// A raw string with no blocks, or one whose blocks do not parse, → false (same
// reasoning as [Engine.ExpressionReferencesCompute]).
func (e *Engine) InterpolationReferencesCompute(raw string) bool {
	if !containsIdentText(raw, computeRoot) {
		return false
	}
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return false
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		if e.ExpressionReferencesCompute(s.text) {
			return true
		}
	}
	return false
}

// containsIdentText is the hot-path pre-filter: does the raw text contain ident as
// a whole word? Substring-only would send `computed_at` / `my.compute` through the
// parser on every keeper-side expression; a false positive here costs a parse, a
// false negative would lose the diagnostic, so the boundary test is conservative
// (an adjacent identifier character or a leading dot disqualifies a hit, nothing
// else does).
func containsIdentText(expr, ident string) bool {
	for i := 0; i+len(ident) <= len(expr); i++ {
		if expr[i:i+len(ident)] != ident {
			continue
		}
		if i > 0 && (isIdentByte(expr[i-1]) || expr[i-1] == '.') {
			continue
		}
		if j := i + len(ident); j < len(expr) && isIdentByte(expr[j]) {
			continue
		}
		return true
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// referencesIdent reports whether the parsed expression reads the bare identifier
// ident anywhere. Every reference form funnels through an IdentKind node —
// `compute.x` is Select(operand=Ident), `compute['x']` is a `_[_]` call over
// Ident, `has(compute.x)` and `size(compute)` are calls over one of those — so a
// single Ident test covers them all, including the two silent forms. String
// literals are Constants, not Idents: nested `.where("…")` CEL is not descended
// into (it is a separate parse with a different root set, hosts.go).
func referencesIdent(root ast.Expr, ident string) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(n ast.Expr) {
		if n.Kind() == ast.IdentKind && n.AsIdent() == ident {
			found = true
		}
	}))
	return found
}
