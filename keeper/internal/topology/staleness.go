package topology

import (
	"context"
	"log/slog"
	"time"
)

// warnStale логирует warn для каждого хоста, чей soulprint устарел
// (`received_at < now - 10m`, ADR-018). Не блокирует — scenario работает на
// last-reported (PM-decision). Отдельная OTel-метрика — пост-MVP slice
// obs-расширения (симметрично keeper/internal/grpc/events_soulprint.go).
//
// now передаётся параметром для детерминизма в тестах.
func warnStale(ctx context.Context, logger *slog.Logger, hosts []*HostFacts, now time.Time) {
	if logger == nil {
		return
	}
	for _, h := range hosts {
		if h.stale(now) {
			logger.LogAttrs(ctx, slog.LevelWarn,
				"topology: soulprint устарел (last-reported старше порога)",
				append(h.logAttrs(), slog.Duration("threshold", stalenessThreshold))...,
			)
		}
	}
}
