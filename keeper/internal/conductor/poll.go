package conductor

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
)

// PollCorridor is a snapshot of the adaptive-poll corridor (ADR-048 "Adaptive
// interval", "Calm" profile 30s/60s/120s). Re-read on every resolve from a
// fresh config snapshot → hot-reload of floor/ceiling/idle changes.
type PollCorridor struct {
	Floor   time.Duration
	Ceiling time.Duration
	Idle    time.Duration
}

// MinPeriodFetcher reads aggregates of the enabled Cadence registry from PG
// (see [cadence.SelectMinPeriod]). Extracted as an interface for unit-testing
// [AdaptivePollInterval] without a live pool.
type MinPeriodFetcher interface {
	SelectMinPeriod(ctx context.Context) (cadence.MinPeriod, error)
}

// AdaptivePollInterval computes the Conductor poll step (ADR-048 "Adaptive
// interval"): clamp(derivedMinPeriod, floor, ceiling); empty enabled registry
// → idle. Stateless by construction — derivedMinPeriod is recomputed from PG
// on every call, so a new leader after failover carries no in-memory poll
// state (same registry → same step).
//
// A fetch error (PG glitch) doesn't bring down the leader: falls back to
// ceiling (the infrequent edge of the corridor, not floor — to avoid
// hammering PG during an outage) + warn. The next resolve retries the query.
//
// corridor is computed lazily (closure) — on every resolve, to see config
// snapshot hot-reloads.
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
			logger.Warn("conductor: derivedMinPeriod query failed — falling back to poll_ceiling",
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
