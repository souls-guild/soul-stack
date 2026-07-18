//go:build integration

package incarnation

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
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
