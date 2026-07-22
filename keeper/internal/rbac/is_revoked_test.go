package rbac

import (
	"context"
	"testing"
	"time"
)

// TestEnforcer_IsRevoked — direct map-lookup on the revoked projection (NIM-77): true
// for a revoked AID, false for an active one and for one absent from the snapshot.
func TestEnforcer_IsRevoked(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"admin": {"*"}},
		Membership: map[string][]string{"archon-alice": {"admin"}},
		Revoked:    map[string]time.Time{"archon-fired": time.Now()},
	}
	e, err := NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if !e.IsRevoked("archon-fired") {
		t.Error("IsRevoked(archon-fired) = false, want true (in the Revoked projection)")
	}
	if e.IsRevoked("archon-alice") {
		t.Error("IsRevoked(archon-alice) = true, want false (active operator)")
	}
	if e.IsRevoked("archon-unknown") {
		t.Error("IsRevoked(archon-unknown) = true, want false (not in the snapshot)")
	}
}

// TestEnforcer_IsRevoked_NilRevokedMap — snapshot without a revoked projection (nil map):
// IsRevoked does not panic and returns false for any AID.
func TestEnforcer_IsRevoked_NilRevokedMap(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(&Snapshot{Roles: map[string][]string{}})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if e.IsRevoked("archon-anyone") {
		t.Error("IsRevoked with nil-Revoked = true, want false")
	}
}

// TestHolder_IsRevoked — delegation to the current Enforcer via fakeSource.
func TestHolder_IsRevoked(t *testing.T) {
	snap := &Snapshot{
		Roles:      map[string][]string{"admin": {"*"}},
		Membership: map[string][]string{"archon-alice": {"admin"}},
		Revoked:    map[string]time.Time{"archon-fired": time.Now()},
	}
	h, err := NewHolder(context.Background(), &fakeSource{snap: snap}, time.Hour, nil)
	if err != nil {
		t.Fatalf("NewHolder: %v", err)
	}
	if !h.IsRevoked("archon-fired") {
		t.Error("Holder.IsRevoked(archon-fired) = false, want true")
	}
	if h.IsRevoked("archon-alice") {
		t.Error("Holder.IsRevoked(archon-alice) = true, want false")
	}
}
