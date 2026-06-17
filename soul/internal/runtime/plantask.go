package runtime

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"
)

// planTask — диспетчер ОДНОЙ задачи в режиме dry_run (Scry, ADR-031): вызывает
// SoulModule.Plan ВМЕСТО Apply и возвращает TaskEvent с машинным результатом
// drift. mod.Apply здесь физически не вызывается — read-only гарантия
// структурная, а не «модуль обещал» (см. Run / runTaskWithRetry).
//
// Default-deny (ADR-031, безопасность): Plan вызывается ТОЛЬКО для модулей,
// объявивших read-safe-capability через [module.PlanReadSafe]. Модуль без неё
// (custom-плагин на BaseModule / core-модуль без pure-read Plan) получает ЯВНЫЙ
// отказ — FAILED с кодом `plan.unsupported`, а НЕ ложное «нет дрифта» (false
// changed=false): неизвестный модуль никогда не выдаёт clean.
//
// Маппинг результата Plan → TaskEvent:
//   - модуль не объявил capability → FAILED (plan.unsupported);
//   - bad address / module not found → FAILED (как в runTask: terminal);
//   - Plan вернул error → FAILED (plan.error);
//   - финальный PlanEvent.changed == true → CHANGED (drift есть);
//   - changed == false → OK (clean).
//
// register_data несёт changed/failed по статусу (buildRegisterData) — drift
// машиночитаем последующими шагами; output Plan в MVP не агрегируется. retry/
// until/changed_when/failed_when к dry_run НЕ применяются (см. runTaskWithRetry).
func (r *ApplyRunner) planTask(ctx context.Context, applyID string, idx int32, task *keeperv1.RenderedTask) *keeperv1.TaskEvent {
	ev := &keeperv1.TaskEvent{ApplyId: applyID, TaskIdx: idx}
	if task == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{Code: "internal.nil_task", Message: "RenderedTask is nil"}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	modName, state, ok := config.SplitModuleAddr(task.GetModule())
	if !ok {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.bad_address",
			Module:  task.GetModule(),
			Message: fmt.Sprintf("expected <namespace>.<module>.<state>, got %q", task.GetModule()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	mod, found := r.registry.Lookup(modName)
	if !found {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "module.not_found",
			Module:  modName,
			Message: fmt.Sprintf("module %q is not registered (task %q)", modName, task.GetName()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// Default-deny: модуль без read-safe-capability на dry_run НЕ опрашивается.
	// Возвращаем явный отказ (FAILED), а не false-clean — неизвестный модуль не
	// должен выдавать «нет дрифта» (ADR-031).
	if _, safe := mod.(module.PlanReadSafe); !safe {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "plan.unsupported",
			Module:  modName,
			Message: fmt.Sprintf("module %q не объявил read-safe Plan — drift на dry_run не поддержан (task %q)", modName, task.GetName()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// Soulprint-инжект симметрично runTask: core-модуль выбирает backend по факту
	// хоста ПЕРЕД read (тот же источник, что в Apply). Это чтение, хост не мутируется.
	if aware, ok := mod.(util.SoulprintAware); ok {
		aware.SetHostFacts(r.hostFacts)
	}

	stream := newInProcPlanStream(ctx)
	planErr := mod.Plan(&pluginv1.PlanRequest{State: state, Params: task.GetParams()}, stream)
	last := stream.lastEvent()

	if planErr != nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{Code: "plan.error", Module: modName, Message: planErr.Error()}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// Ни одного PlanEvent при nil-error: read-safe-модуль ОБЯЗАН слать финальный
	// PlanEvent с машинным changed (drift не определён иначе). Отсутствие события
	// — баг такого модуля; трактуем как FAILED (plan.no_result), а НЕ как clean,
	// чтобы misbehaving-модуль никогда не выдал ложное «нет дрифта» (ADR-031).
	if last == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "plan.no_result",
			Module:  modName,
			Message: fmt.Sprintf("module %q объявил read-safe Plan, но не прислал PlanEvent (task %q)", modName, task.GetName()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// changed → CHANGED (drift), иначе OK (clean).
	if last.GetChanged() {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	} else {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_OK
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// inProcPlanStream — in-process реализация
// `grpc.ServerStreamingServer[pluginv1.PlanEvent]` для вызова core-модулей на
// dry_run, зеркало [inProcApplyStream]. Буферизует PlanEvent-ы; runtime читает
// только финальный (машинный changed). SetTrailer/SendHeader — no-op.
type inProcPlanStream struct {
	grpc.ServerStream
	ctx     context.Context
	events  []*pluginv1.PlanEvent
	hdr     metadata.MD
	trailer metadata.MD
}

func newInProcPlanStream(ctx context.Context) *inProcPlanStream {
	return &inProcPlanStream{ctx: ctx, hdr: metadata.MD{}, trailer: metadata.MD{}}
}

func (s *inProcPlanStream) Context() context.Context { return s.ctx }

func (s *inProcPlanStream) Send(ev *pluginv1.PlanEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *inProcPlanStream) SetHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcPlanStream) SendHeader(md metadata.MD) error {
	s.hdr = metadata.Join(s.hdr, md)
	return nil
}
func (s *inProcPlanStream) SetTrailer(md metadata.MD) { s.trailer = metadata.Join(s.trailer, md) }
func (s *inProcPlanStream) SendMsg(m any) error {
	ev, ok := m.(*pluginv1.PlanEvent)
	if !ok {
		return fmt.Errorf("inproc plan stream: SendMsg got %T, want *pluginv1.PlanEvent", m)
	}
	return s.Send(ev)
}
func (s *inProcPlanStream) RecvMsg(any) error {
	return fmt.Errorf("inproc plan stream: RecvMsg not supported")
}

func (s *inProcPlanStream) lastEvent() *pluginv1.PlanEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}
