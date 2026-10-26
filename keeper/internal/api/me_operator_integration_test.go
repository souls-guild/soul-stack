//go:build integration

// Guard for the owner of a machine operator (ADR-014 amendment NIM-1057)
// through the real server and Postgres. SECURITY invariants:
//
//	/v1/me/operators… reach only the operators the CALLER owns, directly or
//	through a Synod it is a member of; any other AID answers 404 and nothing
//	of it is read or changed.
//	An owner mints a token of its operator only while its own effective rights
//	— read from the RBAC rows, not the request — cover the operator's.
//
// In the two owner-route tests the callers hold no permission in the enforcer,
// so every answer there is also proof that the owner routes ask none.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

func seedPersonOp(t *testing.T, aid string) {
	t.Helper()
	if err := operator.Insert(context.Background(), integrationPool, &operator.Operator{
		AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodLocal,
	}); err != nil {
		t.Fatalf("seed person %s: %v", aid, err)
	}
}

func seedOwnedOp(t *testing.T, aid string, owner operator.Owner) {
	t.Helper()
	op := &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT}
	if owner.AID != "" {
		op.OwnerAID = &owner.AID
	}
	if owner.Synod != "" {
		op.OwnerSynod = &owner.Synod
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seed machine operator %s: %v", aid, err)
	}
}

func execIT(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func grantRoleIT(t *testing.T, role, aid string, perms ...string) {
	t.Helper()
	execIT(t, `INSERT INTO rbac_roles (name, description) VALUES ($1, '') ON CONFLICT (name) DO NOTHING`, role)
	for _, p := range perms {
		execIT(t, `INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2) ON CONFLICT DO NOTHING`, role, p)
	}
	execIT(t, `INSERT INTO rbac_role_operators (role_name, aid) VALUES ($1, $2)`, role, aid)
}

func ownedList(t *testing.T, base, token string) []string {
	t.Helper()
	code, raw := meDo(t, http.MethodGet, base+"/v1/me/operators", token, "")
	if code != http.StatusOK {
		t.Fatalf("GET /v1/me/operators = %d; body=%s", code, raw)
	}
	var page sharedapi.PagedResponse[Operator]
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	var out []string
	for _, op := range page.Items {
		out = append(out, op.AID)
	}
	if page.Total != len(out) {
		t.Fatalf("total %d, items %v", page.Total, out)
	}
	return out
}

func problemType(t *testing.T, raw []byte) string {
	t.Helper()
	var p struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Type
}

func TestIntegration_OwnedOperators_OnlyTheCallersOwn(t *testing.T) {
	truncateOperators(t)
	seedPersonOp(t, "alice")
	seedPersonOp(t, "bob")
	seedPersonOp(t, "carol")
	execIT(t, `INSERT INTO synods (name) VALUES ('team')`)
	execIT(t, `INSERT INTO synod_operators (synod_name, aid) VALUES ('team', 'bob')`)
	seedOwnedOp(t, "dba-sd", operator.Owner{AID: "alice"})
	seedOwnedOp(t, "dba-gw", operator.Owner{Synod: "team"})
	seedOwnedOp(t, "dba-x", operator.Owner{})
	gwToken := accountedToken(t, "dba-gw", operator.JWTKindIssued)
	alice := accountedToken(t, "alice", operator.JWTKindSession)
	bob := accountedToken(t, "bob", operator.JWTKindSession)
	carol := accountedToken(t, "carol", operator.JWTKindSession)

	base, stop := startServer(t, nil)
	defer stop()

	if got := ownedList(t, base, alice.JWT); len(got) != 1 || got[0] != "dba-sd" {
		t.Errorf("alice owns %v, want [dba-sd]", got)
	}
	if got := ownedList(t, base, bob.JWT); len(got) != 1 || got[0] != "dba-gw" {
		t.Errorf("bob owns %v, want [dba-gw] through the Synod", got)
	}
	if got := ownedList(t, base, carol.JWT); len(got) != 0 {
		t.Errorf("carol owns %v, want nothing", got)
	}

	for _, aid := range []string{"dba-gw", "dba-x", "nobody"} {
		if code, raw := meDo(t, http.MethodGet, base+"/v1/me/operators/"+aid+"/jwts", alice.JWT, ""); code != http.StatusNotFound {
			t.Errorf("alice lists %s's tokens = %d, want 404; body=%s", aid, code, raw)
		}
		if code, raw := meDo(t, http.MethodPost, base+"/v1/me/operators/"+aid+"/jwts", alice.JWT, ""); code != http.StatusNotFound {
			t.Errorf("alice issues for %s = %d, want 404; body=%s", aid, code, raw)
		}
	}
	revokeURL := base + "/v1/me/operators/dba-gw/jwts/" + gwToken.JWTID + "/revoke"
	if code, raw := meDo(t, http.MethodPost, revokeURL, alice.JWT, ""); code != http.StatusNotFound {
		t.Errorf("alice revokes dba-gw's token = %d, want 404; body=%s", code, raw)
	}
	if at := jwtRevokedAt(t, gwToken.JWTID); at != nil {
		t.Fatalf("a stranger revoked dba-gw's token")
	}

	code, raw := meDo(t, http.MethodGet, base+"/v1/me/operators/dba-gw/jwts", bob.JWT, "")
	if code != http.StatusOK || !strings.Contains(string(raw), gwToken.JWTID) {
		t.Fatalf("bob lists dba-gw's tokens = %d; body=%s", code, raw)
	}
	if code, raw := meDo(t, http.MethodPost, revokeURL, bob.JWT, ""); code != http.StatusNoContent {
		t.Fatalf("bob revokes dba-gw's token = %d, want 204; body=%s", code, raw)
	}
	if at := jwtRevokedAt(t, gwToken.JWTID); at == nil {
		t.Fatalf("the owner's revoke did not land")
	}
}

func TestIntegration_OwnedOperators_IssueNeedsCoverage(t *testing.T) {
	truncateOperators(t)
	seedPersonOp(t, "alice")
	seedOwnedOp(t, "dba-sd", operator.Owner{AID: "alice"})
	grantRoleIT(t, "sd-reader", "dba-sd", "incarnation.get", "service.list")
	grantRoleIT(t, "dragonfly-reader", "alice", "incarnation.get on service=dragonfly", "service.list")
	alice := accountedToken(t, "alice", operator.JWTKindSession)
	countTokens := func() int {
		var n int
		if err := integrationPool.QueryRow(context.Background(), `SELECT COUNT(*) FROM operator_jwts WHERE aid = 'dba-sd'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	base, stop := startServer(t, nil)
	defer stop()
	url := base + "/v1/me/operators/dba-sd/jwts"
	// The test server runs ttl_max = 1h; the 90-day ceiling is the operator
	// package's integration test.
	body := `{"ttl":"30m","label":"rotation"}`

	code, raw := meDo(t, http.MethodPost, url, alice.JWT, body)
	if code != http.StatusForbidden || problemType(t, raw) != problem.TypeOwnerLacksPermissions || !strings.Contains(string(raw), "incarnation.get") {
		t.Fatalf("narrower owner = %d, want 403 owner-lacks-permissions naming incarnation.get; body=%s", code, raw)
	}
	if n := countTokens(); n != 0 {
		t.Fatalf("a refused issue left %d token rows", n)
	}

	grantRoleIT(t, "sd-reader", "alice")
	code, raw = meDo(t, http.MethodPost, url, alice.JWT, body)
	if code != http.StatusCreated {
		t.Fatalf("covering owner = %d, want 201; body=%s", code, raw)
	}
	var issued IssueTokenReply
	if err := json.Unmarshal(raw, &issued); err != nil || issued.JWT == "" || issued.AID != "dba-sd" {
		t.Fatalf("reply %+v, err %v", issued, err)
	}
	if left := time.Until(issued.ExpiresAt); left < 29*time.Minute || left > 30*time.Minute {
		t.Errorf("expires in %s, want 30m", left)
	}
	if code, raw := meDo(t, http.MethodGet, base+"/v1/me/jwts", issued.JWT, ""); code != http.StatusOK || !strings.Contains(string(raw), `"aid":"dba-sd"`) {
		t.Errorf("the minted token does not act as dba-sd: %d; body=%s", code, raw)
	}
	var by, via string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid, kind FROM operator_jwts WHERE jwt_id = $1`, issued.JWTID).Scan(&by, &via); err != nil || by != "alice" || via != string(operator.JWTKindIssued) {
		t.Errorf("row created_by_aid=%q kind=%q err=%v, want alice / issued", by, via, err)
	}
	var audited int
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type = 'operator.token-issued' AND archon_aid = 'alice' AND payload->>'aid' = 'dba-sd' AND payload->>'jwt_id' = $1`,
		issued.JWTID).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("audit rows = %d, err %v; want 1", audited, err)
	}

	personal := accountedToken(t, "alice", operator.JWTKindPersonal)
	if code, raw := meDo(t, http.MethodPost, url, personal.JWT, body); code != http.StatusForbidden || problemType(t, raw) != problem.TypePersonalJWTFromPersonal {
		t.Errorf("personal token = %d, want 403 personal-jwt-from-personal; body=%s", code, raw)
	}
}

// PUT / DELETE /v1/operators/{aid}/owner through the server: the gate, the 422
// for a bad owner, and the audit record.
func TestIntegration_OperatorOwner_SetAndClear(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-admin", "")
	seedPersonOp(t, "alice")
	seedOwnedOp(t, "dba-sd", operator.Owner{})
	base, stop := startServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "owners", Operators: []string{"archon-admin"}, Permissions: []string{"operator.set-owner", "operator.list"}},
	}})
	defer stop()
	admin := newValidTokenFor(t, "archon-admin", []string{"owners"})
	stranger := newValidTokenFor(t, "alice", nil)
	url := base + "/v1/operators/dba-sd/owner"

	if code, raw := meDo(t, http.MethodPut, url, stranger, `{"owner_aid":"alice"}`); code != http.StatusForbidden {
		t.Errorf("without operator.set-owner = %d, want 403; body=%s", code, raw)
	}
	if code, raw := meDo(t, http.MethodPut, url, admin, `{}`); code != http.StatusUnprocessableEntity {
		t.Errorf("empty body = %d, want 422; body=%s", code, raw)
	}
	if code, raw := meDo(t, http.MethodPut, url, admin, `{"owner_aid":"archon-admin"}`); code != http.StatusUnprocessableEntity {
		t.Errorf("a machine operator as owner = %d, want 422; body=%s", code, raw)
	}
	if code, raw := meDo(t, http.MethodPut, url, admin, `{"owner_aid":"alice"}`); code != http.StatusNoContent {
		t.Fatalf("set = %d, want 204; body=%s", code, raw)
	}
	code, raw := meDo(t, http.MethodGet, base+"/v1/operators?owner_aid=alice", admin, "")
	if code != http.StatusOK || !strings.Contains(string(raw), `"aid":"dba-sd"`) || !strings.Contains(string(raw), `"owner_aid":"alice"`) || !strings.Contains(string(raw), `"total":1`) {
		t.Errorf("list by owner_aid = %d; body=%s", code, raw)
	}
	if code, raw := meDo(t, http.MethodDelete, url, admin, ""); code != http.StatusNoContent {
		t.Fatalf("clear = %d, want 204; body=%s", code, raw)
	}
	var set, cleared int
	_ = integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type = 'operator.owner-set' AND payload->>'owner_aid' = 'alice'`).Scan(&set)
	_ = integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type = 'operator.owner-set' AND payload->>'previous_owner_aid' = 'alice' AND NOT payload ? 'owner_aid'`).Scan(&cleared)
	if set != 1 || cleared != 1 {
		t.Errorf("audit: set rows %d, cleared rows %d; want 1 and 1", set, cleared)
	}
}
