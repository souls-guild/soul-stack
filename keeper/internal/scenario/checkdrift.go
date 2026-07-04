package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// ConvergeScenarioName — имя specialized scenario-kind, описывающего ЖЕЛАЕМОЕ
// конечное состояние сервиса (ADR-031, Slice B). Сервис, поддерживающий drift-
// детект, ОБЯЗАН положить `scenario/converge/main.yml` в репо: Scry-проверка
// прогоняет этот сценарий в `dry_run`-режиме (Soul зовёт `mod.Plan` вместо
// `mod.Apply`) и собирает per-task `changed` в [DriftReport].
//
// Обнаружение — по имени (auto-discover, симметрично остальным сценариям):
// никакого нового YAML-поля сервису добавлять не нужно. Сценарий отсутствует —
// check-drift возвращает 422 «drift-проверка недоступна для этого сервиса»
// (информационно, не error).
const ConvergeScenarioName = "converge"

// ErrConvergeMissing — сервис, под которым запущена incarnation, не несёт
// `scenario/converge/main.yml` в текущем git-снапшоте: drift-проверка для него
// не поддержана (ADR-031). Caller (REST/MCP-handler) маппит в 422
// `drift-unsupported`, incarnation не тронут.
var ErrConvergeMissing = errors.New("scenario: converge scenario is missing — drift check unsupported")

// ErrDriftInputMissing — для converge-параметра не найдено значение ни в
// `incarnation.state.<param>` (auto-from-state по конвенции имени), ни в
// operator-override; параметр обязателен и default-а нет. Caller маппит в 422.
var ErrDriftInputMissing = errors.New("scenario: converge input parameter cannot be resolved from state or override")

// DriftStatus — терминал хоста в DriftReport (per-host агрегат
// [DriftTaskResult]-ов).
type DriftStatus string

const (
	// DriftStatusClean — все task-ы хоста вернули `changed=false` (drift нет).
	DriftStatusClean DriftStatus = "clean"
	// DriftStatusDrifted — хотя бы один task хоста вернул `changed=true`.
	DriftStatusDrifted DriftStatus = "drifted"
	// DriftStatusUnsupported — хотя бы один task хоста — community-модуль без
	// `PlanReadSafe`-capability: Soul вернул `TASK_STATUS_FAILED` с кодом
	// `plan.unsupported` (default-deny, ADR-031). Хост помечен unsupported,
	// если в прогоне НЕТ ни drifted-, ни failed-task-ов (последние имеют
	// приоритет — реальная ошибка перебивает «не поддержано»).
	DriftStatusUnsupported DriftStatus = "unsupported"
	// DriftStatusFailed — хост закрылся не-success-терминалом
	// (failed/cancelled/orphaned/no_match) ИЛИ хотя бы один task вернул
	// FAILED с кодом, отличным от `plan.unsupported` (реальная ошибка). Drift
	// не определён, требует разбора.
	DriftStatusFailed DriftStatus = "failed"
)

// DriftTaskResult — drift-результат одной задачи на одном хосте. Заполняется
// после барьера из [applyrun.SelectTaskRegistersByApplyID] (register_data со
// `changed`/`failed` per task_idx) + [applyrun.SelectByApplyID]
// (error_summary первой упавшей задачи).
type DriftTaskResult struct {
	// Idx — ГЛОБАЛЬНЫЙ plan_index задачи (RenderedTask.Index, сквозной по всему
	// плану прогона и по всем Passage, ADR-056 §S1 fix Variant B). Стабилен между
	// Keeper-side render и Soul-side TaskEvent (ADR-012(d)); не зависит от per-host
	// where: (в отличие от локального TaskEvent.task_idx).
	Idx int `json:"idx"`
	// Module — `<namespace>.<module>.<state>` из RenderedTask.Module (например
	// `core.pkg.installed`).
	Module string `json:"module"`
	// Action — имя задачи из scenario (RenderedTask.Name). Может быть пустым,
	// если у задачи нет `name:`.
	Action string `json:"action,omitempty"`
	// Changed — финальный `changed` из register_data. true → drift на этом
	// task-е. Для `unsupported`/`failed`-task-а — false (Soul не дошёл до
	// чтения).
	Changed bool `json:"changed"`
	// Message — operator-facing описание (пустое для clean-task-а). Заполняется
	// только для failed/unsupported из error_summary (`task <idx> <module>:
	// <message>` уже маскировано); для drifted — пустое (machine-readable
	// changed достаточен в MVP, output модулей агрегировать сложнее).
	Message string `json:"message,omitempty"`
}

// DriftHostReport — per-host агрегат drift-результатов.
type DriftHostReport struct {
	SID    string            `json:"sid"`
	Status DriftStatus       `json:"status"`
	Tasks  []DriftTaskResult `json:"tasks"`
}

// DriftSummary — агрегаты per-host-терминалов по всему прогону. Один из
// hosts_drifted/hosts_failed > 0 → incarnation.status переводится в
// `drift`/остаётся прежним (см. CheckDrift).
type DriftSummary struct {
	HostsDrifted     int `json:"hosts_drifted"`
	HostsClean       int `json:"hosts_clean"`
	HostsUnsupported int `json:"hosts_unsupported"`
	HostsFailed      int `json:"hosts_failed"`
}

// DriftReport — финальный отчёт Scry-проверки (ADR-031, Slice B). НЕ proto
// Keeper↔Soul: то wire-форма несёт RunResult+PlanEvent.changed на уровне одной
// задачи. Этот тип — keeper-internal aggregated view + API/MCP-response.
type DriftReport struct {
	CheckedAt       time.Time         `json:"checked_at"`
	IncarnationName string            `json:"incarnation"`
	ScenarioRef     string            `json:"scenario_ref"`
	Hosts           []DriftHostReport `json:"hosts"`
	Summary         DriftSummary      `json:"summary"`
}

// CheckDriftSpec — параметры одной check-drift-проверки.
//
// InputOverride — переданные оператором значения converge-параметров (опц.):
// перекрывают auto-from-state (`incarnation.state.<param>`) по конвенции имени.
// nil → используется только state.
type CheckDriftSpec struct {
	ApplyID         string
	IncarnationName string
	ServiceRef      artifact.ServiceRef
	InputOverride   map[string]any
	StartedByAID    string
}

// CheckDrift — синхронная Scry-проверка drift-а incarnation (ADR-031, on-demand-
// пилот). По текущему git-снапшоту сервиса прогоняет `scenario/converge/main.yml`
// в dry_run-режиме (Soul зовёт `mod.Plan` вместо `mod.Apply`), собирает per-host
// per-task `changed` и возвращает [DriftReport]. incarnation.status НЕ меняется
// этим методом — это делает caller (REST/MCP-handler) по сводке отчёта.
//
// Поток (paritet [Runner.run] до dispatch-а):
//  1. SelectByName + render-конвейер: ServiceLoader → parseScenario(converge) →
//     ExpandIncludes → topology.LoadIncarnationHosts → essence.Resolve →
//     resolveDriftInput (state ∪ override merge до vault-резолва) →
//     ResolveInputValuesVault → render.Pipeline.Render.
//  2. dispatch (work-queue, ADR-027): InsertPlanned на КАЖДЫЙ roster-хост с
//     Recipe{DryRun:true} + Summons. Acolyte рендерит per-host и шлёт
//     `ApplyRequest{dry_run:true}` Soul-у (claim.go проброс).
//  3. driftBarrier: ждёт терминалов ВСЕХ planned-хостов (любых, включая
//     failed — в отличие от waitBarrier дрейф-режима терминал не возвращает
//     err). После барьера читает `apply_task_register` + `apply_runs` и строит
//     DriftReport через assembleDriftReport.
//
// scenario.converge отсутствует в снапшоте → [ErrConvergeMissing] (incarnation
// не тронут, dispatch не стартует — это «drift-проверка недоступна», не
// failure). state-incarnation НЕ меняется ни на одной ветке этого метода —
// переход в `drift` делает caller.
//
// Acolyte-режим обязателен: чистый Plan-проброс работает через persisted
// Recipe.DryRun, у inline-пути (acolytes=0) DryRun не пробрасывается в proto.
// Отказ при acolyteEnabled=false — explicit (см. CheckDrift).
func (r *Runner) CheckDrift(ctx context.Context, spec CheckDriftSpec) (*DriftReport, error) {
	log := r.logger.With(
		slog.String("apply_id", spec.ApplyID),
		slog.String("incarnation", spec.IncarnationName),
		slog.String("scenario", ConvergeScenarioName),
	)

	ctx, span := tracer.Start(ctx, "scenario.check_drift",
		trace.WithAttributes(
			attribute.String("incarnation", spec.IncarnationName),
			attribute.String("scenario", ConvergeScenarioName),
			attribute.String("apply_id", spec.ApplyID),
		),
	)
	defer span.End()

	if !r.acolyteEnabled {
		// Inline-путь (acolytes=0) не пробрасывает DryRun в ApplyRequest —
		// для check-drift это значит молчаливый «обычный apply» с реальной
		// мутацией. Fail-closed: явный отказ конфигурации, а не неявная
		// мутация. Для check-drift нужен work-queue (Acolyte).
		span.SetStatus(codes.Error, "acolyte_required")
		return nil, fmt.Errorf("scenario: check-drift требует work-queue (keeper.acolytes>0, ADR-027); inline-путь не пробрасывает dry_run")
	}

	inc, err := incarnation.SelectByName(ctx, r.deps.DB, spec.IncarnationName)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}

	// 1. Service-артефакт + парсинг converge-сценария. Отсутствие файла —
	//    «drift-проверка не поддержана» (ErrConvergeMissing), а НЕ ошибка
	//    парсинга: пробуем ReadFile до LoadScenarioManifestFromBytes.
	art, err := r.deps.Loader.Load(ctx, spec.ServiceRef)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift load service: %w", err)
	}
	relMain := fmt.Sprintf(scenarioMainFile, ConvergeScenarioName)
	data, err := r.deps.Loader.ReadFile(art, relMain)
	if err != nil {
		// ReadFile отдаёт generic-error: типизированного sentinel-а
		// (`os.ErrNotExist` после wrap-а) нет, поэтому считаем любое
		// чтение-fail отсутствием converge. Реальная IO-ошибка (битые
		// права/повреждённый снапшот) тоже свернётся в ErrConvergeMissing,
		// поэтому log.Warn — оператор должен видеть в логах сигнал о
		// возможной IO-проблеме, а не молчаливый «converge не определён».
		log.Warn("scenario: check-drift — converge не прочитан, проверка считается недоступной (возможна IO-проблема)",
			slog.String("ref", spec.ServiceRef.Ref), slog.Any("error", err))
		return nil, ErrConvergeMissing
	}
	scn, _, diags, err := artifact.LoadScenarioManifestResolved(art, relMain, data)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift parse %s: %w", relMain, err)
	}
	if diag.HasErrors(diags) {
		err := fmt.Errorf("scenario: check-drift %s невалиден: %s", relMain, firstError(diags))
		span.RecordError(err)
		return nil, err
	}

	expanded, idiags := config.ExpandIncludes(scn.Tasks, scenarioIncludeResolver(r.deps.Loader, art, ConvergeScenarioName))
	if diag.HasErrors(idiags) {
		err := fmt.Errorf("scenario: check-drift раскрытие include в %s/%s: %s", ConvergeScenarioName, scenarioMainFile, firstError(idiags))
		span.RecordError(err)
		return nil, err
	}
	scn.Tasks = expanded

	// Синтез install-шагов из modules[] (ADR-065) — симметрично run(): drift-план
	// обязан совпадать с apply-планом, иначе сам синтез-шаг был бы вечным drift-ом.
	if synthed, names := config.SynthesizeModuleInstalls(scn.Tasks, art.Manifest.Modules); len(names) > 0 {
		scn.Tasks = synthed
		log.Info("scenario: check-drift — синтезированы install-шаги модулей из manifest.modules[] (ADR-065)",
			slog.Any("modules", names))
	}

	// 2. Roster + essence (как в run.go).
	hosts, err := r.deps.Topology.LoadIncarnationHosts(ctx, spec.IncarnationName)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift topology: %w", err)
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("scenario: check-drift incarnation %q не имеет connected-хостов", spec.IncarnationName)
	}
	essenceMap, err := r.deps.Essence.Resolve(essenceInput(art.LocalDir, inc, hosts[0]))
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift essence: %w", err)
	}

	// 3. Drift-input резолв (auto-from-state + override + vault). По конвенции
	//    имени: для каждого параметра converge-схемы, отсутствующего в override,
	//    смотрим incarnation.state[<имя>]; если там тоже нет — отдаём merge
	//    дефолтов/required схеме, она поднимет ErrDriftInputMissing.
	provided := resolveDriftInput(scn.Input, spec.InputOverride, inc.State)
	resolver := r.newInputVaultResolver(ctx, inputVaultAuditCtx{
		aid:         spec.StartedByAID,
		incarnation: spec.IncarnationName,
		scenario:    ConvergeScenarioName,
	}, r.deps.InputDenyPaths)
	effectiveInput, err := config.ResolveInputValuesVault(scn.Input, provided, resolver)
	if err != nil {
		// Required-параметр не резолвится — заворачиваем в ErrDriftInputMissing,
		// чтобы caller отдал 422 c понятным сообщением (отличаем от прочих
		// input-ошибок схемы).
		if isInputRequiredErr(err) {
			return nil, fmt.Errorf("%w: %s", ErrDriftInputMissing, err.Error())
		}
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift input %s/%s: %w", spec.IncarnationName, ConvergeScenarioName, err)
	}

	// 4. Render полного roster-а (как run-goroutine).
	renderIn := render.RenderInput{
		Scenario: scn,
		Essence:  essenceMap,
		Input:    effectiveInput,
		Incarnation: render.IncarnationMeta{
			Name:           inc.Name,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
		},
		Hosts: hosts,
		// State — снимок incarnation.state для `incarnation.state.<path>` (ADR-009/010).
		// Симметрично run-goroutine: converge-scenario может читать pre-run state;
		// check-drift сравнивает desired-vs-actual тем же render-конвейером. Read-only.
		State: inc.State,
		Ctx:   ctx,
		Templates: render.NewSnapshotTemplateReader(
			func(rel string) ([]byte, error) { return artifact.ReadSnapshotFile(art.LocalDir, rel) },
			scenarioTemplatePrefix(ConvergeScenarioName),
		),
	}
	if r.deps.Destiny != nil {
		renderIn.Destiny = r.deps.Destiny.resolverFor(art.Manifest)
	}
	tasks, _, err := r.deps.Render.Render(ctx, renderIn)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift render: %w", err)
	}

	// 5. dispatch planned на КАЖДЫЙ roster-хост с DryRun=true. Recipe.Input
	//    несёт vault-ref КАК ЕСТЬ (инвариант A), Acolyte перерезолвит при claim
	//    тем же путём, что обычный run.
	startedBy := startedByPtr(spec.StartedByAID)
	recipe := &applyrun.Recipe{
		ServiceRef:   spec.ServiceRef,
		ScenarioName: ConvergeScenarioName,
		Input:        spec.InputOverride, // override как был передан; state-merge — в RenderForHost не нужно (Acolyte перечитывает state)
		StartedByAID: startedBy,
		DryRun:       true,
		FromUpgrade:  false, // converge — day-2 scenario/, никогда upgrade/ (ADR-0068)
	}
	for _, h := range hosts {
		if err := applyrun.InsertPlanned(ctx, r.deps.DB, &applyrun.ApplyRun{
			ApplyID:         spec.ApplyID,
			SID:             h.SID,
			IncarnationName: spec.IncarnationName,
			Scenario:        ConvergeScenarioName,
			StartedByAID:    startedBy,
			Recipe:          recipe,
		}); err != nil {
			span.RecordError(err)
			return nil, fmt.Errorf("scenario: check-drift insert planned (%s): %w", h.SID, err)
		}
	}
	r.publishSummons(ctx, log)

	// 6. driftBarrier: ждёт терминалов ВСЕХ planned (любой статус — failed/
	//    no_match тоже терминал). Drift-режим не сваливается на первом failed:
	//    failed-task ≠ провал прогона, это просто статус хоста в отчёте.
	if err := r.driftBarrier(ctx, spec.ApplyID, len(hosts)); err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift barrier: %w", err)
	}

	// 7. Сборка отчёта из persisted-данных: per-host status из apply_runs,
	//    per-task changed из apply_task_register, error_summary для failed.
	report, err := r.assembleDriftReport(ctx, spec, tasks)
	if err != nil {
		span.RecordError(err)
		return nil, fmt.Errorf("scenario: check-drift assemble: %w", err)
	}
	log.Info("scenario: check-drift завершён",
		slog.Int("hosts", len(report.Hosts)),
		slog.Int("hosts_drifted", report.Summary.HostsDrifted),
		slog.Int("hosts_failed", report.Summary.HostsFailed))
	return report, nil
}

// resolveDriftInput строит provided-map для converge-input по конвенции
// «auto-from-state» (PM-default): для каждого параметра converge-схемы
//
//   - если ключ есть в override — берём из override (приоритет оператора);
//   - иначе если есть в state[<имя>] — берём из state (типовой случай:
//     converge читает state, отрендеренный после предыдущего apply);
//   - иначе НЕ кладём в provided (мердж дефолта/required делает
//     ResolveInputValuesVault: default → подставит, required → отвергнет).
//
// Параметры override, отсутствующие в схеме, пробрасываются как есть (грамматику
// «unknown input key» MVP не запрещает, симметрично ResolveInputValues).
func resolveDriftInput(schema config.InputSchemaMap, override, state map[string]any) map[string]any {
	out := make(map[string]any, len(override)+len(schema))
	for k, v := range override {
		out[k] = v
	}
	for name := range schema {
		if _, ok := out[name]; ok {
			continue
		}
		if state == nil {
			continue
		}
		if v, ok := state[name]; ok {
			out[name] = v
		}
	}
	return out
}

// isInputRequiredErr — детект «required-параметр не передан» в ошибке
// [config.ResolveInputValuesVault]. Типизированного sentinel-а в shared/config
// нет (см. requireInputValues в shared/config/input_value.go); привязываемся к
// уникальной форме сообщения, которое формирует ровно requireInputValues:
// `input "<name>" обязателен, но не передан и не имеет default`. Подстрока
// `обязателен, но не передан и не имеет default` не пересекается с другими
// ошибками резолва (type/enum/pattern/min_length/max_length/object-required).
// Используется CheckDrift, чтобы отличить отсутствие значения для converge-
// параметра (422 drift-input-missing) от прочих input-ошибок.
func isInputRequiredErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "обязателен, но не передан и не имеет default")
}

// driftBarrier — drift-вариант [Runner.waitBarrier]: ждёт терминалов ВСЕХ
// planned-хостов прогона. Отличается тем, что failed/cancelled/orphaned для
// drift — НЕ провал прогона (это статусы per-host, отражённые в DriftReport),
// поэтому метод НЕ возвращает err на не-success-терминал, как делает обычный
// waitBarrier (fail-stop). Возвращает только:
//   - nil — все wantHosts достигли терминала (любого);
//   - ошибка — ctx отменён (timeout/Cancel/Shutdown) либо poll-SQL упал.
func (r *Runner) driftBarrier(ctx context.Context, applyID string, wantHosts int) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, applyID)
		if err != nil {
			return fmt.Errorf("poll: %w", err)
		}
		if isAllTerminal(statuses, wantHosts) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("прерван: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// isAllTerminal — true, если wantHosts строк прогона все в терминальных статусах
// (success/failed/cancelled/orphaned/no_match). planned/claimed/dispatched/
// running — не терминал. Использует тот же набор, что обычный barrier, без
// fail-stop: failed для drift — НЕ конец проверки.
//
// InsertPlanned кладёт ровно по одной строке на хост: `==` строгое, `>=`
// маскировало бы баги «лишних» строк (например, дубль InsertPlanned).
func isAllTerminal(statuses []applyrun.HostStatus, wantHosts int) bool {
	terminal := 0
	for _, hs := range statuses {
		switch hs.Status {
		case applyrun.StatusSuccess, applyrun.StatusNoMatch,
			applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
			terminal++
		}
	}
	return terminal == wantHosts
}

// assembleDriftReport собирает DriftReport из persisted-данных прогона после
// барьера: per-host apply_runs (status + error_summary + failed_plan_index) и
// apply_task_register (changed/failed per plan_index).
//
// Маппинг plan_index → module/action — из renderedTasks (Keeper-side render
// держит RenderedTask с Module/Name/Index). Host вне prepared-map (не должно
// случиться: insertPlanned писал на каждый roster-хост) пропускается с warn-ом
// в логе caller-а — но в текущей сборке достаём перечень из роутов apply_runs.
func (r *Runner) assembleDriftReport(ctx context.Context, spec CheckDriftSpec, tasks []*render.RenderedTask) (*DriftReport, error) {
	statuses, err := applyrun.SelectStatusesByApplyID(ctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("statuses: %w", err)
	}
	registers, err := applyrun.SelectTaskRegistersByApplyID(ctx, r.deps.DB, spec.ApplyID)
	if err != nil {
		return nil, fmt.Errorf("task registers: %w", err)
	}

	taskMeta := buildTaskMetaIndex(tasks)
	registerBySID := groupRegistersBySID(registers)
	taskFailures := buildTaskFailureMap(statuses)

	hostReports := make([]DriftHostReport, 0, len(statuses))
	for _, hs := range statuses {
		hr := buildHostReport(hs, taskMeta, registerBySID[hs.SID], taskFailures[hs.SID])
		hostReports = append(hostReports, hr)
	}
	sort.Slice(hostReports, func(i, j int) bool {
		return hostReports[i].SID < hostReports[j].SID
	})

	return &DriftReport{
		CheckedAt:       time.Now().UTC(),
		IncarnationName: spec.IncarnationName,
		ScenarioRef:     ConvergeScenarioName,
		Hosts:           hostReports,
		Summary:         summarize(hostReports),
	}, nil
}

// taskMetaIndex — plan_index → {module, action} из RenderedTask (ключ —
// RenderedTask.Index, ГЛОБАЛЬНЫЙ сквозной индекс по всему плану, ADR-056 §S1 fix
// Variant B). После expand-include порядок индексов стабильный
// (RenderedTask.Index = позиция в полном плане прогона).
type taskMetaIndex map[int]taskMeta

type taskMeta struct {
	module string
	action string
}

func buildTaskMetaIndex(tasks []*render.RenderedTask) taskMetaIndex {
	out := make(taskMetaIndex, len(tasks))
	for _, t := range tasks {
		if t == nil {
			continue
		}
		out[t.Index] = taskMeta{module: t.Module, action: t.Name}
	}
	return out
}

func groupRegistersBySID(rs []applyrun.TaskRegister) map[string][]applyrun.TaskRegister {
	out := make(map[string][]applyrun.TaskRegister)
	for _, r := range rs {
		out[r.SID] = append(out[r.SID], r)
	}
	return out
}

// hostTaskFailure — описание упавшей задачи хоста (для unsupported/failed-
// дифференциации). Источник — apply_runs.failed_plan_index + error_summary
// (записывается recordTaskFailure при FAILED-TaskEvent-е). error_summary
// форматируется как `task <idx> <module>: <message>` — несёт machine-readable
// идентификатор `plan.unsupported` в тексте сообщения.
//
// planIndex — ГЛОБАЛЬНЫЙ сквозной plan_index упавшей задачи (apply_runs.
// failed_plan_index, миграция 081, ADR-056 §S1 fix Variant B): ключ резолва
// module/action против taskMeta (построен по RenderedTask.Index). НЕ локальный
// task_idx — он под staged/per-host-where указывал бы на соседнюю задачу.
type hostTaskFailure struct {
	planIndex int
	summary   string
}

func buildTaskFailureMap(statuses []applyrun.HostStatus) map[string]hostTaskFailure {
	out := make(map[string]hostTaskFailure, len(statuses))
	for _, hs := range statuses {
		// failed_plan_index — глобальный ключ резолва module/action. Старый Soul
		// без plan_index / прогон до миграции 081 → fallback на локальный task_idx
		// (для N=1 они совпадают, поведение БИТ-В-БИТ). Без ни того, ни другого
		// (dispatch-level фейл без TaskEvent-а) — failure-строка не строится.
		if hs.ErrorSummary == nil {
			continue
		}
		idx, ok := failedPlanIndex(hs)
		if !ok {
			continue
		}
		out[hs.SID] = hostTaskFailure{planIndex: idx, summary: *hs.ErrorSummary}
	}
	return out
}

// failedPlanIndex выбирает ГЛОБАЛЬНЫЙ plan_index упавшей задачи хоста:
// failed_plan_index (миграция 081) приоритетен; при его отсутствии (старый Soul
// без эхо plan_index / строка прогона до 081) — fallback на локальный task_idx,
// который для N=1 совпадает с глобальным. (false, _) — упавшая задача не
// зафиксирована (нет ни того, ни другого).
func failedPlanIndex(hs applyrun.HostStatus) (int, bool) {
	if hs.FailedPlanIndex != nil {
		return *hs.FailedPlanIndex, true
	}
	if hs.TaskIdx != nil {
		return *hs.TaskIdx, true
	}
	return 0, false
}

// buildHostReport собирает per-host агрегат: per-task результаты + общий
// DriftStatus. Логика DriftStatus (приоритет, fail-closed):
//
//   - failed/cancelled/orphaned/no_match с TaskError != plan.unsupported → DriftStatusFailed;
//   - failed с TaskError = plan.unsupported → DriftStatusUnsupported;
//   - success, но среди register-data есть changed=true → DriftStatusDrifted;
//   - success и все register changed=false → DriftStatusClean.
//
// no_match — clean (хост нецелевой, drift на нём не определён, но и провала
// нет — симметрично обычному run).
func buildHostReport(hs applyrun.HostStatus, taskMeta taskMetaIndex, registers []applyrun.TaskRegister, failure hostTaskFailure) DriftHostReport {
	results := make([]DriftTaskResult, 0, len(registers)+1)
	hasDrifted := false
	for _, reg := range registers {
		// Корреляция по ГЛОБАЛЬНОМУ plan_index (ADR-056 §S1 fix Variant B):
		// taskMeta построен по RenderedTask.Index (глобальный сквозной индекс по
		// всему плану), а reg.TaskIdx — ЛОКАЛЬНАЯ позиция в ApplyRequest Passage
		// хоста (неуникальна между Passage И между хостами одного Passage при
		// per-host where:). Резолв и Idx — по PlanIndex (тот же глобальный
		// индекс), иначе module/action промаркированы не той задачей.
		meta := taskMeta[reg.PlanIndex]
		changed, _ := boolField(reg.RegisterData, "changed")
		if changed {
			hasDrifted = true
		}
		results = append(results, DriftTaskResult{
			Idx:     reg.PlanIndex,
			Module:  meta.module,
			Action:  meta.action,
			Changed: changed,
		})
	}

	// failure-таска (если хост в failed/cancelled/orphaned): добавляем явной
	// строкой в Tasks с Changed=false и Message из error_summary. Это даёт
	// оператору точку диагностики (`tasks[*].message`), а не голый `status:
	// failed` на уровне host.
	if hs.Status == applyrun.StatusFailed || hs.Status == applyrun.StatusCancelled || hs.Status == applyrun.StatusOrphaned {
		if failure.summary != "" {
			// Резолв module/action по ГЛОБАЛЬНОМУ plan_index (ADR-056 §S1 fix
			// Variant B): taskMeta построен по RenderedTask.Index (глобальный
			// сквозной индекс), failure.planIndex — apply_runs.failed_plan_index
			// (эхо TaskEvent.plan_index). Локальный task_idx тут дал бы module/
			// action соседней задачи под staged/per-host-where — симметрично
			// register-ветке выше (reg.PlanIndex), которую закрыла миграция 079.
			meta := taskMeta[failure.planIndex]
			results = append(results, DriftTaskResult{
				Idx:     failure.planIndex,
				Module:  meta.module,
				Action:  meta.action,
				Changed: false,
				Message: failure.summary,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Idx < results[j].Idx })

	status := classifyHostStatus(hs.Status, failure.summary, hasDrifted)
	return DriftHostReport{
		SID:    hs.SID,
		Status: status,
		Tasks:  results,
	}
}

// classifyHostStatus выводит DriftStatus из per-host apply_runs-статуса +
// сигнала «plan.unsupported» из error_summary первой упавшей задачи.
// `plan.unsupported` — стабильный код TaskError.Code, который Soul-side
// planTask кладёт в TaskError для модуля без PlanReadSafe-capability (см.
// soul/internal/runtime/plantask.go).
func classifyHostStatus(applyStatus applyrun.Status, failureSummary string, hasDrifted bool) DriftStatus {
	switch applyStatus {
	case applyrun.StatusSuccess:
		if hasDrifted {
			return DriftStatusDrifted
		}
		return DriftStatusClean
	case applyrun.StatusNoMatch:
		// FINDING-01 (б): хост нецелевой (on/where отфильтровал все задачи) →
		// drift не определён, но и провала нет. Семантически — clean (нечему
		// дрейфовать).
		return DriftStatusClean
	case applyrun.StatusFailed, applyrun.StatusCancelled, applyrun.StatusOrphaned:
		// failed: разный смысл. plan.unsupported — community-модуль без read-
		// safe-capability (Scry default-deny, не реальная ошибка); прочее —
		// настоящий FAILED (Plan-error/no_result/таймаут).
		if strings.Contains(failureSummary, "plan.unsupported") {
			return DriftStatusUnsupported
		}
		return DriftStatusFailed
	}
	// running/planned/claimed/dispatched сюда не приходят: assembleDriftReport
	// вызывается после driftBarrier (терминалы всех хостов). Любой нестандартный
	// статус — fail-closed как failed.
	return DriftStatusFailed
}

func summarize(hosts []DriftHostReport) DriftSummary {
	var s DriftSummary
	for _, h := range hosts {
		switch h.Status {
		case DriftStatusClean:
			s.HostsClean++
		case DriftStatusDrifted:
			s.HostsDrifted++
		case DriftStatusUnsupported:
			s.HostsUnsupported++
		case DriftStatusFailed:
			s.HostsFailed++
		}
	}
	return s
}

// boolField достаёт bool-значение из register_data (jsonb-`map[string]any`).
// `false` при отсутствии или не-bool-типе (fail-closed: «не подтверждено
// изменение» = clean).
func boolField(m map[string]any, key string) (bool, bool) {
	if m == nil {
		return false, false
	}
	v, ok := m[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	if !ok {
		return false, false
	}
	return b, true
}

// MarkDriftStatus переводит incarnation в `drift` (или сбрасывает обратно в
// `ready`, если drift не обнаружен — сохранение «информационной» семантики).
// Вызывается caller-ом check-drift handler-а после сборки DriftReport.
//
// Безопасный update: переход разрешён только из ready или drift (status-machine
// FOR UPDATE-guard внутри). Если incarnation за время prepare ушла в
// applying/error_locked/destroying — UPDATE-tx no-op (ErrAlreadyFinalized), не
// затираем чужой переход. ready→ready / drift→drift — no-op.
//
// Audit-event пишет caller (REST/MCP), не этот метод: payload собирается на
// уровне handler-а из summary + archon.
func (r *Runner) MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error {
	target := incarnation.StatusReady
	if hasDrift {
		target = incarnation.StatusDrift
	}
	return updateDriftStatus(ctx, r.deps.DB, name, target)
}

// updateDriftStatus — WHERE-guarded UPDATE: переход status разрешён только из
// ready и drift (информационный, не блокирующий). applying/error_locked/
// migration_failed/destroying/destroy_failed → no-op (no rows affected,
// ErrAlreadyFinalized не возвращаем — это безопасный no-op).
//
// 5-секундный detached-ctx — на случай отмены caller-ctx (HTTP-client
// disconnect): drift-маркировка должна закоммититься даже при разрыве
// соединения, она информационная и не блокирует никого.
func updateDriftStatus(ctx context.Context, db applyrun.ExecQueryRower, name string, target incarnation.Status) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	const sql = `UPDATE incarnation
SET status = $2, updated_at = NOW()
WHERE name = $1 AND status IN ('ready', 'drift')`
	if _, err := db.Exec(wctx, sql, name, string(target)); err != nil {
		return fmt.Errorf("incarnation: drift-mark %s: %w", target, err)
	}
	return nil
}
