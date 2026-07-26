package settingsstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// staticSource is a [config.OverlaySource] over a fixed entry set.
type staticSource []config.OverlayEntry

func (s staticSource) Overlay() []config.OverlayEntry { return s }

// keeperFixture copies the golden keeper.yml into a temp file.
func keeperFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash("../../../examples/keeper/keeper.yml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "keeper.yml")
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dst
}

// ADR-0073(e): the registry is the contract — keys must be unique, carry the
// reserved prefix and be storable at all under the migration-035 CHECK.
func TestRegistry_KeyContract(t *testing.T) {
	seen := map[string]bool{}
	paths := map[string]bool{}
	for _, f := range Fields() {
		if seen[f.Key] {
			t.Errorf("duplicate key %q", f.Key)
		}
		seen[f.Key] = true

		if len(f.Key) <= len(KeyPrefix) || f.Key[:len(KeyPrefix)] != KeyPrefix {
			t.Errorf("key %q lacks the reserved %q prefix", f.Key, KeyPrefix)
		}
		if !KeyFormat.MatchString(f.Key) {
			t.Errorf("key %q violates the keeper_settings CHECK", f.Key)
		}
		if paths[f.YAMLPath] {
			t.Errorf("duplicate yaml path %q", f.YAMLPath)
		}
		paths[f.YAMLPath] = true

		if f.Min >= f.Max {
			t.Errorf("key %q: empty range [%v, %v]", f.Key, f.Min, f.Max)
		}
		if f.Read == nil {
			t.Errorf("key %q: no Read accessor", f.Key)
		}
		if f.Default == nil {
			t.Errorf("key %q: no default", f.Key)
		}
		if _, err := f.Parse(f.Format(f.Default)); err != nil {
			t.Errorf("key %q: the built-in default is outside its own bounds: %v", f.Key, err)
		}
	}
}

// Every registered yaml path must actually land in KeeperConfig: the merged
// config validates cleanly AND the value arrives where the consumer reads it.
// This is what catches a typo'd path, which would otherwise fail silently as an
// unknown key at reload time.
func TestRegistry_PathsResolveInKeeperConfig(t *testing.T) {
	for _, f := range Fields() {
		t.Run(f.Key, func(t *testing.T) {
			store, diags, err := config.LoadKeeperStore(keeperFixture(t), config.ValidateOptions{})
			if err != nil || diag.HasErrors(diags) {
				t.Fatalf("load fixture: err=%v diags=%v", err, diags)
			}

			// A value that is inside the bounds but different from the default,
			// so "applied" is distinguishable from "resolved to default".
			var probe any
			switch f.Kind {
			case KindFloat:
				probe = (f.Min + f.Max) / 2
			case KindInt:
				probe = int(f.Min) + 1
			}
			store.SetOverlaySource(staticSource{{Path: f.YAMLPath, Value: probe}})

			res := store.RefreshOverlay(context.Background())
			if !res.Swapped {
				t.Fatalf("overlay at %s rejected: %+v", f.YAMLPath, res.Diagnostics)
			}
			if got := f.Read(store.Get()); got != probe {
				t.Errorf("%s: effective value = %v, want %v (path does not reach the consumer)", f.YAMLPath, got, probe)
			}
		})
	}
}

func TestField_ParseAndBounds(t *testing.T) {
	thr, ok := Lookup("cfg_toll_threshold")
	if !ok {
		t.Fatal("cfg_toll_threshold missing from the registry")
	}
	if v, err := thr.Parse("0.5"); err != nil || v != 0.5 {
		t.Errorf("Parse(0.5) = (%v, %v)", v, err)
	}
	// A type validator alone would accept every one of these.
	for _, bad := range []string{"", "abc", "-1", "0", "1.5", "1e9"} {
		if _, err := thr.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a range/type rejection", bad)
		}
	}
	if got := thr.Bounds(); got != "(0, 1]" {
		t.Errorf("Bounds() = %q, want (0, 1]", got)
	}

	burst, ok := Lookup("cfg_tempo_voyage_create_burst")
	if !ok {
		t.Fatal("cfg_tempo_voyage_create_burst missing from the registry")
	}
	if v, err := burst.Parse("20"); err != nil || v != 20 {
		t.Errorf("Parse(20) = (%v, %v)", v, err)
	}
	for _, bad := range []string{"0", "-5", "1.5", "100000"} {
		if _, err := burst.Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted, want a rejection", bad)
		}
	}
}

func TestLookup_UnknownKeyIsNotAdmitted(t *testing.T) {
	for _, k := range []string{"cfg_nope", "default_destiny_source", "provisioning_allowed_methods", ""} {
		if _, ok := Lookup(k); ok {
			t.Errorf("Lookup(%q) admitted a key outside the registry", k)
		}
	}
}
