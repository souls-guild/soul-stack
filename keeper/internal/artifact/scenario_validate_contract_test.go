package artifact

// `validate:` travels in the scenario listing (NIM-833). The block stopped being
// an internal detail the moment an operator form was expected to show the
// requirements before anything is submitted: a form rendering only input_schema
// shows half the contract, and the other half arrives as a 422 afterwards.
//
// What travels is the TEXT — `that` and `message`. Evaluation stays keeper-side
// (config.EvalValidateRules), so there is one evaluator and one truth; a second
// one in a client would diverge from it, and the question would only be when.
//
// HOW TO BREAK IT ON PURPOSE (each mutation was run and turns the named test red):
//  1. return `rules` unchanged from dropStrippedValidateRules —
//     StrippedSecretDoesNotLeakThroughRuleText.
//  2. decode `validate:` in the main scenarioYAML struct again instead of through
//     validateRulesYAML — MalformedRuleDoesNotTakeTheFormWithIt.
//  3. drop the covenant-first append in mergeCovenantSectionsRaw —
//     ValidateFromCovenantIsPublished.
//  4. compare top-level names in dropStrippedValidateRules again, or have
//     readReachesStripped publish a read that ends on a partly stripped container —
//     NestedStrippedSecretDoesNotLeakThroughRuleText.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// TestListScenarios_PublishesValidateRules — the rules reach the reply, in
// declaration order (the order the keeper evaluates them in, and therefore the
// order the first failure comes from).
func TestListScenarios_PublishesValidateRules(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
input:
  replicas: { type: integer, default: 0 }
validate:
  - that: "int(input.replicas) <= 5"
    message: "at most five replicas"
  - that: "incarnation.id.matches('^[a-z]')"
    message: "the id must start with a letter"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	rules := got[0].Validate
	if len(rules) != 2 {
		t.Fatalf("validate = %+v, want 2 rules", rules)
	}
	if rules[0].That != "int(input.replicas) <= 5" || rules[0].Message != "at most five replicas" {
		t.Errorf("rule 0 = %+v — the predicate and the reason must travel verbatim, not paraphrased", rules[0])
	}
	if !strings.Contains(rules[1].That, "incarnation.id") {
		t.Errorf("rule 1 = %+v, declaration order lost", rules[1])
	}
}

// TestListScenarios_ValidateFromCovenantIsPublished — a scenario whose
// requirements all live in the shared contract (`extends:`) must publish them
// anyway, covenant-first: that is the order MergeCovenant gives the keeper, and a
// form showing the local delta alone would show requirements the run does not
// enforce in the order it enforces them.
func TestListScenarios_ValidateFromCovenantIsPublished(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "covenant", `input:
  version: { type: string, required: true }
validate:
  - that: "input.version != ''"
    message: "version is required"
`)
	writeScenario(t, root, "create_from_souls", `name: create_from_souls
create: true
extends: covenant
validate:
  - that: "input.version != 'banned'"
    message: "that version is withdrawn"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	rules := got[0].Validate
	if len(rules) != 2 {
		t.Fatalf("validate = %+v, want the covenant rule and the local one", rules)
	}
	if rules[0].Message != "version is required" {
		t.Errorf("covenant rule must come first, got %+v", rules)
	}
}

// TestListScenarios_NoValidateOmitsTheField — a scenario with no rules serializes
// exactly as it did before the field existed.
func TestListScenarios_NoValidateOmitsTheField(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", "name: create\ntasks: []\n")

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if got[0].Validate != nil {
		t.Errorf("validate = %+v, want nil", got[0].Validate)
	}
	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"validate"`) {
		t.Errorf("the key must be omitted for a scenario with no rules: %s", b)
	}
}

// TestListScenarios_StrippedSecretDoesNotLeakThroughRuleText — the rule text is the
// third half of the same reply, and it names input fields by name, often beside a
// literal. `GET /v1/services/{id}/scenarios` needs only `service.list` — weaker
// than `incarnation.run` — so publishing the predicate verbatim would hand back the
// name and comparison value of a field that was just stripped from input_schema for
// being a declared secret. "Not on the form" has to be true of all three halves.
func TestListScenarios_StrippedSecretDoesNotLeakThroughRuleText(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `name: create
input:
  admin_password: { $type: MintedToken }
  replicas: { type: integer, default: 1 }
validate:
  - that: "input.admin_password != 'changeme'"
    message: "the default password is not allowed"
  - that: ".input.admin_password != 'changeme'"
    message: "the root-scoped spelling of the same read (NIM-853)"
  - that: "int(input.replicas) <= 5"
    message: "at most five replicas"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	sc := got[0]
	if _, published := sc.InputSchema["admin_password"]; published {
		t.Fatal("precondition: the secret field was not stripped from input_schema")
	}
	for _, r := range sc.Validate {
		if strings.Contains(r.That, "admin_password") || strings.Contains(r.That, "changeme") {
			t.Errorf("a rule naming the stripped secret was published: %+v", r)
		}
	}
	// The rule that touches nothing stripped still travels — the drop is targeted,
	// not a blanket refusal to publish once a secret exists.
	if len(sc.Validate) != 1 || !strings.Contains(sc.Validate[0].That, "replicas") {
		t.Errorf("validate = %+v, want only the non-secret rule", sc.Validate)
	}
}

// nestedSecretTypes and nestedSecretInput put a minted member at every depth the
// strip reaches without removing the container that holds it: a property, an array
// element's property, and an object's open half.
const nestedSecretTypes = `types:
  MintedToken:
    type: secret
  AclUser:
    type: object
    properties:
      name: { type: string }
      minted_pw: { $type: MintedToken }
`

const nestedSecretInput = `input:
  creds:
    type: object
    properties:
      user: { type: string }
      minted_pw: { $type: MintedToken }
  users:
    type: array
    items: { $type: AclUser }
  tokens:
    type: object
    properties:
      label: { type: string }
    additional_properties: { $type: MintedToken }
  replicas: { type: integer, default: 1 }
`

// nestedSecretControls read only what survived the strip, through the same
// containers, so a fix that drops every rule touching them is not mistaken for one
// that drops the leaking rule.
var nestedSecretControls = []string{
	`input.creds.user != ''`,
	`input.creds["user"] != ''`,
	`input.users[0].name != ''`,
	`input.tokens.label != ''`,
	`int(input.replicas) <= 5`,
}

// TestListScenarios_NestedStrippedSecretDoesNotLeakThroughRuleText — NIM-853. A
// minted member nested inside a container that survives the strip must not come back
// through the rule text: not its name, not the literal compared to it. The read is
// asserted on the serialized reply, which is what a `service.list` caller receives.
func TestListScenarios_NestedStrippedSecretDoesNotLeakThroughRuleText(t *testing.T) {
	const literal = "hunter2-literal"
	leaking := map[string]string{
		"dotted":            `input.creds.minted_pw != 'hunter2-literal'`,
		"indexed":           `input.creds["minted_pw"] != 'hunter2-literal'`,
		"mixed spelling":    `input["creds"].minted_pw != 'hunter2-literal'`,
		"root-scoped ident": `.input.creds.minted_pw != 'hunter2-literal'`,
		"has":               `has(input.creds.minted_pw)`,
		"whole map literal": `input.creds != {'user': 'admin', 'minted_pw': 'hunter2-literal'}`,
		"in":                `!('minted_pw' in input.creds)`,
		"key comprehension": `input.creds.all(k, k != 'minted_pw')`,
		"dynamic key":       `input.creds[input.creds.user] != 'hunter2-literal'`,
		"element position":  `input.users[0].minted_pw != 'hunter2-literal'`,
		"element bind":      `input.users.all(u, u.minted_pw != 'hunter2-literal')`,
		"open half":         `input.tokens.anything != 'hunter2-literal'`,
		"bare root":         `input != {'creds': {'minted_pw': 'hunter2-literal'}}`,
		"unparseable":       `input.creds.minted_pw != 'hunter2-literal' &&`,
		"message literal":   `input.creds{minted_pw: 'hunter2-literal'} != null`,
	}

	for name, rule := range leaking {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTypesCatalog(t, root, nestedSecretTypes)
			var b strings.Builder
			b.WriteString("name: create\n" + nestedSecretInput + "validate:\n")
			fmt.Fprintf(&b, "  - that: %q\n    message: leaking\n", rule)
			for _, c := range nestedSecretControls {
				fmt.Fprintf(&b, "  - that: %q\n    message: control\n", c)
			}
			b.WriteString("tasks: []\n")
			writeScenario(t, root, "create", b.String())

			got, err := ListScenarios(root, discardLogger())
			if err != nil {
				t.Fatalf("ListScenarios: %v", err)
			}
			reply, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if !strings.Contains(string(reply), `"user"`) {
				t.Fatalf("precondition: the container holding the secret did not survive the strip: %s", reply)
			}
			for _, secret := range []string{literal, "minted_pw"} {
				if strings.Contains(string(reply), secret) {
					t.Errorf("%q reached the published reply through rule %q:\n%s", secret, rule, reply)
				}
			}

			published := map[string]bool{}
			for _, r := range got[0].Validate {
				published[r.That] = true
			}
			for _, c := range nestedSecretControls {
				if !published[c] {
					t.Errorf("control %q was dropped — it reads only what survived the strip", c)
				}
			}
		})
	}
}

// TestListScenarios_RulesPublishWhenNothingIsStripped — a scenario with no minted
// field publishes every rule, including the ones whose reads cannot be followed: the
// drop exists for what the strip removed, and nothing was.
func TestListScenarios_RulesPublishWhenNothingIsStripped(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
input:
  creds:
    type: object
    properties:
      user: { type: string }
validate:
  - that: "size(input) > 0"
    message: "bare root"
  - that: "input.creds[input.creds.user] != ''"
    message: "dynamic key"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got[0].Validate) != 2 {
		t.Errorf("validate = %+v, want both rules", got[0].Validate)
	}
}

// TestListScenarios_MalformedRuleDoesNotTakeTheFormWithIt — `validate:` is decoded
// in a pass of its own, so a rule the projection cannot read costs the rule and not
// the artifact. Sharing one Unmarshal made a `that: 8080` drop the whole scenario
// from the dropdown — or, in a covenant, every inherited input field from the form,
// after which the run answers 422 for fields the form never offered.
func TestListScenarios_MalformedRuleDoesNotTakeTheFormWithIt(t *testing.T) {
	root := t.TempDir()
	writeCovenant(t, root, "covenant", `input:
  version: { type: string, required: true }
validate:
  - that: 8080
    message: "a rule that cannot decode"
`)
	writeScenario(t, root, "create", `name: create
extends: covenant
input:
  replicas: { type: integer, default: 1 }
validate:
  - that: [not, a, string]
    message: "nor can this one"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("the scenario left the listing over a malformed validate: rule; got %d entries", len(got))
	}
	for _, field := range []string{"version", "replicas"} {
		if _, ok := got[0].InputSchema[field]; !ok {
			t.Errorf("input field %q was lost with the malformed rule: %#v", field, got[0].InputSchema)
		}
	}
}

// TestListScenarios_RuleWithoutPredicateIsNotPublished — a bare `message:` names a
// requirement the keeper does not enforce. soul-lint rejects the file
// (missing_required_field); the listing must not put the promise on a form in the
// meantime.
func TestListScenarios_RuleWithoutPredicateIsNotPublished(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "create", `name: create
validate:
  - message: "a requirement with nothing behind it"
  - that: "true"
    message: "real"
tasks: []
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	rules := got[0].Validate
	if len(rules) != 1 || rules[0].Message != "real" {
		t.Errorf("validate = %+v, want only the rule that has a predicate", rules)
	}
}
