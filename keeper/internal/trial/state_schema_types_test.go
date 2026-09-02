package trial

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The trial harness is the Trial TWIN of the keeper's service load, and it has to
// resolve `state_schema`'s `$type` references for the same reason the keeper does:
// every consumer downstream reads the element SHAPE, and an unresolved
// `{$type: AclUser}` has none.
//
// The consumer that matters here is [config.StripDeclaredSecrets], which
// [stateop.Merge] calls at the end of every merge. Unresolved, it finds no declared
// secrets, so a password that PROD deletes from the record would survive into
// `assert.state_after` and the trial diff — and the case pinning that record would be
// pinning a shape the run never produces. The twin would have diverged in the one
// direction that matters, silently.

const typedUsersService = `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`

const typedUsersCatalog = `types:
  AclUser:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string, required: true }
`

// writeTypedServiceTree lays out `<root>/service.yml` + `types.yml` and returns the
// case file path the harness resolves the service root from.
func writeTypedServiceTree(t *testing.T, service, catalog string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "redis")
	caseDir := filepath.Join(root, "scenario", "create", "tests", "c1")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, "service.yml"), service)
	if catalog != "" {
		writeFile(t, filepath.Join(root, config.TypesCatalogFile), catalog)
	}
	return filepath.Join(caseDir, caseFileName)
}

// The declared secret is found through the reference, so the stripper can remove it.
func TestLoadServiceStateSchema_ResolvesTypeRefs(t *testing.T) {
	schema, err := loadServiceStateSchema(writeTypedServiceTree(t, typedUsersService, typedUsersCatalog))
	if err != nil {
		t.Fatalf("loadServiceStateSchema: %v", err)
	}
	items := schema["redis_users"].Items
	if items == nil || items.TypeRef != "" || items.Properties["name"] == nil {
		t.Fatalf("redis_users.items = %+v, want the reference resolved away", items)
	}

	fields, issues := config.CollectSecretFields(schema)
	if len(issues) != 0 {
		t.Fatalf("issues on a resolved schema: %+v", issues)
	}
	if len(fields) != 1 {
		t.Fatalf("fields = %+v, want the one declared secret -- unresolved gives none", fields)
	}

	// The consequence, stated as the behaviour a case would actually observe: the
	// secret leaves the record, the addressing key stays.
	state := map[string]any{"redis_users": []any{
		map[string]any{"name": "alice", "perms": "+get", "password": "PLAINTEXT"},
	}}
	config.StripDeclaredSecrets(state, schema)
	elem := state["redis_users"].([]any)[0].(map[string]any)
	if _, still := elem["password"]; still {
		t.Errorf("password survived the strip -- the trial twin would diff a record prod never writes: %+v", elem)
	}
	if elem["name"] != "alice" {
		t.Errorf("the addressing key was stripped too: %+v", elem)
	}
}

// A missing or broken catalog is not fatal at L0: the tree may be mid-edit, and
// `soul-lint validate-service` reports the catalog's own errors at the file they
// belong to. The schema is returned unresolved, which loses the secrets -- the same
// "no schema, no check" asymmetry the offline half of the collection-key rule carries.
func TestLoadServiceStateSchema_BrokenCatalogIsNotFatal(t *testing.T) {
	for name, catalog := range map[string]string{
		"absent":    "",
		"duplicate": "types:\n  AclUser: { type: object, properties: { a: { type: string } } }\n  AclUser: { type: object, properties: { b: { type: string } } }\n",
		"unknown":   "types: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			schema, err := loadServiceStateSchema(writeTypedServiceTree(t, typedUsersService, catalog))
			if err != nil {
				t.Fatalf("loadServiceStateSchema: %v", err)
			}
			if schema == nil {
				t.Fatal("schema is nil -- a broken catalog must not take the whole case down")
			}
			if schema["redis_users"].Items.TypeRef == "" {
				t.Error("the reference was substituted from a catalog that does not resolve it")
			}
		})
	}
}

// A service with no `$type` never reads the catalog and behaves exactly as before.
func TestLoadServiceStateSchema_NoTypeRefIsUnchanged(t *testing.T) {
	schema, err := loadServiceStateSchema(writeTypedServiceTree(t, `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      type: object
      properties:
        name:     { type: string }
        password: { type: secret, key: name }
`, ""))
	if err != nil {
		t.Fatalf("loadServiceStateSchema: %v", err)
	}
	fields, issues := config.CollectSecretFields(schema)
	if len(fields) != 1 || len(issues) != 0 {
		t.Fatalf("fields = %+v, issues = %+v, want the inline declaration collected", fields, issues)
	}
}
