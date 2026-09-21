package beaconaddr

import "testing"

// TestAllCoversConsts — All() contains exactly the declared address constants, with
// no duplicates or omissions. Core of the "keeper-enum == soul-registry == shared"
// invariant: both sides check against this list, so it must itself be consistent.
func TestAllCoversConsts(t *testing.T) {
	consts := []string{
		ServiceDown, FileChanged, PortClosed,
		DiskFull, ProcessAbsent, HTTPUnhealthy, Inotify,
	}
	all := All()
	if len(all) != len(consts) {
		t.Fatalf("All() (%d) does not match the number of constants (%d)", len(all), len(consts))
	}

	seen := make(map[string]struct{}, len(all))
	for _, a := range all {
		if a == "" {
			t.Error("empty address in All()")
		}
		if _, dup := seen[a]; dup {
			t.Errorf("duplicate address in All(): %q", a)
		}
		seen[a] = struct{}{}
	}
	for _, c := range consts {
		if _, ok := seen[c]; !ok {
			t.Errorf("constant %q missing from All()", c)
		}
	}
}

// TestAllReturnsCopy — the caller cannot silently mutate the shared list (a fresh
// slice on each call).
func TestAllReturnsCopy(t *testing.T) {
	a := All()
	if len(a) == 0 {
		t.Fatal("All() is empty")
	}
	a[0] = "mutated"
	if All()[0] == "mutated" {
		t.Error("All() returned a shared mutable slice")
	}
}
