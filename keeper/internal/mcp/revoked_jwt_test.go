package mcp

// Per-token revocation on the MCP listener (NIM-1045, ADR-014 amendment). This
// listener has no RejectRevoked: the verifier is the only thing between a
// revoked token and the tool catalog, so these tests are what pins it here.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

const (
	mcpRevokedJWTID = "01JREV0KEDJWT0000000000000"
	mcpLiveJWTID    = "01JG00DJWT0000000000000000"
)

type revokedSet map[string]bool

func (s revokedSet) IsJWTRevoked(id string) bool { return s[id] }

func mcpVerifierRevoking(t *testing.T, ids ...string) *keeperjwt.Verifier {
	t.Helper()
	set := revokedSet{}
	for _, id := range ids {
		set[id] = true
	}
	v, err := keeperjwt.NewVerifier([]byte(sseTestSigningKey), sseTestIssuer, set)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func mcpTokenWithID(t *testing.T, jwtID string) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(sseTestSigningKey), sseTestIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(jwtID, "archon-alice", []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// postMCP sends one tools/list call; panicked=true means the request got past
// authentication into the (absent) handler.
func postMCP(h http.Handler, token string) (rec *httptest.ResponseRecorder, panicked bool) {
	rec = httptest.NewRecorder()
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec, false
}

func TestMCP_RevokedJWTRefusedAtTheDoor(t *testing.T) {
	h := buildMCPHandler(ServerDeps{JWTVerifier: mcpVerifierRevoking(t, mcpRevokedJWTID)})

	rec, panicked := postMCP(h, mcpTokenWithID(t, mcpRevokedJWTID))
	if panicked {
		t.Fatal("a revoked token reached the MCP handler")
	}
	body := rec.Body.String()
	if !strings.Contains(body, mcpCodeUnauthenticated) || !strings.Contains(body, "token revoked") {
		t.Errorf("revoked token answered %s, want an unauthenticated error naming the revocation", body)
	}
}

func TestMCP_LiveJWTPassesTheDoor(t *testing.T) {
	h := buildMCPHandler(ServerDeps{JWTVerifier: mcpVerifierRevoking(t, mcpRevokedJWTID)})

	rec, panicked := postMCP(h, mcpTokenWithID(t, mcpLiveJWTID))
	if !panicked && strings.Contains(rec.Body.String(), mcpCodeUnauthenticated) {
		t.Errorf("a live token of the same operator did not get past authentication: %s", rec.Body.String())
	}
}

func TestMCPEvents_RevokedJWTRefusedAtTheDoor(t *testing.T) {
	h := buildSSEHandler(sseDeps{
		JWTVerifier: mcpVerifierRevoking(t, mcpRevokedJWTID),
		RBAC:        allowAllRBAC{},
		Access:      allowAllAccess{},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/mcp/events?apply_id=01JAPPLY000000000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+mcpTokenWithID(t, mcpRevokedJWTID))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "token revoked") {
		t.Errorf("revoked token on /mcp/events = %d %s, want 401 naming the revocation", rec.Code, rec.Body.String())
	}
}

// An open stream re-asks every interval; revoking the token it was opened with
// must end it as revoking its operator does.
func TestSSEStillAuthorized_RevokedJWTEndsTheStream(t *testing.T) {
	rbac := revokedJWTRBAC{revoked: map[string]bool{mcpRevokedJWTID: true}}
	deps := sseDeps{RBAC: rbac}
	if sseStillAuthorized(deps, "archon-alice", sseGrant{initiator: true, jwtID: mcpRevokedJWTID}) {
		t.Error("a stream opened with a revoked token stays authorized")
	}
	if !sseStillAuthorized(deps, "archon-alice", sseGrant{initiator: true, jwtID: mcpLiveJWTID}) {
		t.Error("a stream opened with a live token lost authorization")
	}
}

type revokedJWTRBAC struct {
	allowAllRBAC
	revoked map[string]bool
}

func (r revokedJWTRBAC) IsJWTRevoked(id string) bool { return r.revoked[id] }

// Tool audit records name the token the call was made with (NIM-1045).
func TestToolAudit_RecordsTheActorToken(t *testing.T) {
	pool := &fakePool{selectFn: func(aid string) (*operator.Operator, error) {
		return &operator.Operator{AID: aid, DisplayName: aid, AuthMethod: operator.AuthMethodJWT, CreatedAt: time.Now()}, nil
	}}
	h, _, rec := newTestHandler(t, pool, &rbactest.Config{
		Roles: []rbactest.Role{{Name: "issuer", Operators: []string{"archon-alice"}, Permissions: []string{"operator.issue-token"}}},
	})
	params, _ := json.Marshal(toolsCallParams{Name: "keeper.operator.issue-token", Arguments: json.RawMessage(`{"aid":"archon-bob","ttl":"1h","label":"sd"}`)})
	resp, _ := h.Dispatch(context.Background(), &keeperjwt.Claims{Subject: "archon-alice", ID: mcpLiveJWTID},
		jsonRPCRequest{JSONRPC: "2.0", ID: mustRawID(1), Method: "tools/call", Params: params})
	if resp.Error != nil {
		t.Fatalf("issue-token: %+v", resp.Error)
	}
	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(rec.events))
	}
	p := rec.events[0].Payload
	if p[audit.PayloadActorJWTID] != mcpLiveJWTID {
		t.Errorf("payload[%s] = %v, want the caller's jti", audit.PayloadActorJWTID, p[audit.PayloadActorJWTID])
	}
	if p["ttl"] != "1h0m0s" || p["label"] != "sd" || p["jwt_id"] == "" {
		t.Errorf("token-issued payload = %v", p)
	}
}
