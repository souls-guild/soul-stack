// Доказательный pilot-гейт агрегатора единой huma-OpenAPI-спеки (Teardown T4a).
//
// Тесты доказывают четыре свойства собранной спеки ([buildFullOpenAPISpec] /
// [HumaFullSpecYAML]):
//
//	(а) НЕТ дубль (path+method) после префиксования — все операции уникальны по
//	    полному пути (иначе buildFullOpenAPISpec вернул бы pathMethodCollisionError).
//	(б) НЕТ schema-merge-коллизий: одноимённые схемы из разных доменов имеют
//	    идентичное тело (HumaProblemError ×20, Voyage/VoyageSummary/VoyageTarget ×2 —
//	    дедуплицируются безопасно). Различие тел → schemaCollisionError → needs_architect.
//	(в) собранная спека ВАЛИДНА как OpenAPI 3.1: openapi==3.1.0 + непустые paths +
//	    components.schemas, каждый path-item несёт ≥1 HTTP-метод-операцию, YAML
//	    парсится без ошибок.
//	(г) спека СОДЕРЖИТ все ожидаемые роуты — множество (path+method) собранной спеки
//	    совпадает с реальными chi-роутами Operator API (chi.Walk(buildRouter) ∪
//	    opt-in-роуты из pathAllowlist; health/meta вне /v1 исключены). Drift-guard:
//	    агрегатор не забыл домен.
package api

import (
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	yaml "gopkg.in/yaml.v3"
)

// TestFullSpec_NoPathMethodCollision — гейт (а). buildFullOpenAPISpec уже падает с
// pathMethodCollisionError при дубле; здесь убеждаемся, что сборка проходит и число
// операций соответствует числу зарегистрированных (без молчаливой потери).
func TestFullSpec_NoPathMethodCollision(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Пересчёт операций: должно быть >100 (контракт ~120). Любой дубль path+method
	// был бы пойман внутри buildFullOpenAPISpec — здесь дополнительно фиксируем
	// порядок величины, чтобы тест ловил случайную усушку набора групп.
	ops := 0
	for _, item := range spec.Paths {
		ops += len(pathItemOps(item))
	}
	if ops < 100 {
		t.Fatalf("собрано %d операций — ожидалось ~120; возможна усохшая регистрация групп", ops)
	}
	t.Logf("гейт (а): %d путей, %d операций, дублей path+method нет", len(spec.Paths), ops)
}

// TestFullSpec_NoSchemaCollision — гейт (б), главный unknown. buildFullOpenAPISpec
// возвращает schemaCollisionError при одноимённых схемах с разным телом. Тест явно
// перебирает ВСЕ группы и собирает имя→{домен→тело}, доказывая, что у любого дубль-
// имени тело идентично (а значит дедуп безопасен и needs_architect не нужен).
func TestFullSpec_NoSchemaCollision(t *testing.T) {
	// Прямая сборка обязана пройти без коллизии.
	if _, err := buildFullOpenAPISpec(); err != nil {
		t.Fatalf("schema-merge коллизия (гейт б): %v\n→ needs_architect: как namespace-ить одноимённые схемы разных доменов", err)
	}

	// Независимый перебор групп: для каждого имени схемы собираем множество
	// различных тел. >1 различное тело под одним именем = коллизия (быть не
	// должно). Дубль-имена с идентичным телом перечисляем для протокола.
	installHumaErrorOverride()
	bodies := map[string]map[string]struct{}{} // name -> set(body)
	dupNames := map[string]int{}               // name -> сколько групп его дали
	for i, g := range fullSpecGroups() {
		api := newHumaCadenceAPI(chi.NewRouter())
		if err := g.register(api); err != nil {
			t.Fatalf("группа #%d register: %v", i, err)
		}
		for name, sch := range api.OpenAPI().Components.Schemas.Map() {
			body, err := yamlMarshalSchema(sch)
			if err != nil {
				t.Fatal(err)
			}
			if bodies[name] == nil {
				bodies[name] = map[string]struct{}{}
			}
			bodies[name][body] = struct{}{}
			dupNames[name]++
		}
	}

	var collided []string
	for name, set := range bodies {
		if len(set) > 1 {
			collided = append(collided, name)
		}
	}
	if len(collided) > 0 {
		sort.Strings(collided)
		t.Fatalf("гейт (б) ПРОВАЛЕН: схемы с одним именем но РАЗНЫМ телом между доменами: %v\n→ needs_architect", collided)
	}

	var shared []string
	for name, n := range dupNames {
		if n > 1 {
			shared = append(shared, name)
		}
	}
	sort.Strings(shared)
	t.Logf("гейт (б): 0 коллизий; одноимённые схемы с идентичным телом (безопасный дедуп): %v", shared)
}

// TestFullSpec_ValidOpenAPI31 — гейт (в). Парсим YAML собранной спеки и проверяем
// обязательные поля 3.1: openapi==3.1.0, непустые paths + components.schemas, каждый
// path-item имеет ≥1 HTTP-метод-операцию, $ref-ы схем разрешимы внутри документа.
func TestFullSpec_ValidOpenAPI31(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("собранная спека не парсится как YAML: %v", err)
	}

	if v, _ := doc["openapi"].(string); v != "3.1.0" {
		t.Errorf("openapi=%q, ожидалось 3.1.0", v)
	}
	if _, ok := doc["info"]; !ok {
		t.Error("обязательное поле info отсутствует")
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("paths пуст или не map — не валидная 3.1-спека")
	}
	comp, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatal("components отсутствует")
	}
	schemas, ok := comp["schemas"].(map[string]any)
	if !ok || len(schemas) == 0 {
		t.Fatal("components.schemas пуст — операции без тел?")
	}

	validMethods := map[string]struct{}{
		"get": {}, "put": {}, "post": {}, "delete": {},
		"options": {}, "head": {}, "patch": {}, "trace": {},
	}
	for p, item := range paths {
		pi, ok := item.(map[string]any)
		if !ok {
			t.Errorf("path-item %q не map", p)
			continue
		}
		hasOp := false
		for k := range pi {
			if _, isMethod := validMethods[strings.ToLower(k)]; isMethod {
				hasOp = true
				break
			}
		}
		if !hasOp {
			t.Errorf("path %q без единой HTTP-операции", p)
		}
	}

	// $ref-целостность: каждый #/components/schemas/<Name> в документе разрешим.
	refs := collectSchemaRefs(doc)
	for ref := range refs {
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		if name == ref {
			continue // не локальный schemas-ref
		}
		if _, ok := schemas[name]; !ok {
			t.Errorf("$ref %q не разрешается — схема %q отсутствует в components.schemas (битый merge)", ref, name)
		}
	}

	t.Logf("гейт (в): валидная 3.1-спека — %d путей, %d схем, все $ref разрешимы", len(paths), len(schemas))
}

// TestFullSpec_CoversAllRoutes — гейт (г), drift-guard. Множество (method, path)
// собранной спеки обязано совпасть с реальными роутами Operator API:
// chi.Walk(buildRouter) даёт роуты non-opt-in доменов; opt-in домены в drift-test-
// router-е = nil, их роуты живут в pathAllowlist — добавляем оттуда. Health/meta
// (/healthz, /readyz, /openapi.yaml, /openapi.json) — вне /v1, не huma-домены, исключаются.
func TestFullSpec_CoversAllRoutes(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	specSet := map[route]struct{}{}
	for path, item := range spec.Paths {
		for method := range pathItemOps(item) {
			specSet[route{method: method, path: normalizePath(path)}] = struct{}{}
		}
	}

	// Реальные роуты: non-opt-in из chi.Walk + opt-in из pathAllowlist.
	realSet := map[route]struct{}{}
	for r := range collectRoutes(t) {
		if strings.HasSuffix(r.path, wildcardSuffix) {
			continue // chi catch-all 404
		}
		if isHealthMetaRoute(r) {
			continue // вне /v1, не huma-домен
		}
		realSet[r] = struct{}{}
	}
	for r := range pathAllowlist {
		realSet[r] = struct{}{}
	}

	var inSpecNotReal, inRealNotSpec []string
	for r := range specSet {
		if _, ok := realSet[r]; !ok {
			inSpecNotReal = append(inSpecNotReal, r.String())
		}
	}
	for r := range realSet {
		if _, ok := specSet[r]; !ok {
			inRealNotSpec = append(inRealNotSpec, r.String())
		}
	}
	sort.Strings(inSpecNotReal)
	sort.Strings(inRealNotSpec)

	if len(inRealNotSpec) > 0 {
		t.Errorf("РОУТ ЕСТЬ, в собранной спеке НЕТ (агрегатор забыл домен/группу) — %d:\n  %s",
			len(inRealNotSpec), strings.Join(inRealNotSpec, "\n  "))
	}
	if len(inSpecNotReal) > 0 {
		t.Errorf("В СПЕКЕ ЕСТЬ, реального роута НЕТ (лишняя/неверно-префиксованная операция) — %d:\n  %s",
			len(inSpecNotReal), strings.Join(inSpecNotReal, "\n  "))
	}

	t.Logf("гейт (г): спека и роуты совпадают — %d роутов покрыто", len(specSet))
}

// TestFullSpec_NoTechnicalSchemaNames — ФИНАЛЬНЫЙ СКВОЗНОЙ ГЕЙТ чистоты спеки (батч N6,
// перед T4c served-switch). Собирает агрегатор-спеку и проверяет, что НИ ОДНО имя схемы НЕ
// несёт технического/дрейф-маркера:
//
//	(1) подстрочные маркеры huma-Go-имён и envelope-дрейфов: "HumaBody" (request/reply-Go-тип
//	    не выровнен), "Response" (reply-дрейф вместо контрактного Reply), "PagedResponse"
//	    (неаласенный generic envelope), "DTO" (handler-DTO-имя);
//	(2) oapi-капитализационные дрейфы (генератор oapi эмитит ALLCAPS-аббревиатуры —
//	    SSHTargetReply вместо SshTargetReply и т.п.).
//
// ЕДИНСТВЕННОЕ исключение — "HumaProblemError" (huma RFC 7807 error-wrapper, не доменная
// схема, имя задаётся huma-фреймворком). Любое иное имя с маркером = незавершённое
// выравнивание → тест краснит со списком оставшихся.
func TestFullSpec_NoTechnicalSchemaNames(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	// Подстрочные маркеры технических/дрейф-имён.
	substrMarkers := []string{"HumaBody", "Response", "PagedResponse", "DTO"}
	// ALLCAPS-аббревиатуры oapi-генератора (капитализационный дрейф контрактного CamelCase).
	capsMarkers := []string{"SSH", "HTTP", "URL", "JSON", "ACL", "DNS", "TLS", "TTL"}

	const allowed = "HumaProblemError" // RFC 7807 wrapper huma — разрешён

	var offenders []string
	for name := range spec.Components.Schemas.Map() {
		if name == allowed {
			continue
		}
		for _, m := range substrMarkers {
			if strings.Contains(name, m) {
				offenders = append(offenders, name+" (маркер "+m+")")
			}
		}
		for _, c := range capsMarkers {
			if strings.Contains(name, c) {
				offenders = append(offenders, name+" (ALLCAPS "+c+")")
			}
		}
	}
	sort.Strings(offenders)

	if len(offenders) > 0 {
		t.Fatalf("ФИНАЛЬНЫЙ ГЕЙТ ПРОВАЛЕН: в спеке остались технические/дрейф-имена схем (%d) — выравнивание не завершено:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}

	t.Logf("финальный гейт: 0 техимён в спеке (%d схем; единственное исключение — %s)",
		len(spec.Components.Schemas.Map()), allowed)
}

// isHealthMetaRoute — health/meta/docs-эндпоинты вне /v1 (не входят в huma-домены,
// в агрегат-спеке отсутствуют). /docs/assets/* оканчивается на /* → отсеивается
// раньше как wildcard.
func isHealthMetaRoute(r route) bool {
	switch r.path {
	case "/healthz", "/readyz", "/openapi.yaml", "/openapi.json", "/docs":
		return true
	}
	return false
}

// collectSchemaRefs рекурсивно собирает все строковые значения ключа "$ref" в
// произвольном YAML-дереве (для проверки $ref-целостности гейта в).
func collectSchemaRefs(node any) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				if k == "$ref" {
					if s, ok := child.(string); ok {
						out[s] = struct{}{}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(node)
	return out
}
