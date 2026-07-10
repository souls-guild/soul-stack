package grpc

import (
	"context"
	"log/slog"
	"time"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// handleHostUtilization — обработчик payload-а [keeperv1.HostUtilization]
// (NIM-86). Кладёт снимок host-vitals в Redis (latest Hash + окно точек);
// Postgres не трогает — телеметрия волатильна и живёт под TTL.
//
// SID — аутентифицированный peer (параметр dispatch из mTLS), НИКОГДА не из
// payload (у HostUtilization поля sid нет — инвариант структурный).
//
// Ошибки Redis-уровня логируются warn-ом и не рвут стрим: Soul шлёт
// утилизацию периодически, следующий такт перезапишет снимок.
func (h *eventStreamHandler) handleHostUtilization(ctx context.Context, sid, sessionID string, ev *keeperv1.HostUtilization) {
	if ev == nil {
		h.logger.Debug("eventstream: HostUtilization payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if h.deps.Redis == nil {
		// dev / unit-сборка без Redis — vitals некуда писать, тихо выходим.
		return
	}
	if err := keeperredis.WriteUtilization(ctx, h.deps.Redis, sid, ev, time.Now()); err != nil {
		h.logger.Warn("eventstream: WriteUtilization failed",
			slog.String("sid", sid), slog.Any("error", err))
	}
}
