package trial

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	securejoin "github.com/cyphar/filepath-securejoin"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// trialHostSID — синтетический SID единственного хоста L0-прогона. L0
// герметичен и не таргетит реальный реестр; один хост достаточен для
// render-only ассерта (per-host вариативность — слой dispatch, вне пилота).
const trialHostSID = "trial-host"

// Level — уровень испытания (ADR-023), по которому кейс маршрутизирован при
// прогоне. Различает строки отчёта: L0 (render-only) / L1 (migration) / L2
// (стенд, skip в MVP).
type Level int

const (
	LevelL0 Level = iota // render-only, hermetic (RunCase)
	LevelL1              // тест миграции state_schema (RunMigrationCase)
	LevelL2              // стенд, в MVP пропускается (ADR-023 post-MVP)
)

// Result — итог прогона одного кейса.
//
// Level — уровень, по которому кейс маршрутизирован (для отчёта). Skipped=true —
// кейс распознан как L2 (маркер stand:/verify:) и пропущен: MVP-harness его не
// исполняет (ADR-023 post-MVP). У пропущенного кейса Pass=true (не валит
// прогон), Failures/Coverage пусты, Case — имя файла. Coverage заполняется
// только для L0 (L1/L2 render-пайплайн не гоняют).
type Result struct {
	Case     string
	Level    Level
	Pass     bool
	Skipped  bool
	Failures []string // человекочитаемые расхождения assert-а; пусто при Pass
	Coverage CoverageReport
}

// renderedCase — результат герметичного render-прохода кейса: плоский план
// задач + переиспользуемые pipeline/RenderInput для последующей свёртки
// state_changes и coverage-sink с накопленными CEL-ветками. Один на прогон
// кейса, разделяется L0-ассертом (RunCase) и L2-исполнением (RunL2Case):
// и тот, и другой стартуют с одного и того же Keeper-side render-плана.
type renderedCase struct {
	tasks    []*render.RenderedTask
	pipeline *render.Pipeline
	in       render.RenderInput
	sink     *coverageSink
}

// renderCase прогоняет Keeper-side render-пайплайн кейса герметично: загружает
// scenario рядом с case.yml, раскрывает include, строит RenderInput из fixtures,
// рендерит план с fixture-vault и coverage-sink-ом. Возвращает renderedCase —
// общий старт для L0-сверки и L2-исполнения. Сам ничего не ассертит.
//
// caseFile — путь к самому case.yml (из LoadCase). scenario/<name>/main.yml
// резолвится как `<dir(case.yml)>/../../main.yml` (tests/<case>/case.yml).
func renderCase(ctx context.Context, c *Case, caseFile string) (renderedCase, error) {
	var rc renderedCase

	scnPath := scenarioPathFor(caseFile)
	scn, _, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		return rc, fmt.Errorf("trial: загрузка scenario %s: %w", scnPath, err)
	}
	if hasErrors(diags) {
		return rc, fmt.Errorf("trial: scenario %s невалиден: %s", scnPath, formatDiags(diags))
	}

	// Раскрытие scenario-include в плоский список до render (orchestration.md §6),
	// так же как в проде scenario.run. Двухуровневый резолв scenario-локально →
	// service-level из дерева-фикстуры.
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if hasErrors(iDiags) {
		return rc, fmt.Errorf("trial: раскрытие include в scenario %s: %s", scnPath, formatDiags(iDiags))
	}
	scn.Tasks = expanded

	// fixtureVault реализует и render.KVReader (vault-resolve params), и
	// cel.KVReader (CEL-функция vault()) — один герметичный reader на обе фазы.
	fv := newFixtureVault(c.Fixtures.Vault)
	engine, err := cel.New(cel.WithVault(fv))
	if err != nil {
		return rc, fmt.Errorf("trial: сборка CEL-движка: %w", err)
	}
	sink := newCoverageSink()
	engine.SetCoverageSink(sink)

	pipeline := render.NewPipeline(fv, engine, nil, nil)

	// apply:destiny резолвится зеркалом прод-модели (slice A, ADR-023):
	// service.yml::destiny[] (декларация зависимости + ref/git) + шаблон
	// default_destiny_source из case.yml (file://, герметично). Сценарии без
	// apply:destiny резолвер не дёргают; service.yml без destiny[] — не ошибка.
	deps, err := loadServiceDestinyDeps(caseFile)
	if err != nil {
		return rc, err
	}
	destiny := newFixtureDestinyResolver(serviceRootFor(caseFile), c.Fixtures.DefaultDestinySource, deps)

	// Эффективный input зеркалом прода (scenario.run §4.5): merge дефолтов
	// scenario `input:` + required + value-валидация. L0 теперь не маскирует
	// отсутствие merge-фазы — кейс может подать только обязательные input.
	effectiveInput, err := config.ResolveInputValues(scn.Input, c.Fixtures.Input)
	if err != nil {
		return rc, fmt.Errorf("trial: input %s: %w", scn.Name, err)
	}

	// Templates: ридер .tmpl снапшота сервиса кейса (двухуровневый резолв
	// scenario-local→service-level, ADR-009). serviceRoot — корень сервиса/
	// _trial-обёртки; scenario-prefix `scenario/<name>` берётся из имени scenario.
	// readWithin клампит выход за пределы serviceRoot (securejoin), зеркаля
	// прод-снапшот.
	svcRoot := serviceRootFor(caseFile)
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(svcRoot, rel) },
		"scenario/"+scn.Name,
	)

	in := render.RenderInput{
		Scenario:    scn,
		Essence:     orEmptyMap(c.Fixtures.Essence),
		Input:       effectiveInput,
		Register:    orEmptyMap(c.Mocks.Register),
		Incarnation: render.IncarnationMeta{Name: scn.Name},
		Hosts:       fixtureHosts(c.Fixtures.Soulprint),
		Destiny:     destiny,
		Templates:   templates,
	}

	tasks, _, err := pipeline.Render(ctx, in)
	if err != nil {
		return rc, fmt.Errorf("trial: render: %w", err)
	}

	rc.tasks = tasks
	rc.pipeline = pipeline
	rc.in = in
	rc.sink = sink
	return rc, nil
}

// RunCase прогоняет один L0-кейс герметично: загружает scenario рядом с
// case.yml, строит render.RenderInput из fixtures, гоняет render-пайплайн с
// fixture-vault и coverage-sink-ом, сверяет []RenderedTask с
// assert.rendered_tasks.
//
// caseFile — путь к самому case.yml (из LoadCase). scenario/<name>/main.yml
// резолвится как `<dir(case.yml)>/../../main.yml` (tests/<case>/case.yml).
func RunCase(ctx context.Context, c *Case, caseFile string) (Result, error) {
	res := Result{Case: c.Name}

	rc, err := renderCase(ctx, c, caseFile)
	if err != nil {
		return res, err
	}
	pipeline, in, sink, tasks := rc.pipeline, rc.in, rc.sink, rc.tasks

	res.Failures = compareRenderedTasks(c.Assert.RenderedTasks, tasks)

	// Рендер state_changes.sets — зеркало прода (scenario.run §7.1,
	// RenderStateChanges после барьера). В L0 нет dispatch/register-накопления, но
	// сам рендер CEL-свёртки sets гоняется всегда: незащищённый `${ input.X }` по
	// optional-без-default input (CEL «no such key») ловится здесь без ассертов —
	// это была слепая зона harness-а, видевшего только tasks. Mocks.Register даёт
	// `register.*` в sets тот же per-host register-контекст, что и в `where:`.
	in.Ctx = ctx
	in.RegisterByHost = map[string]map[string]any{trialHostSID: orEmptyMap(c.Mocks.Register)}
	renderedSets, err := pipeline.RenderStateChanges(in)
	if err != nil {
		return res, fmt.Errorf("trial: render state_changes: %w", err)
	}
	if c.Assert.StateChanges != nil {
		res.Failures = append(res.Failures, compareStateChanges(c.Assert.StateChanges, renderedSets)...)
	}

	// assert.state_after — детерминированный итоговый incarnation.state: базовый
	// fixtures.state + отрендеренные sets (зеркало прод-коммита, run.go:
	// mergeStateChanges(stateBefore, renderedSets)). Сверка ПОЛНАЯ (compareState,
	// как L1): лишний ключ в итоге — расхождение, state фиксируется целиком.
	if c.Assert.StateAfter != nil {
		stateAfter := mergeStateChanges(c.Fixtures.State, renderedSets)
		res.Failures = append(res.Failures, compareState(c.Assert.StateAfter, stateAfter)...)
	}

	res.Pass = len(res.Failures) == 0
	res.Coverage = sink.Report()
	return res, nil
}

// scenarioPathFor выводит путь scenario/<name>/main.yml из пути case.yml.
// Раскладка ([ADR-023]/orchestration.md): scenario/<name>/tests/<case>/case.yml.
func scenarioPathFor(caseFile string) string {
	caseDir := filepath.Dir(caseFile)                  // .../tests/<case>
	scenarioDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/<name>
	return filepath.Join(scenarioDir, "main.yml")
}

// serviceRootFor выводит каталог сервиса из пути case.yml. Раскладка:
// `<service-root>/scenario/<name>/tests/<case>/case.yml` (или для standalone
// L0-обёртки destiny — `<destiny>/_trial/scenario/apply/tests/<case>/case.yml`,
// где service-root = `_trial/`). service.yml (если есть) лежит в этом каталоге.
func serviceRootFor(caseFile string) string {
	caseDir := filepath.Dir(caseFile)                  // .../tests/<case>
	scenarioDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/<name>
	return filepath.Dir(filepath.Dir(scenarioDir))     // .../<service-root>
}

// loadServiceDestinyDeps читает `<service-root>/service.yml` и возвращает его
// destiny[]-зависимости (зеркало прод DestinySource.resolverFor). Отсутствие
// service.yml — не ошибка (кейс может не использовать apply:destiny): тогда
// deps пуст, и первое же apply:destiny отвергнется как необъявленная
// зависимость, симметрично проду.
func loadServiceDestinyDeps(caseFile string) ([]config.DependencyRef, error) {
	svcPath := filepath.Join(serviceRootFor(caseFile), "service.yml")
	if _, err := os.Stat(svcPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	manifest, _, diags, err := config.LoadServiceManifest(svcPath, config.ValidateOptions{})
	if err != nil {
		return nil, fmt.Errorf("trial: загрузка service.yml %s: %w", svcPath, err)
	}
	if hasErrors(diags) {
		return nil, fmt.Errorf("trial: service.yml %s невалиден: %s", svcPath, formatDiags(diags))
	}
	return manifest.Destiny, nil
}

// fixtureScenarioIncludeResolver — двухуровневый scenario-include-резолвер для L0
// (orchestration.md §6): локально `scenario/<name>/<file>`, затем service-level
// `scenario/<file>`. scnPath — путь main.yml сценария
// (`.../scenario/<name>/main.yml`). securejoin клампит выход за пределы базы.
func fixtureScenarioIncludeResolver(scnPath string) config.IncludeResolver {
	// securejoin на относительной базе с ведущим `..` нормализует и теряет выход
	// вверх (см. newFixtureDestinyResolver) — приводим к абсолютному.
	if abs, err := filepath.Abs(scnPath); err == nil {
		scnPath = abs
	}
	scenarioDir := filepath.Dir(scnPath)            // .../scenario/<name>
	serviceScenarioDir := filepath.Dir(scenarioDir) // .../scenario
	return func(name string) ([]byte, string, error) {
		local := filepath.Join(scenarioDir, name)
		data, err := readWithin(scenarioDir, name)
		if err == nil {
			return data, local, nil
		}
		// На service-level фоллбэкаем ТОЛЬКО при отсутствии локального файла;
		// I/O-ошибку (permission denied, битый симлинк) маскировать нельзя.
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("include %q: чтение локально (%s): %w", name, local, err)
		}
		service := filepath.Join(serviceScenarioDir, name)
		data, err = readWithin(serviceScenarioDir, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, "", fmt.Errorf("include %q не найден ни локально (%s), ни на service-level (%s)", name, local, service)
			}
			return nil, "", fmt.Errorf("include %q: чтение service-level (%s): %w", name, service, err)
		}
		return data, service, nil
	}
}

// readWithin читает name строго в пределах base (securejoin-кламп).
func readWithin(base, name string) ([]byte, error) {
	full, err := securejoin.SecureJoin(base, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// fixtureHosts строит roster L0 из fixtures.soulprint — один синтетический
// хост. Coven включает корневую incarnation-метку через пустой набор: L0 не
// таргетит ковены, on:/where: резолвятся в контексте этого хоста.
func fixtureHosts(soulprint map[string]any) []*topology.HostFacts {
	return []*topology.HostFacts{{
		SID:       trialHostSID,
		Soulprint: orEmptyMap(soulprint),
	}}
}

func orEmptyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// compareRenderedTasks сверяет ожидаемые задачи с отрендеренным планом.
// Возвращает список расхождений (пусто = pass). Сверка по index: для каждой
// ExpectedTask берётся RenderedTask с тем же Index.
func compareRenderedTasks(expected []ExpectedTask, got []*render.RenderedTask) []string {
	byIndex := make(map[int]*render.RenderedTask, len(got))
	for _, rt := range got {
		byIndex[rt.Index] = rt
	}

	var fails []string
	for _, et := range expected {
		rt, ok := byIndex[et.Index]
		if !ok {
			fails = append(fails, fmt.Sprintf("task index %d: ожидался в плане, но отрендерено %d задач", et.Index, len(got)))
			continue
		}
		if rt.Module != et.Module {
			fails = append(fails, fmt.Sprintf("task index %d: module = %q, ожидался %q", et.Index, rt.Module, et.Module))
		}
		if et.Params != nil {
			if diff := compareParams(et.Index, et.Params, rt.Params, rt.NoLog); diff != "" {
				fails = append(fails, diff)
			}
		}
	}
	return fails
}

// compareStateChanges сверяет ожидаемый assert.state_changes с отрендеренными
// state_changes.sets (поле → CEL-свёрнутое значение). Возвращает список
// расхождений (пусто = совпало). Обе стороны нормализуются через structpb
// (числа → float64), как в compareParams, чтобы YAML-декод ассерта и CEL-вывод
// рендера сравнивались в одной форме. Лишние поля в рендере (не упомянутые в
// ассерте) расхождением НЕ считаются — ассерт частичен, как у rendered_tasks.
func compareStateChanges(want, got map[string]any) []string {
	wantStruct, err := structpb.NewStruct(want)
	if err != nil {
		return []string{fmt.Sprintf("assert.state_changes некорректны: %v", err)}
	}
	gotStruct, err := structpb.NewStruct(got)
	if err != nil {
		return []string{fmt.Sprintf("state_changes неприводимы к сравнению: %v", err)}
	}
	wantMap := wantStruct.AsMap()
	gotMap := gotStruct.AsMap()

	var fails []string
	for _, field := range sortedKeys(wantMap) {
		gv, ok := gotMap[field]
		if !ok {
			fails = append(fails, fmt.Sprintf("state_changes.%s: ожидалось в наборе, но поле не отрендерено", field))
			continue
		}
		if !deepEqualJSON(wantMap[field], gv) {
			fails = append(fails, fmt.Sprintf("state_changes.%s расходится:\n    ожидалось: %v\n    получено:  %v", field, wantMap[field], gv))
		}
	}
	return fails
}

// presentMarker — sentinel-значение ассерта params: «ключ присутствует и несёт
// непустую строку, точное значение НЕ сверяем». Введён под `template_content`
// шага core.file.rendered (A1, ADR-012(d)): на L0 важен сам факт доставки
// literal-содержимого .tmpl Keeper→Soul (handoff не сломан), а точная посимвольная
// сверка многострочного шаблона хрупка и проверяется на L2/Real-Linux E2E.
// Регресс «template-путь уехал вместо содержимого» ловится тем, что ключа
// template_content в рендере не будет (а template-ключ assert НЕ перечисляет).
const presentMarker = "<present>"

// compareParams сверяет ожидаемые params с CEL-rendered *structpb.Struct.
// Сверка по-ключно (assert.params частичен: лишние ключи рендера не валят кейс,
// симметрично rendered_tasks). Каждое значение сравнивается через нормализован-
// ную Go-форму (structpb нормализует числа в float64).
//
// presentMarker (`<present>`) в ожидаемом значении ослабляет сверку до «ключ
// есть, непустая строка» (см. presentMarker) — для template_content.
//
// noLog — флаг RenderedTask.NoLog: params могут содержать vault-резолвленные
// секреты, поэтому при FAIL значения маскируются (печатаются только ключи),
// чтобы секрет не утёк в stdout/отчёт.
func compareParams(idx int, want map[string]any, got *structpb.Struct, noLog bool) string {
	wantStruct, err := structpb.NewStruct(want)
	if err != nil {
		return fmt.Sprintf("task index %d: assert.params некорректны: %v", idx, err)
	}
	wantMap := wantStruct.AsMap()
	gotMap := map[string]any{}
	if got != nil {
		gotMap = got.AsMap()
	}

	var diffs []string
	for _, key := range sortedKeys(wantMap) {
		wv := wantMap[key]
		gv, ok := gotMap[key]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("ключ %q: ожидался в params, отсутствует", key))
			continue
		}
		if wv == presentMarker {
			s, isStr := gv.(string)
			if !isStr || s == "" {
				diffs = append(diffs, fmt.Sprintf("ключ %q: ожидалась непустая строка (present), получено %v", key, gv))
			}
			continue
		}
		if !deepEqualJSON(wv, gv) {
			diffs = append(diffs, key)
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	if noLog {
		return fmt.Sprintf("task index %d: params расходятся (значения скрыты, no_log):\n    ключи: %v", idx, diffs)
	}
	return fmt.Sprintf("task index %d: params расходятся: %v\n    ожидалось: %v\n    получено:  %v", idx, diffs, wantMap, gotMap)
}

// sortedKeys — детерминированный список ключей map (для no_log-diff, где
// значения скрыты).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
