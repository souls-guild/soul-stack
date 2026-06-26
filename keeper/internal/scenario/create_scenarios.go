package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// ErrCreateScenarioNotEligible — выбранный оператором стартовый сценарий
// (`create_scenario` в POST /v1/incarnations) НЕ входит в create-набор сервиса:
// либо имя невалидно, либо сценарий не помечен `create: true` (operational —
// например `add_user`), либо его нет в снапшоте. Handler маппит в 422
// validation_failed: incarnation НЕ создаётся (отказ на этапе модели).
var ErrCreateScenarioNotEligible = errors.New("scenario: chosen create_scenario is not an eligible bootstrap scenario for this service")

// CreateScenarioLoader — узкая поверхность [artifact.ServiceLoader], нужная
// резолву create-набора: материализовать снапшот service-ref-а (его LocalDir
// сканируется через [artifact.ListScenarios]). *artifact.ServiceLoader
// удовлетворяет; unit-тесты подставляют fake без git-стека.
type CreateScenarioLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
}

// ResolveCreateScenarios возвращает множество имён сценариев сервиса `ref`,
// годных как стартовые (bootstrap новой incarnation): сценарии с top-level
// `create: true` в `scenario/<name>/main.yml` (механизм нескольких create-
// сценариев) ∪ {default [CreateScenarioName]}.
//
// Default `create` включается В НАБОР ВСЕГДА (back-compat): сервис без явных
// флагов с единственным `scenario/create/` продолжает работать без правок, а
// дефолтный create-запрос не отбивается 422 на старом сервисе. Реальное наличие
// файла `scenario/create/main.yml` проверяется уже прогоном (ValidateInput /
// render), не этим резолвом — здесь только membership-гейт выбора.
//
// Снапшот грузится через loader (кешируется loader-ом — повторная загрузка в том
// же запросе = cache hit). Сканирование scenario-каталога переиспользует
// [artifact.ListScenarios] (тот же partial-success: сломанный YAML одного
// сценария warning-ит и пропускается, не валит набор).
func ResolveCreateScenarios(ctx context.Context, loader CreateScenarioLoader, ref artifact.ServiceRef) (map[string]struct{}, error) {
	if loader == nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: loader is not configured")
	}
	art, err := loader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: load service: %w", err)
	}

	set := map[string]struct{}{CreateScenarioName: {}}
	if art == nil {
		return set, nil
	}
	scenarios, err := artifact.ListScenarios(art.LocalDir, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, fmt.Errorf("scenario: resolve create scenarios: list %s: %w", ref.Name, err)
	}
	for _, sc := range scenarios {
		if sc.Create {
			set[sc.Name] = struct{}{}
		}
	}
	return set, nil
}

// ValidateCreateScenarioChoice резолвит и валидирует выбранный оператором
// стартовый сценарий: пустой `chosen` → default [CreateScenarioName] (back-
// compat); непустой обязан входить в create-набор сервиса ([ResolveCreateScenarios]),
// иначе [ErrCreateScenarioNotEligible]. Возвращает фактическое имя сценария для
// прогона (то, что уйдёт в RunSpec.ScenarioName) — заменяет хардкод
// [CreateScenarioName] в create-handler-ах.
//
// Невалидное имя (traversal/мусор по [ScenarioNamePattern]) отбивается ДО резолва
// набора как [ErrCreateScenarioNotEligible] — не подставляем мусор в путь.
func ValidateCreateScenarioChoice(ctx context.Context, loader CreateScenarioLoader, ref artifact.ServiceRef, chosen string) (string, error) {
	if chosen == "" {
		return CreateScenarioName, nil
	}
	if !ValidScenarioName(chosen) {
		return "", fmt.Errorf("%w: name %q does not match %s", ErrCreateScenarioNotEligible, chosen, ScenarioNamePattern)
	}
	// Дефолтный `create` валиден без загрузки снапшота (всегда в наборе) — частый
	// путь, экономим резолв.
	if chosen == CreateScenarioName {
		return CreateScenarioName, nil
	}
	set, err := ResolveCreateScenarios(ctx, loader, ref)
	if err != nil {
		return "", err
	}
	if _, ok := set[chosen]; !ok {
		return "", fmt.Errorf("%w: %q", ErrCreateScenarioNotEligible, chosen)
	}
	return chosen, nil
}
