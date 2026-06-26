package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Тесты переиспользуемых именованных типов input-схемы (types.yml + $type-ref).
// Покрытие: парсинг каталога; резолв $type-поля + items:{$type}; вложенность
// тип→тип; cycle (НЕ зависание); unknown; duplicate; ref_conflict ($type+inline);
// back-compat (схемы без $type).

// --- $type как самостоятельное поле в input: (schema-валидация узла) ---

func TestTypeRef_BareField_NoTypeRequired(t *testing.T) {
	// Узел с $type не обязан объявлять type: — он ссылка, форму даёт тип.
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

// --- ref_conflict: $type ВМЕСТЕ с inline type/properties/items ---

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

// items:{$type} — НЕ конфликт: items-ссылка живёт на родителе-array.
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

// $type невалидной формы (mapping / плохое имя).
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

// --- ParseTypeCatalog: парсинг types.yml ---

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

// --- вложенность тип→тип (резолв $type внутри типа) ---

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

// --- input_type_cycle: НЕ зависание, а ошибка ---

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

// --- ResolveTypeRefs: резолв input: сценария по каталогу ---

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

// --- back-compat: схемы без $type не ломаются ---

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

// Резолв НЕ мутирует каталог: общий тип, использованный дважды, не вызывает
// ложного цикла и не «портится» между потребителями.
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

// TestResolveTypeRefs_DeepPlainObject_NoFalseCycle — регрессия MINOR 2: глубоко
// вложенный ОБЫЧНЫЙ object (структурный спуск properties глубже typeRefResolveLimit)
// БЕЗ единой $type-ссылки НЕ должен ложно давать input_type_cycle. Лимит считает
// только type-ref-хопы (свойство ref-графа), структурный спуск не лимитируется.
func TestResolveTypeRefs_DeepPlainObject_NoFalseCycle(t *testing.T) {
	// Строим цепочку object→properties→object… глубиной заметно больше лимита.
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

// TestParseTypeCatalog_NameNotPascalCase — регрессия MINOR 1: имя типа со
// `snake_case`/underscore (мягче PascalCase-спеки) отвергается
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

// TestParseTypeCatalog_NamePascalCase_OK — PascalCase-имя проходит без
// name-ошибки (граница MINOR 1: ужесточение не задевает валидные имена).
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
