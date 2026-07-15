package clouddriver

import (
	"context"
	"time"
)

// BackoffConfig holds exponential backoff parameters for [Retry] and
// [WaitUntilReady]. A common canon for all drivers: one retry shape instead
// of per-provider dialects. The zero struct is NOT valid — use
// [DefaultBackoff].
type BackoffConfig struct {
	// Initial is the delay before the first retry.
	Initial time.Duration
	// Max is the delay ceiling (the exponent caps out at it).
	Max time.Duration
	// Factor is the delay growth multiplier between attempts (usually 2.0).
	Factor float64
	// MaxAttempts is the maximum number of attempts (including the first).
	// 0 means no limit on count (bounded only by the ctx deadline); for
	// [Retry] this means "retry until success or ctx-cancel".
	MaxAttempts int
}

// DefaultBackoff is a sane default for cloud APIs (throttling-friendly):
// 1s → 2s → 4s → … → 30s, up to 8 attempts.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{
		Initial:     1 * time.Second,
		Max:         30 * time.Second,
		Factor:      2.0,
		MaxAttempts: 8,
	}
}

// next computes the delay for attempt (0-based: attempt=0 → Initial).
func (b BackoffConfig) next(attempt int) time.Duration {
	d := float64(b.Initial)
	for i := 0; i < attempt; i++ {
		d *= b.Factor
		if d >= float64(b.Max) {
			return b.Max
		}
	}
	if d > float64(b.Max) {
		return b.Max
	}
	return time.Duration(d)
}

// Retry runs op with exponential backoff, as long as op returns an error
// classified as transient (via [Classify]+classify). A non-transient error
// is returned immediately (no point retrying auth/quota/not_found).
//
// Returns nil on first success; the last error once MaxAttempts is
// exhausted; ctx.Err() on cancellation/timeout while waiting for backoff.
// This is a common canon for all drivers: idempotent operations
// (DescribeImages, RunInstances under throttling) are wrapped by it
// uniformly.
func Retry(ctx context.Context, cfg BackoffConfig, classify ClassifyFunc, op func() error) error {
	attempt := 0
	for {
		err := op()
		if err == nil {
			return nil
		}
		if !Classify(classify, err).Transient() {
			return err
		}
		attempt++
		if cfg.MaxAttempts > 0 && attempt >= cfg.MaxAttempts {
			return err
		}
		if waitErr := sleepCtx(ctx, cfg.next(attempt-1)); waitErr != nil {
			return waitErr
		}
	}
}

// sleepCtx waits for d or ctx-cancel, whichever comes first. Returns
// ctx.Err() on cancellation, nil once d elapses. The single wait point for
// Retry/WaitUntilReady (ctx-aware, no timer leaks).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Honor an already-cancelled ctx even with a zero delay.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
