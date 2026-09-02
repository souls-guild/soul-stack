package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard tests for NIM-751 / [ADR-0086] §5 on the VALUE side: a `type: secret`
// property reached from `input:` through `$type` is not required of the operator,
// takes no default, and refuses a value supplied anyway.
//
// The shape under test is the one WB's redis is written in and the one §5 exists
// for: one shared type feeding both `state_schema` and a scenario's `input:`.

// aclUserWithSecret — a shared element type whose password the platform mints.
// `required: true` on the secret is deliberate: it is the trap. The state-side
// refusal of that key ([ADR-0086] §7) runs off `state_schema`, so a type reached
// only from `input:` never passes it, and a form that honoured the flag would
// demand a password no operator can produce.
const aclUserWithSecret = `types:
  AclUser:
    type: object
    properties:
      name:
        type: string
        required: true
      password:
        type: secret
        key: name
        required: true
`

// resolveInput parses a type catalog and resolves an `input:` schema against it,
// failing the test on any diagnostic — the fixtures below are all legal.
func resolveInput(t *testing.T, catalogYAML string, in InputSchemaMap) InputSchemaMap {
	t.Helper()
	cat, cdiags := ParseTypeCatalog("types.yml", []byte(catalogYAML))
	if diag.HasErrors(cdiags) {
		t.Fatalf("ParseTypeCatalog: %v", cdiags)
	}
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		t.Fatalf("ResolveTypeRefs: %v", rdiags)
	}
	return resolved
}

// An element without the secret passes: the platform mints it, so its absence is
// the normal case and not a missing required field.
func TestInputSecretType_NotRequiredOfOperator(t *testing.T) {
	schema := resolveInput(t, aclUserWithSecret, InputSchemaMap{
		"users": {Type: "array", Items: &InputSchema{TypeRef: "AclUser"}},
	})
	// The property is still IN the schema — the engine has to know it exists to
	// refuse a value for it. What must not happen is being asked for it. The
	// `required: true` the catalog writes on it must survive the resolve too, or the
	// trap this fixture exists to spring is not armed.
	props := schema["users"].Items.Properties
	if props["password"] == nil {
		t.Fatalf("the resolved schema lost the declaration: %#v", props)
	}
	if !props["password"].Required {
		t.Fatal("fixture is inert: the secret property is not required, so nothing is being guarded")
	}

	merged, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "app"}},
	})
	if err != nil {
		t.Fatalf("an element without the minted secret was refused: %v", err)
	}
	users := merged["users"].([]any)
	if _, present := users[0].(map[string]any)["password"]; present {
		t.Fatalf("the effective input grew a password: %#v", users[0])
	}
}

// The same at the top level, which is a SEPARATE phase reading a separate flag
// ([requireInputValues], not [validateObjectFields]). The flag is carried by the
// type body itself, so the resolved node really does arrive with Required set —
// asserted below, because a fixture that silently lost it would make this test
// pass without exercising anything.
func TestInputSecretType_TopLevelNotRequiredOfOperator(t *testing.T) {
	schema := resolveInput(t, `types:
  MintedToken:
    type: secret
    required: true
`, InputSchemaMap{
		"admin_token": {TypeRef: "MintedToken"},
	})
	if !schema["admin_token"].Required {
		t.Fatalf("fixture is inert: the resolved node is not required, so nothing is being guarded")
	}

	if _, err := ResolveInputValues(schema, map[string]any{}); err != nil {
		t.Fatalf("an operator was asked for a value the platform mints: %v", err)
	}
}

// A `default:` inside the shared type must not materialize into the effective
// input. It is outside the `type: secret` grammar ([ADR-0086] §6), but that refusal
// is reached through `state_schema` only — so the merge has to fail closed.
func TestInputSecretType_DefaultNotMaterialized(t *testing.T) {
	schema := resolveInput(t, `types:
  MintedToken:
    type: secret
`, InputSchemaMap{
		"admin_token": {TypeRef: "MintedToken"},
	})
	// The default is written on the resolved node directly: parsing it out of
	// types.yml is a different rule's business, and this test is about the merge.
	schema["admin_token"].Default = "hunter2"

	// MergeInputDefaults, not ResolveInputValues: this is phase 1 in isolation. Run
	// through the full gate the assertion would still fire when the skip is removed,
	// but via the phase-3 refusal — a green light that says nothing about the phase
	// under test.
	merged := MergeInputDefaults(schema, map[string]any{})
	if v, present := merged["admin_token"]; present {
		t.Fatalf("a minted secret got a default in the effective input: %v", v)
	}
}

// The window [ADR-0083] §6 declares closed: a value supplied anyway is refused,
// not carried into params.value of the state step.
func TestInputSecretType_SuppliedValueRefused(t *testing.T) {
	schema := resolveInput(t, aclUserWithSecret, InputSchemaMap{
		"users": {Type: "array", Items: &InputSchema{TypeRef: "AclUser"}},
	})

	_, err := ResolveInputValues(schema, map[string]any{
		"users": []any{map[string]any{"name": "app", "password": "hunter2"}},
	})
	if err == nil {
		t.Fatal("an operator-supplied value for a declared secret was accepted")
	}
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("refused under the wrong rule: %v", err)
	}
	// The address, so the author can find the property...
	if !strings.Contains(err.Error(), "$.users[0].password") {
		t.Fatalf("the refusal does not name the offending path: %v", err)
	}
	// ...and never the value. This error reaches incarnation.StatusDetails and
	// audit; a password formatted into it is a password stored.
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the refusal echoes the secret the caller sent: %v", err)
	}
}

// ★ The position ordinary value validation does not reach:
// `additional_properties`. [validateObjectFields] skips a key the object's
// `properties:` does not describe, so before NIM-751 covered this shape the whole
// refusal had no reach here — and the form strip DID cover it, which is the worst
// of both: the property is off the form and the value is accepted anyway.
func TestInputSecretType_SuppliedValueUnderAdditionalPropertiesRefused(t *testing.T) {
	schema := resolveInput(t, aclUserWithSecret, InputSchemaMap{
		"users": {
			Type:                 "object",
			Properties:           InputSchemaMap{"admin": {Type: "string"}},
			AdditionalProperties: &InputSchema{TypeRef: "AclUser"},
		},
	})

	_, err := ResolveInputValues(schema, map[string]any{
		"users": map[string]any{
			"alice": map[string]any{"name": "alice", "password": "hunter2"},
		},
	})
	if err == nil {
		t.Fatal("a password under additional_properties was accepted")
	}
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("refused under the wrong rule: %v", err)
	}
	if !strings.Contains(err.Error(), "$.users.alice.password") {
		t.Fatalf("the refusal does not name the offending path: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the refusal echoes the secret the caller sent: %v", err)
	}
}

// The other additional_properties shape: the open map's VALUES are themselves the
// minted secret, so every key an operator writes is a value they must not supply.
func TestInputSecretType_AdditionalPropertiesOfSecretsRefused(t *testing.T) {
	schema := resolveInput(t, `types:
  MintedToken:
    type: secret
`, InputSchemaMap{
		"bag": {Type: "object", AdditionalProperties: &InputSchema{TypeRef: "MintedToken"}},
	})

	_, err := ResolveInputValues(schema, map[string]any{
		"bag": map[string]any{"anything": "hunter2"},
	})
	if err == nil {
		t.Fatal("an open map of minted secrets accepted a value")
	}
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("refused under the wrong rule: %v", err)
	}
}

// The form drops a CONTAINER whose every value is minted, so requiredness must drop
// it too — otherwise the operator is told a field they were never offered is
// required, which is the rule failing one level above where it was implemented.
func TestInputSecretType_RequiredContainerOfSecretsNotDemanded(t *testing.T) {
	cat := `types:
  MintedToken:
    type: secret
`
	t.Run("array of secrets", func(t *testing.T) {
		schema := resolveInput(t, cat, InputSchemaMap{
			"toks": {Type: "array", Required: true, Items: &InputSchema{TypeRef: "MintedToken"}},
		})
		if !schema["toks"].Required {
			t.Fatal("fixture is inert: the container is not required")
		}
		if _, err := ResolveInputValues(schema, map[string]any{}); err != nil {
			t.Fatalf("an operator was asked for a container the form does not show: %v", err)
		}
	})
	t.Run("open map of secrets", func(t *testing.T) {
		schema := resolveInput(t, cat, InputSchemaMap{
			"bag": {Type: "object", Required: true, AdditionalProperties: &InputSchema{TypeRef: "MintedToken"}},
		})
		if !schema["bag"].Required {
			t.Fatal("fixture is inert: the container is not required")
		}
		if _, err := ResolveInputValues(schema, map[string]any{}); err != nil {
			t.Fatalf("an operator was asked for a container the form does not show: %v", err)
		}
	})
}

// Same refusal for a scalar field, where the value never passes through
// [validateObjectFields].
func TestInputSecretType_SuppliedScalarValueRefused(t *testing.T) {
	schema := resolveInput(t, `types:
  MintedToken:
    type: secret
`, InputSchemaMap{
		"admin_token": {TypeRef: "MintedToken"},
	})

	_, err := ResolveInputValues(schema, map[string]any{"admin_token": "hunter2"})
	if err == nil {
		t.Fatal("a scalar declared secret accepted an operator value")
	}
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("refused under the wrong rule: %v", err)
	}
}

// `type: secret` written DIRECTLY in `input:` is the other half of the rule: on a
// form a secret is the `secret: true` modifier, and the two mean opposite things
// about who produces the value. The refusal must carry an address and point at the
// modifier — a bare "not in the enum" leaves the author guessing.
func TestInputSecretType_DirectDeclarationRefusedWithAddress(t *testing.T) {
	src := `name: create
tasks: []
input:
  admin_password:
    type: secret
`
	_, _, diags, err := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	var found *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == "input_type_invalid" {
			found = &diags[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("`type: secret` in input: was accepted; diags = %v", diags)
	}
	if found.Line != 5 || found.Column == 0 {
		t.Fatalf("refused without an address: line = %d, column = %d", found.Line, found.Column)
	}
	if found.YAMLPath != "$.input.admin_password.type" {
		t.Fatalf("YAMLPath = %q", found.YAMLPath)
	}
	if !strings.Contains(found.Hint, "secret: true") {
		t.Fatalf("the hint does not name the input spelling of a secret: %q", found.Hint)
	}
}

// The mirror-image regression the container rule can cause: a field the form still
// SHOWS must still be demandable. An object keeping ordinary properties beside a
// minted open map is fillable, so `required: true` on it has to keep working —
// dropping it would turn a mandatory input optional with nothing said.
func TestInputSecretType_ContainerWithFillablePropertiesStaysRequired(t *testing.T) {
	schema := resolveInput(t, `types:
  MintedToken:
    type: secret
`, InputSchemaMap{
		"f": {
			Type:                 "object",
			Required:             true,
			Properties:           InputSchemaMap{"real": {Type: "string", Required: true}},
			AdditionalProperties: &InputSchema{TypeRef: "MintedToken"},
		},
	})
	if schema["f"].NotAskedOfOperator() {
		t.Fatal("a field with fillable properties was classed as not asked of the operator")
	}

	if _, err := ResolveInputValues(schema, map[string]any{}); err == nil {
		t.Fatal("a required, fillable field stopped being required")
	}
	if _, err := ResolveInputValues(schema, map[string]any{
		"f": map[string]any{"real": "x"},
	}); err != nil {
		t.Fatalf("a legitimate value was refused: %v", err)
	}
	// ...and the minted half of the same field is still closed.
	_, err := ResolveInputValues(schema, map[string]any{
		"f": map[string]any{"real": "x", "anything": "hunter2"},
	})
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("the open half of a kept container accepted a minted value: %v", err)
	}
}

// The other side of the same rule: an object whose every property is minted has no
// fillable half at all, so the form drops it and requiredness must follow. This is
// the `properties:` route to the failure the container predicate was written for.
func TestInputSecretType_AllMintedObjectNotDemanded(t *testing.T) {
	schema := resolveInput(t, `types:
  AllMinted:
    type: object
    properties:
      pw:
        type: secret
`, InputSchemaMap{
		"f": {TypeRef: "AllMinted", Required: true},
	})
	// The overlay writes the reference node's requiredness only from a real YAML
	// bool, so set it here and assert it took — otherwise the fixture proves nothing.
	schema["f"].Required = true
	if !schema["f"].Required {
		t.Fatal("fixture is inert: the field is not required, so requiredness is not being guarded")
	}
	if !schema["f"].NotAskedOfOperator() {
		t.Fatal("an object with nothing fillable is still classed as asked of the operator")
	}
	if _, err := ResolveInputValues(schema, map[string]any{}); err != nil {
		t.Fatalf("an operator was asked for an object with no fillable property: %v", err)
	}
}

// A described key with no usable schema of its own must be judged by that schema,
// not by the object's `additional_properties`. Handing it the AP node refused a
// legitimate, described, non-secret value under a schema never written for it.
func TestInputSecretType_DescribedKeyIsNotJudgedByAdditionalProperties(t *testing.T) {
	schema := InputSchemaMap{
		"u": {
			Type:                 "object",
			Properties:           InputSchemaMap{"name": {}},
			AdditionalProperties: &InputSchema{Type: SecretTypeName},
		},
	}
	if _, err := ResolveInputValues(schema, map[string]any{
		"u": map[string]any{"name": "alice"},
	}); err != nil {
		t.Fatalf("a described, non-secret value was refused: %v", err)
	}
	// The undescribed key beside it is still refused.
	_, err := ResolveInputValues(schema, map[string]any{
		"u": map[string]any{"name": "alice", "other": "hunter2"},
	})
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("the open half accepted a minted value: %v", err)
	}
}

// An explicit null is a key the caller wrote. Both enforcement sites — the described
// property and the undescribed one — must refuse it, or they teach different rules
// about the same input.
func TestInputSecretType_ExplicitNullRefusedOnBothSites(t *testing.T) {
	described := InputSchemaMap{
		"u": {Type: "object", Properties: InputSchemaMap{"pw": {Type: SecretTypeName}}},
	}
	if _, err := ResolveInputValues(described, map[string]any{
		"u": map[string]any{"pw": nil},
	}); !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("a null on a described declared secret was accepted: %v", err)
	}

	undescribed := InputSchemaMap{
		"u": {Type: "object", AdditionalProperties: &InputSchema{Type: SecretTypeName}},
	}
	if _, err := ResolveInputValues(undescribed, map[string]any{
		"u": map[string]any{"pw": nil},
	}); !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Fatalf("a null under additional_properties was accepted: %v", err)
	}
}
