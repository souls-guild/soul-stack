package config

// The `incarnation` half of the `validate:` context, and the boundary that keeps
// "this path has no incarnation yet" from reading as "the incarnation has no such
// field" (NIM-833).
//
// `validate:` is the STATIC check over what arrived on the request: everything
// decidable from the request is decided here, before a single task runs. Its
// context is `input` + `incarnation` and nothing else. `vars` are the service's
// parameters rather than the request, so a rule that needs them is not checking
// the request and does not live here; `compute` resolves inside the run, which is
// later than this point by construction.
//
// The two request paths know different things, and that difference is the whole
// reason this file exists:
//
//   - day-2: the incarnation row is loaded, so `incarnation.*` reads it;
//   - create: the incarnation does not exist yet. Only the identity the request
//     itself carries is knowable — and when the scenario composes its id from
//     `input:` (`id.template`, ADR-0079) not even that, because the id is composed
//     from the very input this gate is still resolving.
//
// Substituting an empty map for the absent half is the failure this exists to
// prevent. A rule reading `incarnation.state` on the create path would then
// evaluate against nothing and PASS, and the scenario author would be looking at a
// green check that checks nothing — worse than no rule, because it is believed.
// So the stance is structural, following [cel.ComputeScope] (shared/cel/scope.go):
// each caller declares what its path knows, and a reference outside that set is
// refused at COMPILE. Compile rather than eval for the reason NIM-619 recorded:
// `has(incarnation.state)` and `size(incarnation)` are silent against any
// eval-time sentinel — the first yields false, the second a count — and those are
// exactly the forms a "check it only if present" rule is written in.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/common"
	celast "github.com/google/cel-go/common/ast"
)

// incarnationRoot is the CEL identifier of the incarnation namespace.
const incarnationRoot = "incarnation"

// IncarnationScope is a caller's stance on the `incarnation` namespace inside
// `validate:`: which of the incarnation's facts this point on the path can
// honestly answer for.
//
// The zero value is [IncarnationAbsent] — the most restrictive one, the opposite
// polarity from [cel.ComputeScope]. That is deliberate and the two differ for a
// reason: `compute` existed in every context before its scope type did, so
// defaulting to "absent" there would have rewritten honest errors. `incarnation`
// is new to this env, no caller has it until one says so, and a context that
// silently inherits the default gets a loud refusal rather than a false green.
type IncarnationScope uint8

const (
	// IncarnationAbsent — the context carries no incarnation at all and names no
	// more specific reason. Any `incarnation.*` reference is refused.
	IncarnationAbsent IncarnationScope = iota

	// IncarnationOutOfScopeDestiny — the isolated destiny pass (ADR-009 V2): a
	// destiny receives run-level values only through `apply: input:`, so its own
	// `validate:` rules check that input and nothing about the incarnation running
	// them.
	IncarnationOutOfScopeDestiny

	// IncarnationOutOfScopeTrial — the offline L0 trial harness, which resolves a
	// case's input contract with no keeper and no row behind it.
	IncarnationOutOfScopeTrial

	// IncarnationRequested — the create path, where the operator supplied the id:
	// the identifier is knowable (it is in the request), the rest of the
	// incarnation is not, because it does not exist yet.
	IncarnationRequested

	// IncarnationComposed — the create path of a scenario that composes its own id
	// from `input:` (`id.template`, ADR-0079). Not even the identifier is knowable
	// here: it is a function of the input this gate is still resolving and is
	// composed after it returns.
	IncarnationComposed

	// IncarnationLoaded — day-2: the incarnation row is loaded and `validate:`
	// reads the same facts the run will.
	IncarnationLoaded
)

// contextName describes the context in the words the author used to get here (the
// path, not the Go caller).
func (s IncarnationScope) contextName() string {
	switch s {
	case IncarnationOutOfScopeDestiny:
		return "the isolated destiny pass"
	case IncarnationOutOfScopeTrial:
		return "an offline L0 trial case"
	case IncarnationRequested:
		return "the create path (the incarnation does not exist yet)"
	case IncarnationComposed:
		return "the create path of a scenario that composes its own id from input:"
	case IncarnationLoaded:
		return "the day-2 pre-flight gate"
	default:
		return "a context with no incarnation"
	}
}

// hint is the way out of this particular context — what to write instead.
func (s IncarnationScope) hint() string {
	switch s {
	case IncarnationOutOfScopeDestiny:
		return "a destiny sees run-level values only through apply: input: (ADR-009 V2 isolation) -- pass the value in explicitly and check input.* here"
	case IncarnationOutOfScopeTrial:
		return "an L0 case has no incarnation behind it; a rule that needs one can only be exercised against a real create or run"
	case IncarnationRequested:
		return "on create only the identifier exists; a rule about the incarnation's state or history belongs on a day-2 scenario, and a topology or roster question belongs in assert:"
	case IncarnationComposed:
		return "the id here IS the input it is composed from -- write the rule over the input.* components that feed id.template"
	default:
		return "check input.* instead, or move the rule to a scenario whose path has the fact"
	}
}

// ValidateContext is the `incarnation` half of the `validate:` activation: the
// caller's stance, the fields that stance ANSWERS FOR, and the values the row
// happens to carry.
//
// The last two are separate on purpose, and conflating them was a bug. What a rule
// may READ is a property of the PATH and must be the same for every request on it;
// what the namespace CONTAINS is a property of one row. Deriving the first from the
// second made `incarnation.state.x` refuse for an incarnation whose state column is
// NULL and pass for its sibling — the same scenario, diagnosed as broken on one
// request and fine on the next. Now `state` is in scope on every day-2 request, and
// an empty state answers the ordinary no-such-key the RUN would answer, which is
// the honest report of an empty row.
//
// All three fields are unexported and set only through the constructors below, so a
// stance and its sets cannot disagree — "Loaded, but answering for nothing" is the
// shape of the bug this whole file is about, and it is not expressible.
//
// The zero value is the input-only context `validate:` had before NIM-833: no
// incarnation, every `incarnation.*` reference refused.
type ValidateContext struct {
	scope IncarnationScope
	known map[string]bool
	inc   map[string]any
}

// RequestedIncarnation is the create-path context: the operator supplied the id,
// which is the one incarnation fact a create request carries. An empty id is not a
// fact, so it degrades to the zero value rather than putting "" in the namespace —
// an empty string would satisfy a length or prefix rule and report a pass.
//
// The caller passes the id it received; the ADR-0085 `incarnation.name` alias is
// its own concern (see the keeper-side builders, which route both spellings
// through shared/cel.IncarnationRoot).
func RequestedIncarnation(id string) ValidateContext {
	if id == "" {
		return ValidateContext{}
	}
	return ValidateContext{
		scope: IncarnationRequested,
		known: map[string]bool{"id": true, "name": true},
		inc:   map[string]any{"id": id, "name": id},
	}
}

// ComposedIncarnation is the create-path context of a scenario carrying
// `id.template` (ADR-0079): the id is composed from the resolved input AFTER this
// gate, so nothing about the incarnation is knowable yet.
func ComposedIncarnation() ValidateContext {
	return ValidateContext{scope: IncarnationComposed}
}

// LoadedIncarnation is the day-2 context. known is what this path ANSWERS FOR —
// fixed, the same for every request — and fields is what THIS row carries, which
// may be a strict subset (a NULL column). A name in fields but not in known is
// dropped from the namespace rather than left silently readable, so the two can
// only disagree in the safe direction.
//
// The keeper-side builder keeps known a SUBSET of what the run's `incarnation.*`
// carries, so a rule that passes pre-flight cannot read a field the run then lacks.
//
// An empty known set degrades to the zero value: a day-2 stance answering for
// nothing is the false-green shape, not a day-2 stance.
func LoadedIncarnation(known []string, fields map[string]any) ValidateContext {
	if len(known) == 0 {
		return ValidateContext{}
	}
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[k] = true
	}
	inc := make(map[string]any, len(fields))
	for k, v := range fields {
		if set[k] {
			inc[k] = v
		}
	}
	return ValidateContext{scope: IncarnationLoaded, known: set, inc: inc}
}

// OutOfScopeIncarnation is the context of a caller that has no incarnation and can
// say WHICH context that is — the isolated destiny pass, an L0 trial case. Any
// stance that carries facts is refused into the zero value: this constructor exists
// to name an absence, and it must not be a second way to declare a presence.
func OutOfScopeIncarnation(s IncarnationScope) ValidateContext {
	switch s {
	case IncarnationOutOfScopeDestiny, IncarnationOutOfScopeTrial:
		return ValidateContext{scope: s}
	default:
		return ValidateContext{}
	}
}

// WithComposedID withdraws a create request's identity because the scenario
// composes its own id (ADR-0079). Only the manifest says so, and the caller that
// built the context has not read it — [ValidateInput] applies this after loading
// the scenario, which is the first point where the fact is known.
//
// It replaces the stance rather than downgrading [IncarnationRequested], because on
// the real path there is nothing to downgrade: a composing scenario REFUSES a
// request that carries an id (422 id_not_composable), so the create caller arrives
// here with an empty one and the zero value. Keying off IncarnationRequested made
// this a no-op on every request an operator actually sends, and the composed hint —
// the one that says the id IS the input it is composed from — was unreachable.
//
// A day-2 stance is returned unchanged: the row exists there whatever the manifest
// says about composing, and this must not blind a run.
func (c ValidateContext) WithComposedID() ValidateContext {
	if c.scope == IncarnationLoaded {
		return c
	}
	return ComposedIncarnation()
}

// Scope reports the stance, for a caller building its own diagnostic or a guard
// test enumerating the contexts.
func (c ValidateContext) Scope() IncarnationScope { return c.scope }

// activation builds the map for prg.Eval. The `incarnation` key is always present
// and is empty in the stances that answer for nothing — which is safe here for the
// reason it is safe for `compute` (shared/cel.Vars.activation): a reference in
// those stances is cut off before eval by [ValidateContext.guard] and never reaches
// this map, so the empty map means only "this rule does not read the namespace".
//
// Within a stance that DOES answer for a field, a value the row does not carry is
// an ordinary no-such-key — the same answer the run gives for a NULL column, and
// the reason known and inc are separate.
func (c ValidateContext) activation(merged map[string]any) map[string]any {
	inc := c.inc
	if inc == nil {
		inc = map[string]any{}
	}
	return map[string]any{"input": merged, incarnationRoot: inc}
}

// knownFields is the set of `incarnation.<field>` names this stance answers for,
// sorted so a refusal message is byte-identical for identical state.
func (c ValidateContext) knownFields() []string {
	out := make([]string, 0, len(c.known))
	for k := range c.known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// guard refuses a rule predicate that reads an `incarnation` field this stance does
// not have. Called by [EvalValidateRules] BEFORE the rule is evaluated, so the
// refusal fires on every reference position — including the branches an evaluation
// would never reach and the `has()`/`size()` forms an eval-time sentinel is silent
// on.
//
// A predicate that does not PARSE is not refused here: the compile step reports a
// syntax error with a position, and it says it better (mirrors shared/cel's guards
// and [IDTemplateInputRefs]). Nothing slips through that way — the compile runs
// with macros ON and therefore accepts strictly less than the macro-free parse, so
// a predicate the compiler accepts is one this walk has already seen. A failure to
// BUILD the parser is a different thing and is returned: it would otherwise
// disable the guard for every rule in the process.
func (c ValidateContext) guard(expr string) error {
	refs, err := incarnationFieldRefs(expr)
	if err != nil {
		return err
	}
	if refs.shadowed {
		return &IncarnationScopeError{
			Expr: expr, Context: c.scope.contextName(),
			Hint:  "a comprehension variable is named `incarnation`, which shadows the namespace and makes the rule unreadable to this guard -- rename the iteration variable (the same name is already forbidden for loop.as:)",
			Known: c.knownFields(),
		}
	}
	if len(refs.fields) == 0 && !refs.dynamic {
		return nil
	}
	if refs.dynamic {
		return &IncarnationScopeError{
			Expr: expr, Context: c.scope.contextName(),
			Hint:  "address the field by name (incarnation.<field>): a dynamic lookup or a bare `incarnation` cannot be checked against what this path knows",
			Known: c.knownFields(),
		}
	}
	for _, f := range refs.fields {
		if !c.known[f] {
			return &IncarnationScopeError{
				Expr: expr, Field: f,
				Context: c.scope.contextName(),
				Hint:    c.scope.hint(),
				Known:   c.knownFields(),
			}
		}
	}
	return nil
}

// ErrIncarnationNotInScope — sentinel for errors.Is on [IncarnationScopeError].
// Lets a caller (and a guard test) tell "this path does not have that fact" from an
// ordinary eval failure without matching message text.
var ErrIncarnationNotInScope = errors.New("config: validate: rule reads an incarnation fact this path does not have")

// IncarnationScopeError — a `validate:` rule reads `incarnation.<field>` in a
// context that cannot answer for it. Distinct from a no-such-key at evaluation
// (the namespace is here, the field is not) and from a compile error (cel-go does
// not know the name at all).
type IncarnationScopeError struct {
	Expr    string
	Field   string // empty for a dynamic lookup or a bare `incarnation`
	Context string
	Hint    string
	Known   []string
}

func (e *IncarnationScopeError) Error() string {
	what := "reads the incarnation namespace"
	if e.Field != "" {
		what = fmt.Sprintf("reads incarnation.%s", e.Field)
	}
	return fmt.Sprintf("validate rule %q %s, which %s cannot answer for (available here: %s); %s",
		e.Expr, what, e.Context, knownList(e.Known), e.Hint)
}

func (e *IncarnationScopeError) Unwrap() error { return ErrIncarnationNotInScope }

func knownList(known []string) string {
	if len(known) == 0 {
		return "nothing — the namespace is empty in this context"
	}
	return strings.Join(known, ", ")
}

// incarnationFieldRefs reads the `incarnation.<field>` selections out of a rule
// predicate: the fields the author actually named, plus a dynamic flag for a
// reference whose field is NOT in the source (`incarnation[input.k]`, or the bare
// namespace handed to `size()`).
//
// AST-based rather than textual for the reason every reference rule in this
// package is: `incarnation.state` inside a CEL string literal is a constant and
// `incarnation` in a `message:` is prose, and a regex would disagree with the
// engine in exactly those places.
//
// The parse runs with macros OFF ([noMacroParserInstance]), which is what makes
// `has(incarnation.state)` visible: with macros on it expands into a test-only
// select that a reference walk has to special-case, and NIM-619 is the record of
// what happens when that form goes unchecked. The price is that a comprehension's
// bound variable is an ordinary argument here rather than a scoped name, so the one
// case where that matters — a variable actually NAMED `incarnation` — is detected
// and reported (shadowed) instead of being read as a namespace access.
//
// A parse failure yields no references and no error: the compile step reports a
// syntax error with a position. A failure to BUILD the parser IS returned — it
// would otherwise silently disable the guard.
func incarnationFieldRefs(expr string) (incRefHit, error) {
	incRefMu.RLock()
	hit, ok := incRefs[expr]
	incRefMu.RUnlock()
	if ok {
		return hit, nil
	}

	p, err := noMacroParserInstance()
	if err != nil {
		return incRefHit{}, err
	}
	parsed, iss := p.Parse(common.NewTextSource(expr))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return incRefHit{}, nil
	}

	idents := map[int64]bool{} // every `incarnation` identifier node
	paired := map[int64]bool{} // the ones a field was read from
	var out incRefHit
	seen := map[string]bool{}

	celast.PostOrderVisit(parsed.Expr(), celast.NewExprVisitor(func(n celast.Expr) {
		switch n.Kind() {
		case celast.IdentKind:
			if n.AsIdent() == incarnationRoot {
				idents[n.ID()] = true
			}
		case celast.SelectKind:
			sel := n.AsSelect()
			op := sel.Operand()
			if !isIncarnationIdentNode(op) {
				return
			}
			paired[op.ID()] = true
			if !seen[sel.FieldName()] {
				seen[sel.FieldName()] = true
				out.fields = append(out.fields, sel.FieldName())
			}
		case celast.CallKind:
			c := n.AsCall()
			if bindsIncarnationName(c) {
				out.shadowed = true
				return
			}
			if c.IsMemberFunction() || c.FunctionName() != indexOperator {
				return
			}
			args := c.Args()
			if len(args) != 2 || !isIncarnationIdentNode(args[0]) {
				return
			}
			if args[1].Kind() != celast.LiteralKind {
				return // incarnation[expr] — the field is not in the source
			}
			key, isStr := args[1].AsLiteral().Value().(string)
			if !isStr {
				return
			}
			paired[args[0].ID()] = true
			if !seen[key] {
				seen[key] = true
				out.fields = append(out.fields, key)
			}
		}
	}))

	for id := range idents {
		if !paired[id] {
			out.dynamic = true
			break
		}
	}

	incRefMu.Lock()
	incRefs[expr] = out
	incRefMu.Unlock()
	return out, nil
}

// ValidateRuleInputRefs reads the `input.<name>` components a `validate:` predicate
// names, plus a dynamic flag for a reference whose name is NOT in the source
// (`input[k]`, or the bare namespace handed to `size()`).
//
// Exported for the PUBLISHING side (keeper/internal/artifact), which has to answer
// "does this rule's text name a field that was stripped from the published schema".
// AST-based, so `input.password` inside a CEL string constant is not a reference
// and the word in a `message:` is prose — the same reason [IDTemplateInputRefs] is.
//
// A predicate that does not parse reports dynamic: the caller's decision is
// fail-closed, and "I could not read this" must not come back as "it names
// nothing".
func ValidateRuleInputRefs(expr string) (names []string, dynamic bool) {
	p, err := noMacroParserInstance()
	if err != nil {
		return nil, true
	}
	parsed, iss := p.Parse(common.NewTextSource(expr))
	if iss != nil && len(iss.GetErrors()) > 0 {
		return nil, true
	}

	idents := map[int64]bool{}
	paired := map[int64]bool{}
	seen := map[string]bool{}

	celast.PostOrderVisit(parsed.Expr(), celast.NewExprVisitor(func(n celast.Expr) {
		switch n.Kind() {
		case celast.IdentKind:
			if n.AsIdent() == inputRootName {
				idents[n.ID()] = true
			}
		case celast.SelectKind:
			sel := n.AsSelect()
			op := sel.Operand()
			if op == nil || op.Kind() != celast.IdentKind || op.AsIdent() != inputRootName {
				return
			}
			paired[op.ID()] = true
			if !seen[sel.FieldName()] {
				seen[sel.FieldName()] = true
				names = append(names, sel.FieldName())
			}
		case celast.CallKind:
			c := n.AsCall()
			if c.IsMemberFunction() || c.FunctionName() != indexOperator {
				return
			}
			args := c.Args()
			if len(args) != 2 || args[0].Kind() != celast.IdentKind || args[0].AsIdent() != inputRootName {
				return
			}
			if args[1].Kind() != celast.LiteralKind {
				return // input[expr] — the name is not in the source
			}
			key, isStr := args[1].AsLiteral().Value().(string)
			if !isStr {
				return
			}
			paired[args[0].ID()] = true
			if !seen[key] {
				seen[key] = true
				names = append(names, key)
			}
		}
	}))

	for id := range idents {
		if !paired[id] {
			return names, true
		}
	}
	return names, false
}

// inputRootName is the CEL identifier of the input namespace.
const inputRootName = "input"

// indexOperator is cel-go's function name for `a[b]` (common/operators.Index),
// spelled out rather than imported for one constant.
const indexOperator = "_[_]"

// comprehensionMacros are the cel-go macros that BIND their first argument as an
// iteration variable. With macros off they are plain member calls, so the binding
// is visible as an Ident in argument position — which is the only reason a
// shadowing name can be spotted at all here.
var comprehensionMacros = map[string]bool{
	"all": true, "exists": true, "exists_one": true, "map": true, "filter": true,
}

// bindsIncarnationName reports whether this call binds an iteration variable named
// `incarnation`. Such an expression is legal CEL and evaluates against the bound
// variable rather than the namespace, but the walk cannot tell the two apart, so it
// is refused with a message asking for a rename rather than guessed at in either
// direction — guessing "not a reference" is the false green this file exists to
// prevent.
func bindsIncarnationName(c celast.CallExpr) bool {
	if !c.IsMemberFunction() || !comprehensionMacros[c.FunctionName()] {
		return false
	}
	args := c.Args()
	return len(args) >= 2 && isIncarnationIdentNode(args[0])
}

func isIncarnationIdentNode(e celast.Expr) bool {
	return e != nil && e.Kind() == celast.IdentKind && e.AsIdent() == incarnationRoot
}

// incRefs caches the reference extraction by predicate text. The answer depends on
// the expression alone, not on the stance asking, so one entry serves every path a
// rule is evaluated on.
type incRefHit struct {
	fields   []string
	dynamic  bool
	shadowed bool
}

var (
	incRefMu sync.RWMutex
	incRefs  = map[string]incRefHit{}
)
