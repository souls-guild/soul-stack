package applysink

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// RunResult records a host's final report on the two OBSERVABLE channels: the
// `run.completed` audit event and the operator SSE frame.
//
// It deliberately does not move the `apply_runs` row. The two transports reach
// their terminal differently — the stream has to check the report's `attempt`
// against a re-claim epoch (ADR-027(g)) and resolve the incarnation, push has
// neither a claim nor a second writer — so each caller owns that transition and
// calls this for the half they share.
//
// A nil report is ignored; its caller has already logged the protocol violation.
func (s *Sink) RunResult(ctx context.Context, sid string, ev *keeperv1.RunResult) {
	if ev == nil {
		return
	}
	s.writeRunCompletedAudit(ctx, sid, ev)
	s.publishRunResult(sid, ev)
}

// writeRunCompletedAudit writes the `run.completed` audit event.
func (s *Sink) writeRunCompletedAudit(ctx context.Context, sid string, ev *keeperv1.RunResult) {
	if s.deps.Audit == nil {
		return
	}
	payload := map[string]any{
		"sid":      sid,
		"apply_id": ev.GetApplyId(),
		"status":   ev.GetStatus().String(),
		// passage (ADR-056 staged-render): the index of the Passage whose terminal
		// carries this report. 0 = the only Passage.
		"passage": ev.GetPassage(),
	}
	if sc := ev.GetStateChanges(); sc != nil {
		if b, err := protojson.Marshal(sc); err != nil {
			s.deps.Logger.Warn("applysink: state_changes marshal failed",
				slog.String("sid", sid),
				slog.String("apply_id", ev.GetApplyId()),
				slog.Any("error", err))
		} else {
			payload["state_changes"] = string(b)
		}
	}

	if err := s.deps.Audit.Write(ctx, &audit.Event{
		EventType:     audit.EventRunCompleted,
		Source:        s.deps.Source,
		CorrelationID: ev.GetApplyId(),
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		s.deps.Logger.Warn("applysink: audit write run.completed failed",
			slog.String("sid", sid),
			slog.String("apply_id", ev.GetApplyId()),
			slog.Any("error", err))
	}
}

// publishRunResult translates RunResult into the SSE channel via applybus,
// classifying the run status:
//
//   - RUN_STATUS_SUCCESS          → apply.completed
//   - RUN_STATUS_CANCELLED        → apply.cancelled
//   - RUN_STATUS_FAILED/ERROR_LOCKED/other → apply.failed
//
// Bus=nil (dev without SSE) → no-op.
func (s *Sink) publishRunResult(sid string, ev *keeperv1.RunResult) {
	if s.deps.Bus == nil {
		return
	}
	var kind applybus.EventKind
	switch ev.GetStatus() {
	case keeperv1.RunStatus_RUN_STATUS_SUCCESS:
		kind = applybus.KindApplyCompleted
	case keeperv1.RunStatus_RUN_STATUS_CANCELLED:
		kind = applybus.KindApplyCancelled
	default:
		kind = applybus.KindApplyFailed
	}

	payload := map[string]any{
		"apply_id":   ev.GetApplyId(),
		"kind":       string(kind),
		"sid":        sid,
		"run_status": ev.GetStatus().String(),
	}
	if sc := ev.GetStateChanges(); sc != nil {
		if b, err := protojson.Marshal(sc); err == nil {
			var asMap map[string]any
			if jerr := json.Unmarshal(b, &asMap); jerr == nil {
				payload["state_changes"] = asMap
			} else {
				payload["state_changes"] = string(b)
			}
		}
	}

	s.deps.Bus.Publish(applybus.Event{
		ApplyID: ev.GetApplyId(),
		Kind:    kind,
		Payload: payload,
	})
}
