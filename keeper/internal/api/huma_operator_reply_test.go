// GOLDEN byte-exact wire-guard для NATIVE wire-DTO OPERATOR-домена (handler-native
// PILOT T5d). operator больше НЕ зависит от legacy-генерата (0 legacy-генерата в operator-файлах),
// поэтому golden сверяет json native-значения с ЗАФИКСИРОВАННОЙ строкой-эталоном (а не
// с legacy-генерата-значением, как делал общий goldenWire для ещё-oapi-доменов). Это пиннит
// точные wire-байты: мутация формы (убрать omitempty / сменить json-тег / тип поля /
// порядок) краснит соответствующий case. Покрыты обе указательных ветки (nil/non-nil):
// omitempty (roles/created_by_aid/metadata/revoked_at) и SENSITIVE jwt.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenOperatorWire сверяет json.Marshal(native) байт-в-байт с зафиксированным
// эталоном. PILOT-форма golden для handler-native-доменов (без legacy-генерата-эталона).
func goldenOperatorWire(t *testing.T, name string, native any, want string) {
	t.Helper()
	got, err := json.Marshal(native)
	if err != nil {
		t.Fatalf("%s: marshal native: %v", name, err)
	}
	if string(got) != want {
		t.Errorf("%s: WIRE DRIFT\n got  = %s\n want = %s", name, got, want)
	}
}

func TestGoldenWire_OperatorReply(t *testing.T) {
	ts := time.Date(2026, 6, 14, 12, 34, 56, 789012345, time.UTC)
	ts2 := time.Date(2026, 6, 13, 1, 2, 3, 456789012, time.UTC)
	aid := "archon-bob"
	roles := []string{"cluster-admin", "operator"}
	meta := map[string]interface{}{"team": "ops", "tier": float64(1)}

	// --- OperatorCreateReply: roles omitempty (обе ветки) ---
	goldenOperatorWire(t, "OperatorCreateReply/roles_set",
		OperatorCreateReply{AID: "archon-alice", CreatedAt: ts, CreatedByAID: aid, DisplayName: "Alice", JWT: "ey.tok", Roles: &roles},
		`{"aid":"archon-alice","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice","jwt":"ey.tok","roles":["cluster-admin","operator"]}`)
	goldenOperatorWire(t, "OperatorCreateReply/roles_nil",
		OperatorCreateReply{AID: "archon-alice", CreatedAt: ts, CreatedByAID: aid, DisplayName: "Alice", JWT: "ey.tok", Roles: nil},
		`{"aid":"archon-alice","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice","jwt":"ey.tok"}`)

	// --- Operator (GET + list-element): enum auth_method + omitempty nullable ---
	goldenOperatorWire(t, "Operator/full",
		Operator{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), BootstrapInitial: false, CreatedAt: ts, CreatedByAID: &aid, DisplayName: "Alice", Metadata: &meta, RevokedAt: &ts2},
		`{"aid":"archon-alice","auth_method":"jwt","bootstrap_initial":false,"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice","metadata":{"team":"ops","tier":1},"revoked_at":"2026-06-13T01:02:03.456789012Z"}`)
	goldenOperatorWire(t, "Operator/bootstrap_nil_optionals",
		Operator{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), BootstrapInitial: true, CreatedAt: ts, CreatedByAID: nil, DisplayName: "Alice", Metadata: nil, RevokedAt: nil},
		`{"aid":"archon-alice","auth_method":"jwt","bootstrap_initial":true,"created_at":"2026-06-14T12:34:56.789012345Z","display_name":"Alice"}`)

	// --- IssueTokenReply: date-time + SENSITIVE jwt ---
	goldenOperatorWire(t, "IssueTokenReply",
		IssueTokenReply{AID: "archon-alice", ExpiresAt: ts, JWT: "ey.tok"},
		`{"aid":"archon-alice","expires_at":"2026-06-14T12:34:56.789012345Z","jwt":"ey.tok"}`)
}
