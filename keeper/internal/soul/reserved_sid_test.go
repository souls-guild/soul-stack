package soul_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// TestIsReservedSID — the reserved set catches EXACTLY the synthetic
// keeper/__run__ and doesn't touch real Soul SIDs, including keeper-like
// names (NIM-36).
func TestIsReservedSID(t *testing.T) {
	cases := []struct {
		sid  string
		want bool
	}{
		{"keeper", true},
		{"__run__", true},
		{"soul-keeper-1", false},
		{"keeper-1", false},
		{"keeper.example.com", false},
		{"host-a.local", false},
		{"", false},
	}
	for _, c := range cases {
		if got := soul.IsReservedSID(c.sid); got != c.want {
			t.Errorf("IsReservedSID(%q) = %v, want %v", c.sid, got, c.want)
		}
	}
}

// TestReservedSIDs_MatchRenderConstants — drift guard: ReservedSIDs literals
// must match the render constants (soul duplicates them since render isn't
// imported by the leaf package soul). A mismatch = a bug (NIM-36).
func TestReservedSIDs_MatchRenderConstants(t *testing.T) {
	if !soul.IsReservedSID(render.KeeperTargetSID) {
		t.Errorf("ReservedSIDs does not contain render.KeeperTargetSID=%q", render.KeeperTargetSID)
	}
	if !soul.IsReservedSID(render.RunSentinelSID) {
		t.Errorf("ReservedSIDs does not contain render.RunSentinelSID=%q", render.RunSentinelSID)
	}
	if len(soul.ReservedSIDs) != 2 {
		t.Errorf("len(ReservedSIDs) = %d, want 2 (keeper + __run__)", len(soul.ReservedSIDs))
	}
}
