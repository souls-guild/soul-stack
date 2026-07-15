package beacon

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/beaconaddr"
)

// TestDefaultRegistry — soul-side half of the invariant "keeper-enum ==
// soul-registry == shared": the [Default] registry covers EXACTLY the
// canonical set [beaconaddr.All] (no more). The keeper-side half (enum ==
// beaconaddr.All) is checked in keeper/internal/oracle; the shared source
// shared/beaconaddr transitively gives keeper-enum == soul-registry — the
// root fix for the S3 bug.
func TestDefaultRegistry(t *testing.T) {
	r := Default()
	canonical := beaconaddr.All()
	for _, name := range canonical {
		if _, ok := r.Lookup(name); !ok {
			t.Errorf("Default-реестр не содержит канонический адрес %q", name)
		}
	}
	if len(r.Names()) != len(canonical) {
		t.Fatalf("soul-registry (%d) рассинхронен с beaconaddr.All (%d): %v", len(r.Names()), len(canonical), r.Names())
	}
	if _, ok := r.Lookup("core.beacon.nope"); ok {
		t.Error("Lookup неизвестного beacon должен вернуть false")
	}
}
