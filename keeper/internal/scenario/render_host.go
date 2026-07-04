package scenario

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// RenderForHost воспроизводит Keeper-side render-конвейер прогона при claim
// (ADR-027, Phase 1.4.3): по persisted-рецепту [applyrun.Recipe] и SID-у
// заклеймленного хоста проходит ТОТ ЖЕ путь, что run-goroutine — load service →
// parse scenario → ExpandIncludes → essence.Resolve → ResolveInputValuesVault
// (СЕКРЕТЫ резолвятся ТУТ, в RAM) → Render (CEL+vault) — и рендерит ПОЛНЫЙ
// roster прогона (как run-goroutine), а не один хост.
//
// Стратегия Y (architect-решение): full-roster render устраняет диалект
// per-host↔full-roster. При single-host render-е (in.Hosts=[host]) диалект
// молча расходился со старым путём: applyRunOnce(targeted≤1) не резал run_once
// (задача попадала на каждый хост вместо одного), soulprint.hosts/.where и
// incarnation.host_count схлопывались до roster-а из одного хоста, cross-host
// state_changes.sets теряли соседей. Список full-roster-зависимостей открытый,
// поэтому не точечные guard-ы, а воспроизведение полного roster-а: Acolyte
// рендерит РОВНО то же, что старый путь, и фильтрует свой SID через
// groupByHost(tasks, plans)[sid] на стороне caller-а.
//
// Цена Y (ADR-027 Trade-offs): каждый из N claim-ов рендерит полный roster →
// O(N²) per-host CEL + N×per-host vault-резолв на прогон vs O(N) старого пути.
// Приемлемо в Phase 1 (единицы-десятки хостов); сотни хостов — кандидат на
// оптимизацию.
//
// loadHostFacts ДО full-roster render-а сохраняет single-SID-валидацию «хост
// ещё в roster-е» (disconnected/revoked между dispatch и claim → ошибка):
// roster грузится для render всё равно, проверка дешёвая и не теряется.
//
// Инвариант A (ADR-027): recipe.Input несёт vault-ref СТРОКАМИ; секреты
// раскрываются ResolveInputValuesVault В RAM (стек этого вызова), в рецепт/PG
// не возвращаются. Инвариант A не меняется full-roster-ом: input-секреты —
// scenario-level (резолвятся один раз, не per-host), full-roster НЕ увеличивает
// секретный материал в RAM. Возвращаемые tasks/plans живут только в памяти
// Acolyte-а до SendApply.
//
// applyID — для audit-ctx резолва vault (от чьего имени и в каком прогоне читался
// секрет; берётся из run-строки, симметрично run-goroutine-пути). Возврат:
// плоский []RenderedTask всего прогона + []DispatchPlan (caller фильтрует по sid).
func RenderForHost(ctx context.Context, deps Deps, recipe *applyrun.Recipe, incarnationName, applyID, sid string) ([]*render.RenderedTask, []render.DispatchPlan, error) {
	if recipe == nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: nil recipe")
	}
	if sid == "" {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: empty sid")
	}

	// 1. Service-артефакт (git-снапшот) + парсинг scenario/<name>/main.yml.
	art, err := deps.Loader.Load(ctx, recipe.ServiceRef)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: load service: %w", err)
	}
	// Acolyte-путь остаётся scenario-only в Slice 1 (ADR-0068): upgrade-рецепт для
	// re-render по хосту — Slice 2/3, recipe пока не несёт FromUpgrade.
	scn, err := parseScenarioFromArtifact(deps.Loader, art, recipe.ScenarioName, false)
	if err != nil {
		return nil, nil, err
	}

	// 2. Раскрытие include в плоский список — ДО render (как в run-goroutine).
	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(deps.Loader, art, recipe.ScenarioName))
	if diag.HasErrors(idiags) {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: раскрытие include в %s/%s: %s",
			recipe.ScenarioName, scenarioMainFile, firstError(idiags))
	}
	scn.Tasks = expanded

	// Синтез install-шагов из modules[] (ADR-065) — Acolyte обязан воспроизвести
	// РОВНО план run-goroutine (те же задачи, те же plan_index), иначе синтез-шаг
	// потерялся бы на claim-пути, а корреляция TaskEvent↔RenderedTask съехала бы.
	scn.Tasks, _ = config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules)

	// 3. Roster прогона (как run-goroutine): весь roster incarnation. SID-валидация
	//    «хост ещё в roster-е» (disconnected/revoked между dispatch и claim →
	//    ошибка) делается ТУТ, до full-roster render-а — roster грузится всё равно,
	//    проверка дешёвая. render оперирует ПОЛНЫМ roster-ом (стратегия Y), Acolyte
	//    фильтрует свой SID на выходе.
	hosts, err := loadRosterWithHost(ctx, deps.Topology, incarnationName, sid)
	if err != nil {
		return nil, nil, err
	}

	// 4. incarnation (для essence-override spec.essence + IncarnationMeta).
	inc, err := incarnation.SelectByName(ctx, deps.DB, incarnationName)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: load incarnation %q: %w", incarnationName, err)
	}

	// 5. Essence (effective-слой). Представитель OS-family — первый хост roster-а,
	//    симметрично run-goroutine (run.go шаг 4: hosts[0]); per-host essence —
	//    расширение.
	essenceMap, err := deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: essence: %w", err)
	}

	// 6. Эффективный input: merge дефолтов/required + scoped-резолв vault-ref
	//    (СЕКРЕТЫ резолвятся ТУТ, в RAM — инвариант A) + value-валидация.
	resolver := buildInputVaultResolver(ctx, deps.Vault, deps.Audit, depsLogger(deps), inputVaultAuditCtx{
		aid:         recipeAID(recipe),
		incarnation: incarnationName,
		scenario:    recipe.ScenarioName,
	}, deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, recipe.Input, resolver)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: input %s/%s: %w", incarnationName, recipe.ScenarioName, err)
	}

	// 7. Render: vault-resolve → CEL → on/where → []RenderedTask + []DispatchPlan.
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts, // ПОЛНЫЙ roster (стратегия Y) — caller фильтрует свой SID
		// State — снимок incarnation.state для `incarnation.state.<path>` (ADR-009/010).
		// Acolyte (failover-claim) обязан воспроизвести РОВНО те же params, что
		// run-goroutine: state коммитится только ПОСЛЕ успешного apply прогона,
		// поэтому inc.State, загруженный сейчас, == pre-run stateBefore прогона —
		// read-only снимок идентичен исходному. Без него Acolyte отрендерил бы
		// `incarnation.state.*` как no-such-key и разошёлся бы с оригиналом.
		State: inc.State,
		Ctx:   ctx,
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(recipe.ScenarioName),
		),
	}
	if deps.Destiny != nil {
		renderIn.Destiny = deps.Destiny.resolverFor(art.Manifest)
	}
	tasks, plans, err := deps.Render.Render(ctx, renderIn)
	if err != nil {
		return nil, nil, fmt.Errorf("scenario: RenderForHost: render %s/%s: %w", incarnationName, recipe.ScenarioName, err)
	}
	return tasks, plans, nil
}

// loadRosterWithHost резолвит ВЕСЬ roster incarnation (для full-roster render-а,
// стратегия Y) и валидирует, что заклеймленный sid в нём присутствует. Хост,
// выпавший из roster-а между dispatch-ем и claim-ом (disconnected / revoked), —
// ошибка: рендерить задание не на чем. Валидация single-SID сохранена ДО
// render-а (roster грузится всё равно — проверка дешёвая, потеря проверки
// disconnected-хоста недопустима).
func loadRosterWithHost(ctx context.Context, topo *topology.Resolver, incarnationName, sid string) ([]*topology.HostFacts, error) {
	hosts, err := topo.LoadIncarnationHosts(ctx, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("scenario: RenderForHost: topology: %w", err)
	}
	for _, h := range hosts {
		if h.SID == sid {
			return hosts, nil
		}
	}
	return nil, fmt.Errorf("scenario: RenderForHost: хост %q не в roster-е incarnation %q (disconnected с момента dispatch-а)", sid, incarnationName)
}

// recipeAID разворачивает recipe.StartedByAID (*string) в "" при nil — форма
// inputVaultAuditCtx.aid (пустой → archon_aid колонка NULL).
func recipeAID(r *applyrun.Recipe) string {
	if r.StartedByAID == nil {
		return ""
	}
	return *r.StartedByAID
}

// depsLogger отдаёт Deps.Logger либо discard-логгер при nil (Deps.Logger
// опционален — NewRunner так же подставляет discard).
func depsLogger(deps Deps) *slog.Logger {
	if deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return deps.Logger
}
