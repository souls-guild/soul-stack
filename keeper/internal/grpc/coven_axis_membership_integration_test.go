//go:build integration

// The coven axis on live PG: telemetry delivery (NIM-248) and Augur subject
// resolution (NIM-249). Both are consumers NIM-124 left behind when it moved
// host↔incarnation membership out of `souls.coven[]` into
// `incarnation_membership`, and both fail in the shape that migration made
// possible — the column still exists, the query still succeeds, the match set
// is simply empty.
//
// The two of them ask DIFFERENT questions, which is the thing these tests are
// here to keep straight:
//
//   - telemetry asks "which incarnation is this host owed a config from" — that
//     is MEMBERSHIP, and it must be read from the relation, because a coven tag
//     is a label anyone may attach, a host tagged with an incarnation's name
//     included;
//   - Augur asks "may this Rite see this host" — that is LABELS, and it reads
//     `souls.coven[]`, the tags an operator attached to the host (NIM-281). An
//     incarnation's own labels describe the incarnation; they reach no host.
//
// The label half of the TELEMETRY axis is a different thing again, and worth not
// confusing with either: since ADR-0082 a service's vars have no hard-wired
// coven dimension, so the overlay claim is proved over a `vars/_stack.yaml`
// foreach step reading `incarnation.covens` — the incarnation's own tags
// selecting overlays of the incarnation's own config, never a label on a host.
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
// That outsider is the whole point of the fixture: it holds the incarnation's
// name as an ordinary host tag and no membership row, so any consumer that
// answers a membership question from labels serves it — and, read the other way
// round, any consumer that answers a label question from the relation serves the
// member instead. One fixture, both mistakes.
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
// so the overlay's effect is observable in the result.
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

// TestIntegration_TelemetryIncarnationCovenSelectsServiceVarsOverlay — the other
// half of the split, and the one most easily misread as inheritance. Membership
// decides WHICH incarnation's config the host is owed; the incarnation's own
// labels decide which overlays of THAT config apply. Nothing is projected onto
// the host: the tag lives on the incarnation, the overlay is chosen while
// resolving the incarnation's vars, and the host is merely served the result. A
// delivery that dropped `incarnation.covens` on the way to the resolver would
// serve the manifest's 45s instead of the overlay's 15s.
func TestIntegration_TelemetryIncarnationCovenSelectsServiceVarsOverlay(t *testing.T) {
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

// TestIntegration_AugurCovenRiteMatchesTagsNotMembership — a Rite's subject is a
// LABEL selector, `coven` XOR `sid`, and under NIM-281 a label is only ever
// where an operator put it. Binding a host to an incarnation attaches nothing,
// so `coven: redis-prod` reaches the host TAGGED `redis-prod` and not the host
// BOUND to the incarnation of that name — the fixture's two hosts differ in
// exactly that, and the assertions run in exactly opposite directions.
//
// The reading to resist is that the denial below is the NIM-249 defect
// returning. It is not: NIM-249 was the subject resolving from a relation that
// no longer carried tags, and it denied everyone. Here the tag holder is served,
// which is what tells the two apart.
//
// What genuinely has no spelling now is "every member of incarnation X" as an
// Augur subject — the Rite grammar has no incarnation dimension, and NIM-281
// removed the tag projection that used to stand in for one. That gap is filed
// separately (NIM-280) rather than papered over here with a membership check the
// subject model does not have.
func TestIntegration_AugurCovenRiteMatchesTagsNotMembership(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, covenAxisInc)
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — the host an operator tagged %q must match a Rite scoped to it",
			got, covenAxisInc)
	}
	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — a bind must not hand the host its incarnation's name as a tag", got)
	}
}

// TestIntegration_AugurCovenRiteDeniesUntaggedHost — the same Rite and a host
// carrying neither the tag nor the membership row: no match, default-deny.
// Paired with the test above it pins that the tag alone does the work, with
// membership neither helping nor being required.
func TestIntegration_AugurCovenRiteDeniesUntaggedHost(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, covenAxisInc)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — no tag must not authorize", got)
	}
}

// TestIntegration_AugurIncarnationTagReachesNoHost — a tag put on the
// incarnation and on no host reaches nobody, member or not. This is the
// widening NIM-281 removed: `coven: cache` used to cover every host of every
// incarnation tagged `cache`, an authorization an operator never granted on any
// host and could not see by looking at one.
//
// The last leg tags the member itself and expects OK, so a green run cannot be
// explained by a broken fixture — the same host, same Rite, one operator-made
// tag apart.
func TestIntegration_AugurIncarnationTagReachesNoHost(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, []string{"cache"})
	seedCovenAxisRite(t, ctx, "cache")
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — an incarnation's tag must not reach its members", got)
	}
	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — an incarnation's tag must not reach a non-member either", got)
	}

	if _, err := integrationPool.Exec(ctx,
		`UPDATE souls SET coven = ARRAY['linux', 'cache'] WHERE sid = $1`, covenAxisMember); err != nil {
		t.Fatalf("tag member with cache: %v", err)
	}
	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — the operator tagged this host %q by hand", got, "cache")
	}
}
