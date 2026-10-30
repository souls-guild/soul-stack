package api

// Guard (NIM-1146): a `view-secrets` grant scoped by an incarnation trait reveals
// on the incarnations that carry the trait, and only on those.
//
// The scoped gate (RequirePermissionMulti + IncarnationScopeSelector) hands the
// enforcer a flat context of incarnation/service/coven; traits are not in it, and a
// `trait.<key>=v` condition there fails closed. So reveal is existence-gated and the
// scope is the handler's (GetInScopeFor), which reads the incarnation's traits. Put
// the scoped gate back and a trait-scoped role is refused on its own incarnation
// with a 403 the handler never sees — which is what the SD gateway's narrowed role
// (`service=dragonfly AND trait.created_via=sd-gateway`) ran into.
//
// Driven through the REAL buildRouter, with the enforcer as the handler's scoper
// (server.go wires deps.RBAC there) and a service snapshot that declares a secret,
// so the in-scope case reaches Vault and the "no Vault read" of the out-of-scope
// cases is a statement the same fixture could contradict.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	revealTraitAID    = "archon-sd-gateway-creator"
	revealNoRightsAID = "archon-reveal-nothing"
	revealSecretValue = "df-admin-s3cret"
)

// revealTraitRow is a SelectByID row with the given service and traits jsonb.
func revealTraitRow(name, service, traits string) pgx.Row {
	now := time.Now()
	return incStaticRow{values: []any{
		name, service, "v1", int(1),
		[]byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		[]byte(traits),
		"create",
		any(nil),
		any(nil),
	}}
}

// revealSecretLoader declares one scalar secret, admin_password, on every service.
type revealSecretLoader struct{ incTestLoader }

func (revealSecretLoader) Load(_ context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error) {
	return &artifact.ServiceArtifact{Ref: ref, Manifest: &config.ServiceManifest{
		StateSchema: config.InputSchemaMap{
			"admin_password": &config.InputSchema{Type: config.SecretTypeName, Label: "Admin password"},
		},
	}}, nil
}

type revealRecordingVault struct{ reads []string }

func (v *revealRecordingVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	v.reads = append(v.reads, path)
	return map[string]any{config.ScalarSecretVaultField: revealSecretValue}, nil
}

func revealTraitEnforcer(t *testing.T) *rbac.Enforcer {
	t.Helper()
	enf, err := rbac.NewEnforcerFromSnapshot(&rbac.Snapshot{
		Roles: map[string][]string{
			"sd-gateway-creator": {"incarnation.view-secrets on service=dragonfly AND trait.created_via=sd-gateway"},
			"holds-nothing":      {},
		},
		Membership: map[string][]string{
			revealTraitAID:    {"sd-gateway-creator"},
			revealNoRightsAID: {"holds-nothing"},
		},
		Revoked: map[string]time.Time{},
	})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot refused `service=dragonfly AND trait.created_via=sd-gateway`: %v", err)
	}
	return enf
}

func TestIncarnationReveal_TraitScopedViewSecrets(t *testing.T) {
	rows := map[string]pgx.Row{
		"df-sd":     revealTraitRow("df-sd", "dragonfly", `{"created_via":"sd-gateway"}`),
		"df-manual": revealTraitRow("df-manual", "dragonfly", `{"created_via":"manual"}`),
		"redis-sd":  revealTraitRow("redis-sd", "redis", `{"created_via":"sd-gateway"}`),
	}
	db := &incTestDB{selectByID: func(name string) pgx.Row {
		if row, ok := rows[name]; ok {
			return row
		}
		return errRow2{pgx.ErrNoRows}
	}}
	enf := revealTraitEnforcer(t)
	auditW := &auditCaptureWriter{}
	vault := &revealRecordingVault{}
	incH := handlers.NewIncarnationHandler(db, nil, nil, incTestResolver{ok: true}, revealSecretLoader{}, auditW, enf, nil)
	incH.SetVaultReader(vault, "")

	verifier, err := keeperjwt.NewVerifier([]byte(metaSigningKey), metaIssuer, keeperjwt.NoRevocation)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	h := revokedGateRouterIncarnation(t, verifier, enf, incH, handlers.NewSoulHandler(nil, nil, nil, nil), nil, false)

	reveal := func(t *testing.T, aid, inc string) *httptest.ResponseRecorder {
		t.Helper()
		auditW.events = nil
		vault.reads = nil
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/incarnations/"+inc+"/secrets/reveal",
			strings.NewReader(`{"secret_id":"admin_password"}`))
		req.Header.Set("Authorization", "Bearer "+revokedGateToken(t, aid))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}
	revealAudit := func(t *testing.T, label string) map[string]any {
		t.Helper()
		evs := auditW.Events()
		if len(evs) != 1 || evs[0].EventType != audit.EventIncarnationSecretRevealed {
			t.Fatalf("%s: audit = %#v, want exactly one %s", label, evs, audit.EventIncarnationSecretRevealed)
		}
		return evs[0].Payload
	}

	t.Run("dragonfly created by the gateway: gate and handler scope both pass", func(t *testing.T) {
		rec := reveal(t, revealTraitAID, "df-sd")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), revealSecretValue) {
			t.Fatalf("reveal on df-sd = %d %s, want 200 with the value — the grant's service and trait both match this incarnation",
				rec.Code, rec.Body.String())
		}
		if p := revealAudit(t, "df-sd"); p["result"] != "ok" {
			t.Errorf("df-sd audit payload = %v, want result ok", p)
		}
		if len(vault.reads) != 1 {
			t.Errorf("df-sd Vault reads = %v, want exactly one", vault.reads)
		}
	})

	outOfScope := []struct{ name, inc, why string }{
		{"dragonfly created by hand", "df-manual", "the trait created_via is manual"},
		{"redis created by the gateway", "redis-sd", "the service is redis"},
	}
	for _, c := range outOfScope {
		t.Run(c.name+": 404 with an out_of_scope audit, no Vault read", func(t *testing.T) {
			rec := reveal(t, revealTraitAID, c.inc)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("reveal on %s = %d %s, want 404 — %s, and an incarnation out of scope is not found",
					c.inc, rec.Code, rec.Body.String(), c.why)
			}
			if p := revealAudit(t, c.inc); p["result"] != "denied" || p["reason"] != "out_of_scope" {
				t.Errorf("%s audit payload = %v, want result denied, reason out_of_scope", c.inc, p)
			}
			if len(vault.reads) != 0 {
				t.Errorf("%s: Vault read %v for an incarnation out of scope", c.inc, vault.reads)
			}
		})
	}

	t.Run("operator without view-secrets: 403 at the gate, no audit", func(t *testing.T) {
		rec := reveal(t, revealNoRightsAID, "df-sd")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("reveal by an Archon without view-secrets = %d %s, want 403", rec.Code, rec.Body.String())
		}
		if evs := auditW.Events(); len(evs) != 0 {
			t.Errorf("audit written for a request refused at the gate: %#v", evs)
		}
		if len(vault.reads) != 0 {
			t.Errorf("Vault read %v for a request refused at the gate", vault.reads)
		}
	})
}
