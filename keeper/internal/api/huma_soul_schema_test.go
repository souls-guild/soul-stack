// Доказательный гейт выравнивания имён SOUL-схем (+ errand-exec) под committed-рукопись
// (тираж-батч N5, по эталонам huma_voyage_schema_test.go / huma_incarnation_schema_test.go).
// Собирает агрегированную huma-спеку (HumaFullSpecYAML) и проверяет, что схемы soul-домена
// названы ТОЧНО как контракт (docs/keeper/openapi.yaml), технические huma-Go-имена отсутствуют,
// enum SoulStatus/SoulTransport вынесены как named-$ref, nested SoulSshTarget сведён (КЛАСС A),
// а list-envelope SoulListReply несёт КОНТРАКТНУЮ CURSOR-форму (6 полей).
//
// МЕХАНИЗМЫ для soul (сверены с рукописью):
//   - REQUEST-RENAME: soulCreateHumaBody → SoulCreateRequest, soulCovenAssignHumaBody →
//     SoulCovenAssignRequest, soulCovenAssignSelectorBody → SoulCovenAssignSelector (input-only
//     КЛАСС C), errandExecHumaBody → ErrandRunRequest (request-схема exec, рукопись :1668).
//   - ENUM-ALIAS: SoulStatus + SoulTransport объявлены standalone в рукописи (:4198/:4207) с
//     $ref → вынесены как named-схемы (huma_soul_status.go).
//   - NESTED КЛАСС A: SoulSshTarget — единый тип input(PUT body)↔output(SoulSshTargetReply.
//     ssh_target через alias SoulSSHTarget→SoulSshTarget). required:[ssh_port,ssh_user,
//     soul_path] (рукопись :6394).
//   - ENVELOPE CURSOR: SoulListReply — 6 полей (items/offset/limit/total + next_cursor +
//     total_approximate), НЕ 4-поля-форма incarnation (huma_soul_envelope.go).
//   - REPLY-RENAME (батч N6): SoulCovenAssignResponse→SoulCovenAssignReply (:7140, alias
//     handler-wire-body → api-named-struct) + SoulSSHTargetReply→SoulSshTargetReply (:6399,
//     класс A alias генерёного SoulSSHTargetReply → api-named-struct). Оба — только
//     OpenAPI-имя/форма, wire (custom MarshalJSON / тот же oapi-тип) не меняется.
package api

import (
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// soulContractSchemas — request/selector/view/envelope/enum-имена soul-домена + errand-exec
// request ровно как в committed-рукописи. Все обязаны присутствовать в собранной спеке.
var soulContractSchemas = []string{
	"SoulCreateRequest",
	"SoulCovenAssignRequest",
	"SoulCovenAssignSelector",
	"SoulCovenAssignReply",
	"SoulSshTarget",
	"SoulSshTargetReply",
	"SoulprintReadReply",
	"SoulListReply",
	"SoulListEntry",
	"SoulStatus",
	"SoulTransport",
	"ErrandRunRequest",
	// Class C доэмиссия typed-soulprint (ADR-018): typed_facts=json.RawMessage не
	// выводил вложенные типы reflect-обходом → SoulprintFacts + 6 под-схем отсутствовали.
	// Alias на typed *SoulprintFacts (huma_soul_soulprint.go) эмитит их. ★ Имя
	// SoulprintCpuFacts (НЕ дрейф SoulprintCPUFacts от oapi-капитализации).
	"SoulprintFacts",
	"SoulprintOsFacts",
	"SoulprintKernelFacts",
	"SoulprintCpuFacts",
	"SoulprintMemoryFacts",
	"SoulprintNetworkFacts",
	"SoulprintNetworkInterface",
}

// soulForbiddenSchemas — технические huma-Go-имена, которые DefaultSchemaNamer дал БЫ из старых
// имён структур. Ни одно не должно остаться после выравнивания.
var soulForbiddenSchemas = []string{
	"SoulCreateHumaBody",
	"SoulCovenAssignHumaBody",
	"SoulCovenAssignSelectorBody",
	"SoulSshTargetHumaBody",
	"ErrandExecHumaBody",
	// Generic-имя неаласенного PagedResponse[SoulListEntry] — envelope-alias обязан его
	// вытеснить контрактным SoulListReply.
	"PagedResponseSoulListEntry",
	// REPLY-RENAME (батч N6): дрейф-имена reply-схем, вытесненные контрактными.
	"SoulCovenAssignResponse", // → SoulCovenAssignReply
	"SoulSSHTargetReply",      // капитализационный дрейф oapi-генератора → SoulSshTargetReply
	"SoulprintResponse",       // → SoulprintReadReply
	// Class C: дрейф-имя CPU-под-факта от oapi-капитализации аббревиатуры (CPU), которое
	// DefaultSchemaNamer дал БЫ из SoulprintCPUFacts — alias обязан вытеснить его
	// контрактным SoulprintCpuFacts.
	"SoulprintCPUFacts",
}

// TestSchemaNames_Soul — гейт N5. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Soul(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range soulContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range soulForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_SoulStatusEnum — гейт N5 (ENUM). SoulStatus и SoulTransport вынесены как
// named-схемы (string + enum), а status/transport-поля reply-структур ссылаются на них через
// $ref (не инлайн `type: string`). Состав enum — доменная истина (6 статусов / 2 транспорта).
func TestSchemaNames_SoulStatusEnum(t *testing.T) {
	y, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	assertStringEnum(t, schemas, "SoulStatus",
		"pending", "connected", "disconnected", "revoked", "expired", "destroyed")
	assertStringEnum(t, schemas, "SoulTransport", "agent", "ssh")

	if !strings.Contains(y, "#/components/schemas/SoulStatus") {
		t.Error("ни одно поле не ссылается на SoulStatus через $ref — статус остался инлайн")
	}
	if !strings.Contains(y, "#/components/schemas/SoulTransport") {
		t.Error("ни одно поле не ссылается на SoulTransport через $ref — transport остался инлайн")
	}
}

// TestSchemaNames_SoulSshTargetNested — гейт N5 (NESTED КЛАСС A). SoulSshTarget — единая схема
// input↔output: PUT ssh-target body И SoulSshTargetReply.ssh_target ссылаются на неё. Форма
// сверена с рукописью :6394 (required:[ssh_port,ssh_user,soul_path]; 4 поля). Мутация (убрать
// alias → output тянет SoulSSHTarget / сменить required-набор) краснит.
func TestSchemaNames_SoulSshTargetNested(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	const targetRef = "#/components/schemas/SoulSshTarget"

	// output SoulSshTargetReply.ssh_target → единая SoulSshTarget (через alias). Имя output-
	// reply-схемы выровнено на контрактное SoulSshTargetReply (батч N6: класс A alias генерёного
	// SoulSSHTargetReply → api-named-struct soulSshTargetReply). Капитализационный дрейф
	// SoulSSHTargetReply вытеснен (в forbidden-наборе).
	if got := propRef(t, schemas, "SoulSshTargetReply", "ssh_target"); got != targetRef {
		t.Errorf("SoulSshTargetReply.ssh_target → %q, ожидался %q (output не сведён на единую SoulSshTarget — alias не сработал)", got, targetRef)
	}

	// Форма SoulSshTarget сверена с рукописью :6394.
	tgt, _ := schemas["SoulSshTarget"].(map[string]any)
	if tgt == nil {
		t.Fatal("SoulSshTarget отсутствует в components.schemas")
	}
	assertRequiredExactly(t, tgt, "SoulSshTarget", "ssh_port", "ssh_user", "soul_path")
	assertProps(t, tgt, "SoulSshTarget", "ssh_port", "ssh_user", "soul_path", "ssh_provider")
}

// TestSchemaNames_SoulListEnvelope — гейт N5 (ENVELOPE CURSOR). ★ soul — единственный cursor-
// домен: SoulListReply несёт РОВНО 6 полей (items/offset/limit/total + next_cursor +
// total_approximate), НЕ 4-поля-форму incarnation. items.$ref на контрактный element
// SoulListEntry; offset/limit/total — int32; required:[items,offset,limit,total]. Мутация
// (4-поля-форма / убрать cursor-поля / неверный $ref) краснит.
func TestSchemaNames_SoulListEnvelope(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	env, ok := schemas["SoulListReply"].(map[string]any)
	if !ok {
		t.Fatal("SoulListReply отсутствует в components/schemas — envelope-alias не сработал")
	}
	props, _ := env["properties"].(map[string]any)
	if props == nil {
		t.Fatal("SoulListReply без properties")
	}

	// ★ Ровно 6 полей — cursor-поля ОБЯЗАНЫ присутствовать (отличие от 4-поля incarnation).
	wantFields := []string{"items", "offset", "limit", "total", "next_cursor", "total_approximate"}
	if len(props) != len(wantFields) {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("SoulListReply несёт %d полей %v, ожидалось ровно 6 (cursor-форма: items/offset/limit/total/next_cursor/total_approximate)", len(props), got)
	}
	for _, f := range wantFields {
		if _, ok := props[f]; !ok {
			t.Errorf("SoulListReply не содержит контрактного поля %q", f)
		}
	}

	// offset/limit/total — int32.
	for _, f := range []string{"offset", "limit", "total"} {
		fp, _ := props[f].(map[string]any)
		if fp == nil {
			continue
		}
		if !schemaTypeHas(fp["type"], "integer") {
			t.Errorf("SoulListReply.%s.type=%v, ожидалось integer", f, fp["type"])
		}
		if format, _ := fp["format"].(string); format != "int32" {
			t.Errorf("SoulListReply.%s.format=%q, ожидалось int32", f, format)
		}
	}

	// next_cursor — string; total_approximate — boolean.
	if nc, _ := props["next_cursor"].(map[string]any); nc != nil && !schemaTypeHas(nc["type"], "string") {
		t.Errorf("SoulListReply.next_cursor.type=%v, ожидалось string", nc["type"])
	}
	if ta, _ := props["total_approximate"].(map[string]any); ta != nil && !schemaTypeHas(ta["type"], "boolean") {
		t.Errorf("SoulListReply.total_approximate.type=%v, ожидалось boolean", ta["type"])
	}

	// items — array с $ref на контрактный element SoulListEntry.
	items, _ := props["items"].(map[string]any)
	if items == nil {
		t.Fatal("SoulListReply.items отсутствует")
	}
	if !schemaTypeHas(items["type"], "array") {
		t.Errorf("SoulListReply.items.type=%v, ожидалось array", items["type"])
	}
	elem, _ := items["items"].(map[string]any)
	if elem == nil {
		t.Fatal("SoulListReply.items.items отсутствует (element-схема)")
	}
	const wantRef = "#/components/schemas/SoulListEntry"
	if ref, _ := elem["$ref"].(string); ref != wantRef {
		t.Errorf("SoulListReply.items.items.$ref=%q, ожидалось %q", ref, wantRef)
	}
}

// TestSchemaNames_SoulprintFactsTyped — гейт Class C (typed-soulprint). Доказывает, что
// SoulprintReadReply.typed_facts ссылается на $ref SoulprintFacts (а НЕ free-form object),
// SoulprintFacts ссылается на 6 под-схем через $ref, а CPU-под-факт назван контрактно
// (SoulprintCpuFacts, НЕ дрейф SoulprintCPUFacts). Мутация (убрать registerSoulprintFacts →
// typed_facts становится free-form object без $ref, под-схемы исчезают) краснит.
func TestSchemaNames_SoulprintFactsTyped(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	// typed_facts → $ref SoulprintFacts (типизирован, не free-form object).
	if got := propRef(t, schemas, "SoulprintReadReply", "typed_facts"); got != "#/components/schemas/SoulprintFacts" {
		t.Errorf("SoulprintReadReply.typed_facts.$ref=%q, ожидался #/components/schemas/SoulprintFacts (alias не сработал — typed_facts остался free-form object)", got)
	}

	// SoulprintFacts.{os,kernel,cpu,memory,network} → $ref на контрактные под-схемы.
	wantFactRefs := map[string]string{
		"os":      "#/components/schemas/SoulprintOsFacts",
		"kernel":  "#/components/schemas/SoulprintKernelFacts",
		"cpu":     "#/components/schemas/SoulprintCpuFacts",
		"memory":  "#/components/schemas/SoulprintMemoryFacts",
		"network": "#/components/schemas/SoulprintNetworkFacts",
	}
	for field, want := range wantFactRefs {
		if got := propRef(t, schemas, "SoulprintFacts", field); got != want {
			t.Errorf("SoulprintFacts.%s.$ref=%q, ожидался %q", field, got, want)
		}
	}

	// SoulprintNetworkFacts.interfaces — array $ref на контрактный SoulprintNetworkInterface.
	net, _ := schemas["SoulprintNetworkFacts"].(map[string]any)
	if net == nil {
		t.Fatal("SoulprintNetworkFacts отсутствует в components/schemas")
	}
	netProps, _ := net["properties"].(map[string]any)
	ifaces, _ := netProps["interfaces"].(map[string]any)
	if ifaces == nil {
		t.Fatal("SoulprintNetworkFacts.interfaces отсутствует")
	}
	elem, _ := ifaces["items"].(map[string]any)
	if ref, _ := elem["$ref"].(string); ref != "#/components/schemas/SoulprintNetworkInterface" {
		t.Errorf("SoulprintNetworkFacts.interfaces.items.$ref=%q, ожидался #/components/schemas/SoulprintNetworkInterface", ref)
	}
}

// loadFullSpecDoc собирает агрегатор-спеку и парсит её в map (для assert-ов по форме схем).
func loadFullSpecDoc(t *testing.T) (string, map[string]any) {
	t.Helper()
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}
	return y, doc
}

// assertStringEnum проверяет, что схема name — string c enum, содержащим ровно want-значения.
func assertStringEnum(t *testing.T, schemas map[string]any, name string, want ...string) {
	t.Helper()
	sch, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("%s не вынесен как named-схема в components/schemas", name)
	}
	if typ, _ := sch["type"].(string); typ != "string" {
		t.Errorf("%s.type=%q, ожидалось string", name, typ)
	}
	rawEnum, ok := sch["enum"].([]any)
	if !ok || len(rawEnum) == 0 {
		t.Fatalf("%s без enum — выноса как enum не произошло", name)
	}
	got := map[string]struct{}{}
	for _, v := range rawEnum {
		if s, ok := v.(string); ok {
			got[s] = struct{}{}
		}
	}
	if len(got) != len(want) {
		var have []string
		for k := range got {
			have = append(have, k)
		}
		sort.Strings(have)
		t.Errorf("enum %s = %v, ожидалось %v", name, have, want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("enum %s не содержит %q", name, w)
		}
	}
}
