//go:build integration

// Guard for /v1/me/jwts (ADR-014 amendment NIM-1052) through the real server and
// Postgres. SECURITY invariants:
//
//	The routes reach the CALLER's rows only: another operator's token is neither
//	listed nor revocable through them, whatever id the request names.
//	A request made with a personal token cannot mint another one.
//
// The caller holds no permission at all (the enforcer denies every check), so
// every answer here is also proof that the routes ask none.

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
)

// accountedToken mints a token for aid with a row, as a sign-in does.
func accountedToken(t *testing.T, aid string, kind operator.JWTKind) operator.MintResult {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	res, err := operator.MintJWT(context.Background(), integrationPool, iss, operator.MintInput{
		AID: aid, TTL: time.Hour, Kind: kind,
	})
	if err != nil {
		t.Fatalf("MintJWT(%s, %s): %v", aid, kind, err)
	}
	return res
}

func meDo(t *testing.T, method, url, token, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func jwtRevokedAt(t *testing.T, jwtID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT revoked_at FROM operator_jwts WHERE jwt_id = $1`, jwtID).Scan(&at); err != nil {
		t.Fatalf("read %s: %v", jwtID, err)
	}
	return at
}

func TestIntegration_MeJWT_OnlyTheCallersRows(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	alice := accountedToken(t, "archon-alice", operator.JWTKindSession)
	bob := accountedToken(t, "archon-bob", operator.JWTKindSession)
	bobIssued := accountedToken(t, "archon-bob", operator.JWTKindIssued)

	base, stop := startServer(t, nil)
	defer stop()

	code, raw := meDo(t, http.MethodGet, base+"/v1/me/jwts", alice.JWT, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200; body=%s", code, raw)
	}
	var list MyJWTListReply
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].JWTID != alice.JWTID {
		t.Errorf("alice's list = %+v, want her one session only", list.Items)
	}
	for _, it := range list.Items {
		if it.AID != "archon-alice" {
			t.Errorf("alice's list carries %s's token %s", it.AID, it.JWTID)
		}
	}

	for _, id := range []string{bob.JWTID, bobIssued.JWTID} {
		code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts/"+id+"/revoke", alice.JWT, "")
		if code != http.StatusNotFound {
			t.Errorf("alice revoking bob's %s = %d, want 404; body=%s", id, code, raw)
		}
		if at := jwtRevokedAt(t, id); at != nil {
			t.Errorf("bob's token %s was revoked through alice's /v1/me", id)
		}
	}

	code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts", alice.JWT, `{"ttl":"30m","label":"backup script"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue = %d, want 201; body=%s", code, raw)
	}
	var issued IssueTokenReply
	if err := json.Unmarshal(raw, &issued); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if issued.AID != "archon-alice" || issued.JWT == "" || !operator.ValidJWTID(issued.JWTID) {
		t.Errorf("issue reply = %+v", issued)
	}
	var kind, createdBy string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT kind, created_by_aid FROM operator_jwts WHERE jwt_id = $1 AND aid = 'archon-alice'`, issued.JWTID).Scan(&kind, &createdBy); err != nil {
		t.Fatalf("read personal row: %v", err)
	}
	if kind != "personal" || createdBy != "archon-alice" {
		t.Errorf("personal row kind=%s created_by=%s", kind, createdBy)
	}

	code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts", issued.JWT, `{"ttl":"30m"}`)
	if code != http.StatusForbidden {
		t.Fatalf("issue with the personal token = %d, want 403; body=%s", code, raw)
	}
	var p problem.Details
	if err := json.Unmarshal(raw, &p); err != nil || p.Type != problem.TypePersonalJWTFromPersonal {
		t.Errorf("problem = %+v (%v), want %s", p, err, problem.TypePersonalJWTFromPersonal)
	}

	code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts/"+issued.JWTID+"/revoke", alice.JWT, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke own = %d, want 204; body=%s", code, raw)
	}
	if at := jwtRevokedAt(t, issued.JWTID); at == nil {
		t.Error("alice's own personal token is not revoked")
	}
	if at := jwtRevokedAt(t, alice.JWTID); at != nil {
		t.Error("revoking one token revoked the session too")
	}
}

func TestIntegration_MeJWT_RecordsOnSuccess(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	alice := accountedToken(t, "archon-alice", operator.JWTKindSession)

	base, stop := startServer(t, nil)
	defer stop()

	code, raw := meDo(t, http.MethodPost, base+"/v1/me/jwts", alice.JWT, `{"label":"ci"}`)
	if code != http.StatusCreated {
		t.Fatalf("issue = %d; body=%s", code, raw)
	}
	var issued IssueTokenReply
	_ = json.Unmarshal(raw, &issued)
	code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts/"+issued.JWTID+"/revoke", alice.JWT, "")
	if code != http.StatusNoContent {
		t.Fatalf("revoke = %d; body=%s", code, raw)
	}

	for evt, want := range map[string]map[string]any{
		"operator.token-issued": {"aid": "archon-alice", "jwt_id": issued.JWTID, "kind": "personal", "label": "ci"},
		"operator.jwt-revoked":  {"aid": "archon-alice", "jwt_id": issued.JWTID, "kind": "personal"},
	} {
		var (
			actor   string
			payload []byte
		)
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT archon_aid, payload FROM audit_log WHERE event_type = $1`, evt).Scan(&actor, &payload); err != nil {
			t.Fatalf("%s: %v", evt, err)
		}
		var got map[string]any
		_ = json.Unmarshal(payload, &got)
		if actor != "archon-alice" {
			t.Errorf("%s archon_aid = %s", evt, actor)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s payload[%s] = %v, want %v (payload %s)", evt, k, got[k], v, payload)
			}
		}
	}
}

// The limits and the OIDC cut-off reach the wire: the list answers the counts
// and the cut-off, and a request past the daily limit is a 409, not a 500.
func TestIntegration_MeJWT_TermsAndDailyLimit(t *testing.T) {
	truncateOperators(t)
	if err := operator.Insert(context.Background(), integrationPool, &operator.Operator{
		AID: "olga", DisplayName: "olga", AuthMethod: operator.AuthMethodOIDC, CreatedVia: "oidc",
	}); err != nil {
		t.Fatalf("seed olga: %v", err)
	}
	signIn := accountedToken(t, "olga", operator.JWTKindSession)
	for range operator.MaxPersonalJWTsPerDay {
		accountedToken(t, "olga", operator.JWTKindPersonal)
	}

	base, stop := startServer(t, nil)
	defer stop()

	code, raw := meDo(t, http.MethodGet, base+"/v1/me/jwts?limit=1", signIn.JWT, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d; body=%s", code, raw)
	}
	var list MyJWTListReply
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.PersonalLastDay != int32(operator.MaxPersonalJWTsPerDay) || list.PersonalDailyLimit != int32(operator.MaxPersonalJWTsPerDay) ||
		list.PersonalActive != int32(operator.MaxPersonalJWTsPerDay) || list.PersonalLimit != int32(operator.MaxActivePersonalJWTs) {
		t.Errorf("counts = %+v", list)
	}
	if list.PersonalExpiresBy == nil || list.PersonalExpiresBy.After(signIn.ExpiresAt.Add(-time.Second)) {
		t.Errorf("personal_expires_by = %v, want at least a second before the sign-in's %s", list.PersonalExpiresBy, signIn.ExpiresAt)
	}

	code, raw = meDo(t, http.MethodPost, base+"/v1/me/jwts", signIn.JWT, `{"ttl":"30m"}`)
	if code != http.StatusConflict {
		t.Fatalf("issue past the daily limit = %d, want 409; body=%s", code, raw)
	}
	var p problem.Details
	if err := json.Unmarshal(raw, &p); err != nil || p.Type != problem.TypePersonalJWTLimit || !strings.Contains(p.Detail, "a day") {
		t.Errorf("problem = %+v (%v), want %s about the daily limit", p, err, problem.TypePersonalJWTLimit)
	}
}
