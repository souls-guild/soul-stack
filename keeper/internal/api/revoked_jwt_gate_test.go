package api

// Guard tests for per-token revocation (NIM-1045, ADR-014 amendment). SECURITY
// invariant:
//
//	A JWT whose `jti` has been revoked is refused with 401 `jwt-revoked` on EVERY
//	authenticated route, while its operator stays active.
//
// Two layers refuse it and each is swept on its own, so removing either one
// turns a sweep red rather than hiding behind the other: the verifier (every
// door that verifies a token goes through it — /v1, both MCP doors, the cookie
// exchange) and RejectRevoked (the /v1 chain's own check). The route table is
// the walked chi tree, as in revoked_gate_test.go.

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

const (
	revokedJWTID = "01JREV0KEDJWT0000000000000"
	liveJWTID    = "01JG00DJWT0000000000000000"
)

// revokedJWTRBAC — both gate AIDs active and holding `*`; the revoked-token
// projection holds exactly ids.
func revokedJWTRBAC(t *testing.T, ids ...string) *rbac.Enforcer {
	t.Helper()
	snap := &rbac.Snapshot{
		Roles:       map[string][]string{"cluster-admin": {"*"}},
		Membership:  map[string][]string{revokedGateAID: {"cluster-admin"}, activeGateAID: {"cluster-admin"}},
		Revoked:     map[string]time.Time{},
		RevokedJWTs: make(map[string]struct{}, len(ids)),
	}
	for _, id := range ids {
		snap.RevokedJWTs[id] = struct{}{}
	}
	enf, err := rbac.NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	return enf
}

// jwtBlindRBAC answers every revocation question about tokens with "no", so a
// sweep through it measures the verifier alone.
type jwtBlindRBAC struct{ RBACProvider }

func (jwtBlindRBAC) IsJWTRevoked(string) bool { return false }

func gateTokenWithID(t *testing.T, jwtID, aid string) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(metaSigningKey), metaIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(jwtID, aid, []string{"cluster-admin"}, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

func assertRevokedJWTEverywhere(t *testing.T, h http.Handler, token string) {
	t.Helper()
	for _, rt := range authenticatedRoutes(t, h) {
		rec, panicked := serveRecovering(h, rt.method, rt.path, token)
		if panicked {
			t.Errorf("%s %s: reached the handler with a revoked token", rt.method, rt.path)
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 for a revoked token; body=%s", rt.method, rt.path, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), problem.TypeJWTRevoked) {
			t.Errorf("%s %s: 401 without %s — the holder cannot tell a revoked token from a revoked operator; body=%s",
				rt.method, rt.path, problem.TypeJWTRevoked, rec.Body.String())
		}
	}
}

// TestRevokedJWT_RejectRevokedRefusesOnEveryRoute — the /v1 chain's own check,
// behind a verifier that knows nothing about revocation.
func TestRevokedJWT_RejectRevokedRefusesOnEveryRoute(t *testing.T) {
	h := revokedGateRouter(t, revokedJWTRBAC(t, revokedJWTID))
	assertRevokedJWTEverywhere(t, h, gateTokenWithID(t, revokedJWTID, activeGateAID))
}

// TestRevokedJWT_VerifierRefusesOnEveryRoute — the verifier alone: the RBAC
// provider the chain asks is blind to revoked tokens, so only Verify can refuse.
// This is the layer the MCP doors and the cookie exchange rely on.
func TestRevokedJWT_VerifierRefusesOnEveryRoute(t *testing.T) {
	src := revokedJWTRBAC(t, revokedJWTID)
	verifier, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer, src)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	h := revokedGateRouterVerifier(t, verifier, jwtBlindRBAC{src}, handlers.NewSoulHandler(nil, nil, nil, nil), nil, false)
	assertRevokedJWTEverywhere(t, h, gateTokenWithID(t, revokedJWTID, activeGateAID))
}

// TestLiveJWT_NeverRefusedAsRevoked — the negative half: another token of the
// same active operator is never refused as revoked, through either layer. A
// gate that refused every token would pass the two sweeps above.
func TestLiveJWT_NeverRefusedAsRevoked(t *testing.T) {
	src := revokedJWTRBAC(t, revokedJWTID)
	verifier, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer, src)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	h := revokedGateRouterVerifier(t, verifier, src, handlers.NewSoulHandler(nil, nil, nil, nil), nil, false)
	token := gateTokenWithID(t, liveJWTID, activeGateAID)
	for _, rt := range authenticatedRoutes(t, h) {
		rec, panicked := serveRecovering(h, rt.method, rt.path, token)
		if panicked {
			continue
		}
		if strings.Contains(rec.Body.String(), problem.TypeJWTRevoked) {
			t.Errorf("%s %s = %d refused a LIVE token as revoked; body=%s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

// An open run-events stream re-asks on every interval; revoking the token it was
// opened with ends it, as revoking its operator does.
func TestRunEventsStillAuthorized_RevokedJWTEndsTheStream(t *testing.T) {
	deps := &runEventsDeps{RBAC: revokedJWTRBAC(t, revokedJWTID)}
	if runEventsStillAuthorized(deps, activeGateAID, runEventsGrant{initiator: true, jwtID: revokedJWTID}) {
		t.Error("a stream opened with a revoked token stays authorized")
	}
	if !runEventsStillAuthorized(deps, activeGateAID, runEventsGrant{initiator: true, jwtID: liveJWTID}) {
		t.Error("a stream opened with a live token lost authorization")
	}
}
