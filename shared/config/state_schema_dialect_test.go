package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// `state_schema` is written in the same dialect as a scenario's `input:` ([NIM-740]).
// These are the guards on the three ways that can go wrong: the JSON-Schema form it
// replaced must be REFUSED rather than re-read as field names, the one relaxation the
// state dialect gets must apply THERE and nowhere else, and the segment of a derived
// Vault path that comes from state data must still be checked after the declaration it
// belongs to moved into a shared type.

// serviceWith wraps a state_schema body into a minimal manifest.
func serviceWith(body string) string {
	return "state_schema:\n" + body
}

// diagAtPath returns the diagnostic with the given code at the given YAML path, or nil.
func diagAtPath(diags []diag.Diagnostic, code, path string) *diag.Diagnostic {
	for i := range diags {
		if diags[i].Code == code && diags[i].YAMLPath == path {
			return &diags[i]
		}
	}
	return nil
}

// The three JSON-Schema root keys are refused BY NAME AND ADDRESS, not ignored.
//
// Ignoring is the failure mode worth a guard: under the new dialect a root key is a
// state field name, so a surviving `properties:` would be read as a field CALLED
// `properties` holding the real fields one level too deep, and `required: [a, b]` as a
// field called `required`. The manifest would load, and the schema in force would be a
// different schema from the one the author is looking at.
func TestStateSchema_JSONSchemaRootFormIsRefusedWithAnAddress(t *testing.T) {
	// Deliberately the JSON-Schema form, all three root keys — this fixture exists to
	// be refused. It must NOT be migrated along with the rest of the corpus.
	src := serviceWith(`  type: object
  required: [namespace]
  properties:
    namespace:
      type: string
`)
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})

	for key, line := range map[string]int{"type": 2, "required": 3, "properties": 4} {
		d := diagAtPath(diags, StateSchemaLegacyFormCode, "$.state_schema."+key)
		if d == nil {
			dump(t, diags)
			t.Fatalf("%q at the root was not refused as the JSON-Schema form", key)
		}
		if d.Line != line || d.Column == 0 {
			t.Errorf("%q refused at %d:%d, want line %d with a column", key, d.Line, d.Column, line)
		}
		if !strings.Contains(d.Message, key) {
			t.Errorf("%q: message does not name the key: %q", key, d.Message)
		}
		if d.Hint == "" {
			t.Errorf("%q: refusal carries no hint saying what to write instead", key)
		}
	}

	// And the field the author actually meant is NOT silently accepted from inside the
	// wrapper: nothing under the refused `properties:` becomes a state field.
	cfg, _, _, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if _, leaked := cfg.StateSchema["namespace"]; leaked {
		t.Error("a field under the refused `properties:` wrapper was read as a top-level state field")
	}
}

// Each of the three stands alone: a manifest carrying only `required:` is refused for
// it, and refused ONCE.
func TestStateSchema_RequiredListAloneIsRefused(t *testing.T) {
	src := serviceWith(`  required: [namespace, owners]
  namespace: { type: string }
`)
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})

	d := diagAtPath(diags, StateSchemaLegacyFormCode, "$.state_schema.required")
	if d == nil {
		dump(t, diags)
		t.Fatal("a root `required:` list was not refused")
	}
	if d.Line == 0 || d.Column == 0 {
		t.Errorf("refusal carries no position: %+v", *d)
	}
	// The refused key must not ALSO be reported as a malformed state field — one
	// mistake, one diagnostic the author can act on.
	if bad := diagAtPath(diags, "type_mismatch", "$.state_schema.required"); bad != nil {
		t.Errorf("the refused key was reported a second time as a field: %+v", *bad)
	}
	// The neighbouring real field still validates normally.
	if s := cfgField(t, src, "namespace"); s.Type != "string" {
		t.Errorf("namespace.type = %q, want string", s.Type)
	}
}

func cfgField(t *testing.T, src, name string) *InputSchema {
	t.Helper()
	cfg, _, _, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	s, ok := cfg.StateSchema[name]
	if !ok || s == nil {
		t.Fatalf("state field %q missing from the decoded schema", name)
	}
	return s
}

// aclUserCatalog is the shared element type: the shape both `input:` and `state_schema`
// describe, declared once. It carries NO secret — the secret belongs to the point of
// USE (see [applyRefProperties]).
const aclUserCatalog = `types:
  AclUser:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string, required: true }
      state: { type: string, default: "on" }
`

// The exception to ADR-062, and its scope. In `state_schema` a `$type` reference may
// ADD properties on top of the named type — this is what lets one AclUser serve two
// state fields while each owns the secret its own Vault path is derived from. In
// `input:` the identical node stays a conflict.
func TestStateSchema_TypeRefMayAddItsOwnProperties(t *testing.T) {
	catalog, cdiags := ParseTypeCatalog("types.yml", []byte(aclUserCatalog))
	if diag.HasErrors(cdiags) {
		dump(t, cdiags)
		t.Fatal("the type catalog does not parse")
	}

	src := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`)
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a $type reference with its own properties was refused in state_schema")
	}

	resolved, rdiags := ResolveStateSchemaTypeRefs(cfg.StateSchema, catalog)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatal("resolving the reference failed")
	}

	items := resolved["redis_users"].Items
	if items == nil || items.Type != "object" {
		t.Fatalf("redis_users.items = %+v, want the resolved AclUser object", items)
	}
	// The type's own shape survived...
	for _, want := range []string{"name", "perms", "state"} {
		if items.Properties[want] == nil {
			t.Errorf("property %q from the type is missing after the resolve", want)
		}
	}
	// ...and the property written at the point of use was added on top.
	pw := items.Properties["password"]
	if pw == nil || pw.Type != SecretTypeName || pw.Key != "name" {
		t.Fatalf("password = %+v, want the declared secret keyed by name", pw)
	}

	// The whole point: the resolved schema is what the deriver reads, so the secret is
	// collected and its path derived from the field it sits under.
	fields, issues := CollectSecretFields(resolved)
	if len(issues) != 0 {
		t.Fatalf("issues on a resolved declaration: %+v", issues)
	}
	if len(fields) != 1 || fields[0].ID() != "redis_users.password" {
		t.Fatalf("fields = %+v, want exactly redis_users.password", fields)
	}
	got, err := fields[0].VaultPath("", "redis", "redis-prod", "alice")
	if err != nil {
		t.Fatalf("VaultPath: %v", err)
	}
	if want := "secret/redis/redis-prod/redis_users/alice"; got != want {
		t.Errorf("VaultPath = %q, want %q", got, want)
	}
}

// The same type under a SECOND state field keeps its own secret and derives its own
// path — which is the reason the address cannot live in the type.
func TestStateSchema_OneTypeUnderTwoFieldsDerivesTwoPaths(t *testing.T) {
	catalog, _ := ParseTypeCatalog("types.yml", []byte(aclUserCatalog))
	src := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
  system_acl_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`)
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("two uses of one type were refused")
	}
	resolved, rdiags := ResolveStateSchemaTypeRefs(cfg.StateSchema, catalog)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatal("resolve failed")
	}
	fields, issues := CollectSecretFields(resolved)
	if len(issues) != 0 || len(fields) != 2 {
		t.Fatalf("fields = %+v, issues = %+v, want one secret per use", fields, issues)
	}
	seen := map[string]bool{}
	for _, f := range fields {
		p, err := f.VaultPath("", "redis", "redis-prod", "alice")
		if err != nil {
			t.Fatalf("VaultPath for %s: %v", f.ID(), err)
		}
		if seen[p] {
			t.Fatalf("both uses derived the same path %q — the address followed the TYPE, not the use", p)
		}
		seen[p] = true
	}
	if !seen["secret/redis/redis-prod/redis_users/alice"] || !seen["secret/redis/redis-prod/system_acl_users/alice"] {
		t.Errorf("derived paths = %v, want one per state field", seen)
	}
}

// The relaxation does NOT reach `input:`. Written there, the identical node is the deep
// merge ADR-062 refused, and it is still refused — otherwise the next reader takes the
// state-side exception for a general rule.
func TestInput_TypeRefWithOwnPropertiesIsStillAConflict(t *testing.T) {
	src := `name: update_users
input:
  users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if d := diagAtPath(diags, "input_type_ref_conflict", "$.input.users.items.properties"); d == nil {
		dump(t, diags)
		t.Fatal("a $type reference with its own properties was accepted in input:")
	}
}

// `type: secret` is a state declaration: the PLATFORM issues the value and it never
// lives in state. On a form it would have to mean the opposite, so the input dialect
// does not take the word at all.
func TestInput_TypeSecretIsNotAnInputType(t *testing.T) {
	src := `name: create
input:
  admin_password: { type: secret }
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	d := diagAtPath(diags, "input_type_invalid", "$.input.admin_password.type")
	if d == nil {
		dump(t, diags)
		t.Fatal("type: secret was accepted in input:")
	}
	if !strings.Contains(d.Hint, "secret: true") {
		t.Errorf("the refusal does not point at the input spelling: %q", d.Hint)
	}

	// And it IS accepted in state_schema — a guard that refuses everywhere guards nothing.
	_, _, sdiags, _ := LoadServiceManifestFromBytes("service.yml",
		[]byte(serviceWith("  admin_password: { type: secret }\n")), ValidateOptions{})
	if diag.HasErrors(sdiags) {
		dump(t, sdiags)
		t.Fatal("type: secret was refused in state_schema")
	}
}

// ★ The one segment of a derived Vault path that comes from OUTSIDE: `<key>` is read
// out of state DATA — a name an operator typed. [ValidVaultPathSegment] and the
// fail-closed return in [SecretField.VaultPath] are what stand between that and a path
// pointing at somebody else's secret, and the declaration having moved into a shared
// type must not move them.
//
// The route here is the whole one, catalog included, because that is what changed: the
// element shape now arrives through `$type` and only the `key:` is written at the point
// of use. A path is only ever compared to the SAFE one — a hostile key must produce an
// error and an EMPTY path, never a scrubbed one, since silently stripping `../` would
// read one incarnation's secret for another rather than refusing.
func TestStateSchema_VaultKeySegmentStaysCheckedAfterTheTypeMove(t *testing.T) {
	catalog, _ := ParseTypeCatalog("types.yml", []byte(aclUserCatalog))
	src := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`)
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("the fixture manifest does not load")
	}
	resolved, _ := ResolveStateSchemaTypeRefs(cfg.StateSchema, catalog)
	fields, issues := CollectSecretFields(resolved)
	if len(fields) != 1 || len(issues) != 0 {
		t.Fatalf("fields = %+v, issues = %+v, want the one declared secret", fields, issues)
	}
	f := fields[0]

	const safe = "secret/redis/redis-prod/redis_users/alice"
	hostile := []string{
		"../../keeper/jwt-signing-key", // the escape the whole check exists for
		"..", ".", "a/b", "a#b", "", "üser", "a b", "alice/../bob",
	}
	for _, key := range hostile {
		got, err := f.VaultPath("", "redis", "redis-prod", key)
		if err == nil {
			t.Errorf("key %q accepted through a $type-resolved declaration, produced %q", key, got)
			continue
		}
		if got != "" {
			t.Errorf("key %q refused but a path came back anyway: %q — the caller may use it", key, got)
		}
	}
	// Fail-closed, not fail-everything: a safe key still resolves, and to the path the
	// state field — not the type — determines.
	got, err := f.VaultPath("", "redis", "redis-prod", "alice")
	if err != nil {
		t.Fatalf("a safe key was rejected: %v", err)
	}
	if got != safe {
		t.Errorf("VaultPath = %q, want %q", got, safe)
	}
	// The guard is the predicate itself, reachable and still refusing: a caller that
	// checks before deriving gets the same answer the deriver would.
	if ValidVaultPathSegment("../x") || !ValidVaultPathSegment("alice") {
		t.Error("ValidVaultPathSegment no longer separates a safe segment from a traversal")
	}
}

// A reference may only ADD. Redeclaring a property the type already carries is the
// divergence [NIM-740] exists to remove — two descriptions of one field, and nothing
// saying which one is in force.
func TestStateSchema_TypeRefPropertyCollisionIsRefused(t *testing.T) {
	catalog, _ := ParseTypeCatalog("types.yml", []byte(aclUserCatalog))
	src := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        perms: { type: integer }
`)
	cfg, _, _, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	_, rdiags := ResolveStateSchemaTypeRefs(cfg.StateSchema, catalog)
	if d := diagAtPath(rdiags, TypeRefOverlayConflictCode, "$.state_schema.redis_users.items.properties.perms"); d == nil {
		dump(t, rdiags)
		t.Fatal("a property the type already declares was silently overridden")
	}
}

// `key:`/`label:` belong to a `type: secret` node. On any other node they are keys
// nothing reads, and in `input:` they are not keys at all.
func TestStateSchema_KeyAndLabelBelongToASecretNode(t *testing.T) {
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml",
		[]byte(serviceWith("  name: { type: string, key: other, label: Name }\n")), ValidateOptions{})
	for _, path := range []string{"$.state_schema.name.key", "$.state_schema.name.label"} {
		if d := diagAtPath(diags, "input_key_invalid_for_type", path); d == nil {
			dump(t, diags)
			t.Fatalf("%s was accepted on a type: string field", path)
		}
	}

	scn := `name: create
input:
  admin: { type: string, key: other }
tasks: []
`
	_, _, idiags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(scn), ValidateOptions{})
	if d := diagAtPath(idiags, "unknown_key", "$.input.admin.key"); d == nil {
		dump(t, idiags)
		t.Fatal("`key:` was accepted as an input schema key")
	}
}

// `additional_properties` next to `$type` is a conflict, in BOTH dialects.
//
// The resolver substitutes the TYPE for a reference node and keeps none of the node's
// own shape, so a schema written here is neither merged nor refused — it is dropped.
// A `type: secret` under it therefore disappeared with no diagnostic at any stage, and
// "no secrets here" about a schema that declares one is the single answer
// [CollectSecretFields] exists to rule out. Found by review, not by a failing test:
// nothing was red, which is exactly the shape of the bug.
func TestStateSchema_AdditionalPropertiesBesideTypeRefIsRefused(t *testing.T) {
	src := serviceWith(`  users:
    $type: UserMap
    additional_properties: { type: secret }
`)
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	d := diagAtPath(diags, "input_type_ref_conflict", "$.state_schema.users.additional_properties")
	if d == nil {
		dump(t, diags)
		t.Fatal("a declared secret under additional_properties beside $type was accepted — the resolver drops it silently")
	}
	if d.Line == 0 || d.Column == 0 {
		t.Errorf("refusal carries no position: %+v", *d)
	}

	scn := `name: create
input:
  users:
    $type: UserMap
    additional_properties: { type: string }
tasks: []
`
	_, _, idiags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(scn), ValidateOptions{})
	if diagAtPath(idiags, "input_type_ref_conflict", "$.input.users.additional_properties") == nil {
		dump(t, idiags)
		t.Fatal("additional_properties beside $type was accepted in input: — the same silent drop")
	}
}

// A declaration judged at LOAD is not judged a second time after the resolve. One
// mistake is one diagnostic, and the load's copy is the better of the two — it carries
// a line and a column, which a value-level walk cannot.
func TestValidateStateSchemaSecrets_DoesNotRepeatTheLoadTimeRefusal(t *testing.T) {
	catalog, _ := ParseTypeCatalog("types.yml", []byte(aclUserCatalog))
	// One reference (so a resolve happens at all) PLUS an unrelated broken inline
	// declaration that the load already refused positionally.
	src := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
  admin_password: { type: secret, key: whatever }
`)
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diagAtPath(diags, "secret_field_key_on_scalar", "$.state_schema.admin_password.key") == nil {
		dump(t, diags)
		t.Fatal("the inline declaration was not refused at load")
	}

	resolved, _ := ResolveStateSchemaTypeRefs(cfg.StateSchema, catalog)
	after := ValidateStateSchemaSecrets(cfg.StateSchema, resolved)
	for _, d := range after {
		if d.Code == "secret_field_key_on_scalar" {
			t.Errorf("the load-time refusal was repeated after the resolve: %+v", d)
		}
	}

	// And what the resolve DID make visible still gets through: a `key:` naming no
	// property of the resolved element is invisible until the type is substituted.
	broken := serviceWith(`  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: uid }
`)
	bcfg, _, bdiags, _ := LoadServiceManifestFromBytes("service.yml", []byte(broken), ValidateOptions{})
	if diag.HasErrors(bdiags) {
		dump(t, bdiags)
		t.Fatal("the load cannot see through $type and must stay quiet here")
	}
	bres, _ := ResolveStateSchemaTypeRefs(bcfg.StateSchema, catalog)
	if diagAtPath(ValidateStateSchemaSecrets(bcfg.StateSchema, bres), "secret_field_key_unknown",
		"$.state_schema.redis_users.items.properties.password.key") == nil {
		t.Fatal("a key: naming no property of the RESOLVED element was not reported")
	}
}

// [ADR-0086] §5: a shared type may declare a secret. A type describing a stored record
// describes its secret too, and refusing it there would force every service using
// `$type` to spell the record out twice — which is the divergence this whole change
// removes.
func TestTypesYML_MayDeclareASecret(t *testing.T) {
	catalog, diags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    properties:
      name:     { type: string, required: true }
      password: { type: secret, key: name }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a type declaring a secret was refused")
	}
	pw := catalog["AclUser"].Properties["password"]
	if pw == nil || pw.Type != SecretTypeName || pw.Key != "name" {
		t.Fatalf("password = %+v, want the declared secret keyed by name", pw)
	}

	// ★ The rule ADR-0086 §5 states — such a property is not asked for on input — is
	// enforced since NIM-751, and enforced OUTSIDE this parse: referencing the type
	// from `input:` stays legal, which is the point of §5 (one type, both contracts).
	// What the reference no longer buys is a form field, a requirement, or the right
	// to supply a value — see input_secret_type_test.go. This asserts the half that
	// belongs here: the reference itself is not refused.
	scn := `name: create
input:
  user: { $type: AclUser }
tasks: []
`
	m, _, sdiags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(scn), ValidateOptions{})
	if diag.HasErrors(sdiags) {
		dump(t, sdiags)
		t.Fatal("referencing the type from input: is refused today — NIM-751 landed, update this guard")
	}
	resolved, rdiags := ResolveTypeRefs(m.Input, catalog)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatal("resolving the reference from input: failed")
	}
	if resolved["user"].Properties["password"].Type != SecretTypeName {
		t.Fatal("the secret property vanished through the input-side resolve")
	}
}

// `type: secret` is still refused in an `input:` block written INLINE — the dialect
// that may spell a declared secret is the state block and a type body, not a form.
func TestInput_InlineTypeSecretStaysRefused(t *testing.T) {
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(`name: create
input:
  admin_password: { type: secret }
tasks: []
`), ValidateOptions{})
	if diagAtPath(diags, "input_type_invalid", "$.input.admin_password.type") == nil {
		dump(t, diags)
		t.Fatal("an inline type: secret was accepted in input:")
	}
}

// The `$type` overlay does not widen inside `types.yml`: a type referencing another
// type and adding properties to it is the deep merge ADR-062 refused. The exception is
// granted to the USE site, where the added property's Vault address is readable.
func TestTypesYML_TypeRefOverlayIsNotWidened(t *testing.T) {
	_, diags := ParseTypeCatalog("types.yml", []byte(`types:
  Base:
    type: object
    properties:
      name: { type: string }
  Derived:
    $type: Base
    properties:
      extra: { type: string }
`))
	if diagAtPath(diags, "input_type_ref_conflict", "$.types.Derived.properties") == nil {
		dump(t, diags)
		t.Fatal("a type extended another type's properties — the deep merge ADR-062 refused")
	}
}

// [ADR-0086] §7: `required` on a `type: secret` property is refused on SATISFIABILITY
// grounds — the value is in Vault, so no state instance can ever contain it, so none
// can ever satisfy the requirement.
//
// The code matters as much as the refusal, and the ADR says why: `required` is a key
// outside `type`/`key`/`label`, so the grammar check would otherwise claim it first and
// report `secret_field_unknown_key` — "type: secret does not take required". That is a
// vocabulary complaint where the truth is a satisfiability one, and it points the author
// at the wrong fix. This pins the code, not merely the failure.
func TestStateSchema_RequiredOnASecretIsRefusedOnItsOwnGround(t *testing.T) {
	for name, src := range map[string]string{
		"collection": `  redis_users:
    type: array
    items:
      type: object
      properties:
        name:     { type: string }
        password: { type: secret, key: name, required: true }
`,
		"scalar": `  admin_password: { type: secret, required: true }
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(serviceWith(src)), ValidateOptions{})
			var got *diag.Diagnostic
			for i := range diags {
				if diags[i].Code == "secret_field_required" {
					got = &diags[i]
				}
				if diags[i].Code == "secret_field_unknown_key" {
					t.Errorf("refused as a grammar mistake — the ADR's wrong-code trap: %+v", diags[i])
				}
			}
			if got == nil {
				dump(t, diags)
				t.Fatal("required on a type: secret was not refused as secret_field_required")
			}
			if !strings.Contains(got.Message, "never lives in state") {
				t.Errorf("message does not state the satisfiability ground: %q", got.Message)
			}
		})
	}
}

// A `required:` on a `$type` reference node is checked like any other. The node takes
// its own field-level `required: true` (the ADR-062 overlay), so the KEY is legal
// there — but a list is still the retired form, and it has to be refused where it is
// written.
//
// Found by review, and it was worse than merely silent. The check sat after the
// reference branch returned, so a list escaped every gate: not `unknown_key` (the key
// is known), not a `$type` conflict (not in that list), and never reaching the shape
// check. `applyRefOverlay` then read "the key was written" and carried the resulting
// `false` over the requiredness the TYPE declares — turning a mandatory input optional
// with nothing reported anywhere.
func TestTypeRef_RequiredMustStillBeABool(t *testing.T) {
	cat, cdiags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    required: true
    properties:
      name: { type: string, required: true }
`))
	if diag.HasErrors(cdiags) {
		dump(t, cdiags)
		t.Fatal("catalog fixture is invalid")
	}

	for name, tc := range map[string]struct{ value, code string }{
		"list":   {"[name]", RequiredListRemovedCode},
		"string": {`"true"`, "input_required_value_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			src := "name: update_users\ninput:\n  user:\n    $type: AclUser\n    required: " + tc.value + "\ntasks: []\n"
			m, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			d := diagAtPath(diags, tc.code, "$.input.user.required")
			if d == nil {
				dump(t, diags)
				t.Fatalf("a %s `required:` on a $type node was accepted", name)
			}
			if d.Line == 0 || d.Column == 0 {
				t.Errorf("refusal carries no position: %+v", *d)
			}

			// ...and the malformed value does NOT carry its zero `false` onto the type.
			// This half is what made the hole dangerous rather than merely untidy.
			resolved, _ := ResolveTypeRefs(m.Input, cat)
			if !resolved["user"].Required {
				t.Error("a malformed `required:` overrode the type's own `required: true` with false")
			}
		})
	}

	// A real bool still overrides, in both directions — the overlay itself is intact.
	for _, tc := range []struct {
		value string
		want  bool
	}{{"true", true}, {"false", false}} {
		src := "name: x\ninput:\n  user:\n    $type: AclUser\n    required: " + tc.value + "\ntasks: []\n"
		m, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("`required: %s` on a $type node was refused", tc.value)
		}
		resolved, _ := ResolveTypeRefs(m.Input, cat)
		if resolved["user"].Required != tc.want {
			t.Errorf("`required: %s` → Required=%v, want %v", tc.value, resolved["user"].Required, tc.want)
		}
	}
}

// §2 names `types.yml` first, and it was the one place with no guard: removing the
// dialect from ParseTypeCatalog would not have turned anything red.
func TestTypesYML_RequiredListIsRefused(t *testing.T) {
	_, diags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    required: [name]
    properties:
      name: { type: string }
`))
	if diagAtPath(diags, RequiredListRemovedCode, "$.types.AclUser.required") == nil {
		dump(t, diags)
		t.Fatal("the list form was accepted inside a type body")
	}
}

// §14, the negative half: `additional_properties: false` FORBIDS keys rather than
// describing any, so on its own it still declares an object that may hold nothing.
// Nothing asserted this, and deleting the exclusion was green.
func TestObject_AdditionalPropertiesFalseDoesNotDescribeContents(t *testing.T) {
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml",
		[]byte(serviceWith("  bag: { type: object, additional_properties: false }\n")), ValidateOptions{})
	if diagAtPath(diags, "missing_required_field", "$.state_schema.bag.properties") == nil {
		dump(t, diags)
		t.Fatal("`additional_properties: false` alone satisfied the object rule")
	}

	// The positive half, next to it so the two cannot be conflated by a later edit.
	_, _, ok, _ := LoadServiceManifestFromBytes("service.yml",
		[]byte(serviceWith("  bag: { type: object, additional_properties: true }\n")), ValidateOptions{})
	if diag.HasErrors(ok) {
		dump(t, ok)
		t.Fatal("`additional_properties: true` alone was refused")
	}
}

// `required: false` on a `type: secret` is refused too — the key has no meaning there
// either way — and the message must not tell that author their field "is required".
// It also pins the trigger: the check reads the DECODED field first, so it answers at
// runtime where there is no AST, which is the ground the rest of the node grammar is
// built on.
func TestStateSchema_RequiredFalseOnASecretIsRefusedTruthfully(t *testing.T) {
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml",
		[]byte(serviceWith("  admin_password: { type: secret, required: false }\n")), ValidateOptions{})
	var got *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == "secret_field_required" {
			got = &diags[i]
		}
	}
	if got == nil {
		dump(t, diags)
		t.Fatal("`required: false` on a type: secret was accepted")
	}
	if strings.Contains(got.Message, "is declared required") {
		t.Errorf("the message asserts something false about `required: false`: %q", got.Message)
	}

	// The runtime path: a schema built in Go, with no AST behind it, is judged the same.
	// This is the half that would have gone silent had the check read only rawRequired.
	_, issues := CollectSecretFields(InputSchemaMap{
		"admin_password": {Type: SecretTypeName, Required: true},
	})
	if !hasIssue(issues, "secret_field_required") {
		t.Errorf("a hand-built schema escaped the check: %+v", issues)
	}
}

// ★ What happens when an operator supplies a value for a `type: secret` property
// reached through `$type` from an `input:` block.
//
// When this test was written the value was refused only by accident — `secret` is in no
// value's type enum, so `valueMatchesType` returned false and the message named a
// vocabulary problem while printing the literal unmasked. NIM-751 made it a rule:
// [ErrSecretTypeNotWritable], ahead of the type check, naming the path and never the
// value. The assertion below pins the SENTINEL rather than the word "secret", which the
// accidental message contained too — matching prose could not tell the two apart.
func TestInput_SecretPropertyThroughTypeRefIsRefusedAtValueTime(t *testing.T) {
	cat, _ := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    properties:
      name:     { type: string, required: true }
      password: { type: secret, key: name }
`))
	m, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(`name: create
input:
  user: { $type: AclUser }
tasks: []
`), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("referencing a secret-carrying type from input: is refused at parse — NIM-751 landed, update this guard")
	}
	resolved, _ := ResolveTypeRefs(m.Input, cat)

	_, err := ResolveInputValues(resolved, map[string]any{
		"user": map[string]any{"name": "alice", "password": "operator-supplied"},
	})
	if err == nil {
		t.Fatal("an operator-supplied value for a type: secret property was ACCEPTED — that is the NIM-751 hole opening")
	}
	if !errors.Is(err, ErrSecretTypeNotWritable) {
		t.Errorf("refused under the wrong rule (the accidental type-vocabulary refusal?): %v", err)
	}
	if strings.Contains(err.Error(), "operator-supplied") {
		t.Errorf("the refusal echoes the value the caller sent: %v", err)
	}
}
