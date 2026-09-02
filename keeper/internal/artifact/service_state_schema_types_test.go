package artifact

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// `state_schema` may reach its element shape through `$type` ([NIM-740]), and
// [ServiceLoader.parseManifest] is the single point where the keeper resolves that.
// Everything downstream — the declared-secret walk, the collection-kind lookup, the
// top-level-field check in core.state — reads the SHAPE, and an unresolved reference
// has none. The failure this guards is silent: a schema that declares a secret would
// answer "no secrets here", and a plaintext password would land in
// `incarnation.state` for good.

const aclUserTypes = `types:
  AclUser:
    type: object
    properties:
      name: { type: string, required: true }
      perms: { type: string, required: true }
`

const usersViaTypeManifest = `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: name }
`

func loadFixtureManifest(t *testing.T, root string) (*config.ServiceManifest, error) {
	t.Helper()
	l := &ServiceLoader{}
	return l.parseManifest(&ServiceArtifact{LocalDir: root, Ref: ServiceRef{Name: "redis"}})
}

// The happy path: the type is substituted, the secret written at the point of use is
// found, and its Vault path is derived from the state field it sits under.
func TestParseManifest_ResolvesStateSchemaTypeRefs(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, usersViaTypeManifest)
	writeTypesCatalog(t, root, aclUserTypes)

	m, err := loadFixtureManifest(t, root)
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	items := m.StateSchema["redis_users"].Items
	if items == nil || items.TypeRef != "" {
		t.Fatalf("redis_users.items = %+v, want the reference resolved away", items)
	}
	if items.Properties["name"] == nil || items.Properties["perms"] == nil {
		t.Errorf("the type's own properties are missing after the resolve: %+v", items.Properties)
	}

	fields, issues := config.CollectSecretFields(m.StateSchema)
	if len(issues) != 0 {
		t.Fatalf("issues on a resolved schema: %+v", issues)
	}
	if len(fields) != 1 {
		t.Fatalf("fields = %+v, want the one declared secret — an unresolved reference would give none", fields)
	}
	got, err := fields[0].VaultPath("", "redis", "redis-prod", "alice")
	if err != nil {
		t.Fatalf("VaultPath: %v", err)
	}
	if want := "secret/redis/redis-prod/redis_users/alice"; got != want {
		t.Errorf("VaultPath = %q, want %q", got, want)
	}
	// And the segment that comes from state data is still checked after the move.
	if _, err := fields[0].VaultPath("", "redis", "redis-prod", "../../keeper/jwt-signing-key"); err == nil {
		t.Error("a traversal key was accepted through a $type-resolved declaration")
	}
}

// A reference to a type the catalog does not hold fails the LOAD. Passing it through
// would leave a schema with no element shape, which reads downstream as a service with
// no declared secrets — the one wrong answer that is invisible.
func TestParseManifest_UnknownStateSchemaTypeFailsTheLoad(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, usersViaTypeManifest)
	writeTypesCatalog(t, root, "types: {}\n")

	_, err := loadFixtureManifest(t, root)
	if err == nil {
		t.Fatal("a manifest referencing an undeclared type loaded clean")
	}
	// The load error carries the first diagnostic's MESSAGE (firstError), which is what
	// the operator reads; it has to name the reference that could not be resolved.
	if !strings.Contains(err.Error(), `$type "AclUser" is not declared`) {
		t.Errorf("error does not name the broken reference: %v", err)
	}
}

// The secret's `key:` is judged against the RESOLVED element. At parse time the sibling
// it names lives in types.yml, which shared/config never reads — so the check has to run
// after the resolve, and it has to still run.
func TestParseManifest_SecretKeyIsCheckedAgainstTheResolvedType(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      $type: AclUser
      properties:
        password: { type: secret, key: uid }
`)
	writeTypesCatalog(t, root, aclUserTypes)

	_, err := loadFixtureManifest(t, root)
	if err == nil {
		t.Fatal("a key: naming no property of the resolved type loaded clean")
	}
	if !strings.Contains(err.Error(), `key: "uid" names no sibling property`) {
		t.Errorf("error does not name the broken key: %v", err)
	}
}

// A manifest with no `$type` never reads the catalog, and loads the same with or
// without one — the resolve must not become a dependency on a file types are optional in.
func TestParseManifest_NoTypeRefNeedsNoCatalog(t *testing.T) {
	root := t.TempDir()
	writeServiceManifest(t, root, `state_schema_version: 1
state_schema:
  redis_users:
    type: array
    items:
      type: object
      properties:
        name:     { type: string }
        password: { type: secret, key: name }
`)

	m, err := loadFixtureManifest(t, root)
	if err != nil {
		t.Fatalf("parseManifest without types.yml: %v", err)
	}
	fields, issues := config.CollectSecretFields(m.StateSchema)
	if len(fields) != 1 || len(issues) != 0 {
		t.Fatalf("fields = %+v, issues = %+v, want the inline declaration collected", fields, issues)
	}
}
