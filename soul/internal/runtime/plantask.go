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

// planTask dispatches a SINGLE task in dry_run mode (Scry, ADR-031): calls
// SoulModule.Plan INSTEAD of Apply and returns a TaskEvent with a
// machine-readable drift result. mod.Apply is never physically called here —
// the read-only guarantee is structural, not "the module promised" (see Run /
// runTaskWithRetry).
//
// Default-deny (ADR-031, security): Plan is called ONLY for modules that
// declare the read-safe capability via [module.PlanReadSafe]. A module without
// it (a custom plugin on BaseModule / a core module without a pure-read Plan)
// gets an EXPLICIT refusal — FAILED with code `plan.unsupported`, never a
// false "no drift" (false changed=false): an unknown module never reports clean.
//
// Plan result → TaskEvent mapping:
//   - module didn't declare the capability → FAILED (plan.unsupported);
//   - bad address / module not found → FAILED (same as runTask: terminal);
//   - Plan returned an error → FAILED (plan.error);
//   - final PlanEvent.changed == true → CHANGED (drift present);
//   - changed == false → OK (clean).
//
// register_data carries changed/failed derived from status (buildRegisterData)
// — drift is machine-readable by later steps; Plan's output isn't aggregated
// in the MVP. retry/until/changed_when/failed_when do NOT apply to dry_run
// (see runTaskWithRetry).
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

	// Default-deny: a module without the read-safe capability is NOT queried on
	// dry_run. We return an explicit refusal (FAILED) rather than a false-clean
	// — an unknown module must not report "no drift" (ADR-031).
	if _, safe := mod.(module.PlanReadSafe); !safe {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "plan.unsupported",
			Module:  modName,
			Message: fmt.Sprintf("module %q did not declare a read-safe Plan - drift on dry_run is unsupported (task %q)", modName, task.GetName()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// Soulprint injection mirrors runTask: the core module picks its backend
	// from the host fact BEFORE the read (same source as Apply). This is a
	// read — the host isn't mutated.
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

	// No PlanEvent at all with a nil error: a read-safe module MUST send a
	// final PlanEvent with a machine-readable changed (drift is otherwise
	// undefined). A missing event is a bug in that module; we treat it as
	// FAILED (plan.no_result), not clean, so a misbehaving module never
	// reports a false "no drift" (ADR-031).
	if last == nil {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_FAILED
		ev.Error = &keeperv1.TaskError{
			Code:    "plan.no_result",
			Module:  modName,
			Message: fmt.Sprintf("module %q declared a read-safe Plan but sent no PlanEvent (task %q)", modName, task.GetName()),
		}
		ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
		return ev
	}

	// changed → CHANGED (drift), otherwise OK (clean).
	if last.GetChanged() {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	} else {
		ev.Status = keeperv1.TaskStatus_TASK_STATUS_OK
	}
	ev.RegisterData = buildRegisterData(ev.GetStatus(), nil)
	return ev
}

// inProcPlanStream is an in-process implementation of
// `grpc.ServerStreamingServer[pluginv1.PlanEvent]` for calling core modules on
// dry_run, a mirror of [inProcApplyStream]. Buffers PlanEvents; the runtime
// only reads the final one (machine-readable changed). SetTrailer/SendHeader
// are no-ops.
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
