package grpc

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleErrandResult — handler for the [keeperv1.ErrandResult] payload
// (ADR-033, slice E2). Symmetric to handleRunResult, but WITHOUT
// state_changes / apply_runs / an incarnation commit: an Errand does NOT
// mutate incarnation.state (ADR-033 §4).
//
// Responsibilities:
//
//  1. Normalize stdout/stderr/output via secret masking + a 64 KiB cap
//     (defense-in-depth: the Soul-side errand runner does the same, but the
//     keeper is the write path for logs/audit).
//  2. Publish a ResultEvent to applybus with kind=KindErrand* (terminal
//     mapping by status) → Dispatcher.waitForResult / SSE subscribers
//     (if we wire up a UI stream later) receive the event.
//
// The `errand.completed`/`errand.failed`/`errand.timed_out` audit events are
// written by the Dispatcher from waitForResult — it owns the initiator
// (StartedByAID) and the correlation. Here, in the gRPC handler, there's no
// initiator information (the proto only carries errand_id), so writing audit
// here would be premature — it'd produce an event without archon_aid.
//
// ApplyBus=nil (dev build without applybus) → drop (diagnostics in the logs).
// Same as handleRunResult — the handler doesn't fail the stream over a
// best-effort pub/sub channel.
func (h *eventStreamHandler) handleErrandResult(ctx context.Context, sid, sessionID string, ev *keeperv1.ErrandResult) {
	_ = ctx // ctx is only used for audit, which isn't written here.
	if ev == nil {
		h.logger.Warn("eventstream: ErrandResult payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	errandID := ev.GetErrandId()
	if errandID == "" {
		h.logger.Warn("eventstream: ErrandResult without errand_id",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.ApplyBus == nil {
		h.logger.Debug("eventstream: ErrandResult without ApplyBus - drop",
			slog.String("sid", sid),
			slog.String("errand_id", errandID))
		return
	}

	status := errand.StatusFromProto(ev.GetStatus())
	kind := errandKindFromStatus(status)

	// Secret masking + cap. The Soul-side errand runner does the same, but
	// the keeper is the write path for logs/audit/SSE: we duplicate it as a
	// safeguard against an accidental proto bypass or an older pre-cap client build.
	stdoutMasked, stdoutTrunc := errand.MaskAndCapBytes(ev.GetStdout())
	stderrMasked, stderrTrunc := errand.MaskAndCapBytes(ev.GetStderr())
	// Echo: if the Soul already truncated, we set the flag from both sides.
	if ev.GetStdoutTruncated() {
		stdoutTrunc = true
	}
	if ev.GetStderrTruncated() {
		stderrTrunc = true
	}

	var output map[string]any
	if out := ev.GetOutput(); out != nil {
		output = errand.MaskOutputMap(out.AsMap())
	}

	exitCode := ev.GetExitCode()
	duration := ev.GetDurationMs()

	payload := errand.ResultEvent{
		ErrandID:        errandID,
		Status:          status,
		ExitCode:        &exitCode,
		Stdout:          stdoutMasked,
		Stderr:          stderrMasked,
		StdoutTruncated: stdoutTrunc,
		StderrTruncated: stderrTrunc,
		DurationMs:      &duration,
		ErrorMessage:    ev.GetErrorMessage(),
		Output:          output,
	}

	h.deps.ApplyBus.Publish(applybus.Event{
		ApplyID: errandID,
		Kind:    kind,
		Payload: payload,
	})
}

// errandKindFromStatus — the terminal EventKind for the applybus publish.
// Non-terminal statuses (RUNNING) should never reach here (the Soul only
// sends ErrandResult at the end); defensive default is failed.
func errandKindFromStatus(s errand.Status) applybus.EventKind {
	switch s {
	case errand.StatusSuccess:
		return applybus.KindErrandCompleted
	case errand.StatusTimedOut:
		return applybus.KindErrandTimedOut
	case errand.StatusCancelled:
		return applybus.KindErrandCancelled
	case errand.StatusModuleNotAllowed:
		return applybus.KindErrandModuleNotAllowed
	default:
		return applybus.KindErrandFailed
	}
}
