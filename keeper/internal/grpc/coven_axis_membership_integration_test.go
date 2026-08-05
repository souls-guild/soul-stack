//go:build integration

// The coven axis on live PG: telemetry delivery (NIM-248) and Augur subject
// resolution (NIM-249). Both are consumers NIM-124 left behind when it moved
// host↔incarnation membership out of `souls.coven[]` into
// `incarnation_membership`, and both fail in the shape that migration made
// possible — the column still exists, the query still succeeds, the match set
// is simply empty.
//
// The two of them need OPPOSITE fixes, which is the thing these tests are here
// to keep straight:
//
//   - telemetry asks "which incarnation is this host owed a config from" — that
//     is MEMBERSHIP, and it must be read from the relation, because the label
//     union deliberately admits a host-attached tag spelled like an
//     incarnation's name;
//   - Augur asks "may this Rite see this host" — that is LABELS, and it must be
//     read as the effective union, so a Rite scoped to an incarnation reaches
//     its members.
//
// The label half of the TELEMETRY axis is expressed differently since ADR-0082:
// a service's vars have no hard-wired coven dimension, so the claim is proved
// over a `vars/_stack.yaml` foreach step — the mechanism that replaced the
// deleted hard-wired `coven/<label>.yaml` overlay.
//
// Live PG rather than the unit fakes: the defect was in which relation the SQL
// touched, and every fixture below turns on hosts that differ ONLY by a
// membership row.
package grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	covenAxisAID      = "archon-alice"
	covenAxisInc      = "redis-prod"
	covenAxisSvc      = "redis-svc"
	covenAxisMember   = "member.example.com"
	covenAxisOutsider = "outsider.example.com"
	covenAxisOmen     = "vault-prod"
	covenAxisPath     = "secret/keeper/db"
)

func resetCovenAxis(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE rites, omens, apply_runs, state_history,
		 incarnation, soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE coven-axis: %v", err)
	}
}

// seedCovenAxis seeds an operator, an incarnation carrying incCovens, and two
// hosts: a plain one (bound by the caller) and an OUTSIDER already tagged with
// the incarnation's own name.
//
// That outsider is the whole point of the fixture. Under ADR-080 it has the
// same effective covens as a member — the union cannot tell a tag from a
// membership — so any consumer that answers a membership question from labels
// serves it, and any consumer that answers a visibility question from the
// relation stops serving real members. One fixture, both mistakes.
func seedCovenAxis(t *testing.T, ctx context.Context, incCovens []string) {
	t.Helper()
	resetCovenAxis(t)

	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: covenAxisAID, DisplayName: covenAxisAID, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := covenAxisAID
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID: covenAxisMember, Transport: soul.TransportAgent, Status: soul.StatusConnected,
		Coven: []string{"linux"}, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert(member): %v", err)
	}
	if err := soul.Insert(ctx, integrationPool, &soul.Soul{
		SID: covenAxisOutsider, Transport: soul.TransportAgent, Status: soul.StatusConnected,
		Coven: []string{"linux", covenAxisInc}, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("soul.Insert(outsider): %v", err)
	}
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: covenAxisInc, Service: covenAxisSvc, ServiceVersion: "v1",
		StateSchemaVersion: 1, State: map[string]any{},
		Status: incarnation.StatusReady, Covens: incCovens, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
}

func bindCovenAxisMember(t *testing.T, ctx context.Context, sid string) {
	t.Helper()
	by := covenAxisAID
	if err := incarnation.AddMembers(ctx, integrationPool, covenAxisInc, []string{sid}, &by); err != nil {
		t.Fatalf("AddMembers(%s): %v", sid, err)
	}
}

// --- telemetry delivery (NIM-248) ---

// newCovenAxisTelemetry assembles a live-PG telemetry source over a service
// snapshot in a temp dir: manifest cadence 45s, plus an optional coven overlay
// (declared by a `vars/_stack.yaml` foreach step, ADR-0082) that drops it to 15s
// so an inherited label is observable in the result.
func newCovenAxisTelemetry(t *testing.T, covenOverlay string) TelemetrySource {
	t.Helper()
	dir := t.TempDir()
	if covenOverlay != "" {
		covenDir := filepath.Join(dir, "vars", "coven")
		if err := os.MkdirAll(covenDir, 0o755); err != nil {
			t.Fatalf("mkdir vars/coven: %v", err)
		}
		if err := os.WriteFile(filepath.Join(covenDir, covenOverlay+".yaml"),
			[]byte("telemetry_interval: 15s\n"), 0o644); err != nil {
			t.Fatalf("write coven overlay: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vars", "_stack.yaml"), []byte(`stack:
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    optional: true
`), 0o644); err != nil {
			t.Fatalf("write _stack.yaml: %v", err)
		}
	}
	manifest := &config.ServiceManifest{
		Name:      covenAxisSvc,
		Telemetry: &config.TelemetryConfig{Interval: telStrPtr("45s"), Collectors: []string{"cpu"}},
	}
	return NewTelemetrySource(
		integrationPool,
		telemetryFakeResolver{ref: artifact.ServiceRef{Name: covenAxisSvc, Git: "file:///srv/redis"}, ok: true},
		&telemetryFakeLoader{art: &artifact.ServiceArtifact{LocalDir: dir, Manifest: manifest}},
		servicevars.NewResolver(discardLogger(t)),
		discardLogger(t),
	)
}

func resolveTelemetry(t *testing.T, ctx context.Context, src TelemetrySource, sid string) *keeperv1.TelemetryConfig {
	t.Helper()
	cfg, err := src.ResolveForSID(ctx, sid)
	if err != nil {
		t.Fatalf("ResolveForSID(%s): %v", sid, err)
	}
	return cfg
}

// TestIntegration_TelemetryReachesExactlyIncarnationMembers — the headline of
// NIM-248. A bound host gets the incarnation's effective config; the tagged
// outsider gets nothing. Before the fix BOTH got nothing, and the difference
// never showed up anywhere: "no incarnation for this host" is a legal branch
// that returns (nil, nil) without a log line, so the entire fleet ran on
// soul-local defaults while the operator's overview kept aggregating members.
func TestIntegration_TelemetryReachesExactlyIncarnationMembers(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	bindCovenAxisMember(t, ctx, covenAxisMember)
	src := newCovenAxisTelemetry(t, "")

	if cfg := resolveTelemetry(t, ctx, src, covenAxisMember); cfg == nil {
		t.Error("a bound host must be served its incarnation's telemetry config")
	} else if cfg.GetIntervalSec() != 45 {
		t.Errorf("interval_sec = %d, want 45 (manifest cadence)", cfg.GetIntervalSec())
	}
	if cfg := resolveTelemetry(t, ctx, src, covenAxisOutsider); cfg != nil {
		t.Errorf("a host merely TAGGED with the incarnation's name got its config: %+v", cfg)
	}
}

// TestIntegration_TelemetryStopsWhenMemberUnbound — unbinding takes the config
// away on the next resolve. Paired with the test above this pins the direction:
// the answer follows the relation, not anything cached or copied onto the host.
func TestIntegration_TelemetryStopsWhenMemberUnbound(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	bindCovenAxisMember(t, ctx, covenAxisMember)
	src := newCovenAxisTelemetry(t, "")

	if cfg := resolveTelemetry(t, ctx, src, covenAxisMember); cfg == nil {
		t.Fatal("precondition: a bound host must be served a config")
	}
	if _, err := incarnation.RemoveMember(ctx, integrationPool, covenAxisInc, covenAxisMember); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if cfg := resolveTelemetry(t, ctx, src, covenAxisMember); cfg != nil {
		t.Errorf("an unbound host kept receiving the incarnation's config: %+v", cfg)
	}
}

// TestIntegration_TelemetryInheritsIncarnationCovenIntoServiceVars — the other
// half of the split. Membership decides WHICH service config the host is owed;
// the incarnation's labels decide which overlays of that config apply. The tag
// lives on the incarnation and on no host, so a delivery reading only
// `souls.coven[]` — or one that dropped the incarnation's covens on the way to
// the resolver — would serve the manifest's 45s instead of the overlay's 15s.
func TestIntegration_TelemetryInheritsIncarnationCovenIntoServiceVars(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, []string{"cache"})
	bindCovenAxisMember(t, ctx, covenAxisMember)
	src := newCovenAxisTelemetry(t, "cache")

	cfg := resolveTelemetry(t, ctx, src, covenAxisMember)
	if cfg == nil {
		t.Fatal("a bound host must be served a config")
	}
	if cfg.GetIntervalSec() != 15 {
		t.Errorf("interval_sec = %d, want 15 — the incarnation's tag must reach its members' service vars",
			cfg.GetIntervalSec())
	}
}

// --- Augur subject resolution (NIM-249) ---

// seedCovenAxisRite adds an Omen and one coven-Rite scoped to riteCoven,
// allowing exactly covenAxisPath.
func seedCovenAxisRite(t *testing.T, ctx context.Context, riteCoven string) {
	t.Helper()
	creator := covenAxisAID
	if err := augur.InsertOmen(ctx, integrationPool, &augur.Omen{
		Name: covenAxisOmen, SourceType: augur.SourceVault,
		Endpoint: "https://vault:8200", AuthRef: "vault:secret/keeper/augur/" + covenAxisOmen,
		CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	coven := riteCoven
	if err := augur.InsertRite(ctx, integrationPool, &augur.Rite{
		Omen: covenAxisOmen, Coven: &coven, Allow: augurAllowPaths(covenAxisPath),
		CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertRite: %v", err)
	}
}

// augurStatusFor runs one AugurRequest as the given host against live PG and
// returns the reply status.
func augurStatusFor(t *testing.T, ctx context.Context, sid string) keeperv1.AugurStatus {
	t.Helper()
	kv := &stubKV{data: map[string]any{"password": "s3cr3t"}}
	h, outCh := newAugurHandler(t, integrationPool, kv, &recordingAudit{}, sid)
	h.processAugurRequest(ctx, sid, "sess-"+sid, &keeperv1.AugurRequest{
		RequestId: "req-" + sid, OmenName: covenAxisOmen, Query: covenAxisPath,
	})
	return recvReply(t, outCh).GetStatus()
}

// TestIntegration_AugurCovenRiteAuthorizesIncarnationMembers — a Rite scoped to
// an incarnation's name authorizes its members. This one regressed loudly:
// a subject matching no Rite is default-denied, so hosts started failing the
// Augur step mid-apply rather than quietly doing nothing.
//
// The tagged outsider is authorized here too, and that is correct rather than a
// hole: a Rite's subject is a LABEL selector, `coven` XOR `sid`, with no
// incarnation dimension to gate on, and host tags are operator-assigned. That
// an incarnation-scoped Rite cannot tell members from same-named-tag holders is
// a real if narrow gap in the Rite grammar, filed separately (NIM-280) rather
// than papered over here with a membership check the model does not have.
func TestIntegration_AugurCovenRiteAuthorizesIncarnationMembers(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, covenAxisInc)
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — a member must match a Rite scoped to its incarnation", got)
	}
}

// TestIntegration_AugurCovenRiteDeniesUnboundHost — the same Rite and the same
// host without the membership row: nothing to inherit, no match, default-deny.
func TestIntegration_AugurCovenRiteDeniesUnboundHost(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, covenAxisInc)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — no membership and no tag must not authorize", got)
	}
}

// TestIntegration_AugurIncarnationTagReachesMembers — a tag put on the
// incarnation, on no host at all, authorizes its members: `coven: cache` covers
// every host of every incarnation tagged `cache`. This is the widening ADR-080
// declared, and the reason the subject side must read the union rather than the
// relation.
func TestIntegration_AugurIncarnationTagReachesMembers(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, []string{"cache"})
	seedCovenAxisRite(t, ctx, "cache")
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — an incarnation's tag is inherited by its members", got)
	}
	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — a non-member inherits nothing from the incarnation", got)
	}
}
