package toll

import (
	"context"
	"log/slog"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// auditDegradedSet writes a cluster.degraded_set audit event. Single-winner —
// only the leader calls it (invariant ADR-038). Best-effort: logs the error, not
// fatal (the Leader must not stop its loop because of an audit flap).
//
// Source=keeper_internal (cluster-initiated, not an operator), archon_aid
// stays empty (the write-path fills in NULL). Payload — numeric parameters,
// no secrets.
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
	// coven_name — optional (ADR-038 amendment, extensions): written only for a
	// per-coven trigger; for a global trigger the field is absent. This lets
	// payload consumers (alert-receiver, UI) distinguish a "cluster-wide
	// drain" (no coven_name) from a "local split in coven=X" (with coven_name).
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

// auditDegradedCleared — symmetric to auditDegradedSet, but for the terminal
// "cleared" snapshot. Written only after a stable grace window of low rate
// (ADR-038 asymmetric hysteresis).
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
