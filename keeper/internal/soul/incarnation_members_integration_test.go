//go:build integration

package soul

import (
	"context"
	"testing"
)

// TestIntegration_SelectIncarnationMembers_ViaMembershipNotCoven — GUARD
// (NIM-124): incarnation members are listed via incarnation_membership, NOT via
// coven==name. A member whose coven does NOT contain the incarnation name IS
// returned; a host whose coven contains a string equal to the name but is NOT a
// member is NOT returned. This is what the host-vitals aggregate
// (SIDsInIncarnationInScope) relies on.
func TestIntegration_SelectIncarnationMembers_ViaMembershipNotCoven(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	// A real member — coven has only a real stable tag, NOT the incarnation name.
	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedMembership(t, "redis-prod", "member.example.com")
	// An impostor — its coven literally contains "redis-prod", but it is NOT a
	// member (no incarnation_membership row).
	seedBulkSoul(t, "impostor.example.com", []string{"redis-prod"})

	got, err := SelectIncarnationMembers(ctx, integrationPool, "redis-prod", 100)
	if err != nil {
		t.Fatalf("SelectIncarnationMembers: %v", err)
	}
	if len(got) != 1 || got[0].SID != "member.example.com" {
		sids := make([]string, len(got))
		for i, s := range got {
			sids[i] = s.SID
		}
		t.Fatalf("members = %v, want only [member.example.com] (membership, not coven==name)", sids)
	}
	// The member is resolved without carrying the incarnation name in coven.
	for _, c := range got[0].Coven {
		if c == "redis-prod" {
			t.Errorf("member coven unexpectedly contains the incarnation name: %v", got[0].Coven)
		}
	}
}
