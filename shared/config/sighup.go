package config

// SIGHUP handler per [ADR-021](docs/architecture.md) (b): in MVP the file-edit
// path is covered by re-reading the config on SIGHUP. inotify/fanotify are
// explicitly post-MVP.
//
// `WatchSIGHUP` is a library API: the `keeper`/`soul` binaries wire up the
// watcher and handle `ReloadResult` via slog (`config.reload_succeeded` /
// `config.reload_failed`) — that comes in M0.4 / separate bin-slices. In this
// slice the binaries stay stubs.

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// reloadChanBuf — the buffer length of the ReloadResult channel. One free slot:
// if the caller falls behind, the last reload result stays in the channel and
// further sends from the handler are non-blocking (the select `default` branch)
// — the handler never blocks on a slow consumer.
const reloadChanBuf = 1

// sighupChanBuf — the buffer length of the internal signal channel.
// `signal.Notify` needs a buffered channel, otherwise the runtime may drop a
// SIGHUP while handling the previous one. One is the standard recommendation
// from the `os/signal` godoc.
const sighupChanBuf = 1

// WatchSIGHUP starts a goroutine that, on each SIGHUP, calls
// `store.Reload(ctx, "signal")` and publishes a `ReloadResult` to the returned
// channel.
//
// Semantics:
//
//   - The channel is buffered to 1. If the caller has not read the previous
//     ReloadResult, the next publish is non-blocking and dropped (we do not
//     queue reloads: only the latest matters, the file is always re-read).
//   - On `ctx.Done()` the handler does `signal.Stop` (unregister), then
//     `close(out)`. The caller may rely on the channel close as the watcher's
//     completion signal.
//   - Several concurrent `WatchSIGHUP` in one process are allowed: each
//     registers its own signal channel via `signal.Notify`, and the runtime
//     delivers SIGHUP to all registered channels.
//   - SIGHUP is available on unix systems; on Windows the constant
//     `syscall.SIGHUP` exists for compatibility but there is no real SIGHUP
//     source — the watcher simply receives no notifications.
func WatchSIGHUP[T any](ctx context.Context, store *Store[T]) <-chan ReloadResult {
	// Fail-fast in the caller's stack: a nil store is a guaranteed footgun
	// (e.g. `LoadKeeperStore` returned `(nil, diags, err)` on a missing config
	// and the operator forgot to check err). Without this check the panic would
	// arrive asynchronously from the watcher goroutine on the first SIGHUP.
	if store == nil {
		panic("config.WatchSIGHUP: store is nil")
	}

	out := make(chan ReloadResult, reloadChanBuf)

	sig := make(chan os.Signal, sighupChanBuf)
	signal.Notify(sig, syscall.SIGHUP)

	go func() {
		defer close(out)
		defer signal.Stop(sig)

		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				res := store.Reload(ctx, ReloadSourceSignal)
				select {
				case out <- res:
				default:
					// Caller is behind: the previous result is still in
					// the channel. Skip it — the next SIGHUP re-reads the
					// file.
				}
			}
		}
	}()

	return out
}
