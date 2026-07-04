package soul_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// TestIsReservedSID — reserved-набор ловит ТОЧНО синтетические keeper/__run__ и
// не задевает реальные Soul-sid, включая keeper-подобные имена (NIM-36).
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

// TestReservedSIDs_MatchRenderConstants — drift-guard: литералы ReservedSIDs
// обязаны совпадать с render-константами (soul дублирует их, т.к. render не
// импортируется leaf-пакетом soul). Расхождение = баг (NIM-36).
func TestReservedSIDs_MatchRenderConstants(t *testing.T) {
	if !soul.IsReservedSID(render.KeeperTargetSID) {
		t.Errorf("ReservedSIDs не содержит render.KeeperTargetSID=%q", render.KeeperTargetSID)
	}
	if !soul.IsReservedSID(render.RunSentinelSID) {
		t.Errorf("ReservedSIDs не содержит render.RunSentinelSID=%q", render.RunSentinelSID)
	}
	if len(soul.ReservedSIDs) != 2 {
		t.Errorf("len(ReservedSIDs) = %d, want 2 (keeper + __run__)", len(soul.ReservedSIDs))
	}
}
