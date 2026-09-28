package cel

import (
	"strings"

	"github.com/google/cel-go/common/ast"
	"github.com/google/cel-go/common/operators"
)

// Choosing a register's NAME by index is refused at compile (NIM-909).
//
// A cross-task register reference is `register.<name>` — a field select on the bare
// `register` ident — and one parser answers for all of it: [config.ExtractRegisterRefs].
// Stratification builds the Passage graph from it, the cross-reference validator (which
// is also `soul-lint`'s `unknown_register_reference`) checks existence from it, the
// Soul's `async:` join set resolves through it ([soul/internal/runtime] flowSet.addNames),
// and [config.IsStaticPredicate] decides from it whether a `when:` reads a register at
// all. It is a dot-form regex, and [config.ExtractRegisterRefs] says so outright:
// *"Dynamic access `register["x"]` is not covered by this form — there is no dot, the
// regex does not match (a safe skip)."*
//
// A safe skip it is not, and the shapes it takes are worth naming one by one, because
// none of them is the one the phrase suggests:
//
//   - RENDER-TIME keys (`params:`/`vars:`/`where:`/`apply.input:`/`on:`/`loop.*`): with
//     the producer in the same Passage the cell raises at render (`no such key: addr`),
//     from a message naming neither the rule nor the fix. With the producer in an
//     EARLIER Passage — pushed there by some unrelated `where: register.X` — it
//     RESOLVES, out of the accumulated per-host context ([render] RegisterByHost). So it
//     works or it fails by a property of a different task.
//   - `when:`: the blindness flips a CLASSIFICATION. [config.IsStaticPredicate] finds no
//     register reference, so the predicate is judged host-invariant and Keeper evaluates
//     it ITSELF against a deliberately empty register map (`render.evalStaticWhen`,
//     whose comment states the guarantee this form breaks) — the RUN fails at render,
//     never reaching a host, whatever the Passage.
//   - `changed_when:`/`failed_when:`: these do travel to the Soul and resolve against
//     the registers of their ApplyRequest, for as long as the producer travels in the
//     same Passage. The dot form has a detector for the moment it stops — reading a
//     register emitted in an EARLIER Passage is [config.CodeCrossPassageWhenGating]
//     ([ADR-056] FC-5), offline and again as a runtime safeguard — and that detector
//     reads dot-form refs, so the index form walks past it and dies on `no such key`.
//   - `retry.until:`: travels to the Soul the same way, and FC-5 excludes it by design
//     ([config.flowControlRead]), so here the dot form is no better guarded. This one
//     the rule tidies rather than repairs.
//
// Refusing one spelling everywhere is what makes that list unnecessary to know.
//
// ★ THE RULE IS NARROW ON PURPOSE, and the narrowness is the whole design. Only an
// index that chooses the register's NAME is refused, because only that one is invisible
// to the dot-form parsers:
//
//	register['addr']              REFUSED — the name is an index; no Passage edge
//	register.hosts['addr']        REFUSED — same, one hop down (the name is still indexed)
//	register['hosts'].addr        REFUSED — the ACCESSOR hop is indexed, so the text holds
//	                                        no `register.hosts.addr` for the regex to
//	                                        find either. ⚠ This spelling RESOLVED before
//	                                        NIM-909 on a keeper task (the `hosts` key is
//	                                        in the activation, [Vars.registerRoot]) — a
//	                                        working form deliberately removed, and the
//	                                        one refusal here whose fix is the LONGER
//	                                        `register.hosts.<name>`
//	register.addr['sha256']       ALLOWED — name selected, edge declared; the index
//	                                        reaches INTO the register's own payload
//	register.hosts.addr['sid-1']  ALLOWED — name selected; the index is the SID, which
//	                                        register.hosts is a keyed map of (NIM-711)
//
// Refusing the last two would break legitimate reads: a register whose payload is a map
// is indexed by its keys all over the corpus, and `register.hosts.<name>` needs a SID
// index by construction. A blanket "no index under register" was the first shape of this
// guard and it was wrong for exactly those two rows.

// indexesRegisterCandidate is the coarse pre-test that keeps the hot path off the
// parser: an expression must mention `register` and contain an opening bracket before
// the AST is worth building. Mirrors [usesRegisterHostsAccessor] in spirit — a false
// positive costs one parse, and the AST decides.
func indexesRegisterCandidate(expr string) bool {
	return strings.Contains(expr, "register") && strings.Contains(expr, "[")
}

// guardRegisterNameByIndex rejects an expression that picks a register's name with an
// index. reportExpr is the AUTHOR's text (what the error shows), scanExpr the text
// actually analysed — they differ after [Engine.rewriteHostsWhere], whose Unparse
// inlines a `.where("<predicate>")` literal into the tree (same asymmetry as
// [Engine.guardRegisterHosts]).
//
// A parse failure is NOT an error here: this is a gate, not a validator — env.Compile
// reports the syntax problem right after, with position information.
//
// Known coarseness, shared with [Engine.guardComputeScope] and accepted for the same
// reason: an identifier `register` bound by a comprehension inside the expression
// (`[1].map(register, register["k"])`) is read as the root and refused. A `loop:`
// variable cannot reach this — `register` is in config.loopReservedNames — so the only
// way in is hand-written comprehension shadowing a context root, and being loud about
// that is the direction this guard exists to move in.
func (e *Engine) guardRegisterNameByIndex(reportExpr, scanExpr string) error {
	if !indexesRegisterCandidate(scanExpr) {
		return nil
	}
	parsed, err := e.parseNoMacro(scanExpr)
	if err != nil {
		return nil
	}
	if !containsRegisterNameIndex(parsed.Expr()) {
		return nil
	}
	return &ErrUnsupported{
		Expr: reportExpr,
		Feature: "choosing a register by index (register[\"name\"] / register.hosts[\"name\"] / " +
			"register[\"hosts\"].name); write register.<name>, or register.hosts.<name> for the " +
			"per-host map — the index form is invisible to the reference machinery (no Passage " +
			"edge, no existence check, no lint), so nothing puts the register in scope before " +
			"the expression is evaluated",
	}
}

// containsRegisterNameIndex reports whether the tree indexes `register` or
// `register.hosts` anywhere — i.e. picks a register NAME with an index.
func containsRegisterNameIndex(root ast.Expr) bool {
	found := false
	ast.PostOrderVisit(root, ast.NewExprVisitor(func(e ast.Expr) {
		if isRegisterNameIndex(e) {
			found = true
		}
	}))
	return found
}

// isRegisterNameIndex — an index call whose operand is the bare `register` ident or
// the `register.hosts` accessor. Anything deeper is a read inside a named register and
// is left alone (see the table in this file's header).
func isRegisterNameIndex(e ast.Expr) bool {
	if e.Kind() != ast.CallKind {
		return false
	}
	c := e.AsCall()
	if c.IsMemberFunction() || c.FunctionName() != operators.Index {
		return false
	}
	args := c.Args()
	if len(args) != 2 {
		return false
	}
	// A non-literal key is still an indexed name (`register[vars.which]`), and it is
	// refused for the same reason: no parser can see which register that is.
	base := args[0]
	return isRegisterIdent(base) || isRegisterHostsSelect(base)
}

// isRegisterHostsSelect — `register.hosts` written as a field select. The index form
// of that hop (`register["hosts"]`) is already an indexed name by the rule above, so
// it is caught by [isRegisterNameIndex]'s first arm and needs no case here.
func isRegisterHostsSelect(e ast.Expr) bool {
	if e.Kind() != ast.SelectKind {
		return false
	}
	s := e.AsSelect()
	return s.FieldName() == registerHostsKey && isRegisterIdent(s.Operand())
}

// InterpolationIndexesRegisterName reports whether an interpolated string
// (`params:` values, `vars:` values, `on:` elements, `loop.items:`) picks a register
// name by index inside a `${ … }` block. The offline counterpart of
// [Engine.guardRegisterNameByIndex], shaped like
// [Engine.InterpolationReferencesCompute]: the AST walk is what makes the answer
// honest, because the same bracket text in literal prose around a block, or inside a
// CEL string literal, is correctly NOT a reference.
//
// A raw string with no blocks, or one whose blocks do not parse, → false.
func (e *Engine) InterpolationIndexesRegisterName(raw string) bool {
	if !indexesRegisterCandidate(raw) {
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
		if e.ExpressionIndexesRegisterName(s.text) {
			return true
		}
	}
	return false
}

// ExpressionIndexesRegisterName is [Engine.InterpolationIndexesRegisterName] for a
// bare expression — a top-level expression key (`where:`, `when:`, `loop.when:`),
// where the whole cell is CEL without the `${ … }` wrapper.
//
// ★ The REWRITE runs first, exactly as in [Engine.compile], and it is not an
// optimisation: `soulprint.hosts.where("register['probe'].leader == sid")` holds the
// index inside a string LITERAL, which [ast.PostOrderVisit] does not descend into, so
// the raw text answers false while the run — which scans the Unparse of the rewritten
// tree — refuses. Without this the linter would stay silent on the one spelling that
// hides a register reference from every other reader too.
//
// `allowHosts: true` is deliberate here and is the one asymmetry with the gate: offline
// there is no host context to decide it from, and the pessimistic choice would make the
// rewrite fail on every destiny-pass cell and take this rule down with it. When the run
// has the flag false, that expression is refused anyway — for the soulprint.hosts
// isolation, one message earlier.
func (e *Engine) ExpressionIndexesRegisterName(expr string) bool {
	if !indexesRegisterCandidate(expr) {
		return false
	}
	scan := expr
	if rewritten, rerr := e.rewriteHostsWhere(expr, true); rerr == nil {
		scan = rewritten
	}
	parsed, err := e.parseNoMacro(scan)
	if err != nil {
		return false
	}
	return containsRegisterNameIndex(parsed.Expr())
}
