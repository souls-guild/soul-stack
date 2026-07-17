package rbac

import (
	"context"
	"testing"
	"time"
)

// TestEnforcer_IsRevoked — прямой map-lookup revoked-проекции (NIM-77): true для
// ревокнутого AID, false для активного и для отсутствующего в снимке.
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
		t.Error("IsRevoked(archon-fired) = false, want true (в Revoked-проекции)")
	}
	if e.IsRevoked("archon-alice") {
		t.Error("IsRevoked(archon-alice) = true, want false (активный оператор)")
	}
	if e.IsRevoked("archon-unknown") {
		t.Error("IsRevoked(archon-unknown) = true, want false (нет в снимке)")
	}
}

// TestEnforcer_IsRevoked_NilRevokedMap — снимок без revoked-проекции (nil map):
// IsRevoked не паникует и отдаёт false для любого AID.
func TestEnforcer_IsRevoked_NilRevokedMap(t *testing.T) {
	e, err := NewEnforcerFromSnapshot(&Snapshot{Roles: map[string][]string{}})
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	if e.IsRevoked("archon-anyone") {
		t.Error("IsRevoked при nil-Revoked = true, want false")
	}
}

// TestHolder_IsRevoked — делегирование в текущий Enforcer через fakeSource.
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
