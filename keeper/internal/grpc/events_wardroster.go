package grpc

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleWardRoster — handler for the [keeperv1.WardRoster] payload (Soul-reconcile,
// ADR-027(g), S6). On (re)connect the Soul declares the apply_ids it's tracking (ReplaceAll);
// the Keeper uses the set to terminate this SID's orphaned `dispatched` rows.
//
// Closes the dispatched-orphan hole “Keeper and Soul both die after dispatch”: the row
// would otherwise get stuck in `dispatched` forever (reclaim is scoped to `claimed`; the Reaper
// deliberately doesn't do a dispatched-timeout). The sweep runs ONLY when a
// WardRoster arrives — an old Soul without this message never sends it, so its
// dispatched rows never get swept (a fail-safe hang, forward-compat).
//
// ApplyRunDB=nil (unit build without PG / ad-hoc push without a scenario-runner) → no-op.
// The authority is the shared PG: a reconnect to any cluster instance checks against the same
// table. Single-winner and the sweep ↔ RunResult race are resolved by the
// `status='dispatched'` filter inside [applyrun.OrphanDispatched].
func (h *eventStreamHandler) handleWardRoster(ctx context.Context, sid, sessionID string, ev *keeperv1.WardRoster) {
	if ev == nil {
		h.logger.Warn("eventstream: WardRoster payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.ApplyRunDB == nil {
		return
	}

	known := wardRosterToActive(ev)
	orphaned, err := applyrun.OrphanDispatched(ctx, h.deps.ApplyRunDB, sid, known)
	if err != nil {
		h.logger.Warn("eventstream: orphan dispatched sweep failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Int("known", len(known)),
			slog.Any("error", err))
		return
	}
	if orphaned > 0 {
		h.deps.Metrics.ObserveApplyOrphaned(orphaned)
		h.logger.Info("eventstream: dispatched-строки осиротены по WardRoster (Soul-reconcile)",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Int("known", len(known)),
			slog.Int64("orphaned", orphaned))
		return
	}
	h.logger.Debug("eventstream: WardRoster обработан, осиротевших dispatched нет",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Int("known", len(known)))
}

// wardRosterToActive maps the proto set [keeperv1.WardRoster] into domain
// [applyrun.ActiveApply], isolating the applyrun CRUD layer from proto generation.
// An empty/nil set → nil (an explicit declaration of “nothing is being tracked”).
func wardRosterToActive(ev *keeperv1.WardRoster) []*applyrun.ActiveApply {
	src := ev.GetActive()
	if len(src) == 0 {
		return nil
	}
	out := make([]*applyrun.ActiveApply, 0, len(src))
	for _, a := range src {
		if a == nil {
			continue
		}
		out = append(out, &applyrun.ActiveApply{
			ApplyID: a.GetApplyId(),
			Attempt: a.GetAttempt(),
		})
	}
	return out
}
