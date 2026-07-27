package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/console"
	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Runtime wiring of the interactive console plane (NIM-143). Two background
// jobs run for the daemon's lifetime:
//
//   - the upstream consumer, which feeds this instance's `console:<kid>`
//     channel into the Hub — the return path for sessions whose Soul stream is
//     held by another Keeper;
//   - the idle sweep, which closes terminals nobody is typing into.
//
// Both degrade quietly: with no Redis the consumer is skipped (single-instance
// mode), and a zero idle timeout disables the sweep.

// consoleIdleSweepInterval is how often abandoned sessions are reaped. Coarse
// on purpose — the timeout is measured in minutes, so a finer tick would only
// add wakeups.
const consoleIdleSweepInterval = time.Minute

// consoleLimits resolves the operator-facing envelope from keeper.yml. Absent
// keys fall through to the package defaults (30 per Archon, 256 per instance,
// 30m idle).
func consoleLimits(cfg *config.KeeperConfig) console.Limits {
	if cfg == nil || cfg.Console == nil {
		return console.Limits{}
	}
	return console.Limits{
		MaxSessionsPerAID: cfg.Console.MaxSessionsPerArchon,
		MaxSessionsGlobal: cfg.Console.MaxSessionsGlobal,
		IdleTimeout:       parseConsoleIdleTimeout(cfg.Console.IdleTimeout),
	}
}

// parseConsoleIdleTimeout converts the configured duration. An unparsable value
// resolves to 0, i.e. the package default — the semantic config phase already
// rejects a malformed `duration`, so this is only the belt to that braces.
func parseConsoleIdleTimeout(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	// An explicit `0s` disables the sweep; the resolver reads 0 as "default",
	// so it is carried through as a negative sentinel.
	if d == 0 {
		return -1
	}
	return d
}

// startConsoleBackground launches the upstream consumer and both sweeps.
func (d *daemon) startConsoleBackground(ctx context.Context, bridge *console.ClusterBridge) {
	d.startConsoleUpstream(ctx, bridge)
	d.startConsoleIdleSweep(ctx)
	d.startConsoleOrphanSweep(ctx, bridge)
}

// consoleOrphanSweepInterval is how often this instance checks the bridged
// sessions it forwards for a vanished socket owner. Tied to the claim TTL
// (90s): sweeping much slower would widen the window an abandoned root shell
// survives, sweeping faster would only re-read claims that cannot have changed.
const consoleOrphanSweepInterval = 30 * time.Second

// startConsoleOrphanSweep reaps consoles whose owning Keeper died.
//
// Kill-on-disconnect covers the socket dying; this covers the KEEPER holding
// that socket dying, where nothing else notices — the EventStream never broke,
// so the Soul keeps the shell alive and its own invariant never fires. Only the
// instance holding the stream can see it, and only while it is bridging that
// session, which is exactly what the ClusterBridge tracks.
func (d *daemon) startConsoleOrphanSweep(ctx context.Context, bridge *console.ClusterBridge) {
	if d.consoleHub == nil || bridge == nil {
		return
	}
	ticker := time.NewTicker(consoleOrphanSweepInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := d.consoleHub.SweepOrphans(ctx); n > 0 {
					d.logger.Warn("console: orphaned sessions reaped (their socket owner is gone)",
						slog.Int("count", n))
				}
			}
		}
	}()
	d.cleanups.push(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			d.logger.Warn("console: orphan sweep did not stop in time")
		}
	})
}

// startConsoleUpstream subscribes to this instance's console channel and feeds
// the Hub. One subscription per Keeper, not per session.
func (d *daemon) startConsoleUpstream(ctx context.Context, bridge *console.ClusterBridge) {
	if bridge == nil || d.redisClient == nil {
		return
	}
	logger := d.logger

	sub, err := keeperredis.SubscribeConsoleUpstream(ctx, d.redisClient, d.cfg.KID, logger)
	if err != nil {
		// Cross-Keeper consoles degrade to same-instance operation; a console
		// whose socket and stream landed together still works.
		logger.Warn("console: upstream subscribe failed — cross-keeper consoles disabled",
			slog.Any("error", err))
		return
	}
	if err := sub.Ready(ctx); err != nil {
		logger.Warn("console: upstream subscribe not ready — cross-keeper consoles disabled",
			slog.Any("error", err))
		_ = sub.Close()
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		console.RunUpstreamConsumer(ctx, sub, d.consoleHub)
	}()
	d.cleanups.push(func() {
		_ = sub.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Warn("console: upstream consumer did not stop in time")
		}
	})
	logger.Info("console: upstream bridge active", slog.String("kid", d.cfg.KID))
}

// startConsoleIdleSweep closes sessions with no operator input.
func (d *daemon) startConsoleIdleSweep(ctx context.Context) {
	if d.consoleHub == nil {
		return
	}
	ticker := time.NewTicker(consoleIdleSweepInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n := d.consoleHub.SweepIdle(ctx); n > 0 {
					d.logger.Info("console: idle sessions closed", slog.Int("count", n))
				}
			}
		}
	}()
	d.cleanups.push(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			d.logger.Warn("console: idle sweep did not stop in time")
		}
	})
}
