// Proof gate for aligning SIGIL + SIGIL-KEY request-schema names with the committed
// hand-written spec (rollout batch N4, after huma_operator_schema_test.go). Assembles the
// aggregated huma spec (HumaFullSpecYAML) and checks the contract names of request bodies
// + absence of technical huma-Go names.
//
// MECHANISMS (checked against the hand-written spec):
//   - SIGIL REQUEST-RENAME: sigilAllowHumaBody → PluginSigilAllowRequest (:5256, class C
//     input-only; the spec references it from requestBody POST /v1/plugins/sigils :2406).
//     NOTE: the contract name is PluginSigilAllowRequest (Plugin-prefix), NOT SigilAllowRequest.
//   - SIGIL-KEY REQUEST-RENAME: sigilKeyIntroduceHumaBody → SigilKeyIntroduceRequest (:5619,
//     class C input-only; ref from POST /v1/sigil/keys :2484).
//   - ENVELOPE/ENUM/NESTED: not applicable. Reply schemas (PluginSigilAllowReply/
//     PluginSigilListReply/SigilKeyIntroduceReply/SigilKeyListReply) are ALREADY named oapi types,
//     contract-stable themselves; the spec declares no standalone enum/shared-nested for sigil.
package api

import (
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// sigilContractSchemas — request names of the sigil + sigil-key domains exactly as in the
// committed hand-written spec. Reply/view schemas (named oapi) included as a presence check.
var sigilContractSchemas = []string{
	"PluginSigilAllowRequest",
	"SigilKeyIntroduceRequest",
	"PluginSigilAllowReply",
	"PluginSigilListReply",
	"SigilKeyIntroduceReply",
	"SigilKeyListReply",
}

// sigilForbiddenSchemas — technical huma-Go names of the old input bodies.
var sigilForbiddenSchemas = []string{
	"SigilAllowHumaBody",
	"SigilKeyIntroduceHumaBody",
	// Контрактное имя — PluginSigilAllowRequest; SigilAllowRequest рукопись НЕ объявляет
	// (страховка от ошибочного rename под имя из explore-карты).
	"SigilAllowRequest",
}

// TestSchemaNames_Sigil — gate N4. Contract names present, technical ones absent.
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

// TestSchemaNames_SigilRequestShapes — request-body shapes checked against the hand-written spec:
// PluginSigilAllowRequest.required=[namespace,name,ref] (:5275). SigilKeyIntroduceRequest —
// all fields optional (make_primary; no required block :5619).
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
