package scenario

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ErrAssertFailed re-export render-сентинела (ADR-009 amendment 2026-06-23,
// двухточечная eval): pre-flight ([Runner.PreflightAssert]) и render-fail-safe
// ([render.Pipeline.EvalAsserts] / [render.Pipeline.Render]) возвращают ОДНУ
// ошибку — caller (create-handler) различает «assert не прошёл» (→ 422
// assert_failed) от прочих сбоев pre-flight по [errors.Is]. Не вводим второй
// сентинел: assert-провал — одна доменная семантика на обе точки.
var ErrAssertFailed = render.ErrAssertFailed

// PreflightAssert вычисляет assert-предикаты сценария НА СОЗДАНИИ прогона
// (request-путь create-handler-а, ДО коммита incarnation и ДО входа в applying —
// ADR-027 amendment, новый pre-flight-гейт; ADR-009 amendment 2026-06-23, форма
// A). Основной UX assert: неверная топология прогона (roster не сходится с
// инвариантом сценария) отклоняется оператору как 422 assert_failed, БЕЗ записи
// incarnation и БЕЗ fail-статуса (error_locked) — отказ переезжает с async
// render-фазы на синхронный request-путь.
//
// Контракт:
//   - roster резолвится по корневой Coven-метке spec.IncarnationName (для create
//     — req.Name, ADR-008): [topology.Resolver.LoadIncarnationHosts] НЕ требует
//     существующей incarnation-записи (фильтр rosterSQL — по `coven[]` souls,
//     declared-роли пустые для ещё-не-созданной — не ошибка). Roster-at-create =
//     connected-souls на момент создания (CreateTyped сразу Start-ит → это roster
//     имминентного прогона). 0 connected souls → topology-assert (size==N) честно
//     не сойдётся → ErrAssertFailed (корректно: нельзя создать+запустить
//     N-шардовый кластер без N хостов).
//   - effectiveInput — материализованные дефолты + required по `input:`-схеме
//     (config.ResolveInputValues, БЕЗ vault-резолва: инвариант A ADR-027 —
//     секрет не материализуется в request-пути; input уже провалидирован
//     ValidateInput-ом до этого вызова, поэтому value-валидация здесь
//     заведомо проходит).
//   - essence — effective-слой для представительного хоста (как run() шаг 4),
//     чтобы assert-предикат, ссылающийся на essence.*, видел те же значения, что
//     render. Для create incarnation-записи ещё нет → синтетический Incarnation
//     из spec (Name/Service/Spec=input) для override-слоя essence.
//   - EvalAsserts эмитит ТОЛЬКО assert-предикаты (общий [render.evalAssertTask] —
//     тот же источник, что render-ветка): первый false → ErrAssertFailed.
//
// no-op для сценариев БЕЗ assert-задач (большинство): EvalAsserts проходит tasks
// и не находит assert → nil. Сценарий не загружается дважды зря — но pre-flight
// сам грузит снапшот (ValidateInput тоже грузил; общий снапшот-кэш loader-а
// делает повторную Load дешёвой).
//
// Ошибки загрузки снапшота / парсинга / roster / essence НЕ оборачиваются в
// ErrAssertFailed — caller маппит их в 500 (внутренний сбой pre-flight), а
// ErrAssertFailed — в 422 (предусловие модели не выполнено).
func (r *Runner) PreflightAssert(ctx context.Context, spec RunSpec) error {
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		return fmt.Errorf("preflight: load service: %w", err)
	}
	scn, err := r.parseScenario(art, spec.ScenarioName, spec.FromUpgrade)
	if err != nil {
		return fmt.Errorf("preflight: %w", err)
	}

	// Раскрытие include ДО проверки наличия assert: в диспетчер-сценарии (redis
	// main.yml) top-level — mode-guard + `include:` веток, а assert (size-guard)
	// живёт ВНУТРИ включаемой ветки. Проверка hasAssertTask на нераскрытом списке
	// его не нашла бы → ложный no-op (баг, пойманный live: error_locked вместо
	// 422). Симметрия с render: render.Pipeline тоже раскрывает include и
	// вычисляет assert в expanded-списке — single-source сохраняется.
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, spec.ScenarioName))
	if diag.HasErrors(idiags) {
		return fmt.Errorf("preflight: раскрытие include в %s/%s: %s", spec.ScenarioName, scenarioMainFile, firstError(idiags))
	}
	scn.Tasks = expanded

	// Быстрый выход: сценарий без assert-задач (большинство) — pre-flight no-op,
	// roster/essence/input не резолвим зря.
	if !hasAssertTask(scn.Tasks) {
		return nil
	}

	hosts, err := r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
	if err != nil {
		return fmt.Errorf("preflight: roster %s: %w", spec.IncarnationName, err)
	}

	// effectiveInput: merge дефолтов + required (vault-ref как строка, инвариант A).
	// ValidateInput уже отбил невалидный input до этого вызова — ошибка здесь
	// невозможна для корректного флоу, но возвращаем её как внутренний сбой (не
	// assert_failed), не глотаем.
	effectiveInput, err := config.ResolveInputValues(scn.Input, spec.Input)
	if err != nil {
		return fmt.Errorf("preflight: input %s/%s: %w", spec.IncarnationName, spec.ScenarioName, err)
	}

	// essence — представительный хост (как run() шаг 4). Пустой roster → assert
	// топологии всё равно не сойдётся; essence-слой пуст (essence.* в pilot-assert
	// не используется, но симметрию с render держим).
	synthetic := &incarnation.Incarnation{
		Name:    spec.IncarnationName,
		Service: art.Manifest.Name,
		Spec:    incarnationSpecFromInput(spec.Input),
	}
	essenceMap, err := r.resolvePreflightEssence(art.LocalDir, synthetic, hosts)
	if err != nil {
		return fmt.Errorf("preflight: essence %s: %w", spec.IncarnationName, err)
	}

	in := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           spec.IncarnationName,
			Service:        spec.ServiceRef.Name,
			ServiceVersion: spec.ServiceRef.Ref,
		},
		Hosts: hosts,
	}
	return r.deps.Render.EvalAsserts(ctx, in)
}

// resolvePreflightEssence резолвит essence для pre-flight по представительному
// хосту (первый roster-хост или синтетический пустой при 0 connected). Зеркало
// run() шага 4 (essenceInput → Essence.Resolve), вынесено для read-only
// pre-flight-пути.
func (r *Runner) resolvePreflightEssence(serviceDir string, inc *incarnation.Incarnation, hosts []*topology.HostFacts) (map[string]any, error) {
	host := &topology.HostFacts{}
	if len(hosts) > 0 {
		host = hosts[0]
	}
	return r.deps.Essence.Resolve(essenceInput(serviceDir, inc, host))
}

// hasAssertTask сообщает, есть ли в плоском списке задач хотя бы одна assert-задача.
// Вызывается ПОСЛЕ ExpandIncludes: assert может жить как на top-level сценария,
// так и внутри include-ветки (диспетчер-паттерн redis main.yml), поэтому проверка
// ведётся по уже раскрытому списку. Дублирования eval нет: фактическое вычисление
// assert делает [render.Pipeline.EvalAsserts]; этот предикат — лишь ранний
// no-op-выход.
func hasAssertTask(tasks []config.Task) bool {
	for i := range tasks {
		if render.IsAssertTask(tasks[i]) {
			return true
		}
	}
	return false
}

// incarnationSpecFromInput строит spec синтетического Incarnation для essence-
// override-слоя pre-flight: кладёт оператор-input под ключ `input` (как
// CreateTyped). essence читает override из spec.essence — у create его нет
// (оператор задаёт essence-override в spec, но pre-flight на создании видит
// только input), поэтому override пуст; base-essence сервиса резолвится из
// snapshot-а. nil-input → пустой spec.
func incarnationSpecFromInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	return map[string]any{"input": input}
}
