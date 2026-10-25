// GOLDEN byte-exact wire-guard for the NATIVE wire-DTO of the OPERATOR domain (handler-native
// PILOT T5d). operator no longer depends on the legacy generator (0 legacy generator in
// operator files), so the golden compares json native values against a PINNED reference
// string (not against a legacy-generator value, as the shared goldenWire did for still-oapi
// domains). This pins the exact wire bytes: a shape mutation (drop omitempty / change a json
// tag / field type / order) reddens the corresponding case. Both pointer branches (nil/non-nil)
// are covered: omitempty (roles/created_by_aid/metadata/revoked_at) and SENSITIVE jwt.
package api

import (
	"encoding/json"
	"testing"
	"time"
)

// goldenOperatorWire compares json.Marshal(native) byte-for-byte with a pinned
// reference. The PILOT golden form for handler-native domains (no legacy-generator reference).
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

	// --- OperatorCreateReply: roles omitempty (both branches) ---
	goldenOperatorWire(t, "OperatorCreateReply/roles_set",
		OperatorCreateReply{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), CreatedAt: ts, CreatedByAID: aid, DisplayName: "Alice", ExpiresAt: &ts2, JWT: "ey.tok", JWTID: "01JTESTJT1000000000000000A", Roles: &roles},
		`{"aid":"archon-alice","auth_method":"jwt","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice","expires_at":"2026-06-13T01:02:03.456789012Z","jwt":"ey.tok","jwt_id":"01JTESTJT1000000000000000A","roles":["cluster-admin","operator"]}`)
	goldenOperatorWire(t, "OperatorCreateReply/roles_nil",
		OperatorCreateReply{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), CreatedAt: ts, CreatedByAID: aid, DisplayName: "Alice", ExpiresAt: &ts2, JWT: "ey.tok", JWTID: "01JTESTJT1000000000000000A", Roles: nil},
		`{"aid":"archon-alice","auth_method":"jwt","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice","expires_at":"2026-06-13T01:02:03.456789012Z","jwt":"ey.tok","jwt_id":"01JTESTJT1000000000000000A"}`)

	// --- OperatorCreateReply of a local Archon: no token, so jwt/jwt_id/expires_at
	// are absent rather than empty (NIM-1051) ---
	goldenOperatorWire(t, "OperatorCreateReply/local",
		OperatorCreateReply{AID: "alice", AuthMethod: OperatorAuthMethod("local"), CreatedAt: ts, CreatedByAID: aid, DisplayName: "Alice"},
		`{"aid":"alice","auth_method":"local","created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","display_name":"Alice"}`)

	// --- Operator (GET + list-element): enum auth_method + omitempty nullable +
	// created_via (ALWAYS present, no omitempty) ---
	goldenOperatorWire(t, "Operator/full",
		Operator{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), BootstrapInitial: false, CreatedAt: ts, CreatedByAID: &aid, CreatedVia: "user", DisplayName: "Alice", Metadata: &meta, RevokedAt: &ts2},
		`{"aid":"archon-alice","auth_method":"jwt","bootstrap_initial":false,"created_at":"2026-06-14T12:34:56.789012345Z","created_by_aid":"archon-bob","created_via":"user","display_name":"Alice","metadata":{"team":"ops","tier":1},"revoked_at":"2026-06-13T01:02:03.456789012Z"}`)
	goldenOperatorWire(t, "Operator/bootstrap_nil_optionals",
		Operator{AID: "archon-alice", AuthMethod: OperatorAuthMethod("jwt"), BootstrapInitial: true, CreatedAt: ts, CreatedByAID: nil, CreatedVia: "bootstrap", DisplayName: "Alice", Metadata: nil, RevokedAt: nil},
		`{"aid":"archon-alice","auth_method":"jwt","bootstrap_initial":true,"created_at":"2026-06-14T12:34:56.789012345Z","created_via":"bootstrap","display_name":"Alice"}`)

	// --- IssueTokenReply: date-time + SENSITIVE jwt ---
	goldenOperatorWire(t, "IssueTokenReply",
		IssueTokenReply{AID: "archon-alice", ExpiresAt: ts, JWT: "ey.tok", JWTID: "01JTESTJT1000000000000000A"},
		`{"aid":"archon-alice","expires_at":"2026-06-14T12:34:56.789012345Z","jwt":"ey.tok","jwt_id":"01JTESTJT1000000000000000A"}`)

	// --- OperatorJWT (GET /v1/operators/{aid}/jwts element, NIM-1045): the
	// optional metadata and revocation fields omitted when unset ---
	goldenOperatorWire(t, "OperatorJWT/minimal",
		OperatorJWT{AID: "archon-alice", CreatedAt: ts2, ExpiresAt: ts, JWTID: "01JTESTJT1000000000000000A", Kind: "session"},
		`{"aid":"archon-alice","created_at":"2026-06-13T01:02:03.456789012Z","expires_at":"2026-06-14T12:34:56.789012345Z","jwt_id":"01JTESTJT1000000000000000A","kind":"session"}`)
	goldenOperatorWire(t, "OperatorJWT/full",
		OperatorJWT{AID: "archon-alice", CreatedAt: ts2, CreatedByAID: &aid, CreatedIP: "10.0.0.1", CreatedUserAgent: "curl/8", ExpiresAt: ts, JWTID: "01JTESTJT1000000000000000A", Kind: "issued", Label: "Service Desk", LastUsedAt: &ts, LastUsedIP: "10.0.0.2", LastUsedUserAgent: "sd/1", RevokedAt: &ts, RevokedByAID: &aid},
		`{"aid":"archon-alice","created_at":"2026-06-13T01:02:03.456789012Z","created_by_aid":"archon-bob","created_ip":"10.0.0.1","created_user_agent":"curl/8","expires_at":"2026-06-14T12:34:56.789012345Z","jwt_id":"01JTESTJT1000000000000000A","kind":"issued","label":"Service Desk","last_used_at":"2026-06-14T12:34:56.789012345Z","last_used_ip":"10.0.0.2","last_used_user_agent":"sd/1","revoked_at":"2026-06-14T12:34:56.789012345Z","revoked_by_aid":"archon-bob"}`)
}
