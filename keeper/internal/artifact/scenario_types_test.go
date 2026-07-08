package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTypesCatalog — кладёт types.yml в корень тестового serviceRoot.
func writeTypesCatalog(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, typesCatalogFile), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile types.yml: %v", err)
	}
}

// DTO резолвит самостоятельную $type-ссылку: узел заменяется телом типа +
// аннотация x-type.
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
		t.Fatalf("target не map: %#v", got[0].InputSchema["target"])
	}
	// $type заменён телом типа.
	if _, stillRef := target[typeRefKey]; stillRef {
		t.Fatalf("$type должен быть резолвнут, остался сырым: %#v", target)
	}
	if target["type"] != "object" {
		t.Fatalf("target.type = %v, ожидался object", target["type"])
	}
	props, ok := target["properties"].(map[string]any)
	if !ok || props["host"] == nil {
		t.Fatalf("target.properties.host отсутствует: %#v", target)
	}
	// x-type аннотация присутствует.
	if target[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("x-type = %v, ожидался Endpoint", target[typeAnnotationKey])
	}
}

// DTO резолвит items:{$type} (массив элементов-типа).
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
		t.Fatalf("nodes.items не map: %#v", nodes["items"])
	}
	if _, stillRef := items[typeRefKey]; stillRef {
		t.Fatalf("items.$type должен быть резолвнут: %#v", items)
	}
	if items["type"] != "object" || items[typeAnnotationKey] != "Node" {
		t.Fatalf("items должен нести форму Node + x-type=Node: %#v", items)
	}
}

// Вложенность тип→тип: тело типа, ссылающегося на другой тип, тоже резолвится.
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
		t.Fatalf("cluster.properties.primary не map: %#v", props["primary"])
	}
	if _, stillRef := primary[typeRefKey]; stillRef {
		t.Fatalf("вложенный primary.$type должен быть резолвнут: %#v", primary)
	}
	if primary[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("primary.x-type = %v, ожидался Endpoint", primary[typeAnnotationKey])
	}
}

// Цикл в каталоге → DTO НЕ зависает (узел остаётся как есть, без бесконечной
// рекурсии). Полную ошибку cycle поднимает soul-lint.
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

	// Не зависает — если бы рекурсия была бесконечной, тест бы не завершился.
	got, err := ListScenarios(root, discardLogger())
	if err != nil {
		t.Fatalf("ListScenarios: %v", err)
	}
	if got[0].InputSchema["root"] == nil {
		t.Fatalf("root должен присутствовать (best-effort при цикле)")
	}
}

// Неизвестный тип → узел остаётся сырым (best-effort; soul-lint поднимет unknown).
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
		t.Fatalf("неизвестный тип — узел остаётся сырым с $type, got %#v", x)
	}
	if _, annotated := x[typeAnnotationKey]; annotated {
		t.Fatalf("неизвестный тип не должен получить x-type: %#v", x)
	}
}

// Back-compat: сценарий без $type и сервис без types.yml не ломаются.
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
		t.Fatalf("port должен пройти насквозь: %#v", port)
	}
	opts := got[0].InputSchema["opts"].(map[string]any)
	props := opts["properties"].(map[string]any)
	if props["verbose"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("вложенный opts.verbose должен пройти насквозь: %#v", props)
	}
}

// Общий тип в двух местах не «портится» между потребителями (clone, не shared
// pointer): аннотация x-type у обоих корректна.
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
		t.Fatalf("оба использования Endpoint должны нести x-type=Endpoint: a=%#v b.items=%#v", a, bItems)
	}
}

// TestListScenarios_TypeRefKeepsRequiredChildren — NIM-72: при резолве $type для
// DTO object-level `required: [name, perms]` тела типа (массив детей) НЕ
// перезаписывается булевым `required: true` узла-ссылки (клоббер списка, на
// который завязан UI). Presentational-ключ description переносится.
func TestListScenarios_TypeRefKeepsRequiredChildren(t *testing.T) {
	root := t.TempDir()
	writeTypesCatalog(t, root, `types:
  AclUser:
    type: object
    required: [name, perms]
    properties:
      name:
        type: string
      perms:
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
		t.Fatalf("user не map: %#v", got[0].InputSchema["user"])
	}
	// $type резолвнут + x-type аннотация.
	if _, stillRef := user[typeRefKey]; stillRef {
		t.Fatalf("$type должен быть резолвнут: %#v", user)
	}
	if user[typeAnnotationKey] != "AclUser" {
		t.Fatalf("x-type = %v, ожидался AclUser", user[typeAnnotationKey])
	}
	// object-level required-массив НЕ перезаписан булевым true узла-ссылки.
	req, ok := user["required"].([]any)
	if !ok {
		t.Fatalf("required должен остаться массивом [name perms], got %#v (%T)", user["required"], user["required"])
	}
	if len(req) != 2 || req[0] != "name" || req[1] != "perms" {
		t.Fatalf("required-массив искажён: %#v", req)
	}
	// properties типа сохранены.
	if _, ok := user["properties"].(map[string]any); !ok {
		t.Fatalf("properties должны сохраниться: %#v", user)
	}
	// presentational description узла-ссылки перенесён.
	if user["description"] != "ACL user" {
		t.Fatalf("description = %v, ожидался перенос с узла-ссылки", user["description"])
	}
}

// TestListScenarios_TypeRefCarriesRequiredWhen — NIM-72: required_when узла-ссылки
// переносится в DTO (отдельный ключ, не конфликтует с required-массивом типа).
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
		t.Fatalf("required_when узла-ссылки должен перенестись в DTO, got %v", target["required_when"])
	}
	if target[typeAnnotationKey] != "Endpoint" {
		t.Fatalf("x-type = %v, ожидался Endpoint", target[typeAnnotationKey])
	}
}
