package scenario

// The `validate:` incarnation context ON THE REQUEST PATHS (NIM-833): the same
// scenario file, run through [ValidateInput] on day-2 and on create, must see
// different facts — and must say so rather than quietly seeing none.
//
// shared/config/validate_scope_guard_test.go guards the stance itself. This file
// guards the wiring: that the run path hands over the loaded row, that the create
// path hands over the requested id and nothing else, and that a scenario composing
// its own id has even that withdrawn — which only the manifest says, so only
// ValidateInput can do it.
//
// HOW TO BREAK IT ON PURPOSE (each mutation was run and turns the named test red):
//  1. pass config.ValidateContext{} instead of inc through to ResolveInputContract
//     in ValidateInput — every test below except CreateRuleOverStateRefuses, which
//     is the one case an input-only context happens to get right.
//  2. drop the `if scn.IDTemplate != ""` withdrawal from ValidateInput —
//     TestValidateInput_ComposedIDScenarioRefusesTheIdentifier.
//  3. add "label" to the map DayTwoIncarnation builds —
//     TestValidateInput_DayTwoCarriesNoLabel.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	scopeIncID    = "redis-billing"
	scopeIncLabel = "redis-billing-prod" // a valid identifier too — see the label test
)

// scopeScenario is a scenario whose whole content is one `validate:` rule, so a
// failure can only come from the rule.
func scopeScenario(rule, extra string) string {
	return "name: run\n" +
		extra +
		"input:\n  port: { type: integer, default: 6379 }\n" +
		"validate:\n" +
		"  - that: \"" + rule + "\"\n" +
		"    message: guard\n" +
		"tasks: []\n"
}

func scopeRun(t *testing.T, yaml string, inc config.ValidateContext) error {
	t.Helper()
	loader := &fakeInputLoader{yaml: yaml}
	_, err := ValidateInput(context.Background(), loader, artifact.ServiceRef{Name: "svc"}, "create", map[string]any{}, inc)
	return err
}

func dayTwoScope() config.ValidateContext {
	return DayTwoIncarnation(scopeIncID, "redis", "v1.2.3", map[string]any{"replicas": 3})
}

// TestValidateInput_DayTwoRuleReadsTheRow — case 1 of the ticket: a rule over
// `incarnation.id` passes on day-2, under both spellings of the root.
func TestValidateInput_DayTwoRuleReadsTheRow(t *testing.T) {
	for _, rule := range []string{
		`incarnation.id == '` + scopeIncID + `'`,
		`incarnation.name == '` + scopeIncID + `'`,
		`incarnation.state.replicas > 0`,
	} {
		if err := scopeRun(t, scopeScenario(rule, ""), dayTwoScope()); err != nil {
			t.Errorf("day-2 rule %q: %v", rule, err)
		}
	}
}

// TestValidateInput_CreateRuleReadsTheRequestedID — case 2: the same rule on the
// create path reads the identifier the request carried, and can reject on it
// BEFORE the incarnation is committed. That is the whole point: today the same
// constraint has to be an assert task, which runs after the row exists (NIM-832).
func TestValidateInput_CreateRuleReadsTheRequestedID(t *testing.T) {
	const rule = `incarnation.id.matches('^[a-z][a-z0-9-]{0,48}[a-z0-9]$')`

	if err := scopeRun(t, scopeScenario(rule, ""), config.RequestedIncarnation(scopeIncID)); err != nil {
		t.Fatalf("a valid requested id was rejected on the create path: %v", err)
	}

	err := scopeRun(t, scopeScenario(rule, ""), config.RequestedIncarnation("9redis-"))
	if !errors.Is(err, ErrValidateFailed) {
		t.Fatalf("an id violating the declared grammar must fail the create gate as ErrValidateFailed, got: %v", err)
	}
}

// TestValidateInput_CreateRuleOverStateRefuses — case 3: on the create path the
// incarnation does not exist, so a rule reading its state must REFUSE. It is
// reported as a pre-flight malfunction, not as a validate failure: the operator's
// input is fine, the scenario is not.
func TestValidateInput_CreateRuleOverStateRefuses(t *testing.T) {
	err := scopeRun(t, scopeScenario(`incarnation.state.replicas > 0`, ""), config.RequestedIncarnation(scopeIncID))
	if err == nil {
		t.Fatal("a rule over incarnation.state PASSED on the create path — it checked nothing and reported success")
	}
	if errors.Is(err, ErrValidateFailed) || errors.Is(err, ErrInputInvalid) {
		t.Fatalf("a scope refusal must not be reported as the operator's input being wrong: %v", err)
	}
	if !errors.Is(err, config.ErrIncarnationNotInScope) {
		t.Fatalf("want config.ErrIncarnationNotInScope, got: %v", err)
	}
	if !strings.Contains(err.Error(), "incarnation.state") {
		t.Errorf("the refusal must name the field the rule read: %v", err)
	}
}

// TestValidateInput_ComposedIDScenarioRefusesTheIdentifier — a create scenario
// carrying `id_template` (ADR-0079) has no id yet at this gate: it is composed from
// the very input being resolved, after the gate returns. The caller cannot know
// that; only the manifest does, so ValidateInput withdraws the identity itself.
func TestValidateInput_ComposedIDScenarioRefusesTheIdentifier(t *testing.T) {
	// A create scenario is caught OFFLINE (config.validateCreateScopeRules), so the
	// service never ships and no operator meets it as a 5xx. The manifest carries
	// both deciding facts, which is what makes that possible.
	declared := scopeScenario(`incarnation.id != ''`, "create: true\nid_template: \"${input.port}-redis\"\n")
	err := scopeRun(t, declared, config.RequestedIncarnation(""))
	if err == nil {
		t.Fatal("a create scenario reading incarnation.id while composing it was accepted")
	}
	if !strings.Contains(err.Error(), "cannot run on the create path") {
		t.Errorf("want the offline create-scope refusal, got: %v", err)
	}

	// The runtime guard is the second line, for the scenario the offline rule does
	// not judge: `id_template` without `create: true` (soul-lint warns
	// id_template_ignored, and the key is only read on the create path anyway).
	//
	// Both stances a create request can actually produce are exercised. The one
	// that matters is the EMPTY id: a composing scenario refuses a request carrying
	// one (422 id_not_composable), so keying the withdrawal off "an id was supplied"
	// made it a no-op on every request an operator really sends.
	undeclared := scopeScenario(`incarnation.id != ''`, "id_template: \"${input.port}-redis\"\n")
	for _, sent := range []string{"", scopeIncID} {
		err := scopeRun(t, undeclared, config.RequestedIncarnation(sent))
		if !errors.Is(err, config.ErrIncarnationNotInScope) {
			t.Fatalf("id=%q: want config.ErrIncarnationNotInScope, got: %v", sent, err)
		}
		if !strings.Contains(err.Error(), "id_template") {
			t.Errorf("id=%q: the refusal must send the author to the input components that feed id_template: %v", sent, err)
		}
	}
}

// TestValidateInput_DayTwoCarriesNoLabel — [ADR-0085] holds in this environment
// too, and `validate:` is the fourth one it has to hold in (the three named in
// keeper/internal/render/label_invariant_guard_test.go are the others). A caption
// is mutable: if a rule could read one, editing a screen would change whether a
// create or a run is allowed to proceed.
func TestValidateInput_DayTwoCarriesNoLabel(t *testing.T) {
	// The builder takes no caption at all, which is the cheapest place to stop it;
	// this asserts the map it produces, through the real function.
	inc := DayTwoIncarnation(scopeIncID, "redis", "v1.2.3", map[string]any{"replicas": 3})

	if err := scopeRun(t, scopeScenario(`incarnation.label != ''`, ""), inc); err == nil {
		t.Error("incarnation.label resolved in the validate: context — ADR-0085: a caption participates in nothing derived")
	}

	// `label` is unexpressible rather than merely unused, and this is the assertion
	// that says so: the field set the day-2 path answers for is fixed and does not
	// contain it, so no value a caller passes can make `incarnation.label` resolve.
	// (Substituting the caption FOR the identifier — the render guard's second
	// mutation — cannot happen here either: DayTwoIncarnation takes no caption.)
	for _, f := range dayTwoFields {
		if strings.EqualFold(f, "label") {
			t.Errorf("dayTwoFields gained %q — a scenario branching on a mutable caption "+
				"turns editing a screen into changing whether a run is allowed to start", f)
		}
	}

	// The day-2 set stays a SUBSET of the run's `incarnation.*`: host_count is the
	// one field the run has and this does not, and it must refuse rather than read
	// a roster the request path never loaded.
	if err := scopeRun(t, scopeScenario(`incarnation.host_count > 0`, ""), inc); !errors.Is(err, config.ErrIncarnationNotInScope) {
		t.Errorf("incarnation.host_count must be refused on the request path, got: %v", err)
	}
}

// TestValidateInput_NullStateIsNotAScopeError — an incarnation whose state column
// is NULL still runs on a path that ANSWERS FOR `state`. The rule gets the ordinary
// no-such-key the run gives; what it must not get is "this path does not have that
// fact", which would report the scenario broken for one incarnation and fine for
// its sibling.
func TestValidateInput_NullStateIsNotAScopeError(t *testing.T) {
	inc := DayTwoIncarnation(scopeIncID, "redis", "v1.2.3", nil)
	err := scopeRun(t, scopeScenario(`incarnation.state.replicas > 0`, ""), inc)
	if errors.Is(err, config.ErrIncarnationNotInScope) {
		t.Fatalf("a NULL state column was diagnosed as the scenario being out of scope: %v", err)
	}
	if err == nil {
		t.Fatal("a rule over an absent state silently passed")
	}
}
