package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// writeFencedService lays out a minimal service tree — service.yml, the scenario,
// and any extra files an `include:` resolves against. Returns the path to main.yml.
// The manifest states no name (NIM-726): the fence's name comes from the caller,
// via `--service-name`, so these trees are laid out exactly as a real one is.
func writeFencedService(t *testing.T, mainYAML string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "service.yml"), []byte("state_schema_version: 1\n"), 0o600); err != nil {
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
    register: pw
    module: core.vault.kv-read
    params:
      path: secret/redis/prod/redis_users/app
`,
		"kv-present targets": `name: deploy
tasks:
  - name: Mint it
    module: core.vault.kv-present
    params:
      targets: "${ input.users.map(u, {'path': 'secret/redis/' + incarnation.name + '/users/' + u.name, 'field': 'password'}) }"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			diags := runJSONAs(t, writeFencedService(t, body, nil), "redis")
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
	diags := runJSONAs(t, writeFencedService(t, main, map[string]string{"deploy/users.yml": included}), "redis")
	if !hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("no %s in %+v", config.VaultOwnNamespaceCode, diags)
	}
}

// Outside the prefix nothing changes: `vault()` still reads a shared CA.
func TestLint_CrossNamespaceVaultSurvives(t *testing.T) {
	diags := runJSONAs(t, writeFencedService(t, `name: deploy
tasks:
  - name: Ship the CA
    module: core.file.present
    params:
      path: /etc/redis/ca.pem
      content: "${ vault('secret/services/shared/tls#ca') }"
`, nil), "redis")
	if hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence fired on a cross-namespace read: %+v", diags)
	}
}

// ★ NIM-726, the guard this ticket exists for. Without a service name the fence
// CANNOT run — and it must SAY SO. It used to return nil: `ScanOwnNamespaceVault`
// opened with `if service == "" { return nil }`, so a scenario writing into its own
// Vault namespace linted `OK`, exit 0, and was refused at render. Removing the
// manifest's `name:` without this warning would have turned that silent fail-open
// from an accident into the permanent shape of the tool.
//
// This test must FAIL on the old behaviour: delete the warning branch in
// scenarioVaultNamespaceDiags and it goes red, because the body below is a real
// fence violation that nothing else reports.
func TestLint_OwnNamespaceFenceUncheckedWithoutServiceName(t *testing.T) {
	body := `name: deploy
tasks:
  - name: Write the ACL
    module: core.file.present
    params:
      path: /etc/redis/users.acl
      content: "${ vault('secret/redis/prod/redis_users/app#password') }"
`
	main := writeFencedService(t, body, nil)

	diags := runJSONAs(t, main, "")
	if !hasCode(diags, FenceUncheckedCode) {
		t.Fatalf("no %s when --service-name is absent: %+v", FenceUncheckedCode, diags)
	}
	// A warning, not an error: linting a scenario standalone stays possible, and the
	// exit code stays 0. Refusing here would only teach operators to drop the linter.
	for _, d := range diags {
		if d.Code != FenceUncheckedCode {
			continue
		}
		if d.Level != diag.LevelWarning {
			t.Fatalf("%s level = %s, want warning", FenceUncheckedCode, d.Level)
		}
		if !strings.Contains(d.Hint, "--service-name") {
			t.Fatalf("hint %q must name the flag that fixes it", d.Hint)
		}
	}
	// And it is genuinely UNCHECKED — the real violation in the body is not reported,
	// which is precisely why the warning has to exist.
	if hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence reported a finding it could not have computed: %+v", diags)
	}

	// Same tree, same body, name supplied: the violation appears and the warning
	// does not. The two halves together are what make the warning honest.
	withName := runJSONAs(t, main, "redis")
	if !hasCode(withName, config.VaultOwnNamespaceCode) {
		t.Fatalf("--service-name given but the fence did not fire: %+v", withName)
	}
	if hasCode(withName, FenceUncheckedCode) {
		t.Fatalf("%s survived a stated service name: %+v", FenceUncheckedCode, withName)
	}
}

// A service tree with no service.yml at all is the standalone-linting case, and it
// behaves identically: the name comes from the flag, never from the tree, so the
// absence of a manifest changes nothing about the fence.
func TestLint_OwnNamespaceFenceIgnoresTheManifest(t *testing.T) {
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
	if diags := runJSONAs(t, mainPath, "redis"); !hasCode(diags, config.VaultOwnNamespaceCode) {
		t.Fatalf("the fence needs no service.yml to run: %+v", diags)
	}
}
