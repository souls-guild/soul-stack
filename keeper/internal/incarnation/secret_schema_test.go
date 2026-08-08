package incarnation

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// CollectStateSchemaSecrets walks a flat state_schema for secret:true (nesting via
// properties/items/additionalProperties).
func TestCollectStateSchemaSecrets(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"admin_token": map[string]any{"type": "string", "secret": true},
			"replicas":    map[string]any{"type": "integer"},
			"tls": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":  map[string]any{"type": "string", "secret": true},
					"port": map[string]any{"type": "integer"},
				},
			},
			"acl": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
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
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"map_field": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string", "secret": true},
			},
		},
	}
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
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"users": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":     map[string]any{"type": "string"},
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
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
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"users": map[string]any{
				"type": "object",
				"additionalProperties": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"password": map[string]any{"type": "string", "secret": true},
					},
				},
			},
		},
	}
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
