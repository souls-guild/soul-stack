package render

import (
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ToProtoTasks конвертирует render-план ([]*RenderedTask) в wire-форму
// ApplyRequest.tasks ([]*keeperv1.RenderedTask). Index (глобальный сквозной
// индекс задачи по всему плану прогона, включая все Passage) едет в proto-поле
// plan_index — это стабильный ключ register-корреляции на Keeper-е (Soul эхает
// его в TaskEvent.plan_index, ADR-056 §S1 fix Variant B). Позиция же задачи в
// ApplyRequest.tasks[] (= TaskEvent.task_idx) ЛОКАЛЬНА для passage/host — на неё
// register-резолв опираться НЕ может. Register идёт (Soul строит registerByName
// для flow-control-предикатов, ADR-012(d)).
//
// When/ChangedWhen/FailedWhen — flow-control CEL-строки, протягиваются как есть
// (Soul вычисляет, ADR-012(d): when — этот slice; changed_when/failed_when —
// следующий). FlowContext — per-host снапшот не-register CEL-контекста.
// Until/RetryCount/RetryDelay — DSL-ядро retry: (destiny/tasks.md §9),
// retry-петля энфорсится Soul-side (applyrunner.runTaskWithRetry); until — CEL-
// строка как есть.
//
// Единственный конвертер render→proto: и scenario-orchestrator (dispatch), и
// trial-L2 (l2_run) зовут его, чтобы при добавлении поля в RenderedTask
// wire-форма не разъехалась двумя копиями.
//
// ★ onchanges/onfail-индексы РЕМАПЯТСЯ global→local (ADR-056 amend). На Keeper-е
// resolveOnChanges/resolveOnFail резолвят register-имена requisite-ов в ГЛОБАЛЬНЫЙ
// RenderedTask.Index (сквозной по всему плану прогона). Но Soul ключует
// registerByIdx ЛОКАЛЬНОЙ позицией задачи в ApplyRequest.tasks[] (applyrunner.go),
// а этот срез — per-host (groupByHost: только задачи, прошедшие where: на хосте)
// и/или per-Passage подмножество плана. Поэтому глобальный Index источника НЕ
// совпадает с его локальной позицией в срезе, и лукап registerByIdx[onchanges_idx]
// промахивался бы → onchanges-задача молча SKIPPED, onfail-rescue молча НЕ
// запускался бы. remapRequisites переводит каждый global-Index в локальную позицию
// по ВХОДНОМУ срезу `tasks` (позиция = локальный индекс, ровно как Soul). N=1 без
// where (Index==localPos для всех) → remap=identity, поведение БИТ-В-БИТ.
//
// Тонкая обёртка над [ToProtoTasksForHost] с пустым sid (golden-path: params едут
// как есть, render_context первого по SID хоста). Вызывается там, где per-host
// материализация render_context не нужна или невозможна (trial L2 single-host,
// push fan-out одним протосрезом, тесты конвертера).
func ToProtoTasks(tasks []*RenderedTask) []*keeperv1.RenderedTask {
	return ToProtoTasksForHost(tasks, "")
}

// ToProtoTasksForHost — основной конвертер render→proto для конкретного хоста.
// Идентичен golden-path, но для self-вариативной core.file.rendered подставляет
// per-host render_context (RenderedTask.RenderContextBySID[sid]) поверх Params:
// иначе все хосты получили бы render_context первого по SID, и self-вариативный
// шаблон (`{{ .self.network.primary_ip }}`) рендерился бы фактами первого хоста
// (CORE-баг, частичное закрытие open Q №25). sid=="" ИЛИ отсутствие SID-ключа ИЛИ
// host-инвариантный render_context (карта nil — заполняется лишь при multi-host) →
// overlay не делается, Params едут как есть (бит-в-бит со старым поведением).
//
// Overlay безопасен по данным: t.Params НЕ мутируется — собирается новый
// *structpb.Struct с теми же Fields, заменяется ровно ключ render_context
// (см. paramsForHost). Один и тот же *RenderedTask остаётся переиспользуем для
// других SID (groupByHost кладёт один указатель в perHost каждого хоста).
func ToProtoTasksForHost(tasks []*RenderedTask, sid string) []*keeperv1.RenderedTask {
	globalToLocal := localIndexMap(tasks)
	out := make([]*keeperv1.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, &keeperv1.RenderedTask{
			Name:         t.Name,
			Module:       t.Module,
			Params:       paramsForHost(t, sid),
			NoLog:        t.NoLog,
			Timeout:      t.Timeout,
			OnchangesIdx: remapRequisites(t.OnChangesIdx, globalToLocal),
			OnfailIdx:    remapRequisites(t.OnFailIdx, globalToLocal),
			// AggregateOf несёт ГЛОБАЛЬНЫЕ Index дочерних destiny-задач applier-а
			// (материализация applier-register, Вариант B) — ремапим global→local
			// той же remapRequisites, что onchanges/onfail: Soul агрегирует по
			// ЛОКАЛЬНОЙ позиции в registerByIdx (applyrunner). Дочерняя задача,
			// отфильтрованная where: на этом хосте / уехавшая в другой Passage,
			// кодируется sentinel-ом (-1) → её вклад в OR нулевой (changed=false).
			AggregateOf: remapRequisites(t.AggregateOf, globalToLocal),
			When:        t.When,
			ChangedWhen: t.ChangedWhen,
			FailedWhen:  t.FailedWhen,
			FlowContext: t.FlowContext,
			Register:    t.Register,
			Until:       t.Until,
			RetryCount:  int32(t.RetryCount),
			RetryDelay:  t.RetryDelay,
			// PlanIndex — глобальный сквозной индекс задачи (по всем Passage),
			// ключ register-корреляции на Keeper-е (ADR-056 §S1 fix Variant B). Soul
			// эхает его в TaskEvent.plan_index. Индексы малы и неотрицательны → int32-
			// сужение безопасно.
			PlanIndex: int32(t.Index),
		})
	}
	return out
}

// paramsForHost отдаёт wire-params задачи для конкретного sid. Golden-path
// (sid=="" / нет per-host render_context для этого SID) → t.Params как есть.
// Иначе — НОВЫЙ *structpb.Struct с теми же Fields, где ключ render_context заменён
// на per-host вариант t.RenderContextBySID[sid] (overlay одного ключа). t.Params
// не мутируется: один и тот же *RenderedTask диспатчится нескольким SID, общая
// Params обязана остаться неизменной (значения прочих Fields шарятся read-only —
// для wire-маршалинга достаточно).
func paramsForHost(t *RenderedTask, sid string) *structpb.Struct {
	if sid == "" || t.RenderContextBySID == nil {
		return t.Params
	}
	rc, ok := t.RenderContextBySID[sid]
	if !ok || rc == nil {
		return t.Params
	}
	fields := make(map[string]*structpb.Value, len(t.Params.GetFields()))
	for k, v := range t.Params.GetFields() {
		fields[k] = v
	}
	fields[paramRenderContext] = structpb.NewStructValue(rc)
	return &structpb.Struct{Fields: fields}
}

// localIndexMap строит карту глобальный RenderedTask.Index → локальная позиция в
// срезе (0-based, порядок = порядок tasks). Срез — то, что реально едет в
// ApplyRequest.tasks[] на конкретный хост/Passage, поэтому позиция в нём — ровно
// тот ключ, которым Soul индексирует registerByIdx (applyrunner.go). Карта нужна
// для remap onchanges/onfail-индексов из global в local.
func localIndexMap(tasks []*RenderedTask) map[int]int32 {
	m := make(map[int]int32, len(tasks))
	for pos, t := range tasks {
		m[t.Index] = int32(pos)
	}
	return m
}

// outOfRangeRequisite — sentinel-индекс onchanges/onfail-источника, ОТСУТСТВУЮЩЕГО
// во входном срезе ToProtoTasks (источник в другом Passage ЛИБО отфильтрован where:
// на этом хосте). Soul трактует registerByIdx[neg]=nil → changed/failed=false
// (applyrunner.go skipOnChanges/skipOnFail: отсутствующий источник «не спасает» от
// skip / «не активирует» onfail). Кодируем отсутствие явным sentinel-ом, а НЕ
// выкидыванием элемента: выкидывание сместило бы остальные индексы и сломало бы
// AND-семантику нескольких источников (хотя бы один changed → выполнить).
const outOfRangeRequisite int32 = -1

// remapRequisites переводит onchanges/onfail-индексы из глобального
// RenderedTask.Index в локальную позицию по карте globalToLocal (см. localIndexMap).
// Источник, отсутствующий в карте (cross-passage / отфильтрован where:), кодируется
// outOfRangeRequisite — Soul трактует его как changed/failed=false (см. константу).
// nil/пусто → nil (безусловный запуск / не-onfail-задача, поле omitempty).
func remapRequisites(globalIdx []int, globalToLocal map[int]int32) []int32 {
	if len(globalIdx) == 0 {
		return nil
	}
	out := make([]int32, len(globalIdx))
	for i, g := range globalIdx {
		if local, ok := globalToLocal[g]; ok {
			out[i] = local
		} else {
			out[i] = outOfRangeRequisite
		}
	}
	return out
}
