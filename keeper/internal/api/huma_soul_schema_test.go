// Evidence gate for aligning SOUL schema names (+ errand-exec) with the committed hand-written spec
// (rollout batch N5, following the huma_voyage_schema_test.go / huma_incarnation_schema_test.go references).
// Assembles the aggregated huma spec (HumaFullSpecYAML) and checks that the soul-domain schemas
// are named EXACTLY like the contract (docs/keeper/openapi.yaml), technical huma-Go names are absent,
// enum SoulStatus/SoulTransport are extracted as named $ref, nested SoulSshTarget is unified (CLASS A),
// and the list-envelope SoulListReply carries the CONTRACT CURSOR shape (6 fields).
//
// MECHANISMS for soul (checked against the hand-written spec):
//   - REQUEST-RENAME: soulCreateHumaBody → SoulCreateRequest, soulCovenAssignHumaBody →
//     SoulCovenAssignRequest, soulCovenAssignSelectorBody → SoulCovenAssignSelector (input-only
//     CLASS C), errandExecHumaBody → ErrandRunRequest (the exec request schema, hand-written spec :1668).
//   - ENUM-ALIAS: SoulStatus + SoulTransport are declared standalone in the hand-written spec (:4198/:4207) with
//     $ref → extracted as named schemas (huma_soul_status.go).
//   - NESTED CLASS A: SoulSshTarget — a single type input(PUT body)↔output(SoulSshTargetReply.
//     ssh_target via alias SoulSSHTarget→SoulSshTarget). required:[ssh_port,ssh_user,
//     soul_path] (hand-written spec :6394).
//   - ENVELOPE CURSOR: SoulListReply — 6 fields (items/offset/limit/total + next_cursor +
//     total_approximate), NOT the 4-field incarnation shape (huma_soul_envelope.go).
//   - REPLY-RENAME (batch N6): SoulCovenAssignResponse→SoulCovenAssignReply (:7140, alias
//     handler-wire-body → api-named-struct) + SoulSSHTargetReply→SoulSshTargetReply (:6399,
//     class A alias of the generated SoulSSHTargetReply → api-named-struct). Both are only
//     the OpenAPI name/shape; the wire (custom MarshalJSON / the same oapi type) does not change.
package api

import (
	"sort"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// soulContractSchemas — request/selector/view/envelope/enum names of the soul domain + the errand-exec
// request exactly as in the committed hand-written spec. All must be present in the assembled spec.
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
	// Class C re-emission of typed-soulprint (ADR-018): typed_facts=json.RawMessage did not
	// derive nested types via reflect traversal → SoulprintFacts + 6 sub-schemas were absent.
	// An alias to typed *SoulprintFacts (huma_soul_soulprint.go) emits them. ★ Name
	// SoulprintCpuFacts (NOT the SoulprintCPUFacts drift from oapi capitalization).
	"SoulprintFacts",
	"SoulprintOsFacts",
	"SoulprintKernelFacts",
	"SoulprintCpuFacts",
	"SoulprintMemoryFacts",
	"SoulprintNetworkFacts",
	"SoulprintNetworkInterface",
}

// soulForbiddenSchemas — technical huma-Go names that DefaultSchemaNamer WOULD give from the old
// struct names. None should remain after the alignment.
var soulForbiddenSchemas = []string{
	"SoulCreateHumaBody",
	"SoulCovenAssignHumaBody",
	"SoulCovenAssignSelectorBody",
	"SoulSshTargetHumaBody",
	"ErrandExecHumaBody",
	// The generic name of an un-aliased PagedResponse[SoulListEntry] — the envelope alias must
	// displace it with the contract SoulListReply.
	"PagedResponseSoulListEntry",
	// REPLY-RENAME (batch N6): drift names of reply schemas, displaced by contract ones.
	"SoulCovenAssignResponse", // → SoulCovenAssignReply
	"SoulSSHTargetReply",      // capitalization drift of the oapi generator → SoulSshTargetReply
	"SoulprintResponse",       // → SoulprintReadReply
	// Class C: the drift name of the CPU sub-fact from oapi capitalization of the acronym (CPU), which
	// DefaultSchemaNamer WOULD give from SoulprintCPUFacts — the alias must displace it
	// with the contract SoulprintCpuFacts.
	"SoulprintCPUFacts",
}

// TestSchemaNames_Soul — gate N5. Contract names present, technical ones absent.
func TestSchemaNames_Soul(t *testing.T) {
	schemas := loadFullSpecSchemas(t)
	for _, name := range soulContractSchemas {
		if _, ok := schemas[name]; !ok {
			t.Errorf("contract schema %q is MISSING from components/schemas (name not aligned)", name)
		}
	}
	for _, name := range soulForbiddenSchemas {
		if _, ok := schemas[name]; ok {
			t.Errorf("technical huma name %q IS PRESENT in the spec -- name not aligned with the contract", name)
		}
	}
}

// TestSchemaNames_SoulStatusEnum — gate N5 (ENUM). SoulStatus and SoulTransport are extracted as
// named schemas (string + enum), and the status/transport fields of reply structs reference them via
// $ref (not inline `type: string`). The enum's members are domain truth (6 statuses / 2 transports).
func TestSchemaNames_SoulStatusEnum(t *testing.T) {
	y, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	assertStringEnum(t, schemas, "SoulStatus",
		"pending", "connected", "disconnected", "revoked", "expired", "destroyed")
	assertStringEnum(t, schemas, "SoulTransport", "agent", "ssh")

	if !strings.Contains(y, "#/components/schemas/SoulStatus") {
		t.Error("no field references SoulStatus via $ref -- status remained inline")
	}
	if !strings.Contains(y, "#/components/schemas/SoulTransport") {
		t.Error("no field references SoulTransport via $ref -- transport remained inline")
	}
}

// TestSchemaNames_SoulSshTargetNested — gate N5 (NESTED CLASS A). SoulSshTarget — a single schema
// input↔output: the PUT ssh-target body AND SoulSshTargetReply.ssh_target reference it. The shape is
// checked against the hand-written spec :6394 (required:[ssh_port,ssh_user,soul_path]; 4 fields). A mutation (removing the
// alias → output pulls SoulSSHTarget / changing the required set) reddens.
func TestSchemaNames_SoulSshTargetNested(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	const targetRef = "#/components/schemas/SoulSshTarget"

	// output SoulSshTargetReply.ssh_target → the single SoulSshTarget (via alias). The output
	// reply-schema name is aligned to the contract SoulSshTargetReply (batch N6: class A alias of the generated
	// SoulSSHTargetReply → api-named-struct soulSshTargetReply). The capitalization drift
	// SoulSSHTargetReply is displaced (in the forbidden set).
	if got := propRef(t, schemas, "SoulSshTargetReply", "ssh_target"); got != targetRef {
		t.Errorf("SoulSshTargetReply.ssh_target -> %q, expected %q (output not consolidated to a single SoulSshTarget -- alias did not work)", got, targetRef)
	}

	// The SoulSshTarget shape is checked against the hand-written spec :6394.
	tgt, _ := schemas["SoulSshTarget"].(map[string]any)
	if tgt == nil {
		t.Fatal("SoulSshTarget is missing from components.schemas")
	}
	assertRequiredExactly(t, tgt, "SoulSshTarget", "ssh_port", "ssh_user", "soul_path")
	assertProps(t, tgt, "SoulSshTarget", "ssh_port", "ssh_user", "soul_path", "ssh_provider")
}

// TestSchemaNames_SoulListEnvelope — gate N5 (ENVELOPE CURSOR). ★ soul is the only cursor
// domain: SoulListReply carries EXACTLY 6 fields (items/offset/limit/total + next_cursor +
// total_approximate), NOT the 4-field incarnation shape. items.$ref to the contract element
// SoulListEntry; offset/limit/total — int32; required:[items,offset,limit,total]. A mutation
// (4-field shape / removing cursor fields / wrong $ref) reddens.
func TestSchemaNames_SoulListEnvelope(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	env, ok := schemas["SoulListReply"].(map[string]any)
	if !ok {
		t.Fatal("SoulListReply is missing from components/schemas -- envelope-alias did not work")
	}
	props, _ := env["properties"].(map[string]any)
	if props == nil {
		t.Fatal("SoulListReply has no properties")
	}

	// ★ Exactly 6 fields — cursor fields MUST be present (differs from the 4-field incarnation).
	wantFields := []string{"items", "offset", "limit", "total", "next_cursor", "total_approximate"}
	if len(props) != len(wantFields) {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("SoulListReply carries %d fields %v, expected exactly 6 (cursor-form: items/offset/limit/total/next_cursor/total_approximate)", len(props), got)
	}
	for _, f := range wantFields {
		if _, ok := props[f]; !ok {
			t.Errorf("SoulListReply is missing contract field %q", f)
		}
	}

	// offset/limit/total — int32.
	for _, f := range []string{"offset", "limit", "total"} {
		fp, _ := props[f].(map[string]any)
		if fp == nil {
			continue
		}
		if !schemaTypeHas(fp["type"], "integer") {
			t.Errorf("SoulListReply.%s.type=%v, expected integer", f, fp["type"])
		}
		if format, _ := fp["format"].(string); format != "int32" {
			t.Errorf("SoulListReply.%s.format=%q, expected int32", f, format)
		}
	}

	// next_cursor — string; total_approximate — boolean.
	if nc, _ := props["next_cursor"].(map[string]any); nc != nil && !schemaTypeHas(nc["type"], "string") {
		t.Errorf("SoulListReply.next_cursor.type=%v, expected string", nc["type"])
	}
	if ta, _ := props["total_approximate"].(map[string]any); ta != nil && !schemaTypeHas(ta["type"], "boolean") {
		t.Errorf("SoulListReply.total_approximate.type=%v, expected boolean", ta["type"])
	}

	// items — an array with a $ref to the contract element SoulListEntry.
	items, _ := props["items"].(map[string]any)
	if items == nil {
		t.Fatal("SoulListReply.items is missing")
	}
	if !schemaTypeHas(items["type"], "array") {
		t.Errorf("SoulListReply.items.type=%v, expected array", items["type"])
	}
	elem, _ := items["items"].(map[string]any)
	if elem == nil {
		t.Fatal("SoulListReply.items.items is missing (element schema)")
	}
	const wantRef = "#/components/schemas/SoulListEntry"
	if ref, _ := elem["$ref"].(string); ref != wantRef {
		t.Errorf("SoulListReply.items.items.$ref=%q, expected %q", ref, wantRef)
	}
}

// TestSchemaNames_SoulprintFactsTyped — gate Class C (typed-soulprint). Proves that
// SoulprintReadReply.typed_facts references $ref SoulprintFacts (and NOT a free-form object),
// SoulprintFacts references 6 sub-schemas via $ref, and the CPU sub-fact is named per contract
// (SoulprintCpuFacts, NOT the SoulprintCPUFacts drift). A mutation (removing registerSoulprintFacts →
// typed_facts becomes a free-form object without $ref, sub-schemas disappear) reddens.
func TestSchemaNames_SoulprintFactsTyped(t *testing.T) {
	_, doc := loadFullSpecDoc(t)
	comp, _ := doc["components"].(map[string]any)
	schemas, _ := comp["schemas"].(map[string]any)

	// typed_facts → $ref SoulprintFacts (typed, not a free-form object).
	if got := propRef(t, schemas, "SoulprintReadReply", "typed_facts"); got != "#/components/schemas/SoulprintFacts" {
		t.Errorf("SoulprintReadReply.typed_facts.$ref=%q, expected #/components/schemas/SoulprintFacts (alias did not work -- typed_facts remained a free-form object)", got)
	}

	// SoulprintFacts.{os,kernel,cpu,memory,network} → $ref to the contract sub-schemas.
	wantFactRefs := map[string]string{
		"os":      "#/components/schemas/SoulprintOsFacts",
		"kernel":  "#/components/schemas/SoulprintKernelFacts",
		"cpu":     "#/components/schemas/SoulprintCpuFacts",
		"memory":  "#/components/schemas/SoulprintMemoryFacts",
		"network": "#/components/schemas/SoulprintNetworkFacts",
	}
	for field, want := range wantFactRefs {
		if got := propRef(t, schemas, "SoulprintFacts", field); got != want {
			t.Errorf("SoulprintFacts.%s.$ref=%q, expected %q", field, got, want)
		}
	}

	// SoulprintNetworkFacts.interfaces — array $ref to the contract SoulprintNetworkInterface.
	net, _ := schemas["SoulprintNetworkFacts"].(map[string]any)
	if net == nil {
		t.Fatal("SoulprintNetworkFacts is missing from components/schemas")
	}
	netProps, _ := net["properties"].(map[string]any)
	ifaces, _ := netProps["interfaces"].(map[string]any)
	if ifaces == nil {
		t.Fatal("SoulprintNetworkFacts.interfaces is missing")
	}
	elem, _ := ifaces["items"].(map[string]any)
	if ref, _ := elem["$ref"].(string); ref != "#/components/schemas/SoulprintNetworkInterface" {
		t.Errorf("SoulprintNetworkFacts.interfaces.items.$ref=%q, expected #/components/schemas/SoulprintNetworkInterface", ref)
	}
}

// loadFullSpecDoc assembles the aggregator spec and parses it into a map (for schema-shape asserts).
func loadFullSpecDoc(t *testing.T) (string, map[string]any) {
	t.Helper()
	y, err := HumaFullSpecYAML()
	if err != nil {
		t.Fatalf("HumaFullSpecYAML: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(y), &doc); err != nil {
		t.Fatalf("spec does not parse: %v", err)
	}
	return y, doc
}

// assertStringEnum checks that schema name is a string with an enum containing exactly the want values.
func assertStringEnum(t *testing.T, schemas map[string]any, name string, want ...string) {
	t.Helper()
	sch, ok := schemas[name].(map[string]any)
	if !ok {
		t.Fatalf("%s was not extracted as a named schema in components/schemas", name)
	}
	if typ, _ := sch["type"].(string); typ != "string" {
		t.Errorf("%s.type=%q, expected string", name, typ)
	}
	rawEnum, ok := sch["enum"].([]any)
	if !ok || len(rawEnum) == 0 {
		t.Fatalf("%s has no enum -- enum extraction did not happen", name)
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
		t.Errorf("enum %s = %v, expected %v", name, have, want)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("enum %s does not contain %q", name, w)
		}
	}
}
