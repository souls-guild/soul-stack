//go:build integration

package incarnation

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// seedMembershipSoul inserts a bare souls row (FK target for membership).
func seedMembershipSoul(t *testing.T, sid string) {
	t.Helper()
	s := &soul.Soul{SID: sid, Status: soul.StatusConnected}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seed soul %s: %v", sid, err)
	}
}

func seedMembershipIncarnation(t *testing.T, name string) {
	t.Helper()
	inc := &Incarnation{
		Name:               name,
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Status:             StatusReady,
	}
	if err := Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seed incarnation %s: %v", name, err)
	}
}

// TestIntegration_Membership_AddListRemoveIdempotent covers the membership store
// (NIM-124): add, list (sorted), idempotent re-add, remove.
func TestIntegration_Membership_AddListRemoveIdempotent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	seedMembershipIncarnation(t, "redis-prod")
	seedMembershipSoul(t, "b.example.com")
	seedMembershipSoul(t, "a.example.com")

	aid := "archon-alice"
	if err := AddMembers(ctx, integrationPool, "redis-prod", []string{"b.example.com", "a.example.com"}, &aid); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
	got, err := ListMemberSIDs(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("ListMemberSIDs: %v", err)
	}
	if len(got) != 2 || got[0] != "a.example.com" || got[1] != "b.example.com" {
		t.Fatalf("members = %v, want [a b] sorted", got)
	}

	// Idempotent: re-adding the same SIDs is a no-op (ON CONFLICT DO NOTHING).
	if err := AddMembers(ctx, integrationPool, "redis-prod", []string{"a.example.com", "b.example.com"}, &aid); err != nil {
		t.Fatalf("AddMembers (idempotent): %v", err)
	}
	got, _ = ListMemberSIDs(ctx, integrationPool, "redis-prod")
	if len(got) != 2 {
		t.Fatalf("after idempotent re-add: members = %v, want 2", got)
	}

	// Remove one.
	if err := RemoveMembers(ctx, integrationPool, "redis-prod", []string{"a.example.com"}); err != nil {
		t.Fatalf("RemoveMembers: %v", err)
	}
	got, _ = ListMemberSIDs(ctx, integrationPool, "redis-prod")
	if len(got) != 1 || got[0] != "b.example.com" {
		t.Fatalf("after remove: members = %v, want [b]", got)
	}
}

// TestIntegration_Membership_CascadeOnSoulDelete — FK sid → souls ON DELETE
// CASCADE: deleting a soul removes its memberships.
func TestIntegration_Membership_CascadeOnSoulDelete(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedMembershipIncarnation(t, "redis-prod")
	seedMembershipSoul(t, "a.example.com")
	if err := AddMembers(ctx, integrationPool, "redis-prod", []string{"a.example.com"}, nil); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls WHERE sid = $1`, "a.example.com"); err != nil {
		t.Fatalf("delete soul: %v", err)
	}
	got, err := ListMemberSIDs(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("ListMemberSIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("members = %v, want [] (cascade on soul delete)", got)
	}
}

// TestIntegration_Membership_CascadeOnIncarnationDelete — FK incarnation_name →
// incarnation ON DELETE CASCADE: deleting the incarnation removes its
// memberships.
func TestIntegration_Membership_CascadeOnIncarnationDelete(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedMembershipIncarnation(t, "redis-prod")
	seedMembershipSoul(t, "a.example.com")
	if err := AddMembers(ctx, integrationPool, "redis-prod", []string{"a.example.com"}, nil); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM incarnation WHERE name = $1`, "redis-prod"); err != nil {
		t.Fatalf("delete incarnation: %v", err)
	}
	var n int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM incarnation_membership WHERE incarnation_name = $1`, "redis-prod").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("membership rows = %d, want 0 (cascade on incarnation delete)", n)
	}
}

// TestIntegration_Membership_ListAndScreen — the operator-facing reads of the
// membership relation against a real schema: [ListMembers] (the JOIN onto souls
// plus the jsonb/timestamptz scan) and [ScreenBindCandidates] (the per-host gate
// the operator bind runs before writing anything). NIM-209.
func TestIntegration_Membership_ListAndScreen(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	seedMembershipIncarnation(t, "redis-prod")
	seedMembershipSoul(t, "node-1.example.com")
	seedMembershipSoul(t, "node-2.example.com")

	aid := "archon-alice"
	if err := AddMembers(ctx, integrationPool, "redis-prod",
		[]string{"node-1.example.com", "node-2.example.com"}, &aid); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}

	members, err := ListMembers(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	if members[0].SID != "node-1.example.com" || members[1].SID != "node-2.example.com" {
		t.Fatalf("members = %v, want sorted by SID", members)
	}
	if members[0].Status != string(soul.StatusConnected) {
		t.Errorf("status = %q, want connected (the host's status, joined from souls)", members[0].Status)
	}
	if members[0].BoundAt.IsZero() {
		t.Error("bound_at is zero — the membership audit column did not scan")
	}
	if members[0].BoundByAID == nil || *members[0].BoundByAID != aid {
		t.Errorf("bound_by_aid = %v, want %q", members[0].BoundByAID, aid)
	}

	// Screening with an unrestricted scope: everything passes.
	rej, err := ScreenBindCandidates(ctx, integrationPool,
		[]string{"node-1.example.com", "node-2.example.com"}, soulpurview.Resolve(rbac.Purview{Unrestricted: true}))
	if err != nil {
		t.Fatalf("ScreenBindCandidates: %v", err)
	}
	if !rej.Empty() {
		t.Fatalf("screening rejected connected, in-scope hosts: %+v", rej)
	}

	// An unknown SID is caught here rather than by the FK.
	rej, err = ScreenBindCandidates(ctx, integrationPool,
		[]string{"ghost.example.com"}, soulpurview.Resolve(rbac.Purview{Unrestricted: true}))
	if err != nil {
		t.Fatalf("ScreenBindCandidates (unknown): %v", err)
	}
	if rej == nil || len(rej.UnknownSIDs) != 1 || rej.UnknownSIDs[0] != "ghost.example.com" {
		t.Fatalf("unknown SID bucket = %+v, want [ghost.example.com]", rej)
	}

	// An empty purview is the fail-closed case: no host is bindable.
	rej, err = ScreenBindCandidates(ctx, integrationPool,
		[]string{"node-1.example.com"}, soulpurview.Resolve(rbac.Purview{}))
	if err != nil {
		t.Fatalf("ScreenBindCandidates (empty scope): %v", err)
	}
	if rej == nil || len(rej.OutOfScope) != 1 {
		t.Fatalf("out-of-scope bucket = %+v, want the host rejected under an empty purview", rej)
	}
}

// TestIntegration_Membership_ScreenJudgesEffectiveCovens pins the bind gate to the
// EFFECTIVE coven set (ADR-080), not to `souls.coven` alone.
//
// A host may be visible purely by inheritance: it carries no tags of its own, but
// belongs to an incarnation that carries `prod`. Every other reader of the coven
// axis — the souls read, the roster resolver, the RBAC pushdown — already sees it
// as prod. If this gate judged the raw column it would refuse a bind the operator
// is plainly entitled to, and the "a where: and a scope check cannot disagree
// about one host" invariant would hold everywhere except here.
//
// The second half is the boundary that must survive: an unlabelled host that
// belongs to nothing inherits nothing, and stays invisible.
func TestIntegration_Membership_ScreenJudgesEffectiveCovens(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	labelled := &Incarnation{
		Name:               "redis-prod",
		Service:            "redis",
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Status:             StatusReady,
		Covens:             []string{"prod"},
	}
	if err := Create(ctx, integrationPool, labelled); err != nil {
		t.Fatalf("seed labelled incarnation: %v", err)
	}
	seedMembershipIncarnation(t, "redis-staging")

	// Neither host carries a coven of its own.
	seedMembershipSoul(t, "inherits.example.com")
	seedMembershipSoul(t, "orphan.example.com")

	aid := "archon-alice"
	if err := AddMembers(ctx, integrationPool, "redis-prod", []string{"inherits.example.com"}, &aid); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}

	expr, err := rbac.ParseScopeExpr("coven=prod")
	if err != nil {
		t.Fatalf("ParseScopeExpr: %v", err)
	}
	prodOnly := soulpurview.Resolve(rbac.Purview{Exprs: []*rbac.ScopeExpr{expr}})

	// Inherited `prod` from redis-prod makes the host bindable elsewhere.
	rej, err := ScreenBindCandidates(ctx, integrationPool, []string{"inherits.example.com"}, prodOnly)
	if err != nil {
		t.Fatalf("ScreenBindCandidates (inherited): %v", err)
	}
	if !rej.Empty() {
		t.Fatalf("host visible only by inheritance was rejected: %+v — the gate is judging souls.coven, not the ADR-080 union", rej)
	}

	// A host that inherits nothing is still out of scope.
	rej, err = ScreenBindCandidates(ctx, integrationPool, []string{"orphan.example.com"}, prodOnly)
	if err != nil {
		t.Fatalf("ScreenBindCandidates (orphan): %v", err)
	}
	if rej == nil || len(rej.OutOfScope) != 1 || rej.OutOfScope[0] != "orphan.example.com" {
		t.Fatalf("out-of-scope bucket = %+v, want [orphan.example.com]", rej)
	}

	// HostInScope (the unbind path) must agree with the screening.
	inScope, known, err := HostInScope(ctx, integrationPool, "inherits.example.com", prodOnly)
	if err != nil {
		t.Fatalf("HostInScope: %v", err)
	}
	if !known || !inScope {
		t.Errorf("HostInScope = (%v, %v), want (true, true) — unbind disagrees with bind about one host", inScope, known)
	}
	if inScope, _, err = HostInScope(ctx, integrationPool, "orphan.example.com", prodOnly); err != nil {
		t.Fatalf("HostInScope (orphan): %v", err)
	}
	if inScope {
		t.Error("orphan host reported in scope")
	}
}
