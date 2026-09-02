package artifact

import (
	"testing"

	"github.com/goccy/go-yaml"

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
			StateSchema: stateSchemaFixture(t, `
redis_users:
  type: array
  items:
    type: object
    properties:
      name:     { type: string }
      password: { type: secret, key: name }
`),
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

// stateSchemaFixture parses a `state_schema:` body written in the input dialect
// ([NIM-740]) through the real decoder. The tests build their fixtures this way rather
// than by hand: the keys whose meaning the struct cannot express — `required` as a bool
// versus a list, `$type` — are resolved in InputSchema.UnmarshalYAML, so a hand-built
// InputSchema would be a shape the parser never produces.
func stateSchemaFixture(t *testing.T, src string) config.InputSchemaMap {
	t.Helper()
	var m config.InputSchemaMap
	if err := yaml.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("state_schema fixture does not parse: %v\n%s", err, src)
	}
	return m
}
