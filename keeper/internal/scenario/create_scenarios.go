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

// CreatePlanLoader — узкая поверхность [artifact.ServiceLoader], нужная
// [ResolveCreatePlan]: объединяет требования [CreateScenarioLoader] (резолв
// create-набора + lifecycle-снапшот) и [InputScenarioLoader] (чтение
// scenario/<name>/main.yml для input-валидации). *artifact.ServiceLoader
// удовлетворяет; unit-тесты подставляют fake без git-стека.
type CreatePlanLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
}

// AssertPreflighter — узкая поверхность scenario.Runner для pre-flight-гейта
// `assert:` ([Runner.PreflightAssert], ADR-009/ADR-027 amendment 2026-06-23,
// форма A). *Runner удовлетворяет; ScenarioStarter-фейки без метода → type-
// assertion в [ResolveCreatePlan] не проходит, assert-гейт no-op (как раньше в
// обоих handler-ах). Дублирует локальные интерфейсы handlers.AssertPreflighter /
// mcp.assertPreflighter — оставлены ради изоляции пакетов (handlers/mcp не тянут
// scenario-internal-интерфейс в свою сигнатуру), но фактический гейт сведён сюда.
type AssertPreflighter interface {
	PreflightAssert(ctx context.Context, spec RunSpec) error
}

// CreatePlan — результат [ResolveCreatePlan]: разрешённый стартовый сценарий
// create + флаги ветвления, общие для REST CreateTyped и MCP callIncarnationCreate.
//
//   - CreateScenario — фактический bootstrap-сценарий (выбор оператора либо
//     дефолт [CreateScenarioName] в stub-режиме без loader-а). Пишется в
//     incarnation.created_scenario (NULL при BareNoScenario, см. handler).
//   - BareNoScenario — сервис НЕ предлагает ни одного `create: true`: инкарнация
//     создаётся StatusReady БЕЗ прогона (created_scenario=NULL).
//   - AutoCreate — политика lifecycle.auto_create целевого сервиса (default true):
//     false → инкарнация ready без прогона, но created_scenario непустое (прогон
//     отложен, не bare).
type CreatePlan struct {
	CreateScenario string
	BareNoScenario bool
	AutoCreate     bool
}

// ResolveCreatePlan — общий резолв стартового сценария create, входной валидации
// и pre-flight-assert-гейта для POST /v1/incarnations (REST CreateTyped) и
// keeper.incarnation.create (MCP). Извлечено ДОСЛОВНО из обоих handler-ов (R2,
// поведение НЕ меняется — те же sentinel-ошибки в том же порядке): дублирование
// ~ветвления убрано, caller-ы маппят возвращённые ошибки в свой транспорт
// (*problemError / toolError) через errors.Is.
//
// Последовательность (как в исходных handler-ах):
//
//  1. loader == nil (stub-режим REST: runner есть, loader нет) → план без резолва
//     набора: {CreateScenarioName, bare=false, autoCreate=true} — legacy-поведение
//     «запускаем `create`». MCP в проде сюда не попадает (loader всегда вместе с
//     runner-ом), но контракт симметричен.
//  2. loader != nil → [ValidateCreateScenarioChoice] (chosen в наборе / required /
//     bare). При bare возврат сразу (без ValidateInput/lifecycle — прогона нет).
//  3. не-bare → [ValidateInput] (required/type/validate против `input:`-схемы
//     ВЫБРАННОГО сценария) + lifecycle.auto_create из снапшота.
//  4. !bare && autoCreate → [AssertPreflighter.PreflightAssert] (если preflighter
//     реализует интерфейс — иначе no-op, как при ScenarioStarter-фейке).
//
// Ошибки (для errors.Is на стороне caller-а): [ErrCreateScenarioRequired] /
// [ErrCreateScenarioNotEligible] / [ErrInputInvalid] / [ErrValidateFailed] /
// [ErrAssertFailed] — доменные (422); прочие (load/parse снапшота, eval-сбой) —
// обёрнутые fmt.Errorf (handler → 500).
func ResolveCreatePlan(
	ctx context.Context,
	loader CreatePlanLoader,
	preflighter any,
	incarnationName string,
	serviceRef artifact.ServiceRef,
	chosenScenario string,
	input map[string]any,
	startedByAID string,
) (CreatePlan, error) {
	// Дефолт: stub-режим / loader не сконфигурирован — legacy `create`, не bare,
	// auto_create=true (как было в обоих handler-ах при nil-loader).
	plan := CreatePlan{CreateScenario: CreateScenarioName, AutoCreate: true}

	if loader != nil {
		// Резолв+валидация выбора стартового сценария ДО ValidateInput: input
		// валидируется против `input:`-схемы ИМЕННО выбранного сценария.
		chosen, isBare, err := ValidateCreateScenarioChoice(ctx, loader, serviceRef, chosenScenario)
		if err != nil {
			return CreatePlan{}, err
		}
		plan.CreateScenario = chosen
		plan.BareNoScenario = isBare

		// bare (нет create-сценария): ValidateInput / lifecycle-резолв пропускаем —
		// прогона не будет, валидировать input против несуществующего сценария нечем.
		if !isBare {
			if err := ValidateInput(ctx, loader, serviceRef, chosen, input); err != nil {
				return CreatePlan{}, err
			}
			art, err := loader.Load(ctx, serviceRef)
			if err != nil {
				return CreatePlan{}, fmt.Errorf("scenario: resolve create plan: load service snapshot: %w", err)
			}
			if art != nil && art.Manifest != nil {
				plan.AutoCreate = art.Manifest.Lifecycle.AutoCreateEnabled()
			}
		}
	}

	// Pre-flight assert-гейт (ADR-009/ADR-027 amendment 2026-06-23, форма A): ПОСЛЕ
	// ValidateInput (input материализован) и ДО incarnation.Create/Start. Гейтится
	// !bare && autoCreate (при bare нет сценария, при autoCreate=false прогон не
	// стартует). Опционален: preflighter без PreflightAssert / сценарий без assert-
	// задач → no-op. render-assert остаётся fail-safe для TOCTOU.
	if !plan.BareNoScenario && plan.AutoCreate {
		if pf, ok := preflighter.(AssertPreflighter); ok {
			if err := pf.PreflightAssert(ctx, RunSpec{
				IncarnationName: incarnationName,
				ServiceRef:      serviceRef,
				ScenarioName:    plan.CreateScenario,
				Input:           input,
				StartedByAID:    startedByAID,
			}); err != nil {
				return CreatePlan{}, err
			}
		}
	}

	return plan, nil
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
