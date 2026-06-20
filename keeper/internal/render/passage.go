// Passage-стратификация ([ADR-056](../../../docs/adr/0056-staged-render-passage.md)).
// Канон логики вынесен в shared/config (passage.go) — ОДИН граф register-
// зависимости для keeper-рантайма, keeper-тестов и ОФЛАЙН soul-lint-а (дубль =
// риск silent-wrong-target). Этот файл — тонкие alias-ы на shared-символы, чтобы
// keeper-side вызовы (run.go, render-тесты) не переписывались.
package render

import (
	"github.com/souls-guild/soul-stack/shared/config"
)

// Passage — alias на канонический [config.Passage] (стратификационный план прогона).
type Passage = config.Passage

// StratifyError-коды — реэкспорт канона shared/config.
const (
	StratifyCycle           = config.StratifyCycle
	StratifyUnknownRegister = config.StratifyUnknownRegister
)

// errStratify — alias на [config.StratifyError] (несёт Code/Msg). Имя сохранено
// для keeper-side тестов (errors.As на *errStratify).
type errStratify = config.StratifyError

// Stratify — alias на канонический [config.Stratify]: вычисляет passage-индексы
// плана задач прогона по графу cross-task register-зависимости.
func Stratify(tasks []config.Task) (Passage, error) {
	return config.Stratify(tasks)
}
