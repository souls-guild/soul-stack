package config

// THE INVARIANT, guarded at the place the `validate:` context is built
// (validate_scope.go, NIM-833): a rule may read exactly the incarnation facts its
// path knows, and reading one it does not know REFUSES.
//
// Why a guard test and not a doc comment: `incarnation` is `cel.DynType`, so no
// type check sees a wrong field, and the tempting implementation — put an empty
// map in the activation and let cel-go say `no such key` — is silently WRONG in
// the two forms a "check it only when present" rule is written in.
// `has(incarnation.state)` evaluates to false and `size(incarnation)` to 0, with
// no error at all, so `!has(incarnation.state) || <check>` passes on the create
// path and the author is looking at a green check that checks nothing. That is
// the whole defect (NIM-619 recorded the same shape for `compute`), and the only
// place it can be caught is here.
//
// HOW TO BREAK IT ON PURPOSE (each mutation was run, and turns exactly the named
// tests red). Only the SilentForms one goes red the defect's own way — by PASSING a
// rule that reads nothing; the rest go red as a raw CEL no-such-key, which is loud
// but is not the intelligible refusal the ticket asks for, and is why they assert
// on the sentinel rather than merely on "an error":
//  1. drop the `inc.guard(rule.That)` pre-pass from EvalValidateRules — six tests
//     below go red, among them all three the ticket names.
//  2. return early from ValidateContext.guard for any stance that carries facts
//     (`if c.scope != IncarnationAbsent { return nil }`) — CreatePathRefuses…,
//     SilentForms…, ComposedIDWithdraws… and LoadedRefusesAFieldItDoesNotCarry.
//  3. drop the empty-id branch from RequestedIncarnation, so an absent id enters
//     the namespace as "" — EmptyRequestedIDIsNotAFact.
//  4. derive the answered-for set from the row again (`known` from `fields`' keys
//     in LoadedIncarnation) — AnsweredForIsNotWhatTheRowCarries and
//     UnansweredValueIsDroppedFromTheNamespace.
//  5. move the guard back inside the eval loop — GuardIsIndependentOfOperatorInput.
//  6. drop the bindsIncarnationName arm from the walk —
//     ShadowingComprehensionVariableIsRefused.

import (
	"errors"
	"strings"
	"testing"
)

const (
	guardIncID    = "redis-billing"
	guardIncState = "replicas"
)

// guardDayTwoFields mirrors what the keeper's day-2 builder answers for
// (scenario.dayTwoFields). Restated here because this package must not import the
// keeper; the two are pinned to each other by
// keeper/internal/scenario/validate_scope_guard_test.go, which exercises the real
// builder end to end.
var guardDayTwoFields = []string{"id", "name", "service", "service_version", "state"}

func dayTwoContext() ValidateContext {
	return LoadedIncarnation(guardDayTwoFields, map[string]any{
		"id":              guardIncID,
		"name":            guardIncID, // the ADR-0085 window alias, as the keeper builds it
		"service":         "redis",
		"service_version": "v1.2.3",
		"state":           map[string]any{guardIncState: 3},
	})
}

func rule(that string) []ValidateRule {
	return []ValidateRule{{That: that, Message: "guard"}}
}

// evalGuard runs one rule and reports the three outcomes apart: passed, failed the
// invariant, refused as out of scope.
func evalGuard(t *testing.T, that string, inc ValidateContext) (fail *ValidateRuleFailure, scopeErr error) {
	t.Helper()
	f, err := EvalValidateRules(rule(that), map[string]any{"port": 6379}, inc)
	if err != nil {
		if !errors.Is(err, ErrIncarnationNotInScope) {
			t.Fatalf("rule %q: unexpected internal error: %v", that, err)
		}
		return nil, err
	}
	return f, nil
}

// TestValidateScope_DayTwoReadsTheRow — the day-2 path answers for the loaded row,
// under both spellings of the identifier.
func TestValidateScope_DayTwoReadsTheRow(t *testing.T) {
	for _, that := range []string{
		`incarnation.id.startsWith("redis-")`,
		`incarnation.name == "` + guardIncID + `"`,
		`incarnation.service == "redis"`,
		`incarnation.state.` + guardIncState + ` > 0`,
	} {
		fail, scopeErr := evalGuard(t, that, dayTwoContext())
		if scopeErr != nil {
			t.Errorf("day-2 rule %q was refused as out of scope: %v", that, scopeErr)
		}
		if fail != nil {
			t.Errorf("day-2 rule %q evaluated false: %v", that, fail)
		}
	}
}

// TestValidateScope_CreatePathSeesTheRequestedID — the identifier a create request
// carries IS a fact, and a rule about it runs on the create path, before the
// incarnation exists. This is the case the whole ticket is for: without it, a
// constraint on the id has to live in a run-time assert, by which point the row is
// already written (NIM-832).
func TestValidateScope_CreatePathSeesTheRequestedID(t *testing.T) {
	inc := RequestedIncarnation(guardIncID)

	fail, scopeErr := evalGuard(t, `incarnation.id.matches("^[a-z][a-z0-9-]{0,48}[a-z0-9]$")`, inc)
	if scopeErr != nil {
		t.Fatalf("a rule about the requested id must run on the create path, got: %v", scopeErr)
	}
	if fail != nil {
		t.Fatalf("rule over a valid id evaluated false: %v", fail)
	}

	// And it must actually be able to REJECT — a rule that can only pass is not a
	// check.
	bad := RequestedIncarnation("9redis-")
	fail, scopeErr = evalGuard(t, `incarnation.id.matches("^[a-z][a-z0-9-]{0,48}[a-z0-9]$")`, bad)
	if scopeErr != nil {
		t.Fatalf("unexpected scope refusal: %v", scopeErr)
	}
	if fail == nil {
		t.Error("an id violating the declared grammar passed the create-path gate")
	}
}

// TestValidateScope_CreatePathRefusesIncarnationState — the third case the ticket
// names: a rule reading a fact that does not exist yet must REFUSE, intelligibly.
// A pass here is the false green this file exists to prevent.
func TestValidateScope_CreatePathRefusesIncarnationState(t *testing.T) {
	fail, scopeErr := evalGuard(t, `incarnation.state.`+guardIncState+` > 0`, RequestedIncarnation(guardIncID))
	if fail != nil {
		t.Fatalf("a rule over incarnation.state reported an ordinary invariant failure: %v", fail)
	}
	if scopeErr == nil {
		t.Fatal("a rule reading incarnation.state PASSED on the create path — " +
			"the incarnation does not exist there, so the rule checked nothing and reported success")
	}

	// Intelligible, not merely loud: the message names the field, the context and
	// what the path does have.
	msg := scopeErr.Error()
	for _, want := range []string{"incarnation.state", "create path", "id"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q: %s", want, msg)
		}
	}
}

// TestValidateScope_SilentFormsAreRefusedToo — `has()` and `size()` are the forms
// an evaluation-time sentinel cannot catch: against an empty map they answer false
// and 0 rather than failing. The guard runs before evaluation and on every
// reference position, so both refuse.
func TestValidateScope_SilentFormsAreRefusedToo(t *testing.T) {
	for _, that := range []string{
		`!has(incarnation.state) || incarnation.state.` + guardIncState + ` > 0`,
		`size(incarnation) > 0`,
		`incarnation["state"] != null`,
	} {
		fail, scopeErr := evalGuard(t, that, RequestedIncarnation(guardIncID))
		if scopeErr == nil {
			t.Errorf("rule %q was not refused on the create path (fail=%v) — "+
				"this is the form that reads nothing and reports success", that, fail)
		}
	}
}

// TestValidateScope_ComposedIDWithdrawsTheIdentifier — a create scenario that
// composes its own id (ADR-0079) has not composed it yet at this point, so even
// the identifier refuses. Passing "" through instead would satisfy a length or
// prefix rule and report a pass.
func TestValidateScope_ComposedIDWithdrawsTheIdentifier(t *testing.T) {
	_, scopeErr := evalGuard(t, `incarnation.id != ""`, ComposedIncarnation())
	if scopeErr == nil {
		t.Fatal("incarnation.id passed under the composed-id stance — the id is not composed until after this gate")
	}
	if !strings.Contains(scopeErr.Error(), "id.template") {
		t.Errorf("the refusal must point at the input components that feed id.template, got: %v", scopeErr)
	}

	// WithComposedID is the withdrawal ValidateInput applies once it has read the
	// manifest; it must not widen anything else.
	if got := RequestedIncarnation(guardIncID).WithComposedID().Scope(); got != IncarnationComposed {
		t.Errorf("WithComposedID on a requested stance = %v, want IncarnationComposed", got)
	}
	if got := dayTwoContext().WithComposedID().Scope(); got != IncarnationLoaded {
		t.Errorf("WithComposedID must leave a day-2 stance alone, got %v", got)
	}
}

// TestValidateScope_EmptyRequestedIDIsNotAFact — an absent id is not the empty
// string. Putting "" in the namespace would make `incarnation.id != "x"` pass and
// `size(incarnation.id) < 64` pass, both without an id.
func TestValidateScope_EmptyRequestedIDIsNotAFact(t *testing.T) {
	inc := RequestedIncarnation("")
	if inc.Scope() != IncarnationAbsent {
		t.Fatalf("RequestedIncarnation(\"\") = %v, want IncarnationAbsent", inc.Scope())
	}
	if _, scopeErr := evalGuard(t, `incarnation.id != "nothing"`, inc); scopeErr == nil {
		t.Error("a rule over incarnation.id passed with no id in the request")
	}
	if got := LoadedIncarnation(nil, nil).Scope(); got != IncarnationAbsent {
		t.Errorf("LoadedIncarnation(nil, nil) = %v, want IncarnationAbsent — a day-2 stance answering for nothing is the false-green shape", got)
	}
}

// TestValidateScope_AnsweredForIsNotWhatTheRowCarries — the day-2 path answers for
// `state` on EVERY request, so an incarnation whose state column is NULL gives the
// ordinary no-such-key the run gives, not "this path does not have that fact".
//
// Deriving the answerable set from the row's keys made the same scenario report
// broken for one incarnation and fine for its sibling — a diagnosis that depends on
// data, about a file.
func TestValidateScope_AnsweredForIsNotWhatTheRowCarries(t *testing.T) {
	empty := LoadedIncarnation(guardDayTwoFields, map[string]any{
		"id": guardIncID, "name": guardIncID, "service": "redis", "service_version": "v1.2.3",
		// no "state" — the column is NULL
	})

	_, err := EvalValidateRules(rule(`incarnation.state.`+guardIncState+` > 0`), nil, empty)
	if errors.Is(err, ErrIncarnationNotInScope) {
		t.Fatalf("a NULL state column was reported as the SCENARIO being out of scope: %v", err)
	}
	// What is left is the ordinary no-such-key the RUN gives for the same row
	// (render.incarnationVars omits the key too) — a fact about the incarnation,
	// not about the file.
	if err == nil || !strings.Contains(err.Error(), "no such key: state") {
		t.Fatalf("want the ordinary no-such-key for an empty row, got: %v", err)
	}
}

// TestValidateScope_UnansweredValueIsDroppedFromTheNamespace — a value the caller
// hands over for a field the stance does not answer for must not be readable. The
// guard and the activation have to agree, or the guard is the only thing standing
// between a rule and a fact the path never promised.
func TestValidateScope_UnansweredValueIsDroppedFromTheNamespace(t *testing.T) {
	c := LoadedIncarnation([]string{"id"}, map[string]any{"id": guardIncID, "label": "caption"})
	if _, ok := c.inc["label"]; ok {
		t.Error("a value outside the answered-for set reached the activation")
	}
}

// TestValidateScope_ZeroContextIsInputOnly — the zero value is what `validate:`
// was before this ticket, and a caller that states no stance must not silently
// inherit an incarnation.
func TestValidateScope_ZeroContextIsInputOnly(t *testing.T) {
	var zero ValidateContext
	if _, scopeErr := evalGuard(t, `incarnation.id != ""`, zero); scopeErr == nil {
		t.Error("the zero context answered for an incarnation it does not have")
	}
	fail, scopeErr := evalGuard(t, `input.port > 0`, zero)
	if scopeErr != nil || fail != nil {
		t.Errorf("input-only rules must be unaffected by the stance (fail=%v, err=%v)", fail, scopeErr)
	}
}

// TestValidateScope_LoadedRefusesAFieldItDoesNotCarry — inside the day-2 stance
// the guard still applies: `host_count` is in the RUN's incarnation namespace and
// not in this one (it is a roster question, and the roster is not read on the
// request path). Refusing beats a no-such-key here for the same reason it does on
// create — the author is told which context lacks the fact, not just that a lookup
// missed.
func TestValidateScope_LoadedRefusesAFieldItDoesNotCarry(t *testing.T) {
	_, scopeErr := evalGuard(t, `incarnation.host_count > 1`, dayTwoContext())
	if scopeErr == nil {
		t.Fatal("incarnation.host_count passed the day-2 gate, which does not read the roster")
	}
	if !strings.Contains(scopeErr.Error(), "host_count") {
		t.Errorf("the refusal must name the field, got: %v", scopeErr)
	}
}

// TestValidateScope_GuardIsIndependentOfOperatorInput — the scope check runs over
// EVERY rule before any of them is evaluated. Inside the eval loop it would sit
// behind the first-false short-circuit, and then whether a scenario looks broken
// would depend on what the operator typed: a request tripping rule 0 gets a clean
// 422, the next request 500s on rule 1. A scenario is broken or it is not.
func TestValidateScope_GuardIsIndependentOfOperatorInput(t *testing.T) {
	rules := []ValidateRule{
		{That: "input.port > 0", Message: "port must be positive"},
		{That: "incarnation.state.tier == 'gold'", Message: "gold only"},
	}
	// The input that makes rule 0 fail — the request that used to hide rule 1.
	fail, err := EvalValidateRules(rules, map[string]any{"port": 0}, RequestedIncarnation(guardIncID))
	if fail != nil {
		t.Fatalf("the failing first rule masked the broken second one: %v", fail)
	}
	if !errors.Is(err, ErrIncarnationNotInScope) {
		t.Fatalf("want the scope refusal regardless of input, got: %v", err)
	}
}

// TestValidateScope_ShadowingComprehensionVariableIsRefused — naming a
// comprehension variable `incarnation` shadows the namespace at evaluation, and the
// macro-free walk cannot tell the two apart. Refused with a rename request rather
// than guessed at: guessing "not a reference" is the false green, and guessing
// "a reference" would refuse a rule that reads only input.
func TestValidateScope_ShadowingComprehensionVariableIsRefused(t *testing.T) {
	const that = `[1, 2].all(incarnation, incarnation > 0)`
	_, scopeErr := evalGuard(t, that, dayTwoContext())
	if scopeErr == nil {
		t.Fatal("a comprehension variable named `incarnation` was read as the namespace and let through")
	}
	if !strings.Contains(scopeErr.Error(), "rename") {
		t.Errorf("the refusal must ask for a rename, got: %v", scopeErr)
	}

	// An ordinary comprehension is untouched — the check is about the NAME.
	fail, scopeErr := evalGuard(t, `[1, 2].all(n, n > 0)`, dayTwoContext())
	if scopeErr != nil || fail != nil {
		t.Errorf("an ordinary comprehension was disturbed (fail=%v, err=%v)", fail, scopeErr)
	}
}

// TestValidateScope_NamespaceInProseIsNotAReference — the guard is AST-based, so
// the word inside a CEL string constant is not a read. A textual guard would
// refuse this rule and there would be no way to write it.
func TestValidateScope_NamespaceInProseIsNotAReference(t *testing.T) {
	fail, scopeErr := evalGuard(t, `string(input.port) != "incarnation.state"`, ValidateContext{})
	if scopeErr != nil {
		t.Fatalf("a string constant naming the namespace was read as a reference: %v", scopeErr)
	}
	if fail != nil {
		t.Fatalf("unexpected rule failure: %v", fail)
	}
}
