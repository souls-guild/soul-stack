package artifact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard tests for NIM-751 / [ADR-0086] §5 on the FORM side: a property a shared
// type declares as `type: secret` must not reach the operator form, however deep it
// sits and whatever shape carries it.
//
// The subject is the projection, not a message: each test reads the schema the
// listing hands to the UI and asserts the property is not in it. A test that only
// pinned an error string would still pass with the property on the form.

// The canonical shape, and the one the WB redis service is written in: a shared
// element type whose password the platform mints, referenced from an array in
// `input:`. The rest of the element must survive — the whole point of §5 is that
// the type stays shared between the form and `state_schema`.
func TestListScenarios_SecretPropertyNotOnForm(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        type: string
        required: true
      perms:
        type: string
      password:
        type: secret
        key: name
`)
	writeScenario(t, root, "create", `input:
  users:
    type: array
    items:
      $type: AclUser
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	props := elementProperties(t, got[0].InputSchema, "users")
	if _, present := props["password"]; present {
		t.Fatalf("the form offers `password`, a value the platform mints: %#v", props)
	}
	// The shape the operator DOES fill in is untouched.
	if _, ok := props["name"]; !ok {
		t.Fatalf("name was stripped along with the secret: %#v", props)
	}
	if _, ok := props["perms"]; !ok {
		t.Fatalf("perms was stripped along with the secret: %#v", props)
	}
	// Belt and braces on the whole reply, not just the node we looked at: the JSON
	// the UI receives must not carry the MARKER anywhere. Matched as the key/value
	// pair, not the bare word — a field legitimately named `secret`, or a
	// description containing it, must not make this test fire.
	if body := marshalSchema(t, got[0].InputSchema); strings.Contains(body, `"type":"secret"`) {
		t.Fatalf("`type: secret` survives somewhere in the projected form: %s", body)
	}
}

// A top-level input field that IS the secret disappears entirely — there is no
// half of it to render.
func TestListScenarios_TopLevelSecretFieldNotOnForm(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `input:
  namespace:
    type: string
  admin_token:
    $type: MintedToken
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if _, present := got[0].InputSchema["admin_token"]; present {
		t.Fatalf("the form offers admin_token: %#v", got[0].InputSchema)
	}
	if _, ok := got[0].InputSchema["namespace"]; !ok {
		t.Fatalf("namespace was stripped with it: %#v", got[0].InputSchema)
	}
}

// A container whose ELEMENT schema is a secret goes away whole. Keeping the array
// and emptying its items would render a widget collecting values nothing reads.
func TestListScenarios_ArrayOfSecretsNotOnForm(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `input:
  tokens:
    type: array
    items:
      $type: MintedToken
  keep:
    type: string
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if _, present := got[0].InputSchema["tokens"]; present {
		t.Fatalf("the form offers an array of minted secrets: %#v", got[0].InputSchema)
	}
	if _, ok := got[0].InputSchema["keep"]; !ok {
		t.Fatalf("keep was stripped with it: %#v", got[0].InputSchema)
	}
}

// The other container position the ADR names: an open map whose values carry the
// minted secret. The typed side missed exactly this one, so the branch is worth its
// own test rather than being assumed symmetric with `items`.
func TestListScenarios_SecretUnderAdditionalPropertiesNotOnForm(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        type: string
      password:
        type: secret
        key: name
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `input:
  users:
    type: object
    additional_properties:
      $type: AclUser
  bag:
    type: object
    additional_properties:
      $type: MintedToken
  keep:
    type: string
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	users, ok := got[0].InputSchema["users"].(map[string]any)
	if !ok {
		t.Fatalf("users is missing: %#v", got[0].InputSchema)
	}
	ap, ok := users["additional_properties"].(map[string]any)
	if !ok {
		t.Fatalf("users.additional_properties is missing: %#v", users)
	}
	props, ok := ap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("users.additional_properties.properties is missing: %#v", ap)
	}
	if _, present := props["password"]; present {
		t.Fatalf("the form offers a minted password under additional_properties: %#v", props)
	}
	if _, ok := props["name"]; !ok {
		t.Fatalf("name was stripped along with the secret: %#v", props)
	}
	// A map whose VALUES are the secret has nothing left to fill in.
	if _, present := got[0].InputSchema["bag"]; present {
		t.Fatalf("the form offers an open map of minted secrets: %#v", got[0].InputSchema)
	}
	if _, ok := got[0].InputSchema["keep"]; !ok {
		t.Fatalf("keep was stripped with it: %#v", got[0].InputSchema)
	}
}

// `form:` is the presentation half of the same reply. A field name whose schema was
// just stripped would ship as a labelled input with nothing behind it.
func TestListScenarios_StrippedFieldLeavesTheFormLayout(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `input:
  namespace:
    type: string
  admin_token:
    $type: MintedToken
form:
  sections:
    - key: main
      title: Main
      fields:
        - name: namespace
          label: Namespace
        - name: admin_token
          label: Admin password
    - key: secrets
      title: Secrets
      fields:
        - name: admin_token
          label: Admin password
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	form := got[0].Form
	if form == nil {
		t.Fatal("the whole form layout disappeared")
	}
	// The section that had nothing but the stripped field goes with it; the mixed
	// one keeps its other field.
	if len(form.Sections) != 1 || form.Sections[0].Key != "main" {
		t.Fatalf("sections = %#v, want only the mixed one", form.Sections)
	}
	for _, f := range form.Sections[0].Fields {
		if f.Name == "admin_token" {
			t.Fatalf("the form layout still labels the stripped field: %#v", form.Sections[0].Fields)
		}
	}
	if len(form.Sections[0].Fields) != 1 || form.Sections[0].Fields[0].Name != "namespace" {
		t.Fatalf("fields = %#v, want namespace kept", form.Sections[0].Fields)
	}
}

// A `form:` naming a parameter that never existed is an AUTHOR error and soul-lint's
// to report (`form_field_unknown`). The drop must not swallow it — otherwise the
// listing hides the only symptom the author would see.
func TestListScenarios_UnknownFormFieldSurvives(t *testing.T) {
	root := t.TempDir()
	// A stripped field sits in the SAME section as the bogus one on purpose: without
	// it nothing is stripped, `dropStrippedFormFields` returns at its early exit, and
	// the loop this test is about never runs.
	writeTypesCatalog(t, root, `types:
  MintedToken:
    type: secret
`)
	writeScenario(t, root, "create", `input:
  namespace:
    type: string
  admin_token:
    $type: MintedToken
form:
  sections:
    - key: main
      fields:
        - name: typo_field
        - name: admin_token
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	form := got[0].Form
	if form == nil || len(form.Sections) != 1 {
		t.Fatalf("an author's broken form was tidied away: %#v", form)
	}
	names := make([]string, 0, len(form.Sections[0].Fields))
	for _, f := range form.Sections[0].Fields {
		names = append(names, f.Name)
	}
	if len(names) != 1 || names[0] != "typo_field" {
		t.Fatalf("fields = %v, want the author error kept and only the stripped field gone", names)
	}
}

// The state contract keeps its declared secrets. `state_schema` travels through the
// SAME raw resolver, and a strip written one level lower would have blanked the
// declaration the platform derives Vault paths from — the failure
// docs/adr/0083-declared-secret-state-fields.md:106-110 forbids by name.
func TestListStateSchema_KeepsDeclaredSecret(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        type: string
`)
	writeServiceManifest(t, root, `state_schema:
  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password:
          type: secret
          key: name
`)

	info, err := ListStateSchema(root, discardLogger())
	if err != nil {
		t.Fatalf("ListStateSchema: %v", err)
	}
	props := elementProperties(t, info.Schema, "redis_users")
	if _, ok := props["password"]; !ok {
		t.Fatalf("the state contract lost its declared secret: %#v", props)
	}
}

// elementProperties digs out `<field>.items.properties` of a raw projected schema,
// failing the test rather than panicking on a shape that is not there.
func elementProperties(t *testing.T, schema map[string]any, field string) map[string]any {
	t.Helper()
	node, ok := schema[field].(map[string]any)
	if !ok {
		t.Fatalf("%s is missing or not a map: %#v", field, schema)
	}
	items, ok := node["items"].(map[string]any)
	if !ok {
		t.Fatalf("%s.items is missing or not a map: %#v", field, node)
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s.items.properties is missing or not a map: %#v", field, items)
	}
	return props
}

func marshalSchema(t *testing.T, schema map[string]any) string {
	t.Helper()
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

// ★ The differential guard. `stripFormSecretNode` (raw maps, here) and
// `config.NotAskedOfOperator` (typed, shared/config) are two walks over one rule,
// kept apart by the fact that the operator form never becomes an `InputSchema`. They
// have already disagreed twice: the form dropped a container the require rule still
// demanded, and later dropped one whose ordinary properties were still fillable. A
// promise that they agree is worth nothing; this compares them.
//
// Both sides are driven from ONE `types.yml` + `input:` through the REAL paths —
// `ListScenarios` for the form, `ParseTypeCatalog` + `ResolveTypeRefs` for the typed
// side — so a divergence in either resolver shows up here too.
func TestStripFormSecrets_AgreesWithTypedPredicate(t *testing.T) {
	const catalog = `types:
  Tok:
    type: secret
  AclUser:
    type: object
    properties:
      name:
        type: string
      password:
        type: secret
        key: name
  AllMinted:
    type: object
    properties:
      pw:
        type: secret
`
	// Each case is one `input:` field. onForm says whether the operator should still
	// be offered it; the two implementations must both say exactly that.
	cases := []struct {
		name   string
		field  string
		onForm bool
	}{
		{"plain string", "f: {type: string}", true},
		{"scalar secret", "f: {$type: Tok}", false},
		{"array of secrets", "f: {type: array, items: {$type: Tok}}", false},
		{"open map of secrets", "f: {type: object, additional_properties: {$type: Tok}}", false},
		{"object with one secret among others", "f: {$type: AclUser}", true},
		{"array of such objects", "f: {type: array, items: {$type: AclUser}}", true},
		{"open map of such objects", "f: {type: object, additional_properties: {$type: AclUser}}", true},
		{"object whose every property is minted", "f: {$type: AllMinted}", false},
		{"array of all-minted objects", "f: {type: array, items: {$type: AllMinted}}", false},
		{"properties kept beside a minted open map",
			"f: {type: object, properties: {real: {type: string}}, additional_properties: {$type: Tok}}", true},
		{"all-minted properties beside a minted open map",
			"f: {type: object, properties: {pw: {$type: Tok}}, additional_properties: {$type: Tok}}", false},
		{"open map of arrays of secrets",
			"f: {type: object, additional_properties: {type: array, items: {$type: Tok}}}", false},
		{"array of open maps of secrets",
			"f: {type: array, items: {type: object, additional_properties: {$type: Tok}}}", false},
		{"closed object with an authored empty properties",
			"f: {type: object, properties: {}, additional_properties: false}", true},
		{"ordinary open map", "f: {type: object, additional_properties: {type: string}}", true},
		{"the `secret: true` MODIFIER is not this rule",
			"f: {type: string, secret: true}", true},
		{"object merely containing a secret-modifier field",
			"f: {type: object, properties: {pw: {type: string, secret: true}}}", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeTypesCatalog(t, root, catalog)
			writeScenario(t, root, "create", "input:\n  "+tc.field+"\n")

			got, err := ListScenarios(root, discardLogger())
			if err != nil {
				t.Fatalf("ListScenarios: %v", err)
			}
			_, onForm := got[0].InputSchema["f"]
			if onForm != tc.onForm {
				t.Fatalf("the form %s the field, want %s",
					keptOrDropped(onForm), keptOrDropped(tc.onForm))
			}

			cat, cdiags := config.ParseTypeCatalog("types.yml", []byte(catalog))
			if diag.HasErrors(cdiags) {
				t.Fatalf("ParseTypeCatalog: %v", cdiags)
			}
			// Parsed through the real manifest loader, not a bare yaml.Unmarshal:
			// InputSchema decodes via goccy's AST (`required` has two meanings,
			// `$type` is not a valid Go-yaml tag), so a v3 decode would hand the
			// predicate an empty struct and this test would pass on nothing.
			scn, _, sdiags, err := config.LoadScenarioManifestFromBytes(
				"scenario/create/main.yml",
				[]byte("name: create\ntasks: []\ninput:\n  "+tc.field+"\n"),
				config.ValidateOptions{})
			if err != nil || diag.HasErrors(sdiags) {
				t.Fatalf("LoadScenarioManifestFromBytes: %v / %v", err, sdiags)
			}
			resolved, rdiags := config.ResolveTypeRefs(scn.Input, cat)
			if diag.HasErrors(rdiags) {
				t.Fatalf("ResolveTypeRefs: %v", rdiags)
			}
			// Non-vacuity, asserted rather than argued: a decode that silently
			// produced an empty node would let every `onForm: true` row pass on
			// nothing. That is not hypothetical — the first draft of this test
			// decoded with yaml.v3 and did exactly that.
			node := resolved["f"]
			if node == nil || (node.Type == "" && node.Items == nil &&
				len(node.Properties) == 0 && node.AdditionalProperties == nil) {
				t.Fatalf("fixture parsed to nothing, the comparison would be vacuous: %#v", node)
			}
			notAsked := node.NotAskedOfOperator()
			if notAsked == onForm {
				t.Fatalf("the two walks disagree: the form %s the field, the engine says notAsked=%v",
					keptOrDropped(onForm), notAsked)
			}
		})
	}
}

func keptOrDropped(kept bool) string {
	if kept {
		return "KEPT"
	}
	return "DROPPED"
}
