package artifact

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// The load-time half of the collection-key uniqueness check ([ADR-0083] §1,
// NIM-704), in the same place and for the same reason as the own-namespace fence:
// this is keeper's one runtime parse of `scenario/<name>/main.yml` that has the
// service manifest — and therefore the state_schema — in scope.

func loadKeyedScenario(t *testing.T, body string) []diag.Diagnostic {
	t.Helper()
	art := &ServiceArtifact{
		LocalDir: t.TempDir(),
		Ref:      ServiceRef{Name: "redis"},
		Manifest: &config.ServiceManifest{
			StateSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"redis_users": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":     map[string]any{"type": "string"},
								"password": map[string]any{"type": "secret", "key": "name"},
							},
						},
					},
				},
			},
		},
	}
	_, _, diags, err := LoadScenarioManifestResolved(art, "scenario/update_users/main.yml", []byte(body), nil)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved: %v", err)
	}
	return diags
}

func hasDuplicateKeyDiag(diags []diag.Diagnostic) bool {
	for _, d := range diags {
		if d.Code == config.SecretKeyDuplicateCode {
			return true
		}
	}
	return false
}

func TestLoadScenarioManifestResolved_DuplicateSecretCollectionKeys(t *testing.T) {
	diags := loadKeyedScenario(t, `name: update_users
tasks:
  - name: Capture the users
    module: core.state.set
    params:
      field: redis_users
      value:
        - { name: alice, password: "${ generate_secret({}) }" }
        - { name: alice, password: "${ generate_secret({}) }" }
`)
	if !hasDuplicateKeyDiag(diags) {
		t.Fatalf("no %s diagnostic in %+v", config.SecretKeyDuplicateCode, diags)
	}
	if !diag.HasErrors(diags) {
		t.Fatal("the scenario still loads clean")
	}
}

// Distinct keys derive distinct paths: the normal case must load.
func TestLoadScenarioManifestResolved_DistinctSecretCollectionKeys(t *testing.T) {
	diags := loadKeyedScenario(t, `name: update_users
tasks:
  - name: Capture the users
    module: core.state.set
    params:
      field: redis_users
      value:
        - { name: alice, password: "${ generate_secret({}) }" }
        - { name: bob, password: "${ generate_secret({}) }" }
`)
	if hasDuplicateKeyDiag(diags) {
		t.Fatalf("the check fired on distinct keys: %+v", diags)
	}
}
