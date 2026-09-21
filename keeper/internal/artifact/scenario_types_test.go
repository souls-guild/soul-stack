package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTypesCatalog — puts types.yml at the root of the test serviceRoot.
func writeTypesCatalog(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, typesCatalogFile), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile types.yml: %v", err)
	}
}

// DTO resolves a standalone $type reference: the node is replaced by the
// type body + the x-type annotation.
func TestListScenarios_ResolvesBareTypeRef(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
      port:
        type: integer
`)
	writeScenario(t, root, "deploy", `input:
  target:
    $type: Endpoint
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	target, ok := got[0].InputSchema["target"].(map[string]any)
	if !ok {
		t.Fatalf("target is not a map: %#v", got[0].InputSchema["target"])
	}
	// $type is replaced by the type body.
	if _, stillRef := target[typeRefKey]; stillRef {
		t.Fatalf("$type should be resolved, still raw: %#v", target)
	}
	if target["type"] != "object" {
		t.Fatalf("target.type = %v, want object", target["type"])
	}
	props, ok := target["properties"].(map[string]any)
	if !ok || props["host"] == nil {
		t.Fatalf("target.properties.host is missing: %#v", target)
	}
	// x-type annotation is present.
	if target[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("x-type = %v, want Endpoint", target[typeAnnotationKey])
	}
}

// DTO resolves items:{$type} (an array of typed elements).
func TestListScenarios_ResolvesItemsTypeRef(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Node:
    type: object
    properties:
      id:
        type: string
`)
	writeScenario(t, root, "scale", `input:
  nodes:
    type: array
    items:
      $type: Node
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	nodes := got[0].InputSchema["nodes"].(map[string]any)
	items, ok := nodes["items"].(map[string]any)
	if !ok {
		t.Fatalf("nodes.items is not a map: %#v", nodes["items"])
	}
	if _, stillRef := items[typeRefKey]; stillRef {
		t.Fatalf("items.$type should be resolved: %#v", items)
	}
	if items["type"] != "object" || items[typeAnnotationKey] != "Node" {
		t.Fatalf("items should carry Node shape + x-type=Node: %#v", items)
	}
}

// Type→type nesting: the body of a type that references another type is
// also resolved.
func TestListScenarios_ResolvesNestedTypeRef(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
  Cluster:
    type: object
    properties:
      primary:
        $type: Endpoint
`)
	writeScenario(t, root, "deploy", `input:
  cluster:
    $type: Cluster
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	cluster := got[0].InputSchema["cluster"].(map[string]any)
	if cluster[typeAnnotationKey] != "Cluster" {
		t.Fatalf("cluster.x-type = %v", cluster[typeAnnotationKey])
	}
	props := cluster["properties"].(map[string]any)
	primary, ok := props["primary"].(map[string]any)
	if !ok {
		t.Fatalf("cluster.properties.primary is not a map: %#v", props["primary"])
	}
	if _, stillRef := primary[typeRefKey]; stillRef {
		t.Fatalf("nested primary.$type should be resolved: %#v", primary)
	}
	if primary[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("primary.x-type = %v, want Endpoint", primary[typeAnnotationKey])
	}
}

// A cycle in the catalog → DTO does NOT hang (the node is left as-is, no
// infinite recursion). The full cycle error is raised by soul-lint.
func TestListScenarios_CycleDoesNotHang(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  A:
    type: object
    properties:
      b:
        $type: B
  B:
    type: object
    properties:
      a:
        $type: A
`)
	writeScenario(t, root, "deploy", `input:
  root:
    $type: A
`)

	// Doesn't hang — if the recursion were infinite, the test wouldn't finish.
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if got[0].InputSchema["root"] == nil {
		t.Fatalf("root should be present (best-effort on cycle)")
	}
}

// Unknown type → the node stays raw (best-effort; soul-lint raises unknown).
func TestListScenarios_UnknownTypeLeftRaw(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Known:
    type: string
`)
	writeScenario(t, root, "deploy", `input:
  x:
    $type: Ghost
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	x := got[0].InputSchema["x"].(map[string]any)
	if x[typeRefKey] != "Ghost" {
		t.Fatalf("unknown type - node remains raw with $type, got %#v", x)
	}
	if _, annotated := x[typeAnnotationKey]; annotated {
		t.Fatalf("unknown type should not get x-type: %#v", x)
	}
}

// Back-compat: a scenario without $type and a service without types.yml
// don't break.
func TestListScenarios_NoTypesCatalog_BackCompat(t *testing.T) {
	root := t.TempDir()
	writeScenario(t, root, "deploy", `input:
  port:
    type: integer
  opts:
    type: object
    properties:
      verbose:
        type: boolean
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	port := got[0].InputSchema["port"].(map[string]any)
	if port["type"] != "integer" {
		t.Fatalf("port should pass through unchanged: %#v", port)
	}
	opts := got[0].InputSchema["opts"].(map[string]any)
	props := opts["properties"].(map[string]any)
	if props["verbose"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("nested opts.verbose should pass through unchanged: %#v", props)
	}
}

// A shared type used in two places doesn't get "corrupted" between
// consumers (clone, not a shared pointer): the x-type annotation is correct
// for both.
func TestListScenarios_SharedTypeIsolated(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`)
	writeScenario(t, root, "deploy", `input:
  a:
    $type: Endpoint
  b:
    type: array
    items:
      $type: Endpoint
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	a := got[0].InputSchema["a"].(map[string]any)
	b := got[0].InputSchema["b"].(map[string]any)
	bItems := b["items"].(map[string]any)
	if a[typeAnnotationKey] != "Endpoint" || bItems[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("both Endpoint uses should carry x-type=Endpoint: a=%#v b.items=%#v", a, bItems)
	}
}

// TestListScenarios_TypeRefKeepsRequiredChildren — NIM-72: when resolving
// $type for the DTO, the requiredness each of the type's own properties declares
// survives, and the reference node's own `required: true` does not reach down and
// clobber them. Since [ADR-0086] §2 both are the same bool at different levels: the
// reference says "this FIELD is mandatory", a property says "this PROPERTY is".
// The presentational description key is carried over.
func TestListScenarios_TypeRefKeepsRequiredChildren(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        required: true
        type: string
      perms:
        required: true
        type: string
      state:
        type: string
`)
	writeScenario(t, root, "add_user", `input:
  user:
    $type: AclUser
    required: true
    description: "ACL user"
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	user, ok := got[0].InputSchema["user"].(map[string]any)
	if !ok {
		t.Fatalf("user is not a map: %#v", got[0].InputSchema["user"])
	}
	// $type is resolved + x-type annotation.
	if _, stillRef := user[typeRefKey]; stillRef {
		t.Fatalf("$type should be resolved: %#v", user)
	}
	if user[typeAnnotationKey] != "AclUser" {
		t.Fatalf("x-type = %v, want AclUser", user[typeAnnotationKey])
	}
	// the type's properties are preserved, each carrying its own requiredness.
	props, ok := user["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties should be preserved: %#v", user)
	}
	for _, name := range []string{"name", "perms"} {
		child, isMap := props[name].(map[string]any)
		if !isMap {
			t.Fatalf("property %q missing from the DTO: %#v", name, props)
		}
		if child["required"] != true {
			t.Errorf("property %q lost its own required flag in the DTO: %#v", name, child)
		}
	}
	if state, _ := props["state"].(map[string]any); state["required"] == true {
		t.Error("property `state` gained a required flag it never declared")
	}
	// the reference node's presentational description is carried over.
	if user["description"] != "ACL user" {
		t.Fatalf("description = %v, want transfer from reference node", user["description"])
	}
}

// TestListScenarios_TypeRefCarriesXRequired — NIM-72: the $type reference node's
// field-level `required: true` is projected as a separate `x-required` annotation.
//
// The annotation is now VESTIGIAL and deliberately kept ([ADR-0086] §13). It existed
// because the DTO key `required` was taken by the object-level list of required
// children; §2 frees the key, so the annotation is redundant — but retiring it is a DTO
// change plus a web-UI re-vendor, which buys this ticket nothing. What the guard pins is
// that it is still produced, so the bundle's `e.required===true || e["x-required"]===true`
// predicate keeps seeing what it saw.
func TestListScenarios_TypeRefCarriesXRequired(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        required: true
        type: string
      perms:
        required: true
        type: string
`)
	writeScenario(t, root, "add_user", `input:
  user:
    $type: AclUser
    required: true
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	user := got[0].InputSchema["user"].(map[string]any)
	// x-required annotation is present and equals true.
	if user[typeRequiredAnnotationKey] != true {
		t.Fatalf("x-required = %v, want true (field-level required from reference node)", user[typeRequiredAnnotationKey])
	}
	// The children keep their own flags, untouched by the reference-level one.
	props := user["properties"].(map[string]any)
	for _, name := range []string{"name", "perms"} {
		if props[name].(map[string]any)["required"] != true {
			t.Errorf("property %q lost its own required flag: %#v", name, props[name])
		}
	}
}

// TestListScenarios_TypeRefNoXRequiredWhenAbsent — NIM-72: without
// field-level `required: true` on the reference node, the x-required
// annotation does NOT appear (field requiredness defaults to false; we
// don't draw a false `*`). Checks both the bare-ref and the items form.
func TestListScenarios_TypeRefNoXRequiredWhenAbsent(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    properties:
      name:
        required: true
        type: string
`)
	writeScenario(t, root, "scenario", `input:
  user:
    $type: AclUser
  users:
    type: array
    items:
      $type: AclUser
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	user := got[0].InputSchema["user"].(map[string]any)
	if _, has := user[typeRequiredAnnotationKey]; has {
		t.Fatalf("bare-ref without required should not carry x-required: %#v", user)
	}
	items := got[0].InputSchema["users"].(map[string]any)["items"].(map[string]any)
	if _, has := items[typeRequiredAnnotationKey]; has {
		t.Fatalf("items-ref should not carry x-required: %#v", items)
	}
}

// TestListScenarios_TypeRefCarriesRequiredWhen — NIM-72: the reference
// node's required_when is carried over into the DTO (a separate key,
// doesn't conflict with the type's required array).
func TestListScenarios_TypeRefCarriesRequiredWhen(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`)
	writeScenario(t, root, "deploy", `input:
  target:
    $type: Endpoint
    required_when: "input.mode == 'remote'"
`)

	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	target := got[0].InputSchema["target"].(map[string]any)
	if target["required_when"] != "input.mode == 'remote'" {
		t.Fatalf("required_when from reference node should transfer into DTO, got %v", target["required_when"])
	}
	if target[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("x-type = %v, want Endpoint", target[typeAnnotationKey])
	}
}
