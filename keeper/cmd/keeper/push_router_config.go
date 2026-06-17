package main

// push_router_config.go — RouterConfigSource adapter поверх config.Store
// (ADR-032 amendment 2026-05-27, P2 W-4 Multi-provider routing).
//
// router-у нужен «живой» снимок CovenDefaultProviders / ClusterDefaultProvider:
// на каждый Reload (SIGHUP / file-watch / API push) Store.Get() возвращает
// свежий *KeeperConfig, отсюда тянем актуальные routing-поля без пересоздания
// PGRouter-а. Иначе пришлось бы пересобирать pushorch.PushRun на каждый
// reload — слишком инвазивно.

import (
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/config"
)

// pushRouterConfigSource реализует [push.RouterConfigSource] поверх
// config.Store[KeeperConfig]. Snapshot() — единый round-trip к Store.Get()
// (атомарный read, hot-reload-safe).
type pushRouterConfigSource struct {
	store *config.Store[config.KeeperConfig]
}

// newPushRouterConfigSource — конструктор adapter-а. Store != nil гарантировано
// daemon-pipeline-ом (setupConfig валит старт при nil).
func newPushRouterConfigSource(store *config.Store[config.KeeperConfig]) *pushRouterConfigSource {
	return &pushRouterConfigSource{store: store}
}

// Snapshot — текущий routing-снимок. При отсутствии push-блока возвращает
// пустые карту/строку — router тогда сразу падает в ErrProviderNotRouted, что
// и нужно (либо provider настроен в souls.ssh_target, либо fail per-host).
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
