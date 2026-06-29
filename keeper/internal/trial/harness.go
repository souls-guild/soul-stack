package trial

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// trialHostSID — синтетический SID хоста single-host сахара (fixtures.soulprint).
// L0 герметичен и не таргетит реальный реестр; для render-only ассерта single
// хоста достаточно. Multi-host roster (fixtures.hosts) несёт собственные SID-ы
// (per-host dispatch-вариативность — слой L3, вне пилота).
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

// loadResolvedScenario загружает scenario/<name>/main.yml по пути case.yml и
// выполняет covenant-резолв ЗЕРКАЛОМ прода (keeper LoadScenarioManifestResolved):
// сливает covenant.yml (по scn.Extends, сиблинг service.yml в корне тестового
// дерева, serviceRootFor) и валидирует form пост-merge. Без этого covenant-сценарий
// падал бы ложным form_field_unknown в semantic-фазе (form гейтнут до merge,
// scenario.go), а CEL compute/input на covenant-полях («${ compute.install }» и др.)
// не резолвился бы. $type-резолв в L0 не гоняется (нет каталога-обёртки) —
// covenant-merge достаточно.
//
// ЕДИНЫЙ источник covenant-резолва для ВСЕХ render-хелперов trial (renderCase из
// harness.go И renderCreateReadSet из redis_create_secrets_coverage_test.go): любой
// новый render-helper обязан звать ЕГО, иначе снова забудет covenant и упадёт на
// «no such key: compute.install». Guard-тесты на уровне плана (loadCreatePlan/
// loadScenarioPlan) covenant НЕ резолвят намеренно — они сверяют []Task до render,
// covenant-поля там не вычисляются.
func loadResolvedScenario(caseFile string) (*config.ScenarioManifest, *config.Document, error) {
	scnPath := scenarioPathFor(caseFile)
	scn, doc, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("trial: загрузка scenario %s: %w", scnPath, err)
	}
	diags = append(diags, config.ResolveScenarioCovenant(scn, doc, serviceRootFor(caseFile))...)
	if hasErrors(diags) {
		return nil, nil, fmt.Errorf("trial: scenario %s невалиден: %s", scnPath, formatDiags(diags))
	}
	return scn, doc, nil
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

	scn, _, err := loadResolvedScenario(caseFile)
	if err != nil {
		return rc, err
	}
	scnPath := scenarioPathFor(caseFile)

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

	// validate: — декларативные input-инварианты (ADR-009 amendment, DSL wave 2),
	// зеркало pre-flight-гейта прода (scenario.ValidateInput). Input-only eval над
	// смерженным input; первый провал обрывает кейс той же ошибкой, что и
	// required_when (testable через expect_render_error). no-op без validate-секции.
	if fail, evErr := config.EvalValidateRules(scn.Validate, effectiveInput); evErr != nil {
		return rc, fmt.Errorf("trial: validate %s: %w", scn.Name, evErr)
	} else if fail != nil {
		return rc, fmt.Errorf("trial: validate %s: %s", scn.Name, fail.Error())
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
		Hosts:       fixtureHosts(scn.Name, c.Fixtures),
		Destiny:     destiny,
		Templates:   templates,
		// State — fixtures.state как pre-run снимок incarnation.state: доступен в
		// CEL как `incarnation.state.<path>` (ADR-009/010), тот же, что merge ниже
		// берёт за stateBefore. nil (кейс без state) → ключ не объявляется
		// (`incarnation.state.x` = no-such-key), поведение прежних кейсов БИТ-В-БИТ.
		State: c.Fixtures.State,
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

	// expect_render_error (ADR-023 amendment): кейс ОЖИДАЕТ обрыв рендера
	// (assert:-провал / required_when). Render-успех → FAIL; ошибка без подстроки
	// → FAIL; ошибка с подстрокой → PASS. Coverage пуст (план не построен), но кейс
	// проходит. Сверяем raw-ошибку renderCase до её оборачивания в return err ниже.
	if c.ExpectRenderError != "" {
		if err == nil {
			res.Failures = append(res.Failures, fmt.Sprintf("ожидался обрыв рендера с подстрокой %q, но render успешен", c.ExpectRenderError))
		} else if !strings.Contains(err.Error(), c.ExpectRenderError) {
			res.Failures = append(res.Failures, fmt.Sprintf("ожидалась ошибка рендера с подстрокой %q, получено: %v", c.ExpectRenderError, err))
		}
		res.Pass = len(res.Failures) == 0
		return res, nil
	}

	if err != nil {
		return res, err
	}
	pipeline, in, sink, tasks := rc.pipeline, rc.in, rc.sink, rc.tasks

	res.Failures = compareRenderedTasks(c.Assert.RenderedTasks, tasks)
	// Presence-форма (assert-by-presence, PILOT): сосуществует с позиционной —
	// обе сверки независимы, кейс может нести любую комбинацию.
	res.Failures = append(res.Failures, compareTaskPresence(c.Assert.TaskPresent, c.Assert.TaskAbsent, tasks)...)

	// Рендер state_changes.sets — зеркало прода (scenario.run §7.1,
	// RenderStateChanges после барьера). В L0 нет dispatch/register-накопления, но
	// сам рендер CEL-свёртки sets гоняется всегда: незащищённый `${ input.X }` по
	// optional-без-default input (CEL «no such key») ловится здесь без ассертов —
	// это была слепая зона harness-а, видевшего только tasks. Mocks.Register даёт
	// `register.*` в sets тот же per-host register-контекст, что и в `where:`.
	in.Ctx = ctx
	// Mocks.Register — единый L0-payload probe (probe-per-host = dispatch-слой L3,
	// вне пилота): один и тот же register-контекст применяется к каждому хосту
	// roster-а по его SID. На single-host roster это ровно {trialHostSID: register}
	// (back-compat бит-в-бит).
	mockReg := orEmptyMap(c.Mocks.Register)
	in.RegisterByHost = make(map[string]map[string]any, len(in.Hosts))
	for _, h := range in.Hosts {
		in.RegisterByHost[h.SID] = mockReg
	}

	// Рендер state_changes гоняется ВСЕГДА (как прод после барьера), даже без
	// ассерта: незащищённый `${ input.X }` по optional-без-default input (CEL «no
	// such key») ловится здесь — слепая зона harness-а, видевшего только tasks.
	ops, err := pipeline.RenderStateOps(in)
	if err != nil {
		return res, fmt.Errorf("trial: render state_changes: %w", err)
	}

	// assert.state_changes — проекция set-операций (поле→значение, back-compat).
	if c.Assert.StateChanges != nil {
		res.Failures = append(res.Failures, compareStateChanges(c.Assert.StateChanges, setOpsProjection(ops))...)
	}

	// assert.state_after — детерминированный итоговый incarnation.state: базовый
	// fixtures.state + применённые по порядку операции state_changes (зеркало
	// прод-коммита, run.go: mergeStateChanges(stateBefore, ops, schema, EvalStateMatch)).
	// Сверка ПОЛНАЯ (compareState, как L1): лишний ключ — расхождение.
	if c.Assert.StateAfter != nil {
		schema, serr := loadServiceStateSchema(caseFile)
		if serr != nil {
			return res, serr
		}
		stateAfter, merr := mergeStateChanges(c.Fixtures.State, ops, schema, pipeline.EvalStateMatch, pipeline.EvalStateOpExpr)
		if merr != nil {
			return res, fmt.Errorf("trial: apply state_changes: %w", merr)
		}
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

// loadServiceStateSchema читает `<service-root>/service.yml` и возвращает его
// state_schema-map (тип коллекции для add-материализации, зеркало прод
// art.Manifest.StateSchema). Отсутствие service.yml / state_schema — не ошибка
// (nil): add в уже существующую коллекцию выводит тип из значения state, schema
// нужна лишь для материализации отсутствующего поля.
func loadServiceStateSchema(caseFile string) (map[string]any, error) {
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
	return manifest.StateSchema, nil
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

// fixtureHosts строит roster L0 прогона из fixtures.
//
// Multi-host (fixtures.hosts задан): roster из N хостов в детерминированном
// порядке по SID (soulprint.hosts-проекция render-движка идёт в порядке
// in.Hosts, не сортирует сама — детерминизм обеспечиваем здесь). Зеркало
// топологии прогона: covens/role/choirs/soulprint берутся из host-записи как
// есть; корректность incarnation.name-метки в covens — на авторе кейса
// (rosterSQL `WHERE $1 = ANY(coven)`: без неё хост выпадет из таргета).
//
// Single-host (fixtures.soulprint, multi не задан): прежнее поведение
// БИТ-В-БИТ — один синтетический хост trial-host с корневой incarnation-меткой
// (incarnationName == RenderInput.Incarnation.Name), per-host вариативность —
// слой dispatch (L3, вне пилота).
func fixtureHosts(incarnationName string, f Fixtures) []*topology.HostFacts {
	if len(f.Hosts) == 0 {
		return []*topology.HostFacts{{
			SID:       trialHostSID,
			Coven:     []string{incarnationName},
			Soulprint: orEmptyMap(f.Soulprint),
		}}
	}

	hosts := make([]*topology.HostFacts, 0, len(f.Hosts))
	for _, h := range f.Hosts {
		hosts = append(hosts, &topology.HostFacts{
			SID:       h.SID,
			Coven:     h.Covens,
			Role:      h.Role,
			Choirs:    h.Choirs,
			Soulprint: orEmptyMap(h.Soulprint),
		})
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].SID < hosts[j].SID })
	return hosts
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

// compareTaskPresence реализует assert-by-presence (PILOT новой модели L0):
// сверяет НАЛИЧИЕ/ОТСУТСТВИЕ вызова задачи в плане, не позицию.
//
// task_present: для каждой записи в плане обязана быть РОВНО ОДНА (после
// дизамбигуации) задача matching — 0 совпадений → fail «ожидалась задача,
// не найдено»; >1 совпадение без when/id-дизамбигуатора → fail-коллизия с
// подсказкой сузить ассерт. task_absent: ≥1 совпадение → fail.
//
// Матч одной задачи — taskMatches (module== ∧ params_subset⊆params ∧ опц.when==
// ∧ опц.id==register∪id). params_subset проверяется тем же compareParams, что и
// позиционная сверка (частичный по-ключно, <present>-маркер), поэтому семантика
// подмножества идентична Params в rendered_tasks.
func compareTaskPresence(present, absent []ExpectedTask, got []*render.RenderedTask) []string {
	var fails []string

	for i, et := range present {
		var matched []*render.RenderedTask
		for _, rt := range got {
			if taskMatches(et, rt) {
				matched = append(matched, rt)
			}
		}
		switch {
		case len(matched) == 0:
			fails = append(fails, fmt.Sprintf("task_present[%d]: ожидалась задача matching %s, в плане (%d задач) не найдено",
				i, describeExpected(et), len(got)))
		case len(matched) > 1:
			fails = append(fails, fmt.Sprintf("task_present[%d]: %s — найдено %d совпадений (коллизия); добавь id/register или when, либо сузь params_subset",
				i, describeExpected(et), len(matched)))
		}
	}

	for i, et := range absent {
		for _, rt := range got {
			if taskMatches(et, rt) {
				fails = append(fails, fmt.Sprintf("task_absent[%d]: %s — ожидалось отсутствие, но задача найдена в плане",
					i, describeExpected(et)))
				break
			}
		}
	}

	return fails
}

// taskMatches — предикат совпадения одной отрендеренной задачи с presence-
// ожиданием. Все заданные условия конъюнктивны; незаданные (пустые) — не
// ограничивают. params_subset матчится переиспользованием compareParams (пустой
// diff == совпадение): он частичен по-ключно и понимает <present>-маркер.
//
// Skip-placeholder (выключенная ветка: static when:false / block-skip / loop-skip,
// а также future-passage stub staged-render) НИКОГДА не матчится — ни как
// task_present, ни как task_absent. Семантика «задача не вызвана»: present по нему
// не должен зеленеть, absent по нему не должен ложно срабатывать. Признак skip —
// `rt.Params == nil`: render реальной module-задачи всегда несёт non-nil
// *structpb.Struct (renderParams возвращает Struct даже на пустых params), а оба
// skip-конструктора (staticSkipPlaceholder/loopSkipPlaceholder) и future-passage
// stub оставляют Params nil. Явного булева Skip-флага у RenderedTask нет, а
// FlowContext неинформативен — он устанавливается и у реальной задачи.
func taskMatches(et ExpectedTask, rt *render.RenderedTask) bool {
	if rt.Params == nil {
		return false
	}
	if rt.Module != et.Module {
		return false
	}
	if et.When != "" && rt.When != et.When {
		return false
	}
	// id-дизамбигуатор: register∪id (T1 — оба сразу запрещены в DSL, поэтому
	// одной строкой адресуем любую из двух).
	if et.ID != "" && rt.Register != et.ID && rt.ID != et.ID {
		return false
	}
	if len(et.ParamsSubset) > 0 {
		if diff := compareParams(rt.Index, et.ParamsSubset, rt.Params, rt.NoLog); diff != "" {
			return false
		}
	}
	return true
}

// describeExpected — человекочитаемое описание presence-ожидания для текста
// расхождения. Не печатает params_subset целиком (может нести vault-секреты) —
// только ключи, как no_log-ветка compareParams.
func describeExpected(et ExpectedTask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module=%q", et.Module)
	if et.When != "" {
		fmt.Fprintf(&b, ", when=%q", et.When)
	}
	if et.ID != "" {
		fmt.Fprintf(&b, ", id=%q", et.ID)
	}
	if len(et.ParamsSubset) > 0 {
		fmt.Fprintf(&b, ", params_subset-ключи=%v", sortedKeys(et.ParamsSubset))
	}
	return b.String()
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
