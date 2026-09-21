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
// The keeper-side case that opened the ticket is now answered by SCOPE rather than
// by a message: `compute` is host-invariant and resolved once per run, so a keeper
// task simply gets it (render.keeperVars). What is left here are the contexts that
// lack it for a structural reason, where no message can be replaced by an
// inclusion.
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
// an `on: keeper` task, a `core.state.<verb>` capture) and the ones that do not (the
// loop axis, `on: [covens]`, the isolated destiny pass, a capture's `match:`). So each
// context declares its own stance in [Vars.ComputeScope], and the guard runs at
// COMPILE — before eval, before any vault() side effect, and on every reference
// position (`compute.x`, `compute['x']`, `has(compute.x)`, `size(compute)`),
// including branches an eval-time sentinel value would never reach.
//
// One stance ([ComputeOutOfScopeFlowControl]) belongs to a context this env never
// sees: `when:`/`changed_when:`/`failed_when:`/`until:` are evaluated by
// [NewFlowControl], which does not declare `compute` and therefore refuses on its
// own. It is named here anyway because soul-lint checks those keys offline, where
// there is no env to do the refusing.

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
// keep working. Instead the contexts that lack the namespace name themselves,
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

	// ComputeOutOfScopeLoopAxis — `loop.items:` / `loop.when:`, the host-invariant
	// loop axis (render.loopInvariantVars).
	ComputeOutOfScopeLoopAxis

	// ComputeOutOfScopeCovenList — `on: [covens]`, resolved once per run
	// (render.resolveCovenList).
	ComputeOutOfScopeCovenList

	// ComputeOutOfScopeDestiny — the isolated destiny pass (ADR-009 V2): a destiny
	// receives run-level values only through `apply: input:`.
	ComputeOutOfScopeDestiny

	// ComputeOutOfScopeStateMatch — the merge-time half of a `core.state.<verb>`
	// capture: the `match:` predicate and a `modify:` `patch:` cell, evaluated once
	// per collection element against elem/key/value and NO scenario context
	// (render.Pipeline.StateOpEvaluators, the same stance ADR-019 takes for
	// migration-CEL). The two are ordinary module params, so `${ compute.x }` in
	// them was already substituted by the render — what reaches merge is the
	// leftover predicate text, and a bare `compute.x` there has nothing to read.
	ComputeOutOfScopeStateMatch

	// ComputeOutOfScopeFlowControl — the flow-control keys `when:` /
	// `changed_when:` / `failed_when:` / `until:`, which are not rendered at all:
	// Keeper copies them into the RenderedTask verbatim and they are evaluated in
	// the Soul-side flow-control sandbox ([NewFlowControl], ADR-012(d)) over a
	// flow_context of input/vars/incarnation/soulprint.self/register.
	//
	// This one is carried by no [Vars] literal, and that is the point of naming it:
	// the sandbox does not declare `compute` at all, so at RUN time cel-go already
	// refuses with its own undeclared-reference error, and a scope stance would be
	// redundant there. Offline is where the name earns its keep — soul-lint has no
	// engine boundary to lean on, so without a stance it would have to either check
	// those keys against the `compute:` block (announcing "the namespace exists
	// here, this name does not" about a namespace that does not exist there — the
	// very inversion this ticket was filed about) or say nothing.
	ComputeOutOfScopeFlowControl
)

// computeScopeCount is the number of stances, so a test can walk every one of them
// instead of a hand-kept list that a new constant silently falls out of.
const computeScopeCount = int(ComputeOutOfScopeFlowControl) + 1

// contextName describes the context in the words the author used to get here
// (the YAML key, not the Go builder). Empty for [ComputeAvailable].
func (s ComputeScope) contextName() string {
	switch s {
	case ComputeOutOfScopeLoopAxis:
		return "loop.items:/loop.when: (the host-invariant loop axis)"
	case ComputeOutOfScopeCovenList:
		return "on: [covens] (resolved once per run, not per host)"
	case ComputeOutOfScopeDestiny:
		return "the isolated destiny pass"
	case ComputeOutOfScopeStateMatch:
		return "a core.state capture's match:/patch: (evaluated per element against elem/key/value)"
	case ComputeOutOfScopeFlowControl:
		return "when:/changed_when:/failed_when:/until: (the Soul-side flow-control sandbox)"
	default:
		return ""
	}
}

// hint is the way out of this particular context — what to write instead.
func (s ComputeScope) hint() string {
	switch s {
	case ComputeOutOfScopeLoopAxis:
		return "the loop axis sees input.*/vars.*/register.*/incarnation.* and soulprint.hosts -- drive the loop from one of those, or build the list inside loop.items: itself"
	case ComputeOutOfScopeCovenList:
		return "build the coven label from input.*/vars.*/incarnation.*"
	case ComputeOutOfScopeDestiny:
		return "a destiny sees run-level computed values only through apply: input: (ADR-009 V2 isolation) -- pass the value in explicitly"
	case ComputeOutOfScopeStateMatch:
		return "match:/patch: see the collection element (elem, or key/value for a map) and nothing else -- write the computed part as ${ compute.<name> }, which the param render substitutes before merge ever reads the text"
	case ComputeOutOfScopeFlowControl:
		return "these predicates run on the Soul side over flow_context (input/vars/incarnation/soulprint.self/register) -- put the computed value in the task's vars: (`n: ${ compute.<name> }`) and write the predicate against vars.n"
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

// ExpressionComputeNames reads the `compute.<name>` selections out of a
// WHOLE-STRING CEL expression: the names the author actually wrote, plus a
// dynamic flag for a reference whose name is NOT in the source
// (`compute[input.key]`, or the bare namespace handed to `size()`).
//
// Exported for soul-lint's `compute_unknown_name`, the rule that catches the
// mistake the old `no such key` message was blamed for and never actually
// diagnosed offline: a name that is not in the scenario's `compute:` block at all.
// Splitting known names from dynamic use is the whole point — a rule that treated
// `compute[k]` as a name would invent one, and one that ignored the flag would
// report every dynamic lookup as a typo.
//
// Unparseable text → no names, not dynamic: a syntax error belongs to the
// compiler, which reports it with a position (as in [Engine.VarRefs]).
func (e *Engine) ExpressionComputeNames(expr string) (names []string, dynamic bool) {
	if !containsIdentText(expr, computeRoot) {
		return nil, false
	}
	parsed, err := e.parseNoMacro(expr)
	if err != nil {
		return nil, false
	}
	return computeNameRefs(parsed.Expr())
}

// InterpolationComputeNames is [Engine.ExpressionComputeNames] over an
// interpolated string (`params:` values, `vars:` values, `loop.items:`): the union
// of the names from every `${ … }` block, dynamic if ANY block is.
func (e *Engine) InterpolationComputeNames(raw string) (names []string, dynamic bool) {
	if !containsIdentText(raw, computeRoot) {
		return nil, false
	}
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return nil, false
	}
	for _, s := range segs {
		if !s.expr {
			continue
		}
		n, d := e.ExpressionComputeNames(s.text)
		names = append(names, n...)
		dynamic = dynamic || d
	}
	return names, dynamic
}

// computeNameRefs walks the parsed expression once, pairing every `compute`
// identifier with the name selected from it. An identifier left unpaired is a use
// whose name the source does not carry → dynamic.
//
// The two readable forms are Select (`compute.x`) and the index operator
// (`compute['x']` with a string literal). `has(compute.x)` and `size(compute)`
// funnel through those same nodes because the parse runs with macros off — the
// first is a call over a Select (readable), the second a call over a bare Ident
// (dynamic), which is exactly the distinction the flag exists to draw.
func computeNameRefs(root ast.Expr) (names []string, dynamic bool) {
	idents := map[int64]bool{} // every `compute` identifier node
	paired := map[int64]bool{} // the ones a name was read from

	ast.PostOrderVisit(root, ast.NewExprVisitor(func(n ast.Expr) {
		switch n.Kind() {
		case ast.IdentKind:
			if n.AsIdent() == computeRoot {
				idents[n.ID()] = true
			}
		case ast.SelectKind:
			s := n.AsSelect()
			if op := s.Operand(); isComputeIdent(op) {
				paired[op.ID()] = true
				names = append(names, s.FieldName())
			}
		case ast.CallKind:
			c := n.AsCall()
			if c.IsMemberFunction() || c.FunctionName() != indexOperator {
				return
			}
			args := c.Args()
			if len(args) != 2 || !isComputeIdent(args[0]) {
				return
			}
			if args[1].Kind() != ast.LiteralKind {
				return // compute[expr] — the name is not in the source
			}
			key, ok := args[1].AsLiteral().Value().(string)
			if !ok {
				return
			}
			paired[args[0].ID()] = true
			names = append(names, key)
		}
	}))

	for id := range idents {
		if !paired[id] {
			return names, true
		}
	}
	return names, false
}

func isComputeIdent(e ast.Expr) bool {
	return e != nil && e.Kind() == ast.IdentKind && e.AsIdent() == computeRoot
}

// indexOperator is cel-go's function name for `a[b]` (common/operators.Index),
// spelled out rather than imported for one constant.
const indexOperator = "_[_]"

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
