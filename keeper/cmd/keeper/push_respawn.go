package main

// push_respawn.go -- implementation of [push.ProviderRespawner] for the daemon wire-up
// (S7-2 hot-reload, ADR-032 amendment 2026-05-27).
//
// Context: setupPushProviderSvc sets up a Redis pub/sub subscription on
// `push-providers:changed`; on a `push_providers` mutation via the REST/MCP facade,
// every node in the cluster receives the name of the changed provider. The daemon listener
// (runPushProviderInvalidationListener) delegates the actual re-spawn to
// `SshDispatcher.RefreshProvider`, which in turn needs a respawner
// -- a component that knows HOW to bring up a new plugin handle with the updated
// env payload. This file contains that implementation: it holds references to
// pluginhost.Host + discovered + PGFallbackProviderResolver, and on invocation
// closes the old handle and spawns a new one.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
)

// pushProviderRespawner -- push.ProviderRespawner on top of pluginhost.Host and
// PGFallbackProviderResolver. Holds the discovered list exactly so it can
// find the manifest+binary by plugin name (multi-provider routing is post-S7,
// right now the list has a single element).
type pushProviderRespawner struct {
	host       *pluginhost.Host
	discovered []pluginhost.Discovered
	resolver   *push.PGFallbackProviderResolver
	logger     *slog.Logger
}

// newPushProviderRespawner assembles the respawner. Returns nil without an error if
// any of the required components is missing (push disabled / Redis disabled
// / pilot single-instance dev) -- `SshDispatcher.RefreshProvider` will then return
// ErrRespawnNotSupported, and the listener will just log and continue.
func newPushProviderRespawner(
	host *pluginhost.Host,
	discovered []pluginhost.Discovered,
	resolver *push.PGFallbackProviderResolver,
	logger *slog.Logger,
) *pushProviderRespawner {
	if host == nil || len(discovered) == 0 || resolver == nil {
		return nil
	}
	return &pushProviderRespawner{
		host:       host,
		discovered: discovered,
		resolver:   resolver,
		logger:     logger,
	}
}

// RespawnProvider -- implementation of [push.ProviderRespawner].
//
// Steps:
//  1. Find Discovered by name (manifest.Name);
//  2. Close oldCloser if passed (the plugin holds a unix socket + child
//     process -- both must be cleaned up BEFORE spawning the new one, to avoid
//     leaking orphan sockets on a Spawn error);
//  3. Resolve fresh params via the resolver (PG -> legacy fallback);
//  4. Build the env payload and Spawn a new plugin handle;
//  5. Wrap BasePlugin in SshProviderPlugin (type-check guard kind=ssh_provider).
//
// Returns:
//   - (SshProvider, io.Closer, nil) -- handle ready for Authorize/Sign.
//   - (nil, nil, error) -- diagnostic; the caller (SshDispatcher) transitions to
//     degraded state.
func (r *pushProviderRespawner) RespawnProvider(ctx context.Context, providerName string, oldCloser io.Closer) (push.SshProvider, io.Closer, error) {
	d := r.findDiscoveredByName(providerName)
	if d == nil {
		return nil, nil, fmt.Errorf("respawn: no discovered SshProvider %q", providerName)
	}

	if oldCloser != nil {
		if cerr := oldCloser.Close(); cerr != nil {
			// Not fatal: we still need to spawn a new one, otherwise the provider
			// stays unavailable. A warning is enough -- the old process will
			// either die on its own or become a zombie until reaped by the OS.
			r.logger.Warn("respawn: close old plugin-handle returned error",
				slog.String("provider", providerName), slog.Any("error", cerr))
		}
	}

	params, resolveErr := r.resolver.ResolveParams(ctx, providerName)
	// resolveErr=ErrPushProviderNotConfigured is fine: the plugin simply
	// starts without an env payload (as in pilot S6). Real PG/transport errors --
	// fail (if there's nothing to respawn from, leave the dispatcher degraded).
	if resolveErr != nil && !errors.Is(resolveErr, push.ErrPushProviderNotConfigured) {
		return nil, nil, fmt.Errorf("respawn: resolve params %q: %w", providerName, resolveErr)
	}

	spawnOpts, _, optErr := buildPushSpawnOptsFromParams(providerName, params)
	if optErr != nil {
		return nil, nil, fmt.Errorf("respawn: build env-payload %q: %w", providerName, optErr)
	}

	plugin, err := r.host.Spawn(ctx, *d, spawnOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("respawn: spawn %s: %w", d.Manifest.Address(), err)
	}
	wrapped, err := pluginhost.NewSshProviderPlugin(plugin)
	if err != nil {
		_ = plugin.Close()
		return nil, nil, fmt.Errorf("respawn: wrap %s: %w", d.Manifest.Address(), err)
	}
	return wrapped, wrapped, nil
}

// findDiscoveredByName -- linear search over discovered (single-provider pilot
// usually holds 1 element; multi-provider is a handful, no point building a map).
func (r *pushProviderRespawner) findDiscoveredByName(name string) *pluginhost.Discovered {
	for i := range r.discovered {
		if r.discovered[i].Manifest != nil && r.discovered[i].Manifest.Name == name {
			return &r.discovered[i]
		}
	}
	return nil
}
