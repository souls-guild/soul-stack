package artifact

import "testing"

// TestResolveUpgradeScenario — резолв found/legacy по `from:` (ADR-0068 §5).
// upgrades уже отсортирован по имени (как отдаёт listFromDir), поэтому при
// нескольких совпадениях ожидается первый по имени.
func TestResolveUpgradeScenario(t *testing.T) {
	upgrades := []Scenario{
		{Name: "v2", FromVersions: []string{"v1.0.0", "v1.2.0"}},
		{Name: "v3", FromVersions: []string{"v1.0.0", "v2.0.0"}},
		{Name: "v4_noop", FromVersions: nil},
	}

	tests := []struct {
		name        string
		upgrades    []Scenario
		fromVersion string
		wantSlug    string
		wantFound   bool
	}{
		{
			name:        "found",
			upgrades:    upgrades,
			fromVersion: "v1.2.0",
			wantSlug:    "v2",
			wantFound:   true,
		},
		{
			name:        "not-found",
			upgrades:    upgrades,
			fromVersion: "v9.9.9",
			wantSlug:    "",
			wantFound:   false,
		},
		{
			// v1.0.0 объявлен и в v2, и в v3 → детерминированно первый по имени (v2).
			name:        "multiple-matches-first-by-name",
			upgrades:    upgrades,
			fromVersion: "v1.0.0",
			wantSlug:    "v2",
			wantFound:   true,
		},
		{
			name:        "empty-from-version",
			upgrades:    upgrades,
			fromVersion: "",
			wantSlug:    "",
			wantFound:   false,
		},
		{
			// Единственный upgrade без from: (пустой FromVersions) не матчит ничего.
			name:        "upgrade-without-from-never-matches",
			upgrades:    []Scenario{{Name: "v4_noop", FromVersions: nil}},
			fromVersion: "v1.0.0",
			wantSlug:    "",
			wantFound:   false,
		},
		{
			name:        "empty-upgrades",
			upgrades:    nil,
			fromVersion: "v1.0.0",
			wantSlug:    "",
			wantFound:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			slug, found := ResolveUpgradeScenario(tt.upgrades, tt.fromVersion)
			if slug != tt.wantSlug || found != tt.wantFound {
				t.Fatalf("ResolveUpgradeScenario(%v) = (%q, %v), want (%q, %v)",
					tt.fromVersion, slug, found, tt.wantSlug, tt.wantFound)
			}
		})
	}
}
