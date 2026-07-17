// GOLDEN byte-exact wire-guard for the huma-native reply-DTOs of the ORACLE domain
// (handler-native T5d-2c). For EVERY reply route it marshals a populated native value and
// pins the bytes against a FIXED golden string (the legacy generated code is removed — this
// pins against a fixed form, not against a generated type). Guards that the wire form of the
// native reply-DTO hasn't drifted:
//
//   - category A (date-time): created_at/updated_at → RFC3339Nano bytes;
//   - category B ([]-vs-null): items WITHOUT omitempty (nil → null, [] → []) — both envelope
//     branches;
//   - category C (omitempty): coven/sid/created_by_aid/where — the key is omitted when nil;
//   - category D (byte-passthrough): VigilView.params / DecreeView.action_input —
//     json.RawMessage as-is (nil → null);
//   - FIELD-ORDER: key order matches the former oapi byte-order (lexicographic by json tag).
//
// A mutation of the native-struct shape (dropping omitempty / changing a json tag / changing
// a field's type / reordering a field) reddens the test.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenOracle marshals a native value and checks the bytes against the expected golden string.
func goldenOracle(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: GOLDEN wire drift:\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_OracleReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	aid := "archon-alice"
	sid := "web1.eu.example.com"
	coven := []string{"prod", "eu"}
	params := json.RawMessage(`{"path":"/etc/passwd"}`)
	actionInput := json.RawMessage(`{"input":{"reason":"alert"}}`)
	where := "event.data.size > 0"

	// --- VigilView: coven/sid/created_by_aid omitempty + params nil→null ---
	goldenOracle(t, "VigilView/full",
		VigilView{Check: "core.beacon.file_changed", Coven: &coven, CreatedAt: ts, CreatedByAID: &aid, Enabled: true, Interval: "30s", Name: "watch-passwd", Params: params, SID: &sid, UpdatedAt: ts2},
		`{"check":"core.beacon.file_changed","coven":["prod","eu"],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","enabled":true,"interval":"30s","name":"watch-passwd","params":{"path":"/etc/passwd"},"sid":"web1.eu.example.com","updated_at":"2026-06-13T01:02:03.456789012Z"}`)
	goldenOracle(t, "VigilView/nil_optionals",
		VigilView{Check: "core.beacon.file_changed", Coven: nil, CreatedAt: ts, CreatedByAID: nil, Enabled: false, Interval: "1m", Name: "watch-passwd", Params: nil, SID: nil, UpdatedAt: ts2},
		`{"check":"core.beacon.file_changed","created_at":"2026-06-14T12:34:56.789012345Z","enabled":false,"interval":"1m","name":"watch-passwd","params":null,"updated_at":"2026-06-13T01:02:03.456789012Z"}`)

	// --- VigilListReply: items non-nil / nil (category B) ---
	goldenOracle(t, "VigilListReply/full",
		VigilListReply{Items: []VigilView{{Check: "c", CreatedAt: ts, Enabled: true, Interval: "30s", Name: "v", Params: params, UpdatedAt: ts}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"check":"c","created_at":"2026-06-14T12:34:56.789012345Z","enabled":true,"interval":"30s","name":"v","params":{"path":"/etc/passwd"},"updated_at":"2026-06-14T12:34:56.789012345Z"}],"limit":50,"offset":0,"total":1}`)
	goldenOracle(t, "VigilListReply/nil_items",
		VigilListReply{Items: nil, Limit: 50, Offset: 10, Total: 0},
		`{"items":null,"limit":50,"offset":10,"total":0}`)

	// --- DecreeView: coven/sid/created_by_aid/where omitempty + action_input nil→null ---
	goldenOracle(t, "DecreeView/full",
		DecreeView{ActionInput: actionInput, ActionScenario: "restart", Cooldown: "5m", Coven: &coven, CreatedAt: ts, CreatedByAID: &aid, Enabled: true, IncarnationName: "redis-prod", Name: "react-passwd", OnBeacon: "watch-passwd", SID: &sid, UpdatedAt: ts2, Where: &where},
		`{"action_input":{"input":{"reason":"alert"}},"action_scenario":"restart","cooldown":"5m","coven":["prod","eu"],"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-alice","enabled":true,"incarnation_name":"redis-prod","name":"react-passwd","on_beacon":"watch-passwd","sid":"web1.eu.example.com","updated_at":"2026-06-13T01:02:03.456789012Z","where":"event.data.size \u003e 0"}`)
	goldenOracle(t, "DecreeView/nil_optionals",
		DecreeView{ActionInput: nil, ActionScenario: "restart", Cooldown: "", Coven: nil, CreatedAt: ts, CreatedByAID: nil, Enabled: false, IncarnationName: "redis-prod", Name: "react-passwd", OnBeacon: "watch-passwd", SID: nil, UpdatedAt: ts2, Where: nil},
		`{"action_input":null,"action_scenario":"restart","cooldown":"","created_at":"2026-06-14T12:34:56.789012345Z","enabled":false,"incarnation_name":"redis-prod","name":"react-passwd","on_beacon":"watch-passwd","updated_at":"2026-06-13T01:02:03.456789012Z"}`)

	// --- DecreeListReply: items non-nil / nil ---
	goldenOracle(t, "DecreeListReply/full",
		DecreeListReply{Items: []DecreeView{{ActionInput: actionInput, ActionScenario: "s", Cooldown: "1m", CreatedAt: ts, Enabled: true, IncarnationName: "i", Name: "d", OnBeacon: "v", UpdatedAt: ts}}, Limit: 50, Offset: 0, Total: 1},
		`{"items":[{"action_input":{"input":{"reason":"alert"}},"action_scenario":"s","cooldown":"1m","created_at":"2026-06-14T12:34:56.789012345Z","enabled":true,"incarnation_name":"i","name":"d","on_beacon":"v","updated_at":"2026-06-14T12:34:56.789012345Z"}],"limit":50,"offset":0,"total":1}`)
	goldenOracle(t, "DecreeListReply/nil_items",
		DecreeListReply{Items: nil, Limit: 50, Offset: 0, Total: 0},
		`{"items":null,"limit":50,"offset":0,"total":0}`)
}
