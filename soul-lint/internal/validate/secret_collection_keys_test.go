package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeSecretCollectionService lays out a service whose state_schema declares one
// collection secret — the wb-service-redis shape — plus the scenario and any file
// an `include:` resolves against. Returns the path to main.yml.
func writeSecretCollectionService(t *testing.T, mainYAML string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	const svc = `name: redis
state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      type: object
      properties:
        name: { type: string }
        perms: { type: string }
        password:
          type: secret
          key: name
`
	if err := os.WriteFile(filepath.Join(root, "service.yml"), []byte(svc), 0o600); err != nil {
		t.Fatal(err)
	}
	scnDir := filepath.Join(root, "scenario", "update_users")
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(scnDir, "main.yml")
	if err := os.WriteFile(mainPath, []byte(mainYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, body := range extra {
		p := filepath.Join(root, "scenario", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return mainPath
}

const duplicateKeysCapture = `name: update_users
tasks:
  - name: Capture the users
    module: core.state.set
    params:
      field: redis_users
      value:
        - { name: alice, perms: "+@read", password: "${ generate_secret({}) }" }
        - { name: alice, perms: "+@write", password: "${ generate_secret({}) }" }
`

// The linter is what `make lint` runs over the corpus, so a copy-pasted users block
// has to be caught while the author is reading the file — not on a run that has
// already touched Vault.
func TestLint_DuplicateSecretCollectionKeys(t *testing.T) {
	diags := runJSON(t, writeSecretCollectionService(t, duplicateKeysCapture, nil))
	if !hasCode(diags, config.SecretKeyDuplicateCode) {
		t.Fatalf("no %s in %+v", config.SecretKeyDuplicateCode, diags)
	}
}

// The reach of the offline half, pinned rather than assumed: the literal spelling
// is the only one it reads, and that spelling ALSO fails the module param check
// (`core.state.*` declares `value:` a string, because every corpus capture is a CEL
// expression). So it fires as the specific second diagnostic, never alone — and
// inside an `include:` not at all, because the expander drops a branch whose file
// has errors before any rule runs over it. The apply-time half is what guards the
// computed collection; if `value:` ever gains a list type, this test is where the
// change in reach shows up.
func TestLint_DuplicateSecretCollectionKeysReach(t *testing.T) {
	diags := runJSON(t, writeSecretCollectionService(t, duplicateKeysCapture, nil))
	if !hasCode(diags, "param_type_mismatch") {
		t.Errorf("a literal list in value: no longer fails the param check -- the include case below may now be reachable: %+v", diags)
	}

	main := `name: update_users
tasks:
  - include: users.yml
`
	included := `- name: Capture the users
  module: core.state.set
  params:
    field: redis_users
    value:
      - { name: alice, password: "${ generate_secret({}) }" }
      - { name: alice, password: "${ generate_secret({}) }" }
`
	inc := runJSON(t, writeSecretCollectionService(t, main, map[string]string{"update_users/users.yml": included}))
	if hasCode(inc, config.SecretKeyDuplicateCode) {
		t.Errorf("an included capture now survives expansion -- update the comments claiming it does not: %+v", inc)
	}
}

// Distinct keys derive distinct paths and are the normal case.
func TestLint_DistinctSecretCollectionKeysSurvive(t *testing.T) {
	diags := runJSON(t, writeSecretCollectionService(t, `name: update_users
tasks:
  - name: Capture the users
    module: core.state.set
    params:
      field: redis_users
      value:
        - { name: alice, password: "${ generate_secret({}) }" }
        - { name: bob, password: "${ generate_secret({}) }" }
`, nil))
	if hasCode(diags, config.SecretKeyDuplicateCode) {
		t.Fatalf("the check fired on distinct keys: %+v", diags)
	}
}

// Without a service.yml there is no state_schema saying which property is a secret
// or which sibling addresses it. Standalone linting stays silent rather than
// guessing — the same choice scenarioVaultNamespaceDiags makes.
func TestLint_DuplicateSecretCollectionKeysSilentWithoutServiceManifest(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "update_users")
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(scnDir, "main.yml")
	if err := os.WriteFile(mainPath, []byte(duplicateKeysCapture), 0o600); err != nil {
		t.Fatal(err)
	}
	if diags := runJSON(t, mainPath); hasCode(diags, config.SecretKeyDuplicateCode) {
		t.Fatalf("the check fired with no service.yml to read the schema from: %+v", diags)
	}
}
