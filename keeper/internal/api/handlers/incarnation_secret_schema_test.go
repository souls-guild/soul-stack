package handlers

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
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

// secretSchemaForIncarnation materializes the snapshot and merges state_schema
// secret + create-scenario input secret under input.<name>.
func TestSecretSchemaForIncarnation_StateAndInput(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: config.InputSchemaMap{
			"admin_token": {Type: "string", Secret: true},
		},
		// create-scenario with a secret input db_password.
		scenarioYAML: "name: create\ninput:\n  db_password: { type: string, secret: true }\n  hostname: { type: string }\n",
	}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}

	schema := h.secretSchemaForIncarnation(context.Background(), inc)
	if schema == nil {
		t.Fatal("schema nil — expected non-empty (state+input secret)")
	}
	if !schema.IsSecret("admin_token") {
		t.Errorf("state.admin_token not secret in schema")
	}
	if !schema.IsSecret("input.db_password") {
		t.Errorf("spec.input.db_password not secret in schema")
	}
	if schema.IsSecret("input.hostname") {
		t.Errorf("input.hostname marked secret — over-collect")
	}
}

// loader error → nil schema (best-effort, GET does not fail).
func TestSecretSchemaForIncarnation_LoadErrorNil(t *testing.T) {
	loader := &fakeLoader{loadErr: context.DeadlineExceeded}
	h := sealSchemaHandler(loader)
	inc := &incarnation.Incarnation{Service: "redis", ServiceVersion: "v1"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("on load error schema must be nil (best-effort): %v", schema)
	}
}

// nil loader → nil schema (degradation to MaskSecrets).
func TestSecretSchemaForIncarnation_NilDeps(t *testing.T) {
	h := &IncarnationHandler{}
	inc := &incarnation.Incarnation{Service: "redis"}
	if schema := h.secretSchemaForIncarnation(context.Background(), inc); schema != nil {
		t.Errorf("without loader/services schema must be nil: %v", schema)
	}
}

// (e) schema-declared secret state field → MASKED on the read-path projection
// (toIncarnationGetView via the service secret schema).
func TestToIncarnationGetView_SchemaMasksDeclaredState(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: config.InputSchemaMap{
			// Use a field name the name-based regex would NOT catch (no `secret`/`token`
			// fragment), to prove the schema layer itself does the masking.
			"join_value": {Type: "string", Secret: true},
			"replicas":   {Type: "integer"},
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
		t.Errorf("non-secret state.replicas = %v, want passthrough (no over-masking)", view.State["replicas"])
	}
	// The stored state is not mutated.
	if inc.State["join_value"] != "plaintext-secret-value" {
		t.Errorf("original inc.State mutated: %v", inc.State["join_value"])
	}
}

// (f) generic state field with config → NOT MASKED (no over-masking) when the
// secret schema is empty.
func TestToIncarnationGetView_GenericStateNotMasked(t *testing.T) {
	loader := &fakeLoader{
		stateSchema: config.InputSchemaMap{"redis_config": {Type: "object"}},
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
		t.Errorf("generic redis_config masked — over-masking: %v", cfg)
	}
}
