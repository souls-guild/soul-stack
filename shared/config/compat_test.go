package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestNormalizeEngineVersion — the five build shapes of ADR-0076(e): a release
// tag, a build past a tag, the same dirty, the un-injected sentinel and a bare
// commit hash. The first three are enforced by their release core; the last two
// carry no version and must NOT be enforced.
func TestNormalizeEngineVersion(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantOK  bool
		comment string
	}{
		{"release_tag", "v0.1.0-beta.1", "0.1.0", true, "pre-release compares by its release core (d)"},
		{"plain_release", "v0.2.3", "0.2.3", true, "leading v is not precedence"},
		{"no_v_prefix", "0.2.3", "0.2.3", true, ""},
		{"describe_distance", "v0.1.0-beta.1-12-gabc1234", "0.1.0", true, "distance suffix is build metadata"},
		{"describe_dirty", "v0.1.0-beta.1-12-gabc1234-dirty", "0.1.0", true, "dirty is build metadata"},
		{"describe_release_distance", "v0.2.0-5-gdeadbee", "0.2.0", true, ""},
		{"build_metadata", "0.2.0+ci.7", "0.2.0", true, "build metadata never affects precedence"},
		{"dev_sentinel", DevVersionSentinel, "", false, "un-injected build: nothing to compare (e)"},
		{"empty", "", "", false, ""},
		{"whitespace", "   ", "", false, ""},
		{"bare_hash", "abc1234", "", false, "--always fallback: no release core"},
		{"bare_hash_dirty", "abc1234-dirty", "", false, ""},
		{"not_a_version", "release-candidate", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeEngineVersion(tc.raw)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("NormalizeEngineVersion(%q) = (%q, %v), want (%q, %v) - %s",
					tc.raw, got, ok, tc.want, tc.wantOK, tc.comment)
			}
		})
	}
}

// TestVersionWindowContains — the half-open semantics of ADR-0076(c): min
// inclusive, max EXCLUSIVE, one-sided windows, and an undeclared window
// admitting everything (backcompat).
func TestVersionWindowContains(t *testing.T) {
	cases := []struct {
		name    string
		window  *VersionWindow
		release string
		want    bool
	}{
		{"inside", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, "0.2.5", true},
		{"min_inclusive", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, "0.1.0", true},
		{"max_exclusive", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, "0.3.0", false},
		{"above_max", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, "0.4.1", false},
		{"below_min", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, "0.0.9", false},
		{"floor_only_ok", &VersionWindow{Min: "0.1.0"}, "9.9.9", true},
		{"floor_only_reject", &VersionWindow{Min: "0.1.0"}, "0.0.1", false},
		{"ceiling_only_ok", &VersionWindow{Max: "0.3.0"}, "0.0.1", true},
		{"ceiling_only_reject", &VersionWindow{Max: "0.3.0"}, "0.3.1", false},
		{"nil_window_unbounded", nil, "9.9.9", true},
		{"empty_declaration_unbounded", &VersionWindow{}, "9.9.9", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.window.Contains(tc.release); got != tc.want {
				t.Fatalf("%s.Contains(%q) = %v, want %v", tc.window.String(), tc.release, got, tc.want)
			}
		})
	}
}

// TestVersionWindowIsEmpty — the `compat_window_empty` condition: max is
// exclusive, so min == max admits nothing.
func TestVersionWindowIsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		window *VersionWindow
		want   bool
	}{
		{"normal", &VersionWindow{Min: "0.1.0", Max: "0.3.0"}, false},
		{"equal_bounds", &VersionWindow{Min: "0.3.0", Max: "0.3.0"}, true},
		{"inverted", &VersionWindow{Min: "0.4.0", Max: "0.3.0"}, true},
		{"one_sided_min", &VersionWindow{Min: "0.3.0"}, false},
		{"one_sided_max", &VersionWindow{Max: "0.3.0"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.window.IsEmpty(); got != tc.want {
				t.Fatalf("(%v).IsEmpty() = %v, want %v", tc.window, got, tc.want)
			}
		})
	}
}

// TestIntersectKeeperWindows — the effective window of ADR-0076(b): max of the
// mins, min of the maxes, the narrowest wins; undeclared entities contribute
// nothing; an unsatisfiable intersection is surfaced, not widened.
func TestIntersectKeeperWindows(t *testing.T) {
	svc := func(w *VersionWindow) CompatEntity {
		return CompatEntity{Kind: CompatEntityService, Name: "redis", Ref: "v1.0.0", Window: w}
	}
	dst := func(name string, w *VersionWindow) CompatEntity {
		return CompatEntity{Kind: CompatEntityDestiny, Name: name, Ref: "v2.1.0", Window: w}
	}

	cases := []struct {
		name     string
		entities []CompatEntity
		want     *VersionWindow
	}{
		{
			name:     "no_declaration_unbounded",
			entities: []CompatEntity{svc(nil), dst("cluster", nil)},
			want:     nil,
		},
		{
			name:     "single_declaration_wins",
			entities: []CompatEntity{svc(&VersionWindow{Min: "0.1.0", Max: "0.3.0"}), dst("cluster", nil)},
			want:     &VersionWindow{Min: "0.1.0", Max: "0.3.0"},
		},
		{
			name: "narrowest_wins_both_sides",
			entities: []CompatEntity{
				svc(&VersionWindow{Min: "0.1.0", Max: "0.9.0"}),
				dst("cluster", &VersionWindow{Min: "0.2.0", Max: "0.4.0"}),
			},
			want: &VersionWindow{Min: "0.2.0", Max: "0.4.0"},
		},
		{
			name: "narrowest_wins_per_side",
			entities: []CompatEntity{
				svc(&VersionWindow{Min: "0.5.0", Max: "0.9.0"}),
				dst("cluster", &VersionWindow{Min: "0.2.0", Max: "0.6.0"}),
			},
			want: &VersionWindow{Min: "0.5.0", Max: "0.6.0"},
		},
		{
			name: "one_sided_contributions_combine",
			entities: []CompatEntity{
				svc(&VersionWindow{Min: "0.2.0"}),
				dst("cluster", &VersionWindow{Max: "0.5.0"}),
			},
			want: &VersionWindow{Min: "0.2.0", Max: "0.5.0"},
		},
		{
			name: "empty_intersection_is_reported",
			entities: []CompatEntity{
				svc(&VersionWindow{Min: "0.3.0"}),
				dst("cluster", &VersionWindow{Max: "0.3.0"}),
			},
			want: &VersionWindow{Min: "0.3.0", Max: "0.3.0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IntersectKeeperWindows(tc.entities)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("IntersectKeeperWindows = %s, want nil (unbounded)", got.String())
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("IntersectKeeperWindows = nil, want %s", tc.want.String())
			case got.Min != tc.want.Min || got.Max != tc.want.Max:
				t.Fatalf("IntersectKeeperWindows = %s, want %s", got.String(), tc.want.String())
			}
		})
	}
}

// TestIntersectMatchesPerEntityCheck — the equivalence the enforcement path
// relies on: a version is admitted by the intersection iff every entity admits
// it. Guards the per-entity gate against drifting from the displayed window.
func TestIntersectMatchesPerEntityCheck(t *testing.T) {
	entities := []CompatEntity{
		{Kind: CompatEntityService, Name: "redis", Window: &VersionWindow{Min: "0.1.0", Max: "0.9.0"}},
		{Kind: CompatEntityDestiny, Name: "cluster", Window: &VersionWindow{Min: "0.2.0", Max: "0.4.0"}},
		{Kind: CompatEntityDestiny, Name: "acl", Window: nil},
	}
	effective := IntersectKeeperWindows(entities)
	for _, release := range []string{"0.0.1", "0.1.0", "0.2.0", "0.3.9", "0.4.0", "0.9.0", "1.0.0"} {
		viaWindow := effective.Contains(release)
		viaEntities := FirstIncompatibleEntity(entities, release) == nil
		if viaWindow != viaEntities {
			t.Fatalf("release %s: effective window says %v, per-entity check says %v", release, viaWindow, viaEntities)
		}
	}
}

// TestFirstIncompatibleEntityAttributesBound — the rejection must name the
// artifact that set the bound, not just "somewhere in this service".
func TestFirstIncompatibleEntityAttributesBound(t *testing.T) {
	entities := []CompatEntity{
		{Kind: CompatEntityService, Name: "redis", Ref: "v1.0.0", Window: &VersionWindow{Min: "0.1.0"}},
		{Kind: CompatEntityDestiny, Name: "cluster", Ref: "v2.1.0", Window: &VersionWindow{Min: "0.1.0", Max: "0.3.0"}},
	}
	bad := FirstIncompatibleEntity(entities, "0.4.1")
	if bad == nil {
		t.Fatal("FirstIncompatibleEntity = nil, want the destiny that caps at 0.3.0")
	}
	if bad.Kind != CompatEntityDestiny || bad.Name != "cluster" {
		t.Fatalf("blamed %s %q, want destiny \"cluster\"", bad.Kind, bad.Name)
	}
}

// TestFirstIncompatibleEntityNotEnforcedWithoutRelease — a build carrying no
// version is never blocked (ADR-0076(e)): the window is said out loud elsewhere,
// never enforced here.
func TestFirstIncompatibleEntityNotEnforcedWithoutRelease(t *testing.T) {
	entities := []CompatEntity{
		{Kind: CompatEntityService, Name: "redis", Window: &VersionWindow{Min: "5.0.0", Max: "6.0.0"}},
	}
	if bad := FirstIncompatibleEntity(entities, ""); bad != nil {
		t.Fatalf("FirstIncompatibleEntity with no release = %v, want nil (not enforced)", bad)
	}
}

// TestKeeperCompatErrorNamesTheVersion — ADR-0076(g): the message carries the
// entity, its window and the running version, and unwraps to the sentinel so the
// render path can pick it out of a generic render failure.
func TestKeeperCompatErrorNamesTheVersion(t *testing.T) {
	err := NewKeeperCompatError(CompatEntity{
		Kind:   CompatEntityDestiny,
		Name:   "redis",
		Ref:    "v2.1.0",
		Window: &VersionWindow{Min: "0.1.0", Max: "0.3.0"},
	}, "v0.4.1-3-gabc1234")

	if !errors.Is(err, ErrKeeperVersionUnsupported) {
		t.Fatal("KeeperCompatError does not unwrap to ErrKeeperVersionUnsupported")
	}
	msg := err.Error()
	for _, want := range []string{"destiny", "redis", "v2.1.0", "[0.1.0, 0.3.0)", "v0.4.1-3-gabc1234"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error text %q is missing %q", msg, want)
		}
	}
}

// TestServiceManifestCompatParse — the `compat:` block parses on service.yml and
// its absence stays valid (backcompat, no migration of existing services).
func TestServiceManifestCompatParse(t *testing.T) {
	const withWindow = `
state_schema_version: 1
state_schema: {}
compat:
  keeper: {min: "0.1.0", max: "0.3.0"}
`
	m, _, diags, err := LoadServiceManifestFromBytes("service.yml", []byte(withWindow), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadServiceManifestFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	w := m.Compat.KeeperWindow()
	if w == nil || w.Min != "0.1.0" || w.Max != "0.3.0" {
		t.Fatalf("keeper window = %v, want [0.1.0, 0.3.0)", w)
	}

	const withoutWindow = `
state_schema_version: 1
state_schema: {}
`
	m2, _, diags2, err := LoadServiceManifestFromBytes("service.yml", []byte(withoutWindow), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadServiceManifestFromBytes (no compat): %v", err)
	}
	if diag.HasErrors(diags2) {
		t.Fatalf("a manifest without compat: must stay valid, got %v", diags2)
	}
	if w := m2.Compat.KeeperWindow(); w != nil {
		t.Fatalf("missing compat: must read as unbounded (nil), got %v", w)
	}
}

// TestDestinyManifestCompatParse — the same block on destiny.yml: a destiny is
// pinned at its own ref, so it declares its own window (ADR-0076(a)).
func TestDestinyManifestCompatParse(t *testing.T) {
	const src = `
name: redis
compat:
  keeper: {min: "0.2.0"}
`
	m, _, diags, err := LoadDestinyManifestFromBytes("destiny.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadDestinyManifestFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	w := m.Compat.KeeperWindow()
	if w == nil || w.Min != "0.2.0" || w.Max != "" {
		t.Fatalf("keeper window = %v, want a floor-only >= 0.2.0", w)
	}
}

// TestCompatSchemaDiagnostics — the authoring errors the validator must catch,
// each with its own code so soul-lint and the UI can key off it.
func TestCompatSchemaDiagnostics(t *testing.T) {
	cases := []struct {
		name     string
		compat   string
		wantCode string
	}{
		{
			name:     "empty_block",
			compat:   "compat: {}\n",
			wantCode: "compat_window_incomplete",
		},
		{
			name:     "no_bounds",
			compat:   "compat:\n  keeper: {}\n",
			wantCode: "compat_window_incomplete",
		},
		{
			name:     "range_operator_rejected",
			compat:   "compat:\n  keeper: {min: \">=0.1.0\"}\n",
			wantCode: "compat_version_invalid",
		},
		{
			name:     "v_prefix_rejected",
			compat:   "compat:\n  keeper: {min: \"v0.1.0\"}\n",
			wantCode: "compat_version_invalid",
		},
		{
			name:     "prerelease_rejected",
			compat:   "compat:\n  keeper: {max: \"0.3.0-beta.1\"}\n",
			wantCode: "compat_version_invalid",
		},
		{
			name:     "two_part_rejected",
			compat:   "compat:\n  keeper: {min: \"0.1\"}\n",
			wantCode: "compat_version_invalid",
		},
		{
			name:     "equal_bounds_empty_window",
			compat:   "compat:\n  keeper: {min: \"0.3.0\", max: \"0.3.0\"}\n",
			wantCode: "compat_window_empty",
		},
		{
			name:     "inverted_bounds_empty_window",
			compat:   "compat:\n  keeper: {min: \"0.4.0\", max: \"0.3.0\"}\n",
			wantCode: "compat_window_empty",
		},
		{
			name:     "unknown_key_inside_window",
			compat:   "compat:\n  keeper: {min: \"0.1.0\", floor: \"0.2.0\"}\n",
			wantCode: "unknown_key",
		},
		{
			name:     "unknown_engine_axis",
			compat:   "compat:\n  soul: {min: \"0.1.0\"}\n",
			wantCode: "unknown_key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: redis\nstate_schema_version: 1\nstate_schema: {}\n" + tc.compat
			_, _, diags, err := LoadServiceManifestFromBytes("service.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadServiceManifestFromBytes: %v", err)
			}
			if !hasDiagCode(diags, tc.wantCode) {
				t.Fatalf("want diagnostic %q, got %v", tc.wantCode, diags)
			}

			// The same grammar must be enforced on destiny.yml (ADR-0076(a)).
			dsrc := "name: redis\n" + tc.compat
			_, _, ddiags, err := LoadDestinyManifestFromBytes("destiny.yml", []byte(dsrc), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadDestinyManifestFromBytes: %v", err)
			}
			if !hasDiagCode(ddiags, tc.wantCode) {
				t.Fatalf("destiny.yml: want diagnostic %q, got %v", tc.wantCode, ddiags)
			}
		})
	}
}
