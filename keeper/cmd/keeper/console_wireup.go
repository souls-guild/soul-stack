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

// consoleRecorderConfig resolves the recording envelope from keeper.yml.
//
// Note what it CANNOT return: "off". Recording is mandatory (ADR-0074(g)), so
// keeper.yml has no key for it and this has no branch for it — the only thing
// policy tunes is the per-session cap.
func consoleRecorderConfig(cfg *config.KeeperConfig) console.RecorderConfig {
	if cfg == nil || cfg.Console == nil || cfg.Console.Recording == nil {
		return console.RecorderConfig{}
	}
	return console.RecorderConfig{MaxBytes: cfg.Console.Recording.MaxSessionBytes}
}

// The three providers below read the LIVE config snapshot on every call rather
// than closing over the boot-time one. That is what makes these keys admissible
// to the SettingsStore overlay (ADR-0073(j.5)): a key the UI can edit but whose
// consumer resolved it once at startup would accept the edit and change nothing
// until the next restart — the silent form of the failure `requires_restart`
// exists to prevent.

// consoleLimitsProvider resolves the operator envelope per check.
func (d *daemon) consoleLimitsProvider() func() console.Limits {
	return func() console.Limits { return consoleLimits(d.store.Get()) }
}

// consoleRecorderConfigProvider resolves the recording envelope per session.
func (d *daemon) consoleRecorderConfigProvider() func() console.RecorderConfig {
	return func() console.RecorderConfig { return consoleRecorderConfig(d.store.Get()) }
}

// consolePlaneEnabledProvider answers "does this cluster carry a console plane"
// per request (NIM-292). Both halves of the plane consult it — the WebSocket
// route and the MCP `keeper.soul.run-command` tool — because they are one
// privilege reached two ways.
func (d *daemon) consolePlaneEnabledProvider() func() bool {
	return func() bool { return d.store.Get().ConsolePlaneEnabled() }
}

// consoleRecordingRetention resolves how long recordings are kept. 0 lets the
// store apply its own default; the semantic phase already rejected a malformed
// duration, so an unparsable value here just falls through to it.
//
// Unlike the three above this one is still read once, at store construction:
// the retention is stamped into the row when a recording is created, and the
// store that stamps it lives in internal/consolepg. Moving it to a provider is
// tracked separately (NIM-292 follow-up) — until then it is the one `console:`
// key that stays file-only, because admitting it without a live apply path is
// exactly what (j.5) forbids.
func consoleRecordingRetention(cfg *config.KeeperConfig) time.Duration {
	if cfg == nil || cfg.Console == nil || cfg.Console.Recording == nil {
		return 0
	}
	d, err := time.ParseDuration(cfg.Console.Recording.Retention)
	if err != nil || d <= 0 {
		return 0
	}
	return d
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
	d.startConsolePlaneWatch(ctx)
}

// consolePlaneWatchInterval is how often the plane switch is re-read for the
// benefit of sessions ALREADY open. New sessions do not wait for this — their
// gate is per request — so this only bounds how long a shell survives the
// operator switching the plane off, and a tighter tick would only re-read a
// config snapshot that cannot have changed.
const consolePlaneWatchInterval = 10 * time.Second

// startConsolePlaneWatch closes live sessions once `console.enabled` goes false.
//
// The gate on the route stops the NEXT console; without this, the ones already
// open would outlive the decision — an operator would switch the plane off, see
// `/v1/console` answer 404, and still have root shells running behind it. The
// switch is a statement about the cluster, so it has to reach the sessions the
// cluster is currently holding.
//
// Idempotent by construction: [console.Hub.CloseAll] skips sessions already
// closed, so a disabled plane with nothing open is a no-op tick.
func (d *daemon) startConsolePlaneWatch(ctx context.Context) {
	if d.consoleHub == nil {
		return
	}
	ticker := time.NewTicker(consolePlaneWatchInterval)
	enabled := d.consolePlaneEnabledProvider()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if enabled() {
					continue
				}
				if n := d.consoleHub.CloseAll(ctx, console.ClosePlaneDisabled,
					"the console plane was switched off on this cluster"); n > 0 {
					d.logger.Warn("console: plane switched off — live sessions closed",
						slog.Int("count", n))
				}
			}
		}
	}()
	d.cleanups.push(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			d.logger.Warn("console: plane watch did not stop in time")
		}
	})
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
