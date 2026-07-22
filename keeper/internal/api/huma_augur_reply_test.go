// GOLDEN byte-exact wire-guard for the huma-native reply-DTO of the AUGUR domain (handler-native
// T5d-2c). For EVERY reply route it marshals a populated native value and pins the bytes
// against a FIXED golden string (the legacy generator is gone — pin against a fixed shape,
// not against a generated type). Guarantees the native reply-DTO wire shape has not drifted:
//
//   - category A (date-time): created_at → RFC3339Nano bytes;
//   - category B ([]-vs-null): items without omitempty (nil → null, [] → []) — both envelope branches;
//   - category C (omitempty): created_by_aid/coven/sid/token_* — key omitted when nil;
//   - category D (byte-passthrough): RiteView.allow — json.RawMessage as-is;
//   - FIELD-ORDER: key order matching the former oapi byte-order (auth_ref/created_at/… for
//     OmenView, allow/coven/created_at/… for RiteView — lexicographic by json tag).
//
// Mutating the native-struct shape (drop omitempty / change json tag / change field type /
// reorder a field) turns this red.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenAugur marshals a native value and checks the bytes against the expected golden string.
func goldenAugur(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: GOLDEN wire-drift:\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_AugurReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	aid := "archon-alice"
	coven := "prod"
	sid := "web1.example.com"
	ttl := "30m"
	nuses := 5
	allow := json.RawMessage(`{"metrics":["up","node_load1"]}`)

	// --- OmenView: created_by_aid omitempty (both branches) + inline enum ---
	goldenAugur(t, "OmenView/full",
		OmenView{AuthRef: "vault:secret/omen", CreatedAt: ts, CreatedByAID: &aid, Endpoint: "https://prom:9090", Name: "prom-eu", SourceType: OmenViewSourceType("prometheus")},
		`{"auth_ref":"vault:secret/omen","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","endpoint":"https://prom:9090","name":"prom-eu","source_type":"prometheus"}`)
	goldenAugur(t, "OmenView/nil_creator",
		OmenView{AuthRef: "vault:secret/omen", CreatedAt: ts, CreatedByAID: nil, Endpoint: "https://prom:9090", Name: "prom-eu", SourceType: OmenViewSourceType("vault")},
		`{"auth_ref":"vault:secret/omen","created_at":"2026-06-14T12:34:56.789012345Z","endpoint":"https://prom:9090","name":"prom-eu","source_type":"vault"}`)

	// --- OmenListReply: items non-nil / nil (category B) ---
	goldenAugur(t, "OmenListReply/full",
		OmenListReply{Items: []OmenView{{Name: "a", SourceType: "elk", CreatedAt: ts}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"auth_ref":"","created_at":"2026-06-14T12:34:56.789012345Z","endpoint":"","name":"a","source_type":"elk"}],"limit":50,"offset":0,"total":1}`)
	goldenAugur(t, "OmenListReply/nil_items",
		OmenListReply{Items: nil, Limit: 50, Offset: 10, Total: 0},
		`{"items":null,"limit":50,"offset":10,"total":0}`)

	// --- RiteView: allow byte-passthrough + coven/sid/token_* omitempty (both branches) ---
	goldenAugur(t, "RiteView/full",
		RiteView{Allow: allow, Coven: &coven, CreatedAt: ts, CreatedByAID: &aid, Delegate: true, ID: 42, Omen: "prom-eu", SID: nil, TokenNumUses: &nuses, TokenTTL: &ttl},
		`{"allow":{"metrics":["up","node_load1"]},"coven":"prod","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","delegate":true,"id":42,"omen":"prom-eu","token_num_uses":5,"token_ttl":"30m"}`)
	goldenAugur(t, "RiteView/sid_subject_nil_optionals",
		RiteView{Allow: allow, Coven: nil, CreatedAt: ts, CreatedByAID: nil, Delegate: false, ID: 7, Omen: "prom-eu", SID: &sid, TokenNumUses: nil, TokenTTL: nil},
		`{"allow":{"metrics":["up","node_load1"]},"created_at":"2026-06-14T12:34:56.789012345Z","delegate":false,"id":7,"omen":"prom-eu","sid":"web1.example.com"}`)

	// --- RiteListReply: items non-nil / nil ---
	goldenAugur(t, "RiteListReply/full",
		RiteListReply{Items: []RiteView{{Allow: allow, ID: 1, Omen: "prom-eu", CreatedAt: ts}}},
		`{"items":[{"allow":{"metrics":["up","node_load1"]},"created_at":"2026-06-14T12:34:56.789012345Z","delegate":false,"id":1,"omen":"prom-eu"}]}`)
	goldenAugur(t, "RiteListReply/nil_items",
		RiteListReply{Items: nil},
		`{"items":null}`)
}
