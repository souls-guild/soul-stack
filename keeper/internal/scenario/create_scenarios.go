package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// ErrCreateScenarioNotEligible — выбранный оператором стартовый сценарий
// (`create_scenario` в POST /v1/incarnations) НЕ входит в create-набор сервиса:
// либо имя невалидно, либо сценарий не помечен `create: true` (operational —
// например `add_user`), либо его нет в снапшоте. Handler маппит в 422
// validation_failed: incarnation НЕ создаётся (отказ на этапе модели).
var ErrCreateScenarioNotEligible = errors.New("scenario: chosen create_scenario is not an eligible bootstrap scenario for this service")

// ErrCreateScenarioRequired — сервис ИМЕЕТ create-сценарии (≥1 с `create: true`),
// но оператор НЕ выбрал ни одного (`create_scenario` пуст). Выбор обязателен:
// input валидируется против `input:`-схемы КОНКРЕТНОГО сценария, без выбора запрос
// некорректен (нечего применять). Handler маппит в 422 validation_failed с
// перечислением годных сценариев. Отличается от [ErrCreateScenarioNotEligible]
// (там выбор СДЕЛАН, но не годен) — здесь выбор ОТСУТСТВУЕТ при непустом наборе.
var ErrCreateScenarioRequired = errors.New("scenario: create_scenario is required (service offers create scenarios)")

// CreateScenarioLoader — узкая поверхность [artifact.ServiceLoader], нужная
// резолву create-набора: материализовать снапшот service-ref-а (его LocalDir
// сканируется через [artifact.ListScenarios]). *artifact.ServiceLoader
// удовлетворяет; unit-тесты подставляют fake без git-стека.
type CreateScenarioLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
}

// ResolveCreateScenarios возвращает множество имён сценариев сервиса `ref`,
// годных как стартовые (bootstrap новой incarnation): РОВНО сценарии с top-level
// `create: true` в `scenario/<name>/main.yml` (механизм нескольких create-
// сценариев). Имя `create` НЕ привилегировано — оно попадает в набор только если
// сам `scenario/create/main.yml` несёт `create: true`, как любой другой.
//
// Сервис без единого `create: true` даёт ПУСТОЙ набор — это валидный случай:
// caller трактует его как bare-инкарнацию (создаётся StatusReady без прогона,
// [ValidateCreateScenarioChoice]), а непустой выбор для такого сервиса → 422.
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

	set := map[string]struct{}{}
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
// стартовый сценарий по трём ветвям контракта (решение пользователя 2026-06-29):
//
//   - chosen НЕПУСТОЙ + в create-наборе → запустить его (возврат имени, bare=false);
//     не в наборе / невалидное имя → [ErrCreateScenarioNotEligible].
//   - chosen ПУСТОЙ + набор НЕпуст (сервис предлагает create-сценарии) →
//     [ErrCreateScenarioRequired]: выбор обязателен (input зависит от сценария).
//   - chosen ПУСТОЙ + набор ПУСТ (нет ни одного `create: true`) → bare-инкарнация
//     (возврат "", bare=true): caller создаёт StatusReady без прогона.
//
// Возврат `(name, bare, err)`: при bare=true name="" — ОДНОЗНАЧНЫЙ контракт (имя
// сценария отсутствует, прогона нет), caller обязан ветвиться на bare ДО трактовки
// name. Заменяет прежний back-compat-шорткат (пустой → дефолтный `create`).
//
// Невалидное имя (traversal/мусор по [ScenarioNamePattern]) отбивается ДО резолва
// набора как [ErrCreateScenarioNotEligible] — не подставляем мусор в путь.
func ValidateCreateScenarioChoice(ctx context.Context, loader CreateScenarioLoader, ref artifact.ServiceRef, chosen string) (string, bool, error) {
	if chosen != "" && !ValidScenarioName(chosen) {
		return "", false, fmt.Errorf("%w: name %q does not match %s", ErrCreateScenarioNotEligible, chosen, ScenarioNamePattern)
	}
	set, err := ResolveCreateScenarios(ctx, loader, ref)
	if err != nil {
		return "", false, err
	}
	if chosen == "" {
		if len(set) == 0 {
			// Нет create-сценариев → bare-инкарнация (без прогона).
			return "", true, nil
		}
		return "", false, fmt.Errorf("%w: choose one of %s", ErrCreateScenarioRequired, sortedNames(set))
	}
	if _, ok := set[chosen]; !ok {
		return "", false, fmt.Errorf("%w: %q", ErrCreateScenarioNotEligible, chosen)
	}
	return chosen, false, nil
}

// sortedNames — детерминированный отсортированный список имён set-а для
// сообщения [ErrCreateScenarioRequired] (стабильный текст 422, тестируемый).
func sortedNames(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
