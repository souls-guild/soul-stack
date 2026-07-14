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
		t.Fatalf("All() (%d) не совпадает с числом констант (%d)", len(all), len(consts))
	}

	seen := make(map[string]struct{}, len(all))
	for _, a := range all {
		if a == "" {
			t.Error("пустой адрес в All()")
		}
		if _, dup := seen[a]; dup {
			t.Errorf("дубль адреса в All(): %q", a)
		}
		seen[a] = struct{}{}
	}
	for _, c := range consts {
		if _, ok := seen[c]; !ok {
			t.Errorf("константа %q отсутствует в All()", c)
		}
	}
}

// TestAllReturnsCopy — the caller cannot silently mutate the shared list (a fresh
// slice on each call).
func TestAllReturnsCopy(t *testing.T) {
	a := All()
	if len(a) == 0 {
		t.Fatal("All() пуст")
	}
	a[0] = "mutated"
	if All()[0] == "mutated" {
		t.Error("All() вернул общий мутируемый срез")
	}
}
