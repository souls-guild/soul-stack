package artifact

// ResolveUpgradeScenario picks the upgrade scenario for the `fromVersion → to`
// transition (ADR-0068 §5): returns the slug of the first upgrade scenario whose
// `from:` contains fromVersion. `upgrades` is already sorted by name ([listFromDir])
// — on multiple matches the first by name wins (deterministic).
//
// Empty fromVersion or no match → ("", false): the upgrade takes the legacy path
// (pin change + state migrations + drift, §5) instead of failing — a deliberate
// fail-open (§5 ★: an undeclared transition is not a 422, so patch upgrades don't
// break). An upgrade scenario without `from:` (empty FromVersions) matches nothing.
func ResolveUpgradeScenario(upgrades []Scenario, fromVersion string) (slug string, found bool) {
	if fromVersion == "" {
		return "", false
	}
	for _, u := range upgrades {
		for _, from := range u.FromVersions {
			if from == fromVersion {
				return u.Name, true
			}
		}
	}
	return "", false
}
