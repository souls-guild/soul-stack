package grpc

import (
	"context"
	"log/slog"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleTaskEvent — handler for the [keeperv1.TaskEvent] payload (M2.4).
//
// Everything a task event leaves behind — the `task.executed` audit event, the
// failure reason on `apply_runs`, the run's notices, the `register:`
// accumulator and the operator SSE frame — is [applysink]'s, and this handler
// is the stream half of its two callers. The push branch of the scenario
// dispatcher is the other (NIM-880): a Soul emits the same protobuf either way,
// so the trace must not depend on how the Keeper received it. Everything the
// two do differently is in their terminal handling, not here — see
// [handleRunResult].
func (h *eventStreamHandler) handleTaskEvent(ctx context.Context, sid, sessionID string, ev *keeperv1.TaskEvent) {
	if ev == nil {
		h.logger.Warn("eventstream: TaskEvent payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	h.events.TaskEvent(ctx, sid, ev)
}
