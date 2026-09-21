package topology

import (
	"context"
	"log/slog"
	"time"
)

// warnStale logs warning for each host whose soulprint is stale
// (`received_at < now - 10m`, ADR-018). Does not block — scenario operates on
// last-reported (PM-decision). Separate OTel metric — post-MVP slice
// of obs extension (symmetric to keeper/internal/grpc/events_soulprint.go).
//
// now is passed as parameter for determinism in tests.
func warnStale(ctx context.Context, logger *slog.Logger, hosts []*HostFacts, now time.Time) {
	if logger == nil {
		return
	}
	for _, h := range hosts {
		if h.stale(now) {
			logger.LogAttrs(ctx, slog.LevelWarn,
				"topology: soulprint is stale (last-reported older than threshold)",
				append(h.logAttrs(), slog.Duration("threshold", stalenessThreshold))...,
			)
		}
	}
}
