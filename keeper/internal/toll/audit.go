package toll

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// auditDegradedSet пишет cluster.degraded_set audit-event. Single-winner —
// только leader зовёт (инвариант ADR-038). Best-effort: ошибка лога, не fatal
// (Leader не должен останавливать loop из-за audit-flap-а).
//
// Source=keeper_internal (cluster-инициированный, не оператор), archon_aid
// остаётся пустой (write-path добавит NULL). Payload — численные параметры,
// секретов нет.
func auditDegradedSet(
	ctx context.Context,
	writer audit.Writer,
	logger *slog.Logger,
	leaderKID string,
	rate float64,
	baseline int64,
	threshold float64,
	windowSeconds int,
	covenName string,
) {
	if writer == nil {
		return
	}
	payload := map[string]any{
		"leader_kid":         leaderKID,
		"rate":               rate,
		"baseline_connected": baseline,
		"threshold":          threshold,
		"window_seconds":     windowSeconds,
	}
	// coven_name — опц. (ADR-038 amendment, extensions): пишется только при
	// per-coven trigger-е; при global-trigger-е поле отсутствует. Так
	// потребители payload-а (alert-receiver, UI) различают «cluster-wide
	// отток» (без coven_name) от «локального split в coven=X» (с coven_name).
	if covenName != "" {
		payload["coven_name"] = covenName
	}
	ev := &audit.Event{
		EventType: audit.EventClusterDegradedSet,
		Source:    audit.SourceKeeperInternal,
		Payload:   payload,
	}
	if err := writer.Write(ctx, ev); err != nil {
		logger.Warn("toll: audit degraded_set write failed",
			slog.Any("error", err))
	}
}

// auditDegradedCleared — симметрично auditDegradedSet, но для terminal-snap-а
// «снят». Пишется только после устойчивого grace-окна низкого rate-а (ADR-038
// asymmetric hysteresis).
func auditDegradedCleared(
	ctx context.Context,
	writer audit.Writer,
	logger *slog.Logger,
	leaderKID string,
	rate float64,
	baseline int64,
	graceSeconds int,
) {
	if writer == nil {
		return
	}
	ev := &audit.Event{
		EventType: audit.EventClusterDegradedCleared,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"leader_kid":         leaderKID,
			"rate":               rate,
			"baseline_connected": baseline,
			"grace_seconds":      graceSeconds,
		},
	}
	if err := writer.Write(ctx, ev); err != nil {
		logger.Warn("toll: audit degraded_cleared write failed",
			slog.Any("error", err))
	}
}
