package render

import (
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// ToProtoTasks конвертирует render-план ([]*RenderedTask) в wire-форму
// ApplyRequest.tasks ([]*keeperv1.RenderedTask). Index — orchestrator-only, в
// proto не идёт; Register идёт (Soul строит registerByName для flow-control-
// предикатов, ADR-012(d)).
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
func ToProtoTasks(tasks []*RenderedTask) []*keeperv1.RenderedTask {
	out := make([]*keeperv1.RenderedTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, &keeperv1.RenderedTask{
			Name:         t.Name,
			Module:       t.Module,
			Params:       t.Params,
			NoLog:        t.NoLog,
			Timeout:      t.Timeout,
			OnchangesIdx: int32Slice(t.OnChangesIdx),
			OnfailIdx:    int32Slice(t.OnFailIdx),
			When:         t.When,
			ChangedWhen:  t.ChangedWhen,
			FailedWhen:   t.FailedWhen,
			FlowContext:  t.FlowContext,
			Register:     t.Register,
			Until:        t.Until,
			RetryCount:   int32(t.RetryCount),
			RetryDelay:   t.RetryDelay,
		})
	}
	return out
}

// int32Slice конвертирует render-индексы onchanges/onfail ([]int) в wire-форму
// ([]int32, proto). Индексы — позиции в плане прогона (всегда >= 0, малы),
// сужение int→int32 безопасно. nil → nil (безусловный запуск, поле omitempty).
func int32Slice(in []int) []int32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}
