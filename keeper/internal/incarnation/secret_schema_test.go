package incarnation

import (
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

// stateSchemaFixture parses a `state_schema:` body written in the input dialect
// ([NIM-740]) through the real decoder, so the fixture is a shape the parser actually
// produces rather than one assembled by hand.
func stateSchemaFixture(t *testing.T, src string) config.InputSchemaMap {
	t.Helper()
	var m config.InputSchemaMap
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("state_schema fixture does not parse: %v\n%s", err, src)
	}
	return m
}

// CollectStateSchemaSecrets walks a state_schema for secret:true (nesting via
// properties/items/additional_properties).
func TestCollectStateSchemaSecrets(t *testing.T) {
	schema := stateSchemaFixture(t, `
admin_token: { type: string, secret: true }
replicas:    { type: integer }
tls:
  type: object
  properties:
    key:  { type: string, secret: true }
    port: { type: integer }
acl:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	for _, want := range []string{"admin_token", "tls.key", "acl[].password"} {
		if !set[want] {
			t.Errorf("secret path %q not collected: %v", want, set)
		}
	}
	if set["replicas"] || set["tls.port"] || set["acl[].name"] {
		t.Errorf("non-secret path marked — over-collect: %v", set)
	}
}

// secret ON THE additionalProperties node itself (the value of an arbitrary map key is
// secret) does NOT enter SecretPathSet: neither `map_field` (would mark the whole map →
// over-mask on read-path) nor `map_field.*` (IsSecret never asks for such a path →
// dead entry). Degradation to the vault+regex masking layer is intentional (★ limitation
// of the schema layer). Regression guard for the ap-secret branch of CollectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesSecretLeaf(t *testing.T) {
	schema := stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	if set["map_field"] {
		t.Errorf("ap-secret-leaf marked `map_field` — over-mask of the whole map: %v", set)
	}
	if set["map_field.*"] {
		t.Errorf("ap-secret-leaf marked `map_field.*` — dead entry (IsSecret never queries this path): %v", set)
	}
	if len(set) != 0 {
		t.Errorf("ap-secret-leaf should not produce any entry (degradation to vault+regex): %v", set)
	}
}

// ap node WITHOUT secret but with nested concrete `properties` that are secret: the schema
// layer MUST cover the exact names (recursion into ap runs), but not the ap node itself.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret(t *testing.T) {
	schema := stateSchemaFixture(t, `
users:
  type: object
  additional_properties:
    type: object
    properties:
      name:     { type: string }
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	// ap-path = map name (`users`), nested concrete `password` → `users.password`.
	if !set["users.password"] {
		t.Errorf("nested concrete secret under ap not collected: %v", set)
	}
	if set["users"] {
		t.Errorf("the ap-node `users` itself marked secret — over-mask: %v", set)
	}
}

// ★ Documents a GAP (seal-review nit, NOT a fix): the collected schema path
// `users.password` does NOT match the real cell path `users.<dynamic-key>.password`.
// Recursion into ap does not insert a segment for an arbitrary key → `users.password`
// is collected, while maskMapLayered walks the path by the CONCRETE map key
// (`users.alice.password`). [audit.SecretPathSet.IsSecret] compares the exact shape AND
// normalizeIdx — but normalizeIdx only generalizes slice indices (`[N]`→`[]`), not
// map keys → no match. The schema layer does NOT mask such a secret (degradation to
// vault+regex by the name `password`). The test pins CURRENT behavior: once dynamic-key
// matching lands in IsSecret (a separate slice), the `!IsSecret(...)` assert will fail —
// a signal to update the limitation in CollectStateSchemaSecrets.
func TestCollectStateSchemaSecrets_AdditionalPropertiesNestedSecret_DynamicKeyGap(t *testing.T) {
	schema := stateSchemaFixture(t, `
users:
  type: object
  additional_properties:
    type: object
    properties:
      password: { type: string, secret: true }
`)
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(schema, "", set)

	// The ap-path is collected without a dynamic-key segment.
	if !set["users.password"] {
		t.Fatalf("expected collected path `users.password`: %v", set)
	}
	// ★ Current behavior: the real cell path with a CONCRETE map key does NOT match the
	// schema layer (the dynamic-key segment `alice` is not covered; normalizeIdx leaves it alone).
	if set.IsSecret("users.alice.password") {
		t.Errorf("IsSecret(users.alice.password) = true — gap unexpectedly closed; update the limitation in CollectStateSchemaSecrets")
	}
	// Control: idx generalization does not help — a map key is not a slice index.
	if set.IsSecret("users.bob.password") {
		t.Errorf("IsSecret(users.bob.password) = true — gap unexpectedly closed; update the limitation in CollectStateSchemaSecrets")
	}
}

// `type: secret` sitting ON an additional_properties node is still entered into the
// path set; only the older `secret: true` FLAG is ignored there.
//
// The distinction is not cosmetic. The flag's path is the MAP's, so honouring it would
// mark the whole map (the ★ limitation above). `type: secret` gets the same path, and
// the older raw-map walk stripped the `secret` key but left the type, so the entry WAS
// made. This layer is belt and braces — a declared secret should never be in state at
// all, and config.CollectSecretFields refuses one in this position — which is exactly
// why it must not quietly narrow when nobody is looking.
func TestCollectStateSchemaSecrets_AdditionalPropertiesTypeSecretStillMarked(t *testing.T) {
	set := audit.SecretPathSet{}
	CollectStateSchemaSecrets(stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: secret }
`), "", set)
	if !set["map_field"] {
		t.Errorf("`type: secret` on an ap node was not marked: %v", set)
	}

	// The flag, by contrast, is ignored there — pinned separately above, restated here
	// so the two cases sit next to each other and cannot be conflated by a later edit.
	flagSet := audit.SecretPathSet{}
	CollectStateSchemaSecrets(stateSchemaFixture(t, `
map_field:
  type: object
  additional_properties: { type: string, secret: true }
`), "", flagSet)
	if len(flagSet) != 0 {
		t.Errorf("`secret: true` on an ap node was marked — over-mask of the whole map: %v", flagSet)
	}
}
