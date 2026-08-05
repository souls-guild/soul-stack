//go:build integration

// Oracle subject resolution against live PG (NIM-224, narrowed by NIM-281,
// widened on the subject axis by NIM-280). Two questions run in sequence here
// and must not be confused; each has its own tests below:
//
//   - the SUBJECT match reads a label on TWO levels (NIM-280): the tags an
//     operator attached to the host (`souls.coven[]`), and the same tag on an
//     incarnation the host is a member of (`incarnation.covens`), which reaches
//     every host on that roster. The union is formed for the duration of one
//     match and nothing is written to `souls`. A name is still not a label on
//     either level, so `subject_coven: [<incarnation-name>]` reaches only hosts
//     an operator actually tagged with that string.
//   - the MEMBERSHIP gate reads `incarnation_membership` directly, because a
//     coven tag is a label anyone holding `soul.coven-assign` may attach and so
//     cannot decide who belongs where.
//
// Together they mean a Decree fires on a host only if a label reached it — its
// own or its incarnation's — AND the roster says it belongs. Targeting is what
// NIM-280 widened; authorization was not, and the tests below hold that line.
//
// Live PG rather than the fake: the questions are answered from two different
// relations, and a fake that answers both from one field cannot fail the way
// production did at NIM-124.

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
// Vigil/Decree scoped on the coven axis. Membership is bound separately by each
// test, so the two acts an operator performs — tagging a host and binding it —
// stay independently controllable, which is the point of every test here.
//
// memberCovens are the tags an operator attached to membershipMember, on top of
// the `linux` both hosts carry. incarnationCovens are the tags carried by the
// incarnation itself; a subject reads them as the second level and they reach
// every host on the roster (NIM-280), while an RBAC scope never sees them. The
// two are kept separate here so a test can say which level it is exercising.
func seedMembershipFixture(t *testing.T, ctx context.Context, subjectCoven, memberCovens, incarnationCovens []string) *eventStreamHandler {
	t.Helper()
	resetOracleCrossSide(t)

	if err := operator.Insert(ctx, integrationPool, &operator.Operator{
		AID: membershipAID, DisplayName: membershipAID, AuthMethod: operator.AuthMethodJWT,
	}); err != nil {
		t.Fatalf("operator.Insert: %v", err)
	}
	creator := membershipAID
	for _, sid := range []string{membershipMember, membershipOutside} {
		covens := []string{"linux"}
		if sid == membershipMember {
			covens = append(covens, memberCovens...)
		}
		if err := soul.Insert(ctx, integrationPool, &soul.Soul{
			SID: sid, Transport: soul.TransportAgent, Status: soul.StatusConnected,
			Coven: covens, CreatedByAID: &creator,
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

// TestIntegration_OracleSubject_HitsExactlyTaggedHosts — a Decree scoped
// `subject_coven: [cache]` fires for the host an operator tagged `cache` and
// bound to the target incarnation, and for no other. Both hosts are otherwise
// identical (same `linux` tag, same registry status), so the only thing
// separating them is the tag the operator attached.
func TestIntegration_OracleSubject_HitsExactlyTaggedHosts(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{"cache"}, []string{"cache"}, nil)
	bindMember(t, ctx, membershipMember)
	bindMember(t, ctx, membershipOutside)

	emit(t, ctx, h, membershipMember)
	emit(t, ctx, h, membershipOutside)

	if !firedFor(t, ctx, membershipMember) {
		t.Error("a host tagged `cache` and bound to the incarnation must match subject_coven: [cache]")
	}
	if firedFor(t, ctx, membershipOutside) {
		t.Error("an untagged host must not match — membership is not a tag")
	}
}

// TestIntegration_OracleSubject_IncarnationNameIsNotALabel — the NIM-281 guard
// on the reactor, kept exactly where NIM-280 did NOT widen. A Decree scoped
// `subject_coven: [redis-prod]` — the incarnation's NAME — fires on nobody. The
// member is bound and the incarnation carries a tag, but the tag is `cache`, and
// a name is a label on neither level: not on the host, not on the incarnation.
// Reaching an incarnation by its own identity is the `incarnation=` dimension.
//
// Distinct from TestIntegration_OracleMembershipGate_CovenTagIsNotMembership
// below: there the string IS a real tag an operator attached to a host and the
// MEMBERSHIP gate refuses; here nothing carries the string at all, so the
// SUBJECT match itself must miss.
func TestIntegration_OracleSubject_IncarnationNameIsNotALabel(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil, []string{"cache"})
	bindMember(t, ctx, membershipMember)

	emit(t, ctx, h, membershipMember)
	emit(t, ctx, h, membershipOutside)

	if firedFor(t, ctx, membershipMember) {
		t.Error("a member matched a subject spelled like its incarnation's NAME — a name is not a label on the host or on the incarnation")
	}
	if firedFor(t, ctx, membershipOutside) {
		t.Error("a non-member matched an incarnation-name-spelled subject")
	}
}

// TestIntegration_OracleSubject_IncarnationTagReachesMembers — the other half of
// the NIM-280 widening, and the one worth pinning down: a Decree scoped
// `subject_coven: [cache]` fires for a host that carries NO tag of its own, on
// the strength of the tag its incarnation carries. The union is formed at match
// time; nothing is written to `souls`.
//
// The non-member is the control. It carries no tag either and is not on the
// roster, so it must stay silent — the second level reaches an incarnation's
// MEMBERS, not everyone.
//
// ⚠ This is the escalation-shaped edge NIM-280 accepted deliberately: tagging an
// incarnation `cache` widens every existing `coven=cache` subject to its whole
// roster without editing a single rule. It is declared behaviour, so it is
// tested positively — a later reading of this file must not "restore" the old
// assertion and quietly narrow targeting back.
func TestIntegration_OracleSubject_IncarnationTagReachesMembers(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{"cache"}, nil, []string{"cache"})
	bindMember(t, ctx, membershipMember)

	emit(t, ctx, h, membershipMember)
	emit(t, ctx, h, membershipOutside)

	if !firedFor(t, ctx, membershipMember) {
		t.Error("an untagged member did not match its incarnation's tag — the subject's second level does not reach the roster (NIM-280)")
	}
	if firedFor(t, ctx, membershipOutside) {
		t.Error("a non-member matched the incarnation's tag — the second level must reach members only")
	}
}

// TestIntegration_OracleSubject_UnboundHostStopsMatching — the membership gate is
// read live, not captured when the Decree was written: unbinding a host stops it
// matching a rule that already exists, even though its tag is untouched. The
// cooldown row from the first fire is cleared so the second emit is judged on its
// own (a live cooldown would suppress it for an unrelated reason).
func TestIntegration_OracleSubject_UnboundHostStopsMatching(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{"cache"}, []string{"cache"}, nil)
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
// other direction: a tagged host bound AFTER the rule was created starts
// matching it, with no re-write of the Decree. The gate is a live read of the
// relation, not a snapshot taken when the Decree was written.
func TestIntegration_OracleSubject_MemberBoundAfterDecreeStartsMatching(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{"cache"}, []string{"cache"}, nil)

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
// separation that makes the subject axis safe to widen. A host carries a coven
// tag spelled exactly like the target incarnation but holds no membership row.
// The subject match passes — the tag is a real label an operator attached — and
// the membership gate must still refuse: otherwise anyone who can put a coven
// tag on a host, or any host that carried the name before migration 099 stripped
// it, could have that incarnation's scenarios enqueued on it
// (cross-incarnation escalation, ADR-030(b)).
//
// This is why the gate reads the relation and never the covens: a label is not a
// membership decision, whoever attached it.
func TestIntegration_OracleMembershipGate_CovenTagIsNotMembership(t *testing.T) {
	ctx := context.Background()
	h := seedMembershipFixture(t, ctx, []string{membershipInc}, nil, nil)

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

// TestIntegration_OracleVigilSource_MatchesTheDecreeSubject — the quiet half of
// the chain. The Vigil must reach exactly the hosts the Decree can fire on: if
// it does not, the host never runs the check, no Portent is ever emitted, and
// the Decree side is never even consulted — a failure with no error anywhere.
// Both halves read `souls.coven[]`, so a host tagged for the rule gets the check
// and an untagged member gets nothing. Asserted through the real [VigilSource]
// against live PG.
func TestIntegration_OracleVigilSource_MatchesTheDecreeSubject(t *testing.T) {
	ctx := context.Background()
	seedMembershipFixture(t, ctx, []string{"cache"}, []string{"cache"}, nil)
	bindMember(t, ctx, membershipMember)
	bindMember(t, ctx, membershipOutside)

	src := NewVigilSource(integrationPool)

	defs, err := src.ActiveVigilsForSID(ctx, membershipMember)
	if err != nil {
		t.Fatalf("ActiveVigilsForSID(tagged): %v", err)
	}
	if len(defs) != 1 || defs[0].GetName() != membershipBeacon {
		t.Errorf("the tagged host should receive the Vigil, got %d defs (%+v)", len(defs), defs)
	}

	outside, err := src.ActiveVigilsForSID(ctx, membershipOutside)
	if err != nil {
		t.Fatalf("ActiveVigilsForSID(untagged): %v", err)
	}
	if len(outside) != 0 {
		t.Errorf("an untagged member should receive no Vigil, got %d — membership must not broadcast a check", len(outside))
	}
}
