package cel

import (
	"errors"
	"strings"
	"testing"
)

// registerIndexVars — a context in which every expression below WOULD resolve if it
// were allowed to: the register exists, the per-host inversion exists, and the
// register.hosts accessor is opted in. So a refusal here is the guard's, never an
// absent key's, and never [Engine.guardRegisterHosts]' (which runs first and would
// otherwise claim the `register["hosts"]` rows with its own Feature text).
func registerIndexVars() Vars {
	return Vars{
		Register:           map[string]any{"addr": map[string]any{"sha256": "abc"}},
		RegisterHosts:      map[string]any{"addr": map[string]any{"node-a": "10.0.0.1"}},
		Vars:               map[string]any{"which": "addr"},
		AllowRegisterHosts: true,
	}
}

// TestRegisterIndex_NameByIndexRefused — ★ GUARD: an index that chooses the
// register's NAME is refused at COMPILE, in every spelling of it. The name is what
// the reference machinery reads to build the Passage graph
// ([config.ExtractRegisterRefs] → [config.Stratify]), and it reads the DOT form
// only; behind an index the name is unreachable, so nothing declares the edge that
// puts the producer in an earlier Passage.
//
// Mutation: drop the guardRegisterNameByIndex call in [Engine.compile]; or make
// [indexesRegisterCandidate] return false; or drop [isRegisterHostsSelect] (the
// `register.hosts["addr"]` row alone).
func TestRegisterIndex_NameByIndexRefused(t *testing.T) {
	e := newEngine(t)
	for _, expr := range []string{
		`register["addr"]`,
		`register['addr'].sha256`,
		`register[vars.which].sha256`,  // a computed key is a name no parser can see at all
		`register.hosts["addr"]`,       // the name indexed one hop down
		`register . hosts ["addr"]`,    // CEL permits whitespace around `.`
		`register["hosts"].addr`,       // the accessor hop indexed: `register.hosts.addr` is gone from the text
		`size(register["addr"].files)`, // nested in a call, found by the post-order walk
	} {
		_, err := e.EvalExpression(expr, registerIndexVars())
		var unsupported *ErrUnsupported
		if !errors.As(err, &unsupported) {
			t.Fatalf("%s: err = %v, want *ErrUnsupported", expr, err)
		}
		if !strings.Contains(unsupported.Feature, "register.<name>") {
			t.Fatalf("%s: feature = %q, want it to name the fix (register.<name>)", expr, unsupported.Feature)
		}
	}
}

// TestRegisterIndex_PayloadAndSIDIndexStillResolve — ★ GUARD on the rule's WIDTH,
// and the test that made the first shape of it fail: "no index under register" would
// refuse both of these, and both are how registers are read.
//
//   - `register.<name>["k"]` — the name is selected, the edge is declared, and the
//     index reaches into the register's OWN payload (any map-valued register);
//   - `register.hosts.<name>["<sid>"]` — the name is selected, and the index is the
//     SID, which the per-host inversion is a keyed map of (NIM-711).
//
// Mutation: in [isRegisterNameIndex], accept any base (drop the
// isRegisterIdent/isRegisterHostsSelect test) — both rows then refuse.
func TestRegisterIndex_PayloadAndSIDIndexStillResolve(t *testing.T) {
	e := newEngine(t)
	for _, tc := range []struct {
		expr string
		want any
	}{
		{`register.addr["sha256"]`, "abc"},
		{`register.hosts.addr["node-a"]`, "10.0.0.1"},
	} {
		val, err := e.EvalExpression(tc.expr, registerIndexVars())
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if got := val.Value(); got != tc.want {
			t.Fatalf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestRegisterIndex_RefusedInFlowControlToo — the refusal is UNCONDITIONAL, and this
// engine is where that costs something: a `changed_when:`/`failed_when:`/`until:`
// predicate is not rendered, it travels to the Soul as text and is evaluated against
// the registers of its own ApplyRequest, where the root is an ordinary map — so this
// expression RESOLVED before the guard (the dot form still does, next door in
// TestFlowControl_RegisterAccessible), for as long as the producer travelled in the
// same Passage. For `changed_when:`/`failed_when:` the dot form has a detector for the
// moment it stops (FC-5, `cross_passage_when_unsupported`) that reads dot-form refs and
// so walks past the index form; `until:` is outside FC-5 by design, and there this rule
// tidies rather than repairs. `when:` never reached here at all — see the header of
// register_index.go, it is classified static and dies at render.
//
// Mutation: gate guardRegisterNameByIndex on `!e.flowControl` in [Engine.compile].
func TestRegisterIndex_RefusedInFlowControlToo(t *testing.T) {
	e := newFlowControlEngine(t)
	_, err := e.EvalPredicate(
		`register["probe"].exit_code == 0`,
		Vars{Register: map[string]any{"probe": map[string]any{"exit_code": 0}}},
	)
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *ErrUnsupported", err)
	}
	// Pinned like every sibling: without it the case also passes when the refusal comes
	// from a widened rule, or from any other ErrUnsupported producer on this path.
	if !strings.Contains(unsupported.Feature, "register.<name>") {
		t.Fatalf("feature = %q, want it to name the fix", unsupported.Feature)
	}
}

// TestRegisterIndex_InsideAWherePredicateBothHalvesSeeIt — ★ GUARD on the
// reportExpr/scanExpr split. A `.where("<predicate>")` argument is a string LITERAL at
// the point either walk would look, and [ast.PostOrderVisit] does not descend into a
// literal — so the index inside it is invisible until [Engine.rewriteHostsWhere]
// inlines the predicate into the tree. The gate scans the rewritten text and the
// offline helper performs the same rewrite, which is the only reason they agree here;
// and the error must still quote the AUTHOR's text, not the Unparse.
//
// `register` is a declared predicate root ([predicateContextRoots]), so this is a
// supported spelling, not a hypothetical.
//
// Mutation: pass `expr` instead of `compiled` to guardRegisterNameByIndex in
// [Engine.compile] (or move the call above rewriteHostsWhere) — the refusal goes away;
// pass `compiled` as reportExpr — the quoted text becomes the rewrite; drop the
// rewriteHostsWhere call in [Engine.ExpressionIndexesRegisterName] — the offline half
// goes silent on it.
func TestRegisterIndex_InsideAWherePredicateBothHalvesSeeIt(t *testing.T) {
	const expr = `soulprint.hosts.where("register['probe'].leader == sid").size() > 0`
	e := newEngine(t)

	_, err := e.EvalExpression(expr, Vars{AllowHosts: true, Register: map[string]any{"probe": map[string]any{}}})
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("runtime: err = %v, want *ErrUnsupported", err)
	}
	if unsupported.Expr != expr {
		t.Fatalf("runtime: error quotes %q, want the author's text %q", unsupported.Expr, expr)
	}
	if !e.ExpressionIndexesRegisterName(expr) {
		t.Fatal("offline: the helper must see the index inside the .where predicate too")
	}
	if !e.InterpolationIndexesRegisterName(`${ ` + expr + ` }`) {
		t.Fatal("offline: the interpolated form must answer the same")
	}
}

// TestRegisterIndex_AnEngineWithoutTheRootSaysSo — ★ GUARD on the gate's POSITION: it
// runs after env.Compile, so an engine that declares no `register` root at all answers
// with cel-go's own undeclared-reference error instead of advice the author must not
// take. Migration CEL is a pure function of the old state and `register.*` is forbidden
// there outright ([ADR-019]); the service-vars engine is the same shape.
//
// Mutation: move the guardRegisterNameByIndex call in [Engine.compile] above
// env.Compile.
func TestRegisterIndex_AnEngineWithoutTheRootSaysSo(t *testing.T) {
	for _, tc := range []struct {
		mode string
		new  func(...Option) (*Engine, error)
	}{
		{"migration", NewMigration},
		{"service vars", NewServiceVars},
	} {
		e, err := tc.new()
		if err != nil {
			t.Fatalf("%s: %v", tc.mode, err)
		}
		_, err = e.EvalExpression(`register["addr"]`, Vars{})
		var unsupported *ErrUnsupported
		if errors.As(err, &unsupported) {
			t.Fatalf("%s: got %v — an engine without the root must not advise register.<name>", tc.mode, err)
		}
		var compileErr *ErrCompile
		if !errors.As(err, &compileErr) {
			t.Fatalf("%s: err = %v, want *ErrCompile naming the undeclared root", tc.mode, err)
		}
	}
}

// TestRegisterIndex_TheAccessorHopIndexedNamesTheLongerFix — `register["hosts"].addr`
// RESOLVED before NIM-909 on a keeper task: `guardRegisterHosts` passes with the flag
// set and the `hosts` key is in the activation. It is refused now for the same reason
// as the rest — the text holds no `register.hosts.addr` for the extractor — so the
// message has to name the fix that form actually needs, which is the longer
// `register.hosts.<name>`, not `register.<name>`.
//
// Mutation: drop the `register.hosts.<name>` clause from the Feature text in
// [Engine.guardRegisterNameByIndex].
func TestRegisterIndex_TheAccessorHopIndexedNamesTheLongerFix(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression(`register["hosts"].addr`, registerIndexVars())
	var unsupported *ErrUnsupported
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want *ErrUnsupported", err)
	}
	if !strings.Contains(unsupported.Feature, "register.hosts.<name>") {
		t.Fatalf("feature = %q, want it to name register.hosts.<name>", unsupported.Feature)
	}
}

// TestRegisterIndex_BracketsInAStringLiteralAreNotAnIndex — the coarse pre-test
// matches this text (it holds `register` and `[`), so only the AST walk can answer
// it. A rule built on the text would refuse an expression that merely QUOTES the
// bad form — including the error message that tells the author about the rule.
//
// Mutation: make [Engine.guardRegisterNameByIndex] return the error from the
// [indexesRegisterCandidate] pre-test, without parsing.
func TestRegisterIndex_BracketsInAStringLiteralAreNotAnIndex(t *testing.T) {
	e := newEngine(t)
	const quoted = `register["addr"]`
	val, err := e.EvalExpression(
		`input.msg == "register[\"addr\"]"`,
		Vars{Input: map[string]any{"msg": quoted}},
	)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := val.Value(); got != true {
		t.Fatalf("got %v, want true", got)
	}
}

// TestRegisterIndex_OfflineHelpersAgreeWithTheGuard — the offline half
// ([Engine.InterpolationIndexesRegisterName] / [Engine.ExpressionIndexesRegisterName])
// answers about the same AST the compile-time gate walks, after the same rewrite (the
// `.where` case has its own test above). What it must NOT report is a bracket that is not
// an index of a name: inside a string literal, or in the literal text around a
// `${ … }` block.
//
// Mutation: in [Engine.InterpolationIndexesRegisterName], test the raw string
// instead of walking its expression segments — the last two rows then report true.
func TestRegisterIndex_OfflineHelpersAgreeWithTheGuard(t *testing.T) {
	e := newEngine(t)

	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`${ register["addr"] }`, true},
		{`ip=${ register.hosts["addr"] }`, true},
		{`ip=${ register.addr["sha256"] }`, false},
		{`${ input.msg == "register[\"addr\"]" }`, false},
		{`a literal register["addr"] outside any block`, false},
		{`${ input.plain }`, false},
	} {
		if got := e.InterpolationIndexesRegisterName(tc.raw); got != tc.want {
			t.Fatalf("InterpolationIndexesRegisterName(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}

	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`register["addr"].changed`, true},
		{`register.addr["sha256"] == vars.want`, false},
		{`register.addr.changed`, false},
	} {
		if got := e.ExpressionIndexesRegisterName(tc.expr); got != tc.want {
			t.Fatalf("ExpressionIndexesRegisterName(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}
