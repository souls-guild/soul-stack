package main

// push_router_config.go — RouterConfigSource adapter over config.Store
// (ADR-032 amendment 2026-05-27, P2 W-4 Multi-provider routing).
//
// The router needs a "live" snapshot of CovenDefaultProviders /
// ClusterDefaultProvider: on every Reload (SIGHUP / file-watch / API push)
// Store.Get() returns a fresh *KeeperConfig, from which we pull the current
// routing fields without recreating the PGRouter. Otherwise we'd have to
// rebuild pushorch.PushRun on every reload -- too invasive.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/config"
)

// pushRouterConfigSource implements [push.RouterConfigSource] over
// config.Store[KeeperConfig]. Snapshot() is a single round-trip to
// Store.Get() (atomic read, hot-reload-safe).
type pushRouterConfigSource struct {
	store *config.Store[config.KeeperConfig]
}

// newPushRouterConfigSource is the adapter's constructor. Store != nil is
// guaranteed by the daemon pipeline (setupConfig fails the start on nil).
func newPushRouterConfigSource(store *config.Store[config.KeeperConfig]) *pushRouterConfigSource {
	return &pushRouterConfigSource{store: store}
}

// Snapshot returns the current routing snapshot. When the push block is
// absent, it returns an empty map/string -- the router then immediately
// falls into ErrProviderNotRouted, which is intended (either the provider is
// configured in souls.ssh_target, or it fails per-host).
func (s *pushRouterConfigSource) Snapshot() push.RouterConfig {
	cfg := s.store.Get()
	if cfg == nil || cfg.Push == nil {
		return push.RouterConfig{}
	}
	return push.RouterConfig{
		CovenDefaultProviders:  cfg.Push.CovenDefaultProviders,
		ClusterDefaultProvider: cfg.Push.ClusterDefaultProvider,
	}
}
