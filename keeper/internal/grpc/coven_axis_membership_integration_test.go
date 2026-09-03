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
//   - Augur asks "may this Rite see this host" — that is the rule's SUBJECT, one
//     of four dimensions (NIM-280), resolved at match time over the host's own
//     labels AND the labels of every incarnation it is a member of.
//
// NIM-280 moved the second bullet. Before it, a Rite's subject read `souls.coven[]`
// and nothing else, so an incarnation's tag reached no host; now `coven:` reaches
// members too, and `incarnation:` addresses the roster by the relation. What did
// NOT move is the direction of the first bullet: membership is still read from
// `incarnation_membership`, so a host merely TAGGED with an incarnation's name is
// still not a member — the incarnation's NAME is an identity, never a label.
//
// ⚠ The widening is real and deliberate: an operator who tags an incarnation
// `cache` grants every Rite scoped to `coven: cache` over every host on that
// roster, without editing a Rite. It is a targeting decision on a secret-reading
// path, which is why `TestIntegration_AugurIncarnationCovenReachesMembers` below
// states it as an assertion rather than leaving it to the SQL.
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
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
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
		ID: covenAxisInc, Service: covenAxisSvc, ServiceVersion: "v1",
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

// seedCovenAxisRite adds an Omen and one Rite carrying the given subject,
// allowing exactly covenAxisPath.
func seedCovenAxisRite(t *testing.T, ctx context.Context, sel subject.Selector) {
	t.Helper()
	creator := covenAxisAID
	if err := augur.InsertOmen(ctx, integrationPool, &augur.Omen{
		ID: covenAxisOmen, SourceType: augur.SourceVault,
		Endpoint: "https://vault:8200", AuthRef: "vault:secret/keeper/augur/" + covenAxisOmen,
		CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertOmen: %v", err)
	}
	r := &augur.Rite{Omen: covenAxisOmen, Allow: augurAllowPaths(covenAxisPath), CreatedByAID: &creator}
	r.SetSubject(sel)
	if err := augur.InsertRite(ctx, integrationPool, r); err != nil {
		t.Fatalf("InsertRite(%s): %v", sel, err)
	}
}

func covenRiteSelector(coven string) subject.Selector {
	return subject.Selector{Covens: []string{coven}}
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

// TestIntegration_AugurCovenRiteMatchesTagsNotMembership — the incarnation's
// NAME is not one of its labels. `coven: redis-prod` reaches the host an
// operator TAGGED `redis-prod`; the host BOUND to the incarnation of that name
// gets nothing, because the fixture's incarnation carries no coven at all and a
// name is an identity, addressed through the `incarnation` dimension instead.
//
// This survived NIM-280 unchanged, and that is the point of keeping it next to
// the tests below: two-level resolution reads the incarnation's LABELS, and it
// still does not manufacture one out of the incarnation's name. The escalation
// NIM-281 closed — a host tagged with a name being indistinguishable from a
// member — stays closed.
//
// The reading to resist is that the denial below is the NIM-249 defect
// returning. It is not: NIM-249 was the subject resolving from a relation that
// no longer carried tags, and it denied everyone. Here the tag holder is served,
// which is what tells the two apart.
func TestIntegration_AugurCovenRiteMatchesTagsNotMembership(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, covenRiteSelector(covenAxisInc))
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
	seedCovenAxisRite(t, ctx, covenRiteSelector(covenAxisInc))

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — no tag must not authorize", got)
	}
}

// TestIntegration_AugurIncarnationCovenReachesMembers — the assertion NIM-280
// reversed, on the path where reversing it matters most: a Rite is the grant
// that lets a host read a secret out of Vault.
//
// A tag on the INCARNATION now reaches every host on its roster, resolved at
// match time. The member is served and the tagged-by-name outsider is not,
// because the outsider is on nobody's roster — so the grant follows the
// relation, exactly one hop, and not the string.
//
// ⚠ What an operator is agreeing to: tagging an incarnation `cache` widens every
// existing `coven: cache` Rite over that roster with no Rite edited and no host
// touched. That is why the second leg below runs — the widening must stop at
// membership, or `coven:` on this path would be a fleet-wide grant.
//
// Nothing is written to `souls`: the last leg re-reads the host's own labels
// through an RBAC-shaped question and finds them unchanged.
func TestIntegration_AugurIncarnationCovenReachesMembers(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, []string{"cache"})
	seedCovenAxisRite(t, ctx, covenRiteSelector("cache"))
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — an incarnation's label must reach the hosts on its roster (NIM-280)", got)
	}
	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — a non-member must not be reached by the incarnation's label", got)
	}

	// The host's own column is untouched: the union happens while matching, never
	// by writing the label onto the host. An RBAC scope, soulprint.self.covens and
	// the push provider choice all read this column and must see what an operator
	// put there — `linux`, and nothing else.
	var own []string
	if err := integrationPool.QueryRow(ctx,
		`SELECT coven FROM souls WHERE sid = $1`, covenAxisMember).Scan(&own); err != nil {
		t.Fatalf("read souls.coven: %v", err)
	}
	if len(own) != 1 || own[0] != "linux" {
		t.Errorf("souls.coven = %v, want [linux] — the match must not write the incarnation's label onto the host", own)
	}
}

// TestIntegration_AugurIncarnationRiteAddressesTheRoster — the dimension NIM-280
// added, and the one that restores what NIM-281 took away without bringing back
// the ambiguity. `incarnation: redis-svc.redis-prod` reads the membership
// relation, so the bound host is served and the host merely TAGGED with the
// incarnation's name is not — the opposite direction to the coven test above,
// over the very same pair of hosts.
//
// The service half is load-bearing: it is addressed as a PAIR so that incarnation
// names may stop being unique without every Rite silently widening to same-named
// incarnations of other services. The last leg proves the pair is actually
// compared by pointing a Rite at the right name under the wrong service.
func TestIntegration_AugurIncarnationRiteAddressesTheRoster(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, subject.Selector{Service: covenAxisSvc, Incarnation: covenAxisInc})
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — a member of the addressed incarnation must be served", got)
	}
	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — a host merely TAGGED %q is on no roster", got, covenAxisInc)
	}

	// Same incarnation name, a different service: the pair does not match, so the
	// member loses the grant it had a moment ago.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE rites SET service = 'other-svc' WHERE omen = $1`, covenAxisOmen); err != nil {
		t.Fatalf("repoint rite at another service: %v", err)
	}
	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — the incarnation address is the (service, name) PAIR", got)
	}
}

// TestIntegration_AugurIncarnationTraitReachesMembers — the fourth dimension,
// same shape as the coven one: a trait set on the incarnation reaches its
// members, and reaches nobody else.
func TestIntegration_AugurIncarnationTraitReachesMembers(t *testing.T) {
	ctx := context.Background()
	seedCovenAxis(t, ctx, nil)
	seedCovenAxisRite(t, ctx, subject.Selector{TraitKey: "owner", TraitValue: "dba"})
	bindCovenAxisMember(t, ctx, covenAxisMember)

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Fatalf("precondition: status = %v, want DENIED before the trait is set anywhere", got)
	}
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET traits = '{"owner":"dba"}'::jsonb WHERE id = $1`, covenAxisInc); err != nil {
		t.Fatalf("set incarnation trait: %v", err)
	}

	if got := augurStatusFor(t, ctx, covenAxisMember); got != keeperv1.AugurStatus_AUGUR_STATUS_OK {
		t.Errorf("status = %v, want OK — an incarnation's trait must reach the hosts on its roster (NIM-280)", got)
	}
	if got := augurStatusFor(t, ctx, covenAxisOutsider); got != keeperv1.AugurStatus_AUGUR_STATUS_DENIED {
		t.Errorf("status = %v, want DENIED — a non-member is not reached by the incarnation's trait", got)
	}
}
