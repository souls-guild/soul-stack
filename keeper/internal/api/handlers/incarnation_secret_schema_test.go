package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// sealSchemaHandler builds an IncarnationHandler with loader+services for the seal
// read-path tests of secretSchemaForIncarnation. db/etc are nil — the test calls only
// the schema-builder.
func sealSchemaHandler(loader *fakeLoader) *IncarnationHandler {
	return &IncarnationHandler{
		loader:   loader,
		services: &fakeResolver{ok: true},
	}
}

// collectStateSchemaSecrets walks a flat state_schema for secret:true (nesting via
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
	collectStateSchemaSecrets(schema, "", set)

	for _, want := range []string{"admin_token", "tls.key", "acl[].password"} {
		if !set[want] {
			t.Errorf("secret-путь %q не собран: %v", want, set)
		}
	}
	if set["replicas"] || set["tls.port"] || set["acl[].name"] {
		t.Errorf("несекретный путь помечен — over-collect: %v", set)
	}
}

// secret ON THE additionalProperties node itself (the value of an arbitrary map key is
// secret) does NOT enter SecretPathSet: neither `map_field` (would mark the whole map →
// over-mask on read-path) nor `map_field.*` (IsSecret never asks for such a path →
// dead entry). Degradation to the vault+regex masking layer is intentional (★ limitation
// of the schema layer). Regression guard for the ap-secret branch of collectStateSchemaSecrets.
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
	collectStateSchemaSecrets(schema, "", set)

	if set["map_field"] {
		t.Errorf("ap-secret-leaf пометил `map_field` — over-mask всей map: %v", set)
	}
	if set["map_field.*"] {
		t.Errorf("ap-secret-leaf пометил `map_field.*` — мёртвая запись (IsSecret такой путь не запрашивает): %v", set)
	}
	if len(set) != 0 {
		t.Errorf("ap-secret-leaf не должен давать ни одной записи (деградация к vault+regex): %v", set)
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
	collectStateSchemaSecrets(schema, "", set)

	// ap-path = map name (`users`), nested concrete `password` → `users.password`.
	if !set["users.password"] {
		t.Errorf("вложенный конкретный secret под ap не собран: %v", set)
	}
	if set["users"] {
		t.Errorf("сам ap-узел `users` помечен secret — over-mask: %v", set)
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
// a signal to update the limitation in collectStateSchemaSecrets.
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
	collectStateSchemaSecrets(schema, "", set)

	// The ap-path is collected without a dynamic-key segment.
	if !set["users.password"] {
		t.Fatalf("ожидался собранный путь `users.password`: %v", set)
	}
	// ★ Current behavior: the real cell path with a CONCRETE map key does NOT match the
	// schema layer (the dynamic-key segment `alice` is not covered; normalizeIdx leaves it alone).
	if set.IsSecret("users.alice.password") {
		t.Errorf("IsSecret(users.alice.password) = true — gap неожиданно закрыт; обнови ограничение в collectStateSchemaSecrets")
	}
	// Control: idx generalization does not help — a map key is not a slice index.
	if set.IsSecret("users.bob.password") {
		t.Errorf("IsSecret(users.bob.password) = true — gap неожиданно закрыт; обнови ограничение в collectStateSchemaSecrets")
	}
}

// secretSchemaForIncarnation materializes the snapshot and merges state_schema
// secret + create-scenario input secret under input.<name>.
func TestSecretSchemaForIncarnation_StateAndInput(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"admin_token": map[string]any{"type": "string", "secret": true},
			},
		},
		// create-scenario with a secret input db_password.
		scenarioYAML: "name: create\ninput:\n  db_password: { type: string, secret: true }\n  hostname: { type: string }\n",
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}

	schema := h.secretSchemaForIncarnation(context.Background(), inc)
	if schema == nil {
		t.Fatal("schema nil — ожидалась непустая (state+input secret)")
	}
	if !schema.IsSecret("admin_token") {
		t.Errorf("state.admin_token не secret в схеме")
	}
	if !schema.IsSecret("input.db_password") {
		t.Errorf("spec.input.db_password не secret в схеме")
	}
	if schema.IsSecret("input.hostname") {
		t.Errorf("input.hostname помечен secret — over-collect")
	}
}

// loader error → nil schema (best-effort, GET does not fail).
func TestSecretSchemaForIncarnation_LoadErrorNil(t *testing.T) {
	loader := &fakeLoader{loadErr: context.DeadlineExceeded}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("при load-ошибке schema должна быть nil (best-effort): %v", schema)
	}
}

// nil loader → nil schema (degradation to MaskSecrets).
func TestSecretSchemaForIncarnation_NilDeps(t *testing.T) {
	h := &IncarnationHandler{}
	inc := &incarnation.Incarnation{Service: "redis"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("без loader/services schema должна быть nil: %v", schema)
	}
}

// (e) schema-declared secret state field → MASKED on the read-path projection
// (toIncarnationGetView via the service secret schema).
func TestToIncarnationGetView_SchemaMasksDeclaredState(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				// Use a field name the name-based regex would NOT catch (no `secret`/`token`
				// fragment), to prove the schema layer itself does the masking.
				"join_value": map[string]any{"type": "string", "secret": true},
				"replicas":   map[string]any{"type": "integer"},
			},
		},
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{
		Service:        "redis",
		ServiceVersion: "v1",
		State: map[string]any{
			"join_value": "plaintext-secret-value",
			"replicas":   float64(3),
		},
	}
	schema := h.secretSchemaForIncarnation(context.Background(), inc)
	view := toIncarnationGetView(inc, schema)

	if view.State["join_value"] != "***MASKED***" {
		t.Errorf("schema-secret state.join_value = %v, want masked (e)", view.State["join_value"])
	}
	if view.State["replicas"] != float64(3) {
		t.Errorf("несекретный state.replicas = %v, want passthrough (нет over-masking)", view.State["replicas"])
	}
	// The stored state is not mutated.
	if inc.State["join_value"] != "plaintext-secret-value" {
		t.Errorf("исходный inc.State мутирован: %v", inc.State["join_value"])
	}
}

// (f) generic state field with config → NOT MASKED (no over-masking) when the
// secret schema is empty.
func TestToIncarnationGetView_GenericStateNotMasked(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"redis_config": map[string]any{"type": "object"}},
		},
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{
		Service:        "redis",
		ServiceVersion: "v1",
		State: map[string]any{
			"redis_config": map[string]any{"maxmemory": "256mb", "loglevel": "notice"},
		},
	}
	schema := h.secretSchemaForIncarnation(context.Background(), inc) // nil (no secret)
	view := toIncarnationGetView(inc, schema)

	cfg := view.State["redis_config"].(map[string]any)
	if cfg["maxmemory"] != "256mb" || cfg["loglevel"] != "notice" {
		t.Errorf("generic redis_config замаскирован — over-masking: %v", cfg)
	}
}
