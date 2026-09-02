package artifact

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// The load-time half of the own-namespace fence ([ADR-0083] §7). It sits here
// because this is keeper's one runtime parse of `scenario/<name>/main.yml` that
// has the service manifest in scope — a scenario never states which service owns
// it, and the fence is keyed on that name.

func loadFencedScenario(t *testing.T, service, body string) []diag.Diagnostic {
	t.Helper()
	// The fence is keyed on the REGISTERED name (art.Ref.Name), not on anything the
	// manifest says — NIM-726 removed the manifest copy.
	art := &ServiceArtifact{
		Ref:      ServiceRef{Name: service},
		LocalDir: t.TempDir(),
		Manifest: &config.ServiceManifest{},
	}
	_, _, diags, err := LoadScenarioManifestResolved(art, "scenario/deploy/main.yml", []byte(body), nil)
	if err != nil {
		t.Fatalf("LoadScenarioManifestResolved: %v", err)
	}
	return diags
}

func fenceDiag(diags []diag.Diagnostic) *diag.Diagnostic {
	for i := range diags {
		if diags[i].Code == config.VaultOwnNamespaceCode {
			return &diags[i]
		}
	}
	return nil
}

func TestLoadScenarioManifestResolved_OwnNamespaceFenced(t *testing.T) {
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
			diags := loadFencedScenario(t, "redis", body)
			d := fenceDiag(diags)
			if d == nil {
				t.Fatalf("no %s diagnostic in %+v", config.VaultOwnNamespaceCode, diags)
			}
			if d.Level != diag.LevelError {
				t.Fatalf("level = %v, want error", d.Level)
			}
			if !diag.HasErrors(diags) {
				t.Fatal("the scenario still loads clean")
			}
		})
	}
}

// The fence runs BEFORE the `input:`-empty early return in the same function: a
// scenario that declares no input is fenced like any other.
func TestLoadScenarioManifestResolved_OwnNamespaceFencedWithoutInput(t *testing.T) {
	diags := loadFencedScenario(t, "redis", `name: deploy
tasks:
  - name: Echo
    module: core.exec.run
    params:
      cmd: "echo ${ vault('secret/redis/prod/redis_users/app#password') }"
`)
	if fenceDiag(diags) == nil {
		t.Fatalf("no fence diagnostic: %+v", diags)
	}
}

// Outside the service's own prefix every channel survives untouched — the class
// examples/service/*/vars/ already depends on (a shared TLS CA).
func TestLoadScenarioManifestResolved_CrossNamespaceSurvives(t *testing.T) {
	diags := loadFencedScenario(t, "redis", `name: deploy
tasks:
  - name: Ship the CA
    module: core.file.present
    params:
      path: /etc/redis/ca.pem
      content: "${ vault('secret/services/shared/tls#ca') }"
`)
	if d := fenceDiag(diags); d != nil {
		t.Fatalf("the fence fired on a cross-namespace read: %+v", *d)
	}
	if diag.HasErrors(diags) {
		var msgs []string
		for _, d := range diags {
			msgs = append(msgs, d.Code+": "+d.Message)
		}
		t.Fatalf("unexpected errors: %s", strings.Join(msgs, "; "))
	}
}
