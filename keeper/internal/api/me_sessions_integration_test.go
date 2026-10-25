//go:build integration

// Guard for POST /v1/me/sessions/revoke (ADR-058 amendment NIM-1050) through the
// real server and Postgres. SECURITY invariants:
//
//	It ends the CALLER's web sessions only: never another operator's, never a
//	personal or issued token of the caller's.
//	keep_current leaves exactly the session the request is made with; without
//	it that session ends too.
//	An ended session is in the revocation set the verifier reads, so every
//	Bearer exchanged from it — it carries the session's jti — is refused.

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func revokeMySessions(t *testing.T, base, token, body string) int64 {
	t.Helper()
	code, raw := meDo(t, http.MethodPost, base+"/v1/me/sessions/revoke", token, body)
	if code != http.StatusOK {
		t.Fatalf("POST /v1/me/sessions/revoke %s = %d, want 200; body=%s", body, code, raw)
	}
	var reply MySessionsRevokeReply
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
	return reply.Revoked
}

func TestIntegration_MeSessionsRevoke_OnlyTheCallersSessions(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	here := accountedToken(t, "archon-alice", operator.JWTKindSession)
	laptop := accountedToken(t, "archon-alice", operator.JWTKindSession)
	phone := accountedToken(t, "archon-alice", operator.JWTKindSession)
	script := accountedToken(t, "archon-alice", operator.JWTKindPersonal)
	ci := accountedToken(t, "archon-alice", operator.JWTKindIssued)
	bob := accountedToken(t, "archon-bob", operator.JWTKindSession)

	base, stop := startServer(t, nil)
	defer stop()

	if n := revokeMySessions(t, base, here.JWT, `{"keep_current":true}`); n != 2 {
		t.Fatalf("all but this one ended %d sessions, want 2", n)
	}
	for _, c := range []struct {
		name    string
		jwtID   string
		revoked bool
	}{
		{"this session", here.JWTID, false},
		{"the laptop session", laptop.JWTID, true},
		{"the phone session", phone.JWTID, true},
		{"the personal token", script.JWTID, false},
		{"the issued token", ci.JWTID, false},
		{"bob's session", bob.JWTID, false},
	} {
		if got := jwtRevokedAt(t, c.jwtID) != nil; got != c.revoked {
			t.Errorf("%s revoked = %v, want %v", c.name, got, c.revoked)
		}
	}

	snap, err := rbac.LoadSnapshot(context.Background(), integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	enf, err := rbac.NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if !enf.IsJWTRevoked(laptop.JWTID) || enf.IsJWTRevoked(here.JWTID) {
		t.Errorf("revocation set: laptop=%v here=%v, want true/false", enf.IsJWTRevoked(laptop.JWTID), enf.IsJWTRevoked(here.JWTID))
	}

	if n := revokeMySessions(t, base, here.JWT, ""); n != 1 {
		t.Fatalf("end all ended %d sessions, want 1 (this one)", n)
	}
	if jwtRevokedAt(t, here.JWTID) == nil {
		t.Error("end all left the session the request was made with")
	}
	if jwtRevokedAt(t, script.JWTID) != nil || jwtRevokedAt(t, bob.JWTID) != nil {
		t.Error("end all reached a personal token or another operator's session")
	}

	var (
		actor   string
		payload []byte
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT archon_aid, payload FROM audit_log WHERE event_type = 'operator.sessions-revoked' ORDER BY created_at LIMIT 1`).Scan(&actor, &payload); err != nil {
		t.Fatalf("audit: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(payload, &got)
	if actor != "archon-alice" || got["aid"] != "archon-alice" || got["revoked"] != float64(2) || got["keep_current"] != true {
		t.Errorf("audit actor=%s payload=%s", actor, payload)
	}
}
