//go:build integration

// Oracle subject resolution over the membership relation (NIM-224), on live
// PG. These assert the seam NIM-124 left behind: it moved host↔incarnation
// membership out of `souls.coven[]` into `incarnation_membership` and
// converted the roster, the bulk soul selector, the Choir check and form-prep
// — but not the Oracle. A Decree scoped to an incarnation therefore matched
// nothing, silently: the column still existed, the query still succeeded, the
// intersection was simply always empty.
//
// The fix has two halves that must not be confused, and each half has its own
// tests below:
//
//   - the SUBJECT match reads the effective label union (own ∪ inherited,
//     ADR-080), so `subject_coven: [<incarnation>]` reaches that
//     incarnation's members;
//   - the MEMBERSHIP gate reads `incarnation_membership` directly, because
//     the union deliberately admits host-attached tags and so cannot decide
//     who belongs where.
//
// Live PG rather than the fake: the defect was in which relation was read,
// and a fake that answers both questions from one field cannot fail the way
// production did.

package grpc

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

const (
	membershipAID     = "archon-alice"
	membershipInc     = "redis-prod"
	membershipSvc     = "redis-svc"
	membershipBeacon  = "svc-down"
	membershipDecree  = "restart-redis"
	membershipMember  = "member.example.com"
	membershipOutside = "outsider.example.com"
)

// seedMembershipFixture seeds operator + two hosts + an incarnation + a
// Vigil/Decree scoped to the INCARNATION NAME on the coven axis — the shape
// an operator writes for "react on the hosts of this incarnation", and the
// shape NIM-124 broke. Neither host carries the name in `souls.coven[]`;
// membership is bound separately by each test.
//
// incarnationCovens are the tags carried by the incarnation itself, inherited
// by its members on top of its name (ADR-080).
func seedMembershipFixture(t *testing.T, ctx context.Context, subjectCoven []string, incarnationCovens []string) *eventStreamHandler {
	t.Helper()
	resetOracleCrossSide(t)

	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: membershipAID, DisplayName: membershipAID, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := membershipAID
	for _, sid := range []string{membershipMember, membershipOutside} {
		if err := soul.Insert(ctx, integrationPool, &soul.Soul{
			SID: sid, Transport: soul.TransportAgent, Status: soul.StatusConnected,
			Coven: []string{"linux"}, CreatedByAID: &creator,
		}); err != nil {
			t.Fatalf("soul.Insert(%s): %v", sid, err)
		}
	}
	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		Name: membershipInc, Service: membershipSvc, ServiceVersion: "v1",
		StateSchemaVersion: 1, State: map[string]any{},
		Status: incarnation.StatusReady, Covens: incarnationCovens, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
	if err := oracle.InsertVigil(ctx, integrationPool, &oracle.Vigil{
		Name: membershipBeacon, Coven: subjectCoven, IntervalSpec: "30s",
		CheckAddr: "core.beacon.service_down", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertVigil: %v", err)
	}
	if err := oracle.InsertDecree(ctx, integrationPool, &oracle.Decree{
		Name: membershipDecree, OnBeacon: membershipBeacon,
		SubjectCoven: subjectCoven, IncarnationName: membershipInc,
		ActionScenario: "restart_service", Cooldown: "5m", Enabled: true, CreatedByAID: &creator,
	}); err != nil {
		t.Fatalf("InsertDecree: %v", err)
	}

	h, _ := newCrossSideHandler(t, fixedResolver{
		service: membershipSvc,
		ref:     artifact.ServiceRef{Name: membershipSvc, Git: "file:///srv/redis", Ref: "main"},
	})
	return h
}

// bindMember binds one host to the fixture's incarnation.
func bindMember(t *testing.T, ctx context.Context, sid string) {
	t.Helper()
	by := membershipAID
	if err := incarnation.AddMembers(ctx, integrationPool, membershipInc, []string{sid}, &by); err != nil {
		t.Fatalf("AddMembers(%s): %v", sid, err)
	}
}

// firedFor reports whether the reactor fired for a host — read from
// oracle_fires, the cooldown state written only after a successful enqueue.
func firedFor(t *testing.T, ctx context.Context, sid string) bool {
	t.Helper()
	_, hasFired, err := oracle.LastFiredAt(ctx, integrationPool, membershipDecree, sid)
	if err != nil {
		t.Fatalf("LastFiredAt(%s): %v", sid, err)
	}
	return hasFired
}

// emit sends a Portent as the given host over the handler.
func emit(t *testing.T, ctx context.Context, h *eventStreamHandler, sid string) {
	t.Helper()
	h.handlePortentEvent(ctx, sid, "session-"+sid,
		soulSchedulerPortent(t, membershipBeacon, sid, nil))
}

// TestIntegration_OracleSubject_HitsExactlyIncarnationMembers — a Decree
// scoped `subject_coven: [<incarnation name>]` fires for a host bound to that
// incarnation and for no other. Both hosts are otherwise identical (same own
// coven `linux`, same registry status), so the only thing separating them is
// the membership row. This is the case that regressed at NIM-124: before the
// fix NEITHER host matched, because the name lives in no host's column any
// more.
func TestIntegration_OracleSubject_HitsExactlyIncarnationMembers(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil)
	bindMember(t, ctx, membershipMember)

	emit(t, ctx, h, membershipMember)
	emit(t, ctx, h, membershipOutside)

	if !firedFor(t, ctx, membershipMember) {
		t.Error("member of the target incarnation must match a subject_coven scoped to its name")
	}
	if firedFor(t, ctx, membershipOutside) {
		t.Error("a host outside the incarnation must not match")
	}
}

// TestIntegration_OracleSubject_InheritsIncarnationCovens — the union is not
// only the incarnation's NAME: a tag carried by the incarnation itself
// (`incarnation.covens[]`) reaches its members too, so a Decree scoped
// `subject_coven: [cache]` fires on the hosts of an incarnation tagged
// `cache` without that tag being stamped onto any host.
func TestIntegration_OracleSubject_InheritsIncarnationCovens(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{"cache"}, []string{"cache"})
	bindMember(t, ctx, membershipMember)

	emit(t, ctx, h, membershipMember)
	emit(t, ctx, h, membershipOutside)

	if !firedFor(t, ctx, membershipMember) {
		t.Error("a tag on the incarnation must reach its members (ADR-080 union)")
	}
	if firedFor(t, ctx, membershipOutside) {
		t.Error("a non-member inherits nothing from the incarnation")
	}
}

// TestIntegration_OracleSubject_UnboundHostStopsMatching — membership is read
// live, not captured when the Decree was written: unbinding a host stops it
// matching a rule that already exists. The cooldown row from the first fire
// is cleared so the second emit is judged on its own (a live cooldown would
// suppress it for an unrelated reason).
func TestIntegration_OracleSubject_UnboundHostStopsMatching(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil)
	bindMember(t, ctx, membershipMember)

	emit(t, ctx, h, membershipMember)
	if !firedFor(t, ctx, membershipMember) {
		t.Fatal("precondition: a bound host must fire")
	}

	if err := incarnation.RemoveMembers(ctx, integrationPool, membershipInc, []string{membershipMember}); err != nil {
		t.Fatalf("RemoveMembers: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `DELETE FROM oracle_fires`); err != nil {
		t.Fatalf("clear oracle_fires: %v", err)
	}

	emit(t, ctx, h, membershipMember)
	if firedFor(t, ctx, membershipMember) {
		t.Error("a host removed from the incarnation must stop matching")
	}
}

// TestIntegration_OracleSubject_MemberBoundAfterDecreeStartsMatching — the
// other direction: a host bound AFTER the rule was created starts matching it,
// with no re-write of the Decree. This is the property that makes labelling
// the incarnation the recommended shape (ADR-080) — a rule covers whoever
// joins later.
func TestIntegration_OracleSubject_MemberBoundAfterDecreeStartsMatching(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil)

	emit(t, ctx, h, membershipMember)
	if firedFor(t, ctx, membershipMember) {
		t.Fatal("precondition: an unbound host must not fire")
	}

	bindMember(t, ctx, membershipMember)
	emit(t, ctx, h, membershipMember)

	if !firedFor(t, ctx, membershipMember) {
		t.Error("a host bound after the Decree was created must start matching it")
	}
}

// TestIntegration_OracleMembershipGate_CovenTagIsNotMembership — the
// separation that makes the fix safe. A host carries a HOST-ATTACHED coven tag
// spelled exactly like the target incarnation but holds no membership row.
// The subject match passes (the union is a label question and the tag is a
// real label), and the membership gate must still refuse: otherwise anyone who
// can put a coven tag on a host — or any host that carried the name before
// migration 099 stripped it — could have that incarnation's scenarios enqueued
// on it (cross-incarnation escalation, ADR-030(b)).
//
// This is why the gate reads the relation rather than the resolved covens:
// answering it from the union would turn the ADR-080 widening into a privilege
// hole.
func TestIntegration_OracleMembershipGate_CovenTagIsNotMembership(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil)

	// Stamp the incarnation's name onto the outsider as an ordinary host tag.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE souls SET coven = $1 WHERE sid = $2`,
		[]string{"linux", membershipInc}, membershipOutside); err != nil {
		t.Fatalf("stamp coven: %v", err)
	}

	emit(t, ctx, h, membershipOutside)

	if firedFor(t, ctx, membershipOutside) {
		t.Error("a coven tag spelled like an incarnation must not pass the membership gate")
	}
	var runs int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM apply_runs WHERE sid = $1`, membershipOutside).Scan(&runs); err != nil {
		t.Fatalf("count apply_runs: %v", err)
	}
	if runs != 0 {
		t.Errorf("no scenario may be enqueued for a non-member, got %d apply_runs", runs)
	}
}

// TestIntegration_OracleVigilSource_ReachesIncarnationMembers — the quiet half
// of the chain. A Vigil scoped to the incarnation must be broadcast to its
// members: if it is not, the host never runs the check, no Portent is ever
// emitted, and the Decree side is never even consulted — a failure with no
// error anywhere. Asserted through the real [VigilSource] against live PG.
func TestIntegration_OracleVigilSource_ReachesIncarnationMembers(t *testing.T) {
	ctx := context.Background()
	seedMembershipFixture(t, ctx, []string{membershipInc}, nil)
	bindMember(t, ctx, membershipMember)

	src := NewVigilSource(integrationPool)

	defs, err := src.ActiveVigilsForSID(ctx, membershipMember)
	if err != nil {
		t.Fatalf("ActiveVigilsForSID(member): %v", err)
	}
	if len(defs) != 1 || defs[0].GetName() != membershipBeacon {
		t.Errorf("member should receive the incarnation-scoped Vigil, got %d defs (%+v)", len(defs), defs)
	}

	outside, err := src.ActiveVigilsForSID(ctx, membershipOutside)
	if err != nil {
		t.Fatalf("ActiveVigilsForSID(outsider): %v", err)
	}
	if len(outside) != 0 {
		t.Errorf("a host outside the incarnation should receive no Vigil, got %d", len(outside))
	}
}
