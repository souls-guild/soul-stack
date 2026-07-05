package artifact

// ResolveUpgradeScenario выбирает upgrade-сценарий для перехода `fromVersion → to`
// (ADR-0068 §5): возвращает slug первого upgrade-сценария, чей `from:` содержит
// fromVersion. `upgrades` уже отсортирован по имени ([listFromDir]) — при нескольких
// совпадениях берётся первый по имени (детерминированно).
//
// Пустой fromVersion или отсутствие совпадения → ("", false): апгрейд идёт legacy-
// веткой (смена пина + state-миграции + drift, §5), а не падает — это осознанный
// fail-open (§5 ★: undeclared-переход не 422, чтобы патч-апгрейды не ломались).
// Upgrade-сценарий без `from:` (пустой FromVersions) не матчит ничего.
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
