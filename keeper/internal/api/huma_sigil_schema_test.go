// Доказательный гейт выравнивания имён SIGIL + SIGIL-KEY request-схем под committed-
// рукопись (тираж-батч N4, по эталону huma_operator_schema_test.go). Собирает
// агрегированную huma-спеку (HumaFullSpecYAML) и проверяет контрактные имена request-тел
// + отсутствие технических huma-Go-имён.
//
// МЕХАНИЗМЫ (сверены с рукописью):
//   - SIGIL REQUEST-RENAME: sigilAllowHumaBody → PluginSigilAllowRequest (:5256, class C
//     input-only; рукопись ссылается на него из requestBody POST /v1/plugins/sigils :2406).
//     ВНИМАНИЕ: контрактное имя — PluginSigilAllowRequest (Plugin-prefix), НЕ SigilAllowRequest.
//   - SIGIL-KEY REQUEST-RENAME: sigilKeyIntroduceHumaBody → SigilKeyIntroduceRequest (:5619,
//     class C input-only; ref из POST /v1/sigil/keys :2484).
//   - ENVELOPE/ENUM/NESTED: не применяются. Reply-схемы (PluginSigilAllowReply/
//     PluginSigilListReply/SigilKeyIntroduceReply/SigilKeyListReply) УЖЕ named oapi-типы,
//     контрактны сами; рукопись standalone enum/shared-nested для sigil НЕ объявляет.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// sigilContractSchemas — request-имена sigil + sigil-key доменов ровно как в committed-
// рукописи. Reply/view-схемы (named oapi) включены как контроль присутствия.
var sigilContractSchemas = []string{
	"PluginSigilAllowRequest",
	"SigilKeyIntroduceRequest",
	"PluginSigilAllowReply",
	"PluginSigilListReply",
	"SigilKeyIntroduceReply",
	"SigilKeyListReply",
}

// sigilForbiddenSchemas — технические huma-Go-имена старых input-тел.
var sigilForbiddenSchemas = []string{
	"SigilAllowHumaBody",
	"SigilKeyIntroduceHumaBody",
	// Контрактное имя — PluginSigilAllowRequest; SigilAllowRequest рукопись НЕ объявляет
	// (страховка от ошибочного rename под имя из explore-карты).
	"SigilAllowRequest",
}

// TestSchemaNames_Sigil — гейт N4. Контрактные имена присутствуют, технические — нет.
func TestSchemaNames_Sigil(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range sigilContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("контрактная схема %q ОТСУТСТВУЕТ в components/schemas (имя не выровнено)", name)
		}
	}
	for _, name := range sigilForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("техническое huma-имя %q ПРИСУТСТВУЕТ в спеке — имя не выровнено под контракт", name)
		}
	}
}

// TestSchemaNames_SigilRequestShapes — формы request-тел сверены с рукописью:
// PluginSigilAllowRequest.required=[namespace,name,ref] (:5275). SigilKeyIntroduceRequest —
// все поля опциональны (make_primary; required-блока нет :5619).
func TestSchemaNames_SigilRequestShapes(t *testing.T) {
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("спека не парсится: %v", err)
	}
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	allow, _ := schemas["PluginSigilAllowRequest"].(map[string]any)
	if allow == nil {
		t.Fatal("PluginSigilAllowRequest отсутствует")
	}
	assertRequiredExactly(t, allow, "PluginSigilAllowRequest", "namespace", "name", "ref")

	intro, _ := schemas["SigilKeyIntroduceRequest"].(map[string]any)
	if intro == nil {
		t.Fatal("SigilKeyIntroduceRequest отсутствует")
	}
	if req, ok := intro["required"]; ok {
		t.Errorf("SigilKeyIntroduceRequest.required=%v — рукопись :5619 НЕ объявляет required (make_primary опционален)", req)
	}
}
