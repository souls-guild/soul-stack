package config

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

const twoDefaults = OverlayJSON(`[{"days":["mon","tue"],"start_time":"02:00","duration_minutes":120,"timezone":"Europe/Moscow","comment":"nightly"},{"days":["sun"],"start_time":"23:30","duration_minutes":60,"timezone":"UTC"}]`)

var twoDefaultsDecoded = []KeeperMaintenanceWindow{
	{Days: []string{"mon", "tue"}, StartTime: "02:00", DurationMinutes: 120, Timezone: "Europe/Moscow", Comment: "nightly"},
	{Days: []string{"sun"}, StartTime: "23:30", DurationMinutes: 60, Timezone: "UTC"},
}

// The one structural overlay value lands as a list, whether the file has no
// `maintenance:` block, has one without the key, or (judged with force, as for an
// instance whose file is silent) sets the key itself.
func TestOverlay_DefaultWindowsMergeAsAList(t *testing.T) {
	cases := map[string]string{
		"no maintenance block":    "",
		"block without the key":   "\nmaintenance:\n  window_retention: 1h\n",
		"the file sets it, force": "\nmaintenance:\n  default_windows:\n    - days: [fri]\n      start_time: \"10:00\"\n      duration_minutes: 30\n      timezone: UTC\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			store, src, _ := overlayStore(t, extra)
			entries := []OverlayEntry{{Path: "$.maintenance.default_windows", Value: twoDefaults}}
			if diags := store.ValidateOverlay(entries); diag.HasErrors(diags) {
				t.Fatalf("ValidateOverlay: %v", diags)
			}
			if strings.Contains(extra, "default_windows") {
				merged, diags := applyOverlayWith(store.Path(), mustRead(t, store.Path()), entries, true)
				if len(diags) > 0 {
					t.Fatalf("forced merge: %v", diags)
				}
				cfg, _, diags, _ := LoadKeeperFromBytes(store.Path(), merged, ValidateOptions{})
				if diag.HasErrors(diags) {
					t.Fatalf("forced merge does not load: %v", diags)
				}
				if got := cfg.Maintenance.DefaultWindowList(); !reflect.DeepEqual(got, twoDefaultsDecoded) {
					t.Fatalf("forced merge = %+v, want the overlay's list", got)
				}
				return
			}
			src.set(entries...)
			if res := store.RefreshOverlay(context.Background()); !res.Swapped {
				t.Fatalf("overlay not applied: %+v", res.Diagnostics)
			}
			if got := store.Get().Maintenance.DefaultWindowList(); !reflect.DeepEqual(got, twoDefaultsDecoded) {
				t.Fatalf("default_windows = %+v, want %+v", got, twoDefaultsDecoded)
			}
			if extra != "" && store.Get().Maintenance.WindowRetention != "1h" {
				t.Errorf("the sibling key was lost: %+v", store.Get().Maintenance)
			}
		})
	}
}

// The idempotence guard compares an OverlayJSON like a scalar, so the same list
// read again is no change, and JSON output shows the list itself.
func TestOverlayJSON_ComparesAndEncodesAsItsStructure(t *testing.T) {
	store, src, _ := overlayStore(t, "")
	src.set(OverlayEntry{Path: "$.maintenance.default_windows", Value: twoDefaults})
	if res := store.RefreshOverlay(context.Background()); !res.Swapped {
		t.Fatalf("overlay not applied: %+v", res.Diagnostics)
	}
	src.set(OverlayEntry{Path: "$.maintenance.default_windows", Value: OverlayJSON(string(twoDefaults))})
	if res := store.RefreshOverlay(context.Background()); res.Swapped {
		t.Error("the same list read again swapped the config")
	}

	out, err := json.Marshal(map[string]any{"value": twoDefaults})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"value":` + string(twoDefaults) + `}`; string(out) != want {
		t.Errorf("json = %s, want %s", out, want)
	}
}

// A default window is refused on the rules of an incarnation's weekly window,
// wherever it comes from: the file here, the overlay through the same pipeline.
func TestKeeperConfig_DefaultWindowsValidatedAsWeeklyWindows(t *testing.T) {
	cases := map[string]struct {
		window string
		path   string
		reason string
	}{
		"duplicate day":     {`{days: [mon, mon], start_time: "02:00", duration_minutes: 60, timezone: UTC}`, "[0].days", `lists "mon" twice`},
		"bad start":         {`{days: [mon], start_time: "2:00", duration_minutes: 60, timezone: UTC}`, "[0].start_time", "must be HH:MM"},
		"too long":          {`{days: [mon], start_time: "02:00", duration_minutes: 1441, timezone: UTC}`, "[0].duration_minutes", "between 1 and 1440"},
		"host zone":         {`{days: [mon], start_time: "02:00", duration_minutes: 60, timezone: Local}`, "[0].timezone", "depends on the Keeper host"},
		"unknown zone":      {`{days: [mon], start_time: "02:00", duration_minutes: 60, timezone: Mars/Olympus}`, "[0].timezone", "not a known IANA zone"},
		"no days":           {`{days: [], start_time: "02:00", duration_minutes: 60, timezone: UTC}`, "[0].days", "at least one day"},
		"comment too long":  {`{days: [mon], start_time: "02:00", duration_minutes: 60, timezone: UTC, comment: "` + strings.Repeat("x", 501) + `"}`, "[0].comment", "at most 500"},
		"unknown field key": {`{days: [mon], start_time: "02:00", duration_minutes: 60, timezone: UTC, kind: once}`, "[0].kind", `unknown field "kind"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := fixtureKeeperPath(t)
			src := append(mustRead(t, path), []byte("\nmaintenance:\n  default_windows:\n    - "+tc.window+"\n")...)
			_, _, diags, _ := LoadKeeperFromBytes(path, src, ValidateOptions{})
			for _, d := range diags {
				if d.Level == diag.LevelError && d.YAMLPath == "$.maintenance.default_windows"+tc.path && strings.Contains(d.Message, tc.reason) {
					return
				}
			}
			t.Fatalf("no error at %s mentioning %q; got %v", tc.path, tc.reason, diags)
		})
	}

	path := fixtureKeeperPath(t)
	var b strings.Builder
	b.WriteString("\nmaintenance:\n  default_windows:\n")
	for range MaintenanceMaxWindows + 1 {
		b.WriteString("    - {days: [mon], start_time: \"02:00\", duration_minutes: 60, timezone: UTC}\n")
	}
	_, _, diags, _ := LoadKeeperFromBytes(path, append(mustRead(t, path), []byte(b.String())...), ValidateOptions{})
	if !diag.HasErrors(diags) {
		t.Errorf("%d default windows were accepted; the cap is %d", MaintenanceMaxWindows+1, MaintenanceMaxWindows)
	}
}

// A zone is resolved alike from the cache and without it, and only a name of
// IANA shape is kept: a spelling the filesystem also resolves cannot add entries.
func TestLoadMaintenanceZone_CachesOnlyIANAShapedNames(t *testing.T) {
	for _, name := range []string{"Europe/Moscow", "Europe/Moscow", "Etc/GMT+3", "UTC"} {
		loc, err := LoadMaintenanceZone(name)
		if err != nil || loc.String() != name {
			t.Fatalf("%s: %v, %v", name, loc, err)
		}
	}
	for _, name := range []string{"Europe/./Moscow", "Europe//Moscow", "Europe/../Europe/Moscow"} {
		_, _ = LoadMaintenanceZone(name)
		if _, cached := maintenanceZones.Load(name); cached {
			t.Errorf("%q was cached", name)
		}
	}
	if _, cached := maintenanceZones.Load("Europe/Moscow"); !cached {
		t.Error("an IANA name was not cached")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
