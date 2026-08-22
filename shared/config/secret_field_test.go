package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// redisStateSchema — the collection shape wb-service-redis actually declares
// ([ADR-0083] §1): one secret per element of a top-level array, addressed by a sibling.
func redisStateSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"redis_users": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []any{"name", "perms", "state"},
					"properties": map[string]any{
						"name":  map[string]any{"type": "string"},
						"perms": map[string]any{"type": "string"},
						"state": map[string]any{"type": "string", "enum": []any{"on", "off"}},
						"password": map[string]any{
							"type":  "secret",
							"key":   "name",
							"label": "Redis user password",
						},
					},
				},
			},
		},
	}
}

// TestCollectSecretFields_Collection — the redis shape yields one field, and the path is
// derived from (service, incarnation, state field, key) with nothing authored.
func TestCollectSecretFields_Collection(t *testing.T) {
	fields, issues := CollectSecretFields(redisStateSchema())
	if len(issues) != 0 {
		t.Fatalf("issues on a valid declaration: %+v", issues)
	}
	want := SecretField{
		State: "redis_users", Property: "password", Key: "name",
		Label: "Redis user password",
		Path:  ".properties.redis_users.items.properties.password",
	}
	if len(fields) != 1 || !reflect.DeepEqual(fields[0], want) {
		t.Fatalf("fields = %+v, want exactly [%+v]", fields, want)
	}
	f := fields[0]
	if !f.Collection() {
		t.Error("Collection() = false on a keyed secret")
	}
	if f.ID() != "redis_users.password" {
		t.Errorf("ID() = %q, want redis_users.password", f.ID())
	}
	ref, err := f.VaultRef("", "redis", "redis-prod", "default_admin")
	if err != nil {
		t.Fatalf("VaultRef: %v", err)
	}
	if want := "vault:secret/redis/redis-prod/redis_users/default_admin#password"; ref != want {
		t.Errorf("VaultRef = %q, want %q", ref, want)
	}
}

// TestCollectSecretFields_Scalar — a top-level `type: secret` needs no key and lands at
// a 3-segment path with the `value` field ([ADR-0083] §1, closing ADR-070's deferred
// "singleton secrets without enumerate").
func TestCollectSecretFields_Scalar(t *testing.T) {
	fields, issues := CollectSecretFields(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_password": map[string]any{"type": "secret"},
		},
	})
	if len(issues) != 0 {
		t.Fatalf("issues on a valid scalar declaration: %+v", issues)
	}
	if len(fields) != 1 {
		t.Fatalf("fields = %+v, want one", fields)
	}
	f := fields[0]
	if f.Collection() || f.ID() != "admin_password" || f.VaultField() != "value" {
		t.Fatalf("scalar field misread: %+v", f)
	}
	// The key argument is ignored rather than smuggled into the path: a scalar secret
	// has one value, and a caller passing a key must not silently address a sibling.
	ref, err := f.VaultRef("secret", "redis", "redis-prod", "ignored")
	if err != nil {
		t.Fatalf("VaultRef: %v", err)
	}
	if want := "vault:secret/redis/redis-prod/admin_password#value"; ref != want {
		t.Errorf("VaultRef = %q, want %q", ref, want)
	}
}

// TestCollectSecretFields_UnsupportedLocation — a `type: secret` outside the two shapes
// is REPORTED, not ignored. Ignoring it is the dangerous failure: the author believes
// the value is in Vault while nothing collects it, so a plaintext password ends up in
// `incarnation.state` and no mask covers it.
func TestCollectSecretFields_UnsupportedLocation(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
	}{
		{"nested object", map[string]any{"properties": map[string]any{
			"tls": map[string]any{"type": "object", "properties": map[string]any{
				"key": map[string]any{"type": "secret"},
			}},
		}}},
		{"array of arrays", map[string]any{"properties": map[string]any{
			"matrix": map[string]any{"type": "array", "items": map[string]any{
				"type": "array", "items": map[string]any{"type": "secret"},
			}},
		}}},
		{"under additionalProperties", map[string]any{"properties": map[string]any{
			"users": map[string]any{"type": "object", "additionalProperties": map[string]any{
				"type": "secret",
			}},
		}}},
		{"the array element itself", map[string]any{"properties": map[string]any{
			"tokens": map[string]any{"type": "array", "items": map[string]any{"type": "secret"}},
		}}},
		{"inside a combinator", map[string]any{"properties": map[string]any{
			"either": map[string]any{"oneOf": []any{
				map[string]any{"type": "string"},
				map[string]any{"type": "secret"},
			}},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fields, issues := CollectSecretFields(c.schema)
			if len(fields) != 0 {
				t.Errorf("collected %+v from an unsupported location", fields)
			}
			if !hasIssue(issues, "secret_field_unsupported_location") {
				t.Fatalf("issues = %+v, want secret_field_unsupported_location", issues)
			}
		})
	}
}

// TestCollectSecretFields_KeyRules — `key:` addresses one element, so it must name a
// sibling string property; every other spelling fails closed.
func TestCollectSecretFields_KeyRules(t *testing.T) {
	withKey := func(secret map[string]any) map[string]any {
		return map[string]any{"properties": map[string]any{
			"redis_users": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string"},
					"perms":    map[string]any{"type": "object"},
					"password": secret,
				},
			}},
		}}
	}
	cases := []struct {
		name   string
		schema map[string]any
		code   string
	}{
		{"absent", withKey(map[string]any{"type": "secret"}), "secret_field_key_required"},
		{"not a string", withKey(map[string]any{"type": "secret", "key": 7}), "secret_field_key_invalid"},
		{"names no sibling", withKey(map[string]any{"type": "secret", "key": "uid"}), "secret_field_key_unknown"},
		{"sibling is not a string", withKey(map[string]any{"type": "secret", "key": "perms"}), "secret_field_key_not_string"},
		{"on a scalar", map[string]any{"properties": map[string]any{
			"admin_password": map[string]any{"type": "secret", "key": "name"},
		}}, "secret_field_key_on_scalar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fields, issues := CollectSecretFields(c.schema)
			if len(fields) != 0 {
				t.Errorf("collected %+v from a broken key declaration", fields)
			}
			if !hasIssue(issues, c.code) {
				t.Fatalf("issues = %+v, want %s", issues, c.code)
			}
		})
	}
}

// TestCollectSecretFields_UnknownKey — a constraint written next to `type: secret` reads
// as enforced and is not: the value never passes through state validation, because it is
// never in state. Rejecting it is the only way the author finds out.
func TestCollectSecretFields_UnknownKey(t *testing.T) {
	fields, issues := CollectSecretFields(map[string]any{"properties": map[string]any{
		"admin_password": map[string]any{"type": "secret", "minLength": 40, "pattern": "^x"},
	}})
	if len(fields) != 0 {
		t.Errorf("collected %+v despite an unknown key", fields)
	}
	if !hasIssue(issues, "secret_field_unknown_key") {
		t.Fatalf("issues = %+v, want secret_field_unknown_key", issues)
	}
	// Offenders are listed in sorted order — identical input must produce identical text.
	if got := issueMessage(issues, "secret_field_unknown_key"); !strings.Contains(got, "minLength, pattern") {
		t.Errorf("message = %q, want the offenders sorted as \"minLength, pattern\"", got)
	}
}

// TestCollectSecretFields_RequiredRejected — `required` naming a secret can never be
// satisfied: the value is in Vault, so it is absent from every state instance.
func TestCollectSecretFields_RequiredRejected(t *testing.T) {
	schema := redisStateSchema()
	items := schema["properties"].(map[string]any)["redis_users"].(map[string]any)["items"].(map[string]any)
	items["required"] = []any{"name", "perms", "state", "password"}

	fields, issues := CollectSecretFields(schema)
	if len(fields) != 0 {
		t.Errorf("collected %+v despite required: naming the secret", fields)
	}
	if !hasIssue(issues, "secret_field_required") {
		t.Fatalf("issues = %+v, want secret_field_required", issues)
	}
}

// TestCollectSecretFields_DataSubtreesNotScanned — a `default:` holding `{type: secret}`
// is DATA that happens to look like a declaration. Scanning it would reject a legal
// schema, so the data-bearing keys are skipped.
func TestCollectSecretFields_DataSubtreesNotScanned(t *testing.T) {
	fields, issues := CollectSecretFields(map[string]any{"properties": map[string]any{
		"policy": map[string]any{
			"type":    "object",
			"default": map[string]any{"type": "secret"},
		},
	}})
	if len(fields) != 0 || len(issues) != 0 {
		t.Fatalf("fields = %+v, issues = %+v, want both empty", fields, issues)
	}
}

// TestSecretField_VaultPathFailsClosed — `key` comes from state DATA (an
// operator-supplied user name) and is the one segment an attacker can influence. Every
// segment is checked, and a rejected one yields an ERROR rather than a sanitized path:
// silently stripping `../` would read one incarnation's secret for another.
func TestSecretField_VaultPathFailsClosed(t *testing.T) {
	f := SecretField{State: "redis_users", Property: "password", Key: "name"}
	bad := []string{"../../keeper/jwt-signing-key", "a/b", "a#b", "..", ".", "", "üser", "a b"}
	for _, key := range bad {
		if got, err := f.VaultPath("secret", "redis", "redis-prod", key); err == nil {
			t.Errorf("key %q accepted, produced %q", key, got)
		}
	}
	// The other segments are checked too — a service or incarnation name that got past
	// registration must not reach Vault unvalidated.
	if _, err := f.VaultPath("secret", "redis/../keeper", "redis-prod", "alice"); err == nil {
		t.Error("unsafe service segment accepted")
	}
	if _, err := f.VaultPath("secret", "redis", "../prod", "alice"); err == nil {
		t.Error("unsafe incarnation segment accepted")
	}
	if _, err := f.VaultPath("secret", "redis", "redis-prod", ""); err == nil {
		t.Error("a collection secret resolved without a key")
	}
	// And a valid one still resolves, so the guard is not just "everything fails".
	if _, err := f.VaultPath("secret", "redis", "redis-prod", "default_admin"); err != nil {
		t.Errorf("a safe key was rejected: %v", err)
	}
}

// TestCollectSecretFields_Deterministic — identical input must produce byte-identical
// output; the walk sorts every map it iterates.
func TestCollectSecretFields_Deterministic(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"b_secret": map[string]any{"type": "secret"},
		"a_secret": map[string]any{"type": "secret"},
		"nested":   map[string]any{"type": "object", "properties": map[string]any{"z": map[string]any{"type": "secret"}}},
	}}
	first, firstIssues := CollectSecretFields(schema)
	for i := 0; i < 20; i++ {
		got, gotIssues := CollectSecretFields(schema)
		if !reflect.DeepEqual(got, first) || !reflect.DeepEqual(gotIssues, firstIssues) {
			t.Fatalf("run %d differs:\n fields %+v vs %+v\n issues %+v vs %+v", i, got, first, gotIssues, firstIssues)
		}
	}
	if len(first) != 2 || first[0].State != "a_secret" || first[1].State != "b_secret" {
		t.Fatalf("fields = %+v, want a_secret then b_secret", first)
	}
}

// TestServiceManifest_SecretFieldDiagnosticIsPositional — the load-time refusal points
// at the declaration in the file, not at the top of the manifest. A diagnostic without a
// position is a diagnostic the author has to hunt for.
func TestServiceManifest_SecretFieldDiagnosticIsPositional(t *testing.T) {
	src := `name: redis
state_schema_version: 1
state_schema:
  type: object
  properties:
    redis_users:
      type: array
      items:
        type: object
        properties:
          name: { type: string }
          password:
            type: secret
`
	_, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	var found *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == "secret_field_key_required" {
			found = &diags[i]
			break
		}
	}
	if found == nil {
		dump(t, diags)
		t.Fatal("expected secret_field_key_required")
	}
	if found.Line == 0 {
		t.Errorf("diagnostic carries no line: %+v", *found)
	}
	if want := "$.state_schema.properties.redis_users.items.properties.password"; found.YAMLPath != want {
		t.Errorf("YAMLPath = %q, want %q", found.YAMLPath, want)
	}
}

// TestServiceManifest_ValidSecretFieldLoadsClean — the shape wb-service-redis migrates
// to must load without a diagnostic; a guard that rejects everything guards nothing.
func TestServiceManifest_ValidSecretFieldLoadsClean(t *testing.T) {
	src := `name: redis
state_schema_version: 1
state_schema:
  type: object
  properties:
    redis_users:
      type: array
      items:
        type: object
        additionalProperties: false
        required: [name, perms, state]
        properties:
          name: { type: string }
          perms: { type: string }
          state: { type: string, enum: [on, off] }
          password:
            type: secret
            key: name
            label: "Redis user password"
`
	cfg, _, diags, _ := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatal("a valid type: secret declaration produced errors")
	}
	fields, issues := CollectSecretFields(cfg.StateSchema)
	if len(fields) != 1 || len(issues) != 0 {
		t.Fatalf("fields = %+v, issues = %+v after a clean load", fields, issues)
	}
}

func hasIssue(issues []SecretFieldIssue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func issueMessage(issues []SecretFieldIssue, code string) string {
	for _, i := range issues {
		if i.Code == code {
			return i.Message
		}
	}
	return ""
}

// The skip set names JSON Schema keywords. Under a `properties` bag the keys are field
// names the author chose, so a field named `default` used to take its whole subtree out
// of the walk — and the walk is what makes a refusal loud. A declaration the deriver
// cannot support came back as "no secrets here": no field, no diagnostic, and a
// plaintext value in incarnation.state, which is the one answer this function's
// contract says it must never give by accident.
func TestCollectSecretFields_FieldNamedLikeASchemaKeyword(t *testing.T) {
	for _, name := range []string{"default", "const", "enum", "examples"} {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				name: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"nested": map[string]any{
							"type":       "object",
							"properties": map[string]any{"pw": map[string]any{"type": SecretTypeName}},
						},
					},
				},
			},
		}
		fields, issues := CollectSecretFields(schema)
		if len(fields) != 0 {
			t.Fatalf("%s: fields = %+v, want none -- the position is not one the path is derived from", name, fields)
		}
		if len(issues) != 1 || issues[0].Code != "secret_field_unsupported_location" {
			t.Errorf("%s: issues = %v, want the refusal to be loud", name, issues)
		}
	}
	// The keyword itself is still skipped where it IS a keyword: a `default:` holding a
	// value shaped like a declaration is data, and scanning it would reject a legal schema.
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "default": map[string]any{"type": SecretTypeName}},
		},
	}
	fields, issues := CollectSecretFields(schema)
	if len(fields) != 0 || len(issues) != 0 {
		t.Errorf("fields=%+v issues=%v, want a default: subtree treated as data", fields, issues)
	}
}

// TestEffectiveVaultMount_TrimsSlashes — keeper/internal/vault.Client trims cfg.KVMount
// before it builds a request, so a mount written with a slash reads back under the
// trimmed name. If derivation kept the slash, the path this package writes and the ref
// it hands out would name a location the client never reads: `secret//redis/prod`
// against the client's `secret/redis/prod`.
func TestEffectiveVaultMount_TrimsSlashes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "secret"},
		{"/", "secret"},
		{"secret", "secret"},
		{"secret/", "secret"},
		{"/secret", "secret"},
		{"/kv-prod/", "kv-prod"},
	} {
		if got := EffectiveVaultMount(tc.in); got != tc.want {
			t.Errorf("EffectiveVaultMount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Without the trim the mount segment fails [ValidVaultPathSegment] and derivation
	// aborts outright — every declared secret of the service becomes unresolvable.
	f := SecretField{State: "users", Property: "password", Key: "username"}
	got, err := f.VaultPath("secret/", "redis", "prod", "app")
	if err != nil {
		t.Fatalf("VaultPath with a trailing-slash mount: %v", err)
	}
	if got != "secret/redis/prod/users/app" {
		t.Errorf("VaultPath = %q, want %q", got, "secret/redis/prod/users/app")
	}
}

// TestStripDeclaredSecrets_Shapes — the stripper is the last thing standing between a
// plaintext secret and `incarnation.state` / `state_history`, both of which are
// permanent. It has to recognise every shape a collection can actually arrive in: the
// `[]any` a JSONB round-trip produces, the `map[string]any` a map-typed collection
// materializes as, the `[]map[string]any` Go-built state produces, and a container it
// does not recognise at all, which it descends into rather than skips.
func TestStripDeclaredSecrets_Shapes(t *testing.T) {
	el := func(name, pw string) map[string]any {
		return map[string]any{"name": name, "perms": "+get", "state": "on", "password": pw}
	}
	cases := map[string]func() any{
		"list":        func() any { return []any{el("a", "s1"), el("b", "s2")} },
		"typed list":  func() any { return []map[string]any{el("a", "s1")} },
		"map":         func() any { return map[string]any{"a": el("a", "s1")} },
		"nested list": func() any { return []any{[]any{el("a", "s1")}} },
		"map of list": func() any { return map[string]any{"g": []any{el("a", "s1")}} },
	}
	for name, build := range cases {
		state := map[string]any{"redis_users": build()}
		StripDeclaredSecrets(state, redisStateSchema())
		for _, elem := range collectionElements(state["redis_users"]) {
			if _, still := elem["password"]; still {
				t.Errorf("%s: password survived the strip: %+v", name, elem)
			}
			if elem["name"] == nil {
				t.Errorf("%s: the addressing key was stripped too: %+v", name, elem)
			}
		}
		if len(collectionElements(state["redis_users"])) == 0 {
			t.Errorf("%s: no elements reached the stripper at all", name)
		}
	}
}

// A scalar declaration takes the whole property out, not a sub-key.
func TestStripDeclaredSecrets_Scalar(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_password": map[string]any{"type": SecretTypeName},
			"port":           map[string]any{"type": "integer"},
		},
	}
	state := map[string]any{"admin_password": "hunter2", "port": 6379}
	StripDeclaredSecrets(state, schema)
	if _, still := state["admin_password"]; still {
		t.Errorf("scalar secret survived: %+v", state)
	}
	if state["port"] != 6379 {
		t.Errorf("a non-secret property was touched: %+v", state)
	}
}
