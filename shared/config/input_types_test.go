package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Tests for reusable named input-schema types (types.yml + $type-ref).
// Coverage: catalog parsing; resolving a $type field + items:{$type}; type→type
// nesting; cycle (NO hang); unknown; duplicate; ref_conflict ($type+inline);
// back-compat (schemas without $type).

// --- $type as a standalone field in input: (node schema validation) ---

func TestTypeRef_BareField_NoTypeRequired(t *testing.T) {
	// A node with $type need not declare type: — it is a reference, the type gives the shape.
	src := `name: x
input:
  cfg:
    $type: RedisConfig
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("$type-узел не должен требовать type: (он ссылка)")
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("чистая $type-ссылка должна валидироваться без ошибок")
	}
}

// --- ref_conflict: $type TOGETHER WITH inline type/properties/items ---

func TestTypeRef_ConflictWithType(t *testing.T) {
	src := `name: x
input:
  cfg:
    $type: RedisConfig
    type: object
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_type_ref_conflict") {
		dump(t, diags)
		t.Fatalf("$type + type: должно дать input_type_ref_conflict")
	}
}

func TestTypeRef_ConflictWithProperties(t *testing.T) {
	src := `name: x
input:
  cfg:
    $type: RedisConfig
    properties:
      port:
        type: integer
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_type_ref_conflict") {
		dump(t, diags)
		t.Fatalf("$type + properties: должно дать input_type_ref_conflict")
	}
}

func TestTypeRef_ConflictWithItems(t *testing.T) {
	src := `name: x
input:
  cfg:
    $type: RedisConfig
    items:
      type: string
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_type_ref_conflict") {
		dump(t, diags)
		t.Fatalf("$type + items: на одном узле должно дать input_type_ref_conflict")
	}
}

// items:{$type} is NOT a conflict: the items reference lives on the array parent.
func TestTypeRef_ItemsRef_NotConflict(t *testing.T) {
	src := `name: x
input:
  servers:
    type: array
    items:
      $type: ServerSpec
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "input_type_ref_conflict") {
		dump(t, diags)
		t.Fatalf("items:{$type} не должно считаться конфликтом")
	}
}

// $type of invalid form (mapping / bad name).
func TestTypeRef_NonStringValue(t *testing.T) {
	src := `name: x
input:
  cfg:
    $type:
      nested: true
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("$type не-строка должно дать type_mismatch")
	}
}

func TestTypeRef_NameInvalid(t *testing.T) {
	src := `name: x
input:
  cfg:
    $type: bad-name.with-dots
`
	_, _, diags, _ := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "input_type_ref_name_invalid") {
		dump(t, diags)
		t.Fatalf("имя $type с точками/дефисами должно дать input_type_ref_name_invalid")
	}
}

// --- ParseTypeCatalog: parsing types.yml ---

func TestParseTypeCatalog_Basic(t *testing.T) {
	src := `types:
  ServerSpec:
    type: object
    properties:
      host:
        type: string
      port:
        type: integer
`
	catalog, diags := ParseTypeCatalog("types.yml", []byte(src))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("валидный types.yml не должен давать ошибок")
	}
	spec, ok := catalog["ServerSpec"]
	if !ok {
		t.Fatalf("ServerSpec должен быть в каталоге")
	}
	if spec.Type != "object" {
		t.Fatalf("ServerSpec.Type = %q, ожидался object", spec.Type)
	}
	if _, ok := spec.Properties["port"]; !ok {
		t.Fatalf("ServerSpec.properties.port отсутствует")
	}
}

func TestParseTypeCatalog_Empty(t *testing.T) {
	catalog, diags := ParseTypeCatalog("types.yml", []byte(""))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("пустой types.yml — пустой каталог без ошибок")
	}
	if len(catalog) != 0 {
		t.Fatalf("пустой types.yml → пустой каталог, got %d", len(catalog))
	}
}

func TestParseTypeCatalog_NoTypesSection(t *testing.T) {
	catalog, diags := ParseTypeCatalog("types.yml", []byte("# comment only\n"))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("types.yml без секции types: валиден (пустой каталог)")
	}
	if len(catalog) != 0 {
		t.Fatalf("нет секции types: → пустой каталог")
	}
}

func TestParseTypeCatalog_UnknownTopLevel(t *testing.T) {
	src := `types:
  A:
    type: string
junk: true
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("посторонний top-level ключ в types.yml → unknown_key")
	}
}

// --- input_type_duplicate ---

func TestParseTypeCatalog_Duplicate(t *testing.T) {
	src := `types:
  Dup:
    type: string
  Dup:
    type: integer
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "input_type_duplicate") {
		dump(t, diags)
		t.Fatalf("два типа с одним именем → input_type_duplicate")
	}
}

// --- type→type nesting (resolving $type inside a type) ---

func TestParseTypeCatalog_NestedTypeRef(t *testing.T) {
	src := `types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
      port:
        type: integer
  Cluster:
    type: object
    properties:
      primary:
        $type: Endpoint
      replicas:
        type: array
        items:
          $type: Endpoint
`
	catalog, diags := ParseTypeCatalog("types.yml", []byte(src))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("вложенность тип→тип должна резолвиться без ошибок")
	}
	cluster := catalog["Cluster"]
	if cluster == nil {
		t.Fatalf("Cluster отсутствует")
	}
	primary := cluster.Properties["primary"]
	if primary == nil || primary.TypeRef != "" {
		t.Fatalf("primary должен быть резолвнут (TypeRef очищен), got %+v", primary)
	}
	if primary.Type != "object" || primary.Properties["host"] == nil {
		t.Fatalf("primary должен нести форму Endpoint, got %+v", primary)
	}
	replicas := cluster.Properties["replicas"]
	if replicas == nil || replicas.Items == nil {
		t.Fatalf("replicas.items должен присутствовать")
	}
	if replicas.Items.TypeRef != "" || replicas.Items.Type != "object" {
		t.Fatalf("replicas.items должен быть резолвнут в Endpoint, got %+v", replicas.Items)
	}
}

// --- input_type_cycle: NOT a hang, but an error ---

func TestParseTypeCatalog_DirectCycle(t *testing.T) {
	src := `types:
  A:
    $type: B
  B:
    $type: A
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "input_type_cycle") {
		dump(t, diags)
		t.Fatalf("A→B→A должно дать input_type_cycle (а не зависание)")
	}
}

func TestParseTypeCatalog_SelfCycle(t *testing.T) {
	src := `types:
  Recur:
    type: object
    properties:
      child:
        $type: Recur
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "input_type_cycle") {
		dump(t, diags)
		t.Fatalf("самоссылка Recur→Recur должна дать input_type_cycle")
	}
}

func TestParseTypeCatalog_IndirectCycle(t *testing.T) {
	src := `types:
  A:
    type: object
    properties:
      b:
        $type: B
  B:
    type: object
    properties:
      c:
        $type: C
  C:
    type: object
    properties:
      a:
        $type: A
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "input_type_cycle") {
		dump(t, diags)
		t.Fatalf("транзитивный цикл A→B→C→A должен дать input_type_cycle")
	}
}

// --- input_type_unknown ---

func TestParseTypeCatalog_UnknownRef(t *testing.T) {
	src := `types:
  A:
    type: object
    properties:
      x:
        $type: Ghost
`
	_, diags := ParseTypeCatalog("types.yml", []byte(src))
	if !hasCode(diags, "input_type_unknown") {
		dump(t, diags)
		t.Fatalf("ссылка на отсутствующий тип → input_type_unknown")
	}
}

// --- ResolveTypeRefs: resolving a scenario's input: against the catalog ---

func TestResolveTypeRefs_BareField(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := InputSchemaMap{
		"target": {TypeRef: "Endpoint"},
	}
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв валидной ссылки не должен давать ошибок")
	}
	tgt := resolved["target"]
	if tgt == nil || tgt.TypeRef != "" {
		t.Fatalf("target должен быть резолвнут, got %+v", tgt)
	}
	if tgt.Type != "object" || tgt.Properties["host"] == nil {
		t.Fatalf("target должен нести форму Endpoint, got %+v", tgt)
	}
}

func TestResolveTypeRefs_ItemsRef(t *testing.T) {
	cat, _ := ParseTypeCatalog("types.yml", []byte(`types:
  Node:
    type: object
    properties:
      id:
        type: string
`))
	in := InputSchemaMap{
		"nodes": {
			Type:  "array",
			Items: &InputSchema{TypeRef: "Node"},
		},
	}
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв items:{$type} не должен давать ошибок")
	}
	nodes := resolved["nodes"]
	if nodes.Items == nil || nodes.Items.TypeRef != "" {
		t.Fatalf("nodes.items должен быть резолвнут, got %+v", nodes.Items)
	}
	if nodes.Items.Type != "object" || nodes.Items.Properties["id"] == nil {
		t.Fatalf("nodes.items должен нести форму Node, got %+v", nodes.Items)
	}
}

func TestResolveTypeRefs_Unknown(t *testing.T) {
	cat, _ := ParseTypeCatalog("types.yml", []byte(`types:
  Known:
    type: string
`))
	in := InputSchemaMap{
		"x": {TypeRef: "Missing"},
	}
	_, rdiags := ResolveTypeRefs(in, cat)
	if !hasCode(rdiags, "input_type_unknown") {
		dump(t, rdiags)
		t.Fatalf("ссылка на отсутствующий тип → input_type_unknown")
	}
}

// --- back-compat: schemas without $type do not break ---

func TestResolveTypeRefs_NoRef_PassThrough(t *testing.T) {
	cat := TypeCatalog{}
	in := InputSchemaMap{
		"port": {Type: "integer"},
		"opts": {
			Type: "object",
			Properties: InputSchemaMap{
				"verbose": {Type: "boolean"},
			},
		},
	}
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("схемы без $type не должны давать ошибок")
	}
	if resolved["port"].Type != "integer" {
		t.Fatalf("port должен пройти насквозь")
	}
	if resolved["opts"].Properties["verbose"].Type != "boolean" {
		t.Fatalf("вложенный opts.verbose должен пройти насквозь")
	}
}

func TestResolveTypeRefs_Nil(t *testing.T) {
	resolved, rdiags := ResolveTypeRefs(nil, TypeCatalog{})
	if resolved != nil || rdiags != nil {
		t.Fatalf("nil input → nil без диагностик")
	}
}

// Resolve does NOT mutate the catalog: a shared type used twice causes no false
// cycle and does not "corrupt" between consumers.
func TestResolveTypeRefs_SharedTypeNoFalseCycle(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  Endpoint:
    type: object
    properties:
      host:
        type: string
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := InputSchemaMap{
		"a": {TypeRef: "Endpoint"},
		"b": {
			Type:  "array",
			Items: &InputSchema{TypeRef: "Endpoint"},
		},
	}
	_, rdiags := ResolveTypeRefs(in, cat)
	if hasCode(rdiags, "input_type_cycle") {
		dump(t, rdiags)
		t.Fatalf("один тип в двух местах — не цикл")
	}
}

// TestResolveTypeRefs_DeepPlainObject_NoFalseCycle — MINOR 2 regression: a deeply
// nested PLAIN object (structural properties descent past typeRefResolveLimit)
// WITHOUT a single $type reference must NOT falsely produce input_type_cycle. The
// limit counts only type-ref hops (a ref-graph property), structural descent is not limited.
func TestResolveTypeRefs_DeepPlainObject_NoFalseCycle(t *testing.T) {
	// Build an object→properties→object… chain noticeably deeper than the limit.
	depth := typeRefResolveLimit + 50
	leaf := &InputSchema{Type: "string"}
	cur := leaf
	for i := 0; i < depth; i++ {
		cur = &InputSchema{
			Type:       "object",
			Properties: InputSchemaMap{"child": cur},
		}
	}
	in := InputSchemaMap{"root": cur}

	_, rdiags := ResolveTypeRefs(in, TypeCatalog{})
	if hasCode(rdiags, "input_type_cycle") {
		dump(t, rdiags)
		t.Fatalf("глубокий обычный object (без $type) не должен давать input_type_cycle")
	}
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("глубокий обычный object без $type не должен давать ошибок резолва")
	}
}

// TestParseTypeCatalog_NameNotPascalCase — MINOR 1 regression: a type name with
// `snake_case`/underscore (looser than the PascalCase spec) is rejected with
// input_type_ref_name_invalid. PascalCase ^[A-Z][A-Za-z0-9]*$ (naming-rules.md).
func TestParseTypeCatalog_NameNotPascalCase(t *testing.T) {
	cases := []string{"acl_user", "Acl_User", "aclUser"}
	for _, name := range cases {
		src := "types:\n  " + name + ":\n    type: string\n"
		_, diags := ParseTypeCatalog("types.yml", []byte(src))
		if !hasCode(diags, "input_type_ref_name_invalid") {
			dump(t, diags)
			t.Fatalf("имя типа %q (не PascalCase) должно дать input_type_ref_name_invalid", name)
		}
	}
}

// TestParseTypeCatalog_NamePascalCase_OK — a PascalCase name passes without a
// name error (MINOR 1 boundary: the tightening does not touch valid names).
func TestParseTypeCatalog_NamePascalCase_OK(t *testing.T) {
	for _, name := range []string{"AclUser", "Endpoint", "Cluster2", "A"} {
		src := "types:\n  " + name + ":\n    type: string\n"
		_, diags := ParseTypeCatalog("types.yml", []byte(src))
		if hasCode(diags, "input_type_ref_name_invalid") {
			dump(t, diags)
			t.Fatalf("PascalCase-имя %q не должно давать input_type_ref_name_invalid", name)
		}
	}
}

// --- NIM-72: overlay field-level required/required_when from a $type reference node ---

// TestResolveTypeRefs_OverlayRequired — a field-level `required: true` on the
// reference node is NOT lost when resolving $type; the type's object-level
// required-children (RequiredProps) are preserved — they are DIFFERENT model fields.
func TestResolveTypeRefs_OverlayRequired(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    additional_properties: false
    required: [name, perms]
    properties:
      name:  { type: string }
      perms: { type: string }
      state: { type: string, default: "on", enum: [on, off] }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог AclUser невалиден")
	}
	in := schemaFromInput(t, `user:
  $type: AclUser
  required: true
  description: "ACL user"
`)
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв не должен давать ошибок")
	}
	u := resolved["user"]
	if u == nil || u.TypeRef != "" {
		t.Fatalf("user должен быть резолвнут (TypeRef очищен), got %+v", u)
	}
	// (a) the type's shape is substituted.
	if u.Type != "object" || u.Properties["name"] == nil || u.Properties["perms"] == nil || u.Properties["state"] == nil {
		t.Fatalf("user должен нести форму AclUser (object + name/perms/state), got %+v", u)
	}
	// (b) object-level required-children are preserved.
	if len(u.RequiredProps) != 2 || u.RequiredProps[0] != "name" || u.RequiredProps[1] != "perms" {
		t.Fatalf("object-level required [name perms] должны сохраниться, got %v", u.RequiredProps)
	}
	// (c) field-mandatory carried over from the reference node.
	if !u.Required {
		t.Fatalf("field-level `required: true` узла-ссылки должен перенестись (Required=true)")
	}
	// (d) description carried over (regression guard for the former overlay).
	if u.Description != "ACL user" {
		t.Fatalf("description узла-ссылки должен перенестись, got %q", u.Description)
	}
}

// TestResolveTypeRefs_OverlayRequired_Enforced — behavioral: after resolving $type,
// field-mandatory rejects a missing parameter, and the preserved object-level
// required-children reject an incomplete object. Before the fix an empty user passed
// silently (the type's requiredKind == requiredList did not trigger field-mandatory).
func TestResolveTypeRefs_OverlayRequired_Enforced(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    required: [name, perms]
    properties:
      name:  { type: string }
      perms: { type: string }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := schemaFromInput(t, `user:
  $type: AclUser
  required: true
`)
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв не должен давать ошибок")
	}
	// user not passed → field-mandatory rejects (NIM-72 regression).
	if _, err := ResolveInputValues(resolved, map[string]any{}); err == nil {
		t.Fatalf("отсутствующий field-mandatory user должен отсекаться")
	}
	// user passed but without the required perms → object-level required rejects.
	_, err := ResolveInputValues(resolved, map[string]any{
		"user": map[string]any{"name": "app"},
	})
	if err == nil {
		t.Fatalf("неполный user (без perms) должен отсекаться")
	}
	if !strings.Contains(err.Error(), "perms") {
		t.Fatalf("ошибка должна указывать на perms, got %v", err)
	}
	// full user → OK.
	if _, err := ResolveInputValues(resolved, map[string]any{
		"user": map[string]any{"name": "app", "perms": "on"},
	}); err != nil {
		t.Fatalf("полный user должен проходить: %v", err)
	}
}

// TestResolveTypeRefs_OverlayRequiredWhen — a required_when on the reference node
// carries over to the resolved schema if the type did not set it (conditional
// requiredness is not lost).
func TestResolveTypeRefs_OverlayRequiredWhen(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  Endpoint:
    type: object
    properties:
      host: { type: string }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := schemaFromInput(t, `target:
  $type: Endpoint
  required_when: "input.mode == 'remote'"
`)
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв не должен давать ошибок")
	}
	if resolved["target"].RequiredWhen != "input.mode == 'remote'" {
		t.Fatalf("required_when узла-ссылки должен перенестись, got %q", resolved["target"].RequiredWhen)
	}
}

// TestResolveTypeRefs_OverlayRequiredFalse — edge case: an explicit `required: false`
// on the reference node does NOT make the field mandatory (overlay does not invent
// requiredness, a false on the reference does not become field-mandatory).
func TestResolveTypeRefs_OverlayRequiredFalse(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  AclUser:
    type: object
    required: [name, perms]
    properties:
      name:  { type: string }
      perms: { type: string }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := schemaFromInput(t, `user:
  $type: AclUser
  required: false
`)
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв не должен давать ошибок")
	}
	if resolved["user"].Required {
		t.Fatalf("явный `required: false` узла-ссылки НЕ должен делать поле обязательным")
	}
	// Behaviorally: a missing user is NOT rejected (not field-mandatory).
	if _, err := ResolveInputValues(resolved, map[string]any{}); err != nil {
		t.Fatalf("required:false → отсутствующий user должен проходить, got %v", err)
	}
}

// TestResolveTypeRefs_OverlayRequiredWhen_TypeWins — edge case: required_when set
// BOTH on the type (types.yml) AND on the reference node → the TYPE's required_when
// wins (overlay carries over only when the type lacks it, branch `resolved.RequiredWhen == ""`).
func TestResolveTypeRefs_OverlayRequiredWhen_TypeWins(t *testing.T) {
	cat, diags := ParseTypeCatalog("types.yml", []byte(`types:
  Endpoint:
    type: object
    required_when: "input.mode == 'type_side'"
    properties:
      host: { type: string }
`))
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("каталог невалиден")
	}
	in := schemaFromInput(t, `target:
  $type: Endpoint
  required_when: "input.mode == 'ref_side'"
`)
	resolved, rdiags := ResolveTypeRefs(in, cat)
	if diag.HasErrors(rdiags) {
		dump(t, rdiags)
		t.Fatalf("резолв не должен давать ошибок")
	}
	if resolved["target"].RequiredWhen != "input.mode == 'type_side'" {
		t.Fatalf("required_when типа должен победить (тип-wins), got %q", resolved["target"].RequiredWhen)
	}
}
