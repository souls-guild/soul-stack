package validate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// writeFencedService lays out a minimal service tree — service.yml (the only
// place the service NAME is written down), the scenario, and any extra files an
// `include:` resolves against. Returns the path to main.yml.
func writeFencedService(t *testing.T, mainYAML string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "service.yml"), []byte("name: redis\nstate_schema_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scnDir := filepath.Join(root, "scenario", "deploy")
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

// The linter is what `make lint` runs over the example corpus, so the fence has
// to bite offline in every spelling — not only at the keeper.
func TestLint_OwnNamespaceVaultFenced(t *testing.T) {
	cases := map[string]string{
		"cel macro": `name: deploy
tasks:
  - name: Write the ACL
    module: core.file.present
    params:
      path: /etc/redis/users.acl
      content: "${ vault('secret/redis/' + incarnation.name + '/redis_users/app#password') }"
`,
		"vault ref in params": `name: deploy
tasks:
  - name: Write the ACL
    module: core.file.present
    params:
      path: /etc/redis/users.acl
      content: "vault:secret/redis/prod/redis_users/app#password"
`,
		"kv-read path": `name: deploy
tasks:
  - name: Read it back
    on: keeper
    register: pw
    module: core.vault.kv-read
    params:
      path: secret/redis/prod/redis_users/app
`,
		"kv-present targets": `name: deploy
tasks:
  - name: Mint it
    on: keeper
    module: core.vault.kv-present
    params:
      targets: "${ input.users.map(u, {'path': 'secret/redis/' + incarnation.name + '/users/' + u.name, 'field': 'password'}) }"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			diags := runJSON(t, writeFencedService(t, body, nil))
			if !hasCode(diags, config.VaultOwnNamespaceCode) {
				t.Fatalf("no %s in %+v", config.VaultOwnNamespaceCode, diags)
			}
		})
	}
}

// A path in an INCLUDED file. The keeper's load-time fence structurally cannot
// see this one (include bodies are parsed inside ExpandIncludes); the linter can,
// because it expands first — so the corpus is checked at the level `make lint`
// runs at, not only at render.
func TestLint_OwnNamespaceVaultFencedInIncludedFile(t *testing.T) {
	main := `name: deploy
tasks:
  - include: users.yml
`
	included := `- name: Write the ACL
  module: core.file.present
  params:
    path: /etc/redis/users.acl
    content: "${ vault('secret/redis/prod/redis_users/app#password') }"
`
	diags := runJSON(t, writeFencedService(t, main, map[string]string{"deploy/users.yml": included}))
	if !hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("no %s in %+v", config.VaultOwnNamespaceCode, diags)
	}
}

// Outside the prefix nothing changes: `vault()` still reads a shared CA.
func TestLint_CrossNamespaceVaultSurvives(t *testing.T) {
	diags := runJSON(t, writeFencedService(t, `name: deploy
tasks:
  - name: Ship the CA
    module: core.file.present
    params:
      path: /etc/redis/ca.pem
      content: "${ vault('secret/services/shared/tls#ca') }"
`, nil))
	if hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence fired on a cross-namespace read: %+v", diags)
	}
}

// Without a service.yml there is no name to key the fence on. Standalone linting
// stays silent rather than inventing one — the same choice
// scenarioCompatFloorDiags makes, and the reason the keeper keeps its own copy of
// the fence where the manifest is never absent.
func TestLint_OwnNamespaceVaultSilentWithoutServiceManifest(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "deploy")
	if err := os.MkdirAll(scnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(scnDir, "main.yml")
	if err := os.WriteFile(mainPath, []byte(`name: deploy
tasks:
  - name: Write the ACL
    module: core.file.present
    params:
      path: /etc/redis/users.acl
      content: "${ vault('secret/redis/prod/redis_users/app#password') }"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if diags := runJSON(t, mainPath); hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence fired with no service.yml to name the service: %+v", diags)
	}
}
