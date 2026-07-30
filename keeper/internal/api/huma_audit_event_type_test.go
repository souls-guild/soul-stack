package api

// Guard that the CONTRACT carries the audit catalog — not merely that the committed
// snapshot matches whatever the code currently emits.
//
// WHY IT IS NOT COVERED BY TestCommittedOpenAPI_NoDrift. That test compares
// docs/keeper/openapi.yaml against the huma dump, so the two can only agree or
// disagree — it has no opinion on what the dump SAYS. If the enum stopped being
// emitted (a huma upgrade changing how SchemaProvider results are inlined, someone
// reverting AuditEvent.Type to a plain string), the drift guard would go red once,
// the next `make gen-openapi` would write a spec with no enum, and everything would
// be green again with the contract quietly back to a bare string. That is the same
// silence the ticket set out to remove, one level up, so the presence and the
// contents of the enum are asserted directly.

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestAuditEventType_SpecEnumIsTheCatalog — the built spec declares AuditEvent.type
// as a string enum whose values are exactly [audit.AllEventTypes], in that order.
func TestAuditEventType_SpecEnumIsTheCatalog(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	schema, ok := spec.Components.Schemas.Map()["AuditEvent"]
	if !ok {
		t.Fatal("schema AuditEvent is absent from the spec")
	}
	typeProp, ok := schema.Properties["type"]
	if !ok {
		t.Fatal("AuditEvent has no `type` property")
	}
	if typeProp.Type != "string" {
		t.Errorf("AuditEvent.type is %q, want string - the enum must narrow the value set without changing the wire type", typeProp.Type)
	}

	want := audit.AllEventTypes()
	if len(typeProp.Enum) == 0 {
		t.Fatalf("AuditEvent.type carries no enum, so the contract still publishes a bare string and a client has nothing to check its coverage of the %d event types against (NIM-346)", len(want))
	}
	if len(typeProp.Enum) != len(want) {
		t.Fatalf("AuditEvent.type enum has %d values, the catalog has %d - the spec no longer publishes the full set of what keeper can write", len(typeProp.Enum), len(want))
	}
	for i, v := range want {
		got, ok := typeProp.Enum[i].(string)
		if !ok {
			t.Fatalf("AuditEvent.type enum[%d] is %T, want string", i, typeProp.Enum[i])
		}
		if got != string(v) {
			t.Errorf("AuditEvent.type enum[%d] = %q, catalog has %q - the published order must follow the catalog (sorted by wire value), otherwise the committed spec churns on unrelated edits", i, got, v)
		}
	}
}

// TestAuditEventType_QueryFilterStaysUnconstrained — the `?type=` filter must NOT
// gain the enum.
//
// huma answers an out-of-enum query value with 422, so constraining the filter would
// turn `?type=tide.started` from "200, no matches" into a rejected request — and that
// filter runs against audit_log, which holds rows written before a type was retired
// from the catalog (tide.* and errand_run.* are exactly such history). Narrowing an
// output is safe because the server only ever emits catalog values; narrowing this
// input would make history unqueryable. Asserted rather than left to a comment: the
// two schemas sit one struct apart and the enum would look like a consistency fix.
func TestAuditEventType_QueryFilterStaysUnconstrained(t *testing.T) {
	spec, err := buildFullOpenAPISpec()
	if err != nil {
		t.Fatalf("buildFullOpenAPISpec: %v", err)
	}

	item, ok := spec.Paths["/v1/audit"]
	if !ok {
		t.Fatal("path /v1/audit is absent from the spec")
	}
	if item.Get == nil {
		t.Fatal("/v1/audit has no GET operation")
	}

	var found bool
	for _, p := range item.Get.Parameters {
		if p.Name != "type" {
			continue
		}
		found = true
		if p.Schema == nil {
			t.Fatal("query parameter `type` has no schema")
		}
		if len(p.Schema.Enum) > 0 {
			t.Errorf("query parameter `type` gained an enum (%d values): huma answers an out-of-enum value with 422, "+
				"so the filter would stop matching history written under a type since retired from the catalog", len(p.Schema.Enum))
		}
		if p.Schema.Items != nil && len(p.Schema.Items.Enum) > 0 {
			t.Errorf("query parameter `type` gained a per-item enum (%d values): same 422 on historical types", len(p.Schema.Items.Enum))
		}
	}
	if !found {
		t.Error("query parameter `type` is absent from GET /v1/audit - the multi-value filter is part of the contract")
	}
}
