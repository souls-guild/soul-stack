package conductor

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
)

// PollCorridor — снимок коридора адаптивного опроса (ADR-048 «Adaptive interval»,
// профиль «Спокойный» 30s/60s/120s). Перечитывается на каждом resolve из свежего
// config-снимка → hot-reload смены floor/ceiling/idle.
type PollCorridor struct {
	Floor   time.Duration
	Ceiling time.Duration
	Idle    time.Duration
}

// MinPeriodFetcher читает агрегаты enabled-реестра Cadence из PG (см.
// [cadence.SelectMinPeriod]). Вынесен в интерфейс ради unit-тестирования
// [AdaptivePollInterval] без живого пула.
type MinPeriodFetcher interface {
	SelectMinPeriod(ctx context.Context) (cadence.MinPeriod, error)
}

// AdaptivePollInterval вычисляет шаг опроса Conductor (ADR-048 «Adaptive
// interval»): clamp(derivedMinPeriod, floor, ceiling); пустой enabled-реестр →
// idle. Stateless by construction — derivedMinPeriod пересчитывается из PG на
// каждом вызове, поэтому новый лидер после failover не несёт in-memory состояния
// опроса (тот же реестр → тот же шаг).
//
// Ошибка fetch (PG-glitch) не роняет лидера: fallback на ceiling (нечастый край
// коридора, не floor — чтобы не молотить PG в шторм) + warn. Следующий resolve
// повторит запрос.
//
// corridor вычисляется лениво (closure) — на каждом resolve, чтобы видеть
// hot-reload config-снимка.
func AdaptivePollInterval(
	ctx context.Context,
	corridor func() PollCorridor,
	fetcher MinPeriodFetcher,
	logger *slog.Logger,
) time.Duration {
	c := corridor()
	mp, err := fetcher.SelectMinPeriod(ctx)
	if err != nil {
		if logger != nil {
			logger.Warn("conductor: derivedMinPeriod query failed — fallback на poll_ceiling",
				slog.Duration("poll_ceiling", c.Ceiling), slog.Any("error", err))
		}
		return c.Ceiling
	}
	derived, ok := mp.DerivedMinPeriod()
	if !ok {
		return c.Idle
	}
	return cadence.Clamp(derived, c.Floor, c.Ceiling)
}
