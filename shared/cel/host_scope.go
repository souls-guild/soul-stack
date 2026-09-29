package cel

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Host scoping — telling "this context stands on a host" apart from "this context
// decides once for the whole run" (NIM-908).
//
// The two are not interchangeable and the language never said which one an author
// was in. `apply: input:` used to render once, in the environment of the first
// targeted host by SID, and hand the result to every host: nine hosts all received
// the first one's address, silently, and the run went green. Since NIM-908 that
// input renders PER HOST, which fixes the value and moves the question one level
// down — to the contexts that legitimately have no host to render for.
//
// There are two ways a per-host value reaches such a context, and this file refuses
// both:
//
//   - directly, `soulprint.self.<path>` where no host is bound. Today that is a
//     bare `no such key: sid` (the contexts omit [Vars.SoulprintSelf], and
//     [Vars.activation] substitutes an empty map) — the NIM-619 shape, a sentence
//     about a key that is spelled correctly and exists everywhere else;
//   - indirectly, `input.<name>` for a name whose `apply: input:` expression reads
//     a per-host root. The value is real, it is just SOMEBODY ELSE'S. That is the
//     original defect wearing one more layer, and the layer is what made it
//     invisible: nothing in the referencing text says the name is host-variant.
//
// The mechanism follows [ComputeScope] rather than adding a third one: each context
// declares its stance in [Vars.HostScope], the guard runs at COMPILE — before eval,
// before any vault() side effect, on every reference position and on branches an
// eval-time sentinel would never reach — and keeper/internal/render carries a guard
// test asserting that every context builder in the package states a stance, so a
// new builder cannot inherit [HostBound] in silence.
//
// Why the refusal and not per-host evaluation: a host-free context is host-free for
// a structural reason, not an accidental one. `loop:` fans out at render into the
// flat task layer and its iteration set must be the same on every host, `on:`
// selects covens once per run, and an `on: keeper` task has no roster at all. Each
// of those would have to become per-host DISPATCH to answer a per-host expression,
// which is open Q #25 and its own ADR.
type HostScope uint8

const (
	// HostBound — a host is bound: `soulprint.self` is that host's facts and
	// `input.<name>` is that host's rendering of it. Every per-host context (a
	// task's params/where/vars, a destiny's vars.yml, `apply: input:` itself).
	//
	// The zero value, for the same reason [ComputeAvailable] is: the alternative
	// polarity would make every ad-hoc Vars literal outside this package opt in to
	// keep working, and would rewrite the honest undeclared-reference errors the
	// restricted engines already produce by non-declaration.
	HostBound HostScope = iota

	// HostFreeLoopAxis — `loop.items:` / `loop.when:` (render.loopInvariantVars).
	// The loop expands at render into N tasks that every targeted host runs, so the
	// iteration set cannot depend on who is running it.
	HostFreeLoopAxis

	// HostFreeCovenList — `on: [covens]` (render.resolveCovenList), resolved once
	// per run: it is the key that DECIDES the roster, so it cannot read it.
	HostFreeCovenList

	// HostFreeKeeper — an `on: keeper` task (render.keeperVars). The keeper is not a
	// host and the task renders once per run with no roster in scope; a per-host
	// value reaches `incarnation.state` from here through `register.hosts.<name>`,
	// the SID-keyed accessor, not through a host binding.
	HostFreeKeeper

	// HostFreeCompute — the scenario-level `compute:` block
	// (render.Pipeline.resolveCompute), resolved once per run before the task loop.
	// Host-invariance is the property the whole namespace rests on: `compute.<name>`
	// is offered to keeper-side and Soul-side contexts alike precisely because
	// nothing about it is per-host.
	HostFreeCompute

	// HostFreeStateMatch — the merge-time half of a `core.state.<verb>` capture
	// (render.Pipeline.StateOpEvaluators): `match:` and a `modify:` `patch:` cell,
	// evaluated once per collection element against elem/key/value and no scenario
	// context at all.
	HostFreeStateMatch

	// HostFreeStaticWhen — a `when:` that Keeper decides itself, at render, for the
	// whole roster (render.Pipeline.staticWhenSkips). Carried by no [Vars] literal,
	// and that is the point of naming it: a static `when:` is evaluated in the
	// Soul-side flow-control sandbox against the flow_context of the roster's FIRST
	// host, so the refusal has to be raised at the decision site
	// ([Engine.GuardHostFreePredicate]) rather than by an env this predicate never
	// meets. Only the input half can arrive here — `soulprint` in a `when:` makes it
	// non-static (config.IsStaticPredicate), and a non-static `when:` is shipped to
	// each host instead of being decided here.
	HostFreeStaticWhen

	// HostFreeIncludeWhen — a conditional include's `when:`
	// (render.Pipeline.evalIncludeWhen), decided once per include group. The group
	// is spliced into the plan or dropped from it wholesale, so there is no host to
	// decide it for even in principle: half a plan cannot go to one host.
	HostFreeIncludeWhen

	// HostFreeTemplatePath — the `template:` path of a `core.file.rendered` task
	// (render.Pipeline.resolveTemplateUsesInput). Keeper reads the file ONCE per
	// task, before the per-host loop, so the path has to name one file for the whole
	// roster even though the CONTENT it renders is per host. Carried by no [Vars]
	// literal either: the site borrows the keeper context to resolve a `${ … }` path
	// and only overrides the stance.
	HostFreeTemplatePath
)

// hostScopeCount is the number of stances, so a test can walk every one of them
// instead of a hand-kept list a new constant silently falls out of.
const hostScopeCount = int(HostFreeTemplatePath) + 1

// contextName describes the context in the words the author used to get here (the
// YAML key, not the Go builder). Empty for [HostBound].
func (s HostScope) contextName() string {
	switch s {
	case HostFreeLoopAxis:
		return "loop.items:/loop.when: (the host-invariant loop axis)"
	case HostFreeCovenList:
		return "on: [covens] (resolved once per run, before the roster exists)"
	case HostFreeKeeper:
		return "an on: keeper task (rendered once per run, no roster in scope)"
	case HostFreeCompute:
		return "the compute: block (resolved once per run, before the task loop)"
	case HostFreeStateMatch:
		return "a core.state capture's match:/patch: (evaluated per element against elem/key/value)"
	case HostFreeStaticWhen:
		return "a static when: (Keeper decides it once, for the whole roster)"
	case HostFreeIncludeWhen:
		return "a conditional include's when: (decided once per include group, for the whole plan)"
	case HostFreeTemplatePath:
		return "a core.file.rendered template: path (Keeper reads the file once per task, before the per-host loop)"
	default:
		return ""
	}
}

// selfHint is the way out when the expression reads `soulprint.self` directly.
func (s HostScope) selfHint() string {
	switch s {
	case HostFreeLoopAxis:
		return "the loop expands at render into tasks every targeted host runs, so the iteration set is the same for all of them -- drive the loop from input.*/vars.*/register.*/incarnation.* or soulprint.hosts, and put the per-host part in the task's where: (per-host targeting) or in a core.file.rendered template, whose render_context is dispatched per host"
	case HostFreeCovenList:
		return "on: chooses the roster, so it cannot read it -- select with stable coven labels and narrow per host with where:, which is evaluated against each host's own soulprint.self"
	case HostFreeKeeper:
		return "a keeper task has no host -- read the run's per-host facts as soulprint.hosts (scenario pass) or register.hosts.<name> (the SID-keyed accessor), both of which name the host they came from"
	case HostFreeCompute:
		return "compute: is host-invariant by construction, which is why every context may read it -- move the per-host part into the task's where: or into a core.file.rendered template, the two places a per-host VALUE can actually differ between hosts"
	case HostFreeStateMatch:
		return "match:/patch: see the collection element (elem, or key/value for a map) and nothing else -- render the per-host part into an ordinary param, which the param render substitutes before merge ever reads the text"
	case HostFreeStaticWhen:
		// Unreachable in practice and kept so the stance table has no hole: a `when:`
		// naming soulprint is not static (config.IsStaticPredicate), so it is shipped
		// to each host instead of being decided here and never meets this guard. It
		// says that rather than offering a move the author cannot make.
		return "a when: that reads soulprint is not static -- it is shipped to each host and evaluated there, so it never reaches this decision point; if you are seeing this, the predicate reached it by some other route and that is a bug in the render, not in your file"
	case HostFreeIncludeWhen:
		return "an include is spliced in or dropped for the whole plan, so its when: must be a run-level decision -- gate on input.*/vars.*/incarnation.* and use a task-level where: for the per-host part"
	case HostFreeTemplatePath:
		return "the path picks WHICH file Keeper reads, once for the whole task -- keep it host-invariant and put the per-host part INSIDE the template, whose render_context is built and dispatched per host"
	default:
		return ""
	}
}

// variantInputHint is the way out when the expression reads an input name that
// renders per host. Deliberately different from [HostScope.selfHint]: the author
// wrote a name that looks host-invariant, so the advice has to start by saying
// where the variance came from.
func (s HostScope) variantInputHint() string {
	switch s {
	case HostFreeLoopAxis, HostFreeCovenList, HostFreeKeeper, HostFreeCompute, HostFreeStateMatch, HostFreeStaticWhen, HostFreeIncludeWhen, HostFreeTemplatePath:
		return "this context decides ONCE for the whole roster, so it needs a value that is the same for everyone -- pass a second, host-invariant input (one that reaches neither soulprint.self nor register, including through a vars: hop) and read that here. The per-host name itself stays readable wherever a host IS bound, which is most places: a task's params: and where:, the destiny's vars.yml, and a core.file.rendered template. Those all render per host and are dispatched per host"
	default:
		return ""
	}
}

// Describe exposes the human-facing halves of a stance for a caller building its own
// diagnostic rather than raising an error from this package — render's two
// predicate-site guards, which have a YAML key to name and no compiled expression to
// quote. Shared rather than restated so the sites and [Engine.guardHostScope] say
// the same thing about the same context. [HostBound] returns empty strings.
func (s HostScope) Describe() (context, selfHint, variantInputHint string) {
	return s.contextName(), s.selfHint(), s.variantInputHint()
}

// hostStance is a context's host-boundness as [Engine.compile] takes it: the stance
// plus the input names that render per host in THIS run. Bundled because compile's
// parameter list is already at the limit where a seventh positional flag stops
// being readable, and because the two are only ever meaningful together.
type hostStance struct {
	scope HostScope
	// variantInputs / variantVars are sorted and deduplicated by the producer
	// (render.hostVariantInputNames, render.hostVariantDestinyVars) so the cache tag
	// below is canonical.
	variantInputs []string
	variantVars   []string
}

// hostStance is [Vars]'s view of the above.
func (v Vars) hostStance() hostStance {
	return hostStance{scope: v.HostScope, variantInputs: v.HostVariantInputs, variantVars: v.HostVariantVars}
}

// cacheTag discriminates the compile-cache key ([Engine.compile]).
//
// Empty for [HostBound]: the guard never rewrites, only rejects, so an expression
// that compiles in a host-bound context has the identical program everywhere and
// tagging it would fragment the cache of every ordinary per-host expression for
// nothing.
//
// Non-empty otherwise, and it carries the NAMES and not just a marker byte. Both
// halves are load-bearing. Without any tag, `soulprint.self.sid` compiled and cached
// in a host-bound context would be served straight from the cache in a host-free one
// — the lookup happens before any guard, so the check would be bypassed entirely.
// Without the names, an expression reading `input.addr` would be cached under one
// run's variant set and served under another's: the Engine outlives a run, and two
// runs of the same scenario against different destinies genuinely have different
// sets.
//
// The `\x00` join is ambiguous in principle (["a\x00b"] and ["a","b"] tag alike), and
// the tag is a PREFIX whose remainder is arbitrary expression text. Both are
// unreachable: a name here came from a CEL identifier select, which cannot hold a
// control byte, and YAML's c-printable excludes `\x00` outright. The same aliasing
// has always been true of the `\x01`-`\x04` tags beside it ([Engine.compile]) — it is
// a property of the scheme, not of this tag.
func (h hostStance) cacheTag() string {
	if h.scope == HostBound {
		return ""
	}
	if len(h.variantInputs) == 0 && len(h.variantVars) == 0 {
		return "\x05\x06"
	}
	// The two lists are separated by \x07 rather than concatenated: `input.x` and
	// `vars.x` are different references and must not tag alike.
	return "\x05" + strings.Join(h.variantInputs, "\x00") + "\x07" + strings.Join(h.variantVars, "\x00") + "\x06"
}

// ErrNoHostBound — sentinel for errors.Is. Lets a caller (and a guard test) tell
// "this context has no host" from an ordinary [ErrEval] no-such-key without matching
// message text.
var ErrNoHostBound = errors.New("evaluation context has no host bound")

// ErrHostOutOfScope — the expression reaches for a per-host value in a context that
// decides once per run. Distinct from [ErrOutOfScope] (a whole namespace is absent)
// and from [ErrEval] (the namespace is here, the key is not): the root exists in
// this engine and means something everywhere else, it just has no host to mean it
// about here.
//
// Reference is what the author wrote that cannot be answered — `soulprint.self` or
// `input.<name>`.
type ErrHostOutOfScope struct {
	Expr      string
	Reference string
	Context   string
	Hint      string
}

func (e *ErrHostOutOfScope) Error() string {
	return fmt.Sprintf(
		"CEL no host bound %q: %s is per-host, and %s renders once per run with no host -- answering it would silently hand over the first host's value; %s",
		e.Expr, e.Reference, e.Context, e.Hint,
	)
}

func (e *ErrHostOutOfScope) Unwrap() error { return ErrNoHostBound }

// soulprintRoot / soulprintSelfField / inputRoot are the CEL identifiers this guard
// reads. soulprint.hosts is deliberately NOT among them: it is the run's roster, the
// same list for every host, and refusing it here would take away the one legitimate
// way a host-free context reaches host facts.
const (
	soulprintRoot      = "soulprint"
	soulprintSelfField = "self"
	inputRoot          = "input"
	varsRoot           = "vars"
)

// guardHostScope rejects a per-host reference in a context that has no host
// ([HostScope]). Called from [Engine.compile] AFTER env.Compile, which is the rule
// NIM-909 settled: an engine that declares neither root ([NewMigration],
// [NewServiceVars]) must answer with cel-go's own undeclared-reference error rather
// than with advice about a root the author cannot use there.
//
// compiled is the text AFTER [Engine.rewriteHostsWhere], not the author's — a
// `soulprint.hosts.where("<predicate>")` argument is a string LITERAL until the
// rewrite inlines it, so scanning the original would see nothing inside it. expr is
// the original, kept for the message: an author must be able to find the text this
// names in their own file.
//
// A loop variable named `soulprint` or `input` shadows the root in the activation
// ([Vars.activation] merges Loop last), so a declared loop name suppresses the guard
// for that root. shared/config forbids both names for `loop.as:`/`loop.index_as:`,
// so this is unreachable from YAML; it keeps a programmatic caller honest.
func (e *Engine) guardHostScope(expr, compiled string, stance hostStance, loopNames []string) error {
	if stance.scope == HostBound {
		return nil
	}
	touchesSoulprint := containsIdentText(compiled, soulprintRoot) && !shadowedByLoop(soulprintRoot, loopNames)
	touchesInput := len(stance.variantInputs) > 0 &&
		containsIdentText(compiled, inputRoot) && !shadowedByLoop(inputRoot, loopNames)
	touchesVars := len(stance.variantVars) > 0 &&
		containsIdentText(compiled, varsRoot) && !shadowedByLoop(varsRoot, loopNames)
	if !touchesSoulprint && !touchesInput && !touchesVars {
		return nil
	}

	reads := e.PredicateReads(compiled, []string{soulprintRoot, inputRoot, varsRoot})

	if touchesSoulprint {
		// Whole covers `size(soulprint)` and `soulprint[k]` — a shape the walk holds
		// no field name for. Fail-closed: it MAY be reaching for self, and the
		// alternative is the silence this whole file exists to remove.
		sp := reads[soulprintRoot]
		if sp.Whole || sp.Fields[soulprintSelfField] {
			ref := soulprintRoot + "." + soulprintSelfField
			if sp.Whole && !sp.Fields[soulprintSelfField] {
				ref = "a whole-namespace read of " + soulprintRoot + " (which includes " + ref + ")"
			}
			return &ErrHostOutOfScope{
				Expr:      expr,
				Reference: ref,
				Context:   stance.scope.contextName(),
				Hint:      stance.scope.selfHint(),
			}
		}
	}

	if touchesInput {
		in := reads[inputRoot]
		if in.Whole {
			return &ErrHostOutOfScope{
				Expr:      expr,
				Reference: fmt.Sprintf("a whole-namespace read of %s, which carries the per-host name(s) %s", inputRoot, strings.Join(stance.variantInputs, ", ")),
				Context:   stance.scope.contextName(),
				Hint:      stance.scope.variantInputHint(),
			}
		}
		// Sorted so a predicate naming two per-host inputs reports the same one every
		// time: a message that changes between identical runs is a message nobody can
		// write a test against.
		hit := make([]string, 0, 1)
		for _, name := range stance.variantInputs {
			if in.Fields[name] {
				hit = append(hit, name)
			}
		}
		if len(hit) > 0 {
			sort.Strings(hit)
			return &ErrHostOutOfScope{
				Expr:      expr,
				Reference: fmt.Sprintf("%s.%s (rendered per host: its apply: input: value reaches soulprint.self or register, directly or through the applier's vars:)", inputRoot, hit[0]),
				Context:   stance.scope.contextName(),
				Hint:      stance.scope.variantInputHint(),
			}
		}
	}

	// vars: the MIRROR of the input half, one layer down. A destiny's own `vars.yml`
	// resolves per host, so a var reading a per-host `input.<name>` (or
	// `soulprint.self` directly) is per-host too — and the reference that reaches a
	// host-free context says only `vars.<name>`, which looks as ordinary as any
	// other. Measured before this branch existed: a destiny `when: vars.addr == …`
	// true on the SECOND host was decided on the first and the task was skipped for
	// everyone, silently, green.
	if touchesVars {
		vr := reads[varsRoot]
		if vr.Whole {
			return &ErrHostOutOfScope{
				Expr:      expr,
				Reference: fmt.Sprintf("a whole-namespace read of %s, which carries the per-host name(s) %s", varsRoot, strings.Join(stance.variantVars, ", ")),
				Context:   stance.scope.contextName(),
				Hint:      stance.scope.variantInputHint(),
			}
		}
		hit := make([]string, 0, 1)
		for _, name := range stance.variantVars {
			if vr.Fields[name] {
				hit = append(hit, name)
			}
		}
		if len(hit) > 0 {
			sort.Strings(hit)
			return &ErrHostOutOfScope{
				Expr:      expr,
				Reference: fmt.Sprintf("%s.%s (a destiny local that resolves per host: its vars.yml value reaches soulprint.self or a per-host input)", varsRoot, hit[0]),
				Context:   stance.scope.contextName(),
				Hint:      stance.scope.variantInputHint(),
			}
		}
	}

	return nil
}

// GuardHostFreePredicate is [Engine.guardHostScope] for a predicate this engine
// never compiles: a static `when:` and a conditional include's `when:` are decided
// by render against a flow_context, in the Soul-side sandbox
// ([NewFlowControl]) — a different engine, with a different env, which knows
// nothing about `apply: input:` and therefore cannot tell a per-host name from any
// other. The refusal has to be raised where the decision is made.
//
// expr is a BARE predicate (the whole string is CEL). variantInputs is the run's
// per-host input names; empty ⇒ nil, so every scenario whose apply.input is
// host-invariant is untouched.
//
// Exported deliberately narrow: it answers about one predicate and one stance, and
// shares the stance's own words with the compile-time guard so the two cannot drift
// into describing the same context differently.
func (e *Engine) GuardHostFreePredicate(expr string, scope HostScope, variantInputs, variantVars []string) error {
	if scope == HostBound || expr == "" || (len(variantInputs) == 0 && len(variantVars) == 0) {
		return nil
	}
	return e.guardHostScope(expr, expr, hostStance{scope: scope, variantInputs: variantInputs, variantVars: variantVars}, nil)
}

func shadowedByLoop(root string, loopNames []string) bool {
	for _, n := range loopNames {
		if n == root {
			return true
		}
	}
	return false
}
