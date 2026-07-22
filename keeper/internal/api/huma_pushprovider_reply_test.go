// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the PUSH-PROVIDER domain (handler-native T5d).
// push-provider no longer depends on the legacy generator — golden compares the json of native values
// against a PINNED reference string. Both updated_by_aid branches (nil/non-nil) and
// params ({} normalization) are covered. A shape mutation of the native struct (dropping omitempty / changing a
// json tag / field type / order) reddens the corresponding case.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

func goldenPushProviderWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_PushProviderReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	aid := "archon-alice"
	params := map[string]interface{}{"vault_addr": "https://vault:8200", "role": "push"}

	// --- PushProvider: updated_by_aid set + params populated ---
	goldenPushProviderWire(t, "PushProvider/full",
		PushProvider{CreatedAt: ts, CreatedByAID: aid, Name: "openssh", Params: params, UpdatedAt: ts2, UpdatedByAID: &aid},
		`{"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","name":"openssh","params":{"role":"push","vault_addr":"https://vault:8200"},"updated_at":"2026-06-13T01:02:03.456789012Z","updated_by_aid":"archon-alice"}`)
	// updated_by_aid nil → key omitted (omitempty); params empty {} (handler normalizes).
	goldenPushProviderWire(t, "PushProvider/nil_updated_by",
		PushProvider{CreatedAt: ts, CreatedByAID: aid, Name: "openssh", Params: map[string]interface{}{}, UpdatedAt: ts, UpdatedByAID: nil},
		`{"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","name":"openssh","params":{},"updated_at":"2026-06-14T12:34:56.789012345Z"}`)

	// --- PushProviderListReply (envelope as top-level reply DTO) ---
	pvN := PushProvider{CreatedAt: ts, CreatedByAID: aid, Name: "openssh", Params: params, UpdatedAt: ts2, UpdatedByAID: &aid}
	goldenPushProviderWire(t, "PushProviderListReply/full",
		PushProviderListReply{Items: []PushProvider{pvN}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","name":"openssh","params":{"role":"push","vault_addr":"https://vault:8200"},"updated_at":"2026-06-13T01:02:03.456789012Z","updated_by_aid":"archon-alice"}],"limit":50,"offset":0,"total":1}`)
	goldenPushProviderWire(t, "PushProviderListReply/empty_items",
		PushProviderListReply{Items: []PushProvider{}, Limit: 50, Offset: 10, Total: 0},
		`{"items":[],"limit":50,"offset":10,"total":0}`)
}
