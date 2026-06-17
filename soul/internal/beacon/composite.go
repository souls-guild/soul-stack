package beacon

import "log/slog"

// CompositeRegistry — соединяет core (статический) и plugin (resolver-over-
// pluginhost) beacon-наборы для scheduler-а. Lookup идёт последовательно: сперва
// core (priority — чтобы plugin не подменил `core.beacon.*` именами), потом
// plugin. Полная аналогия [soul/internal/runtime.CompositeRegistry] для
// SoulModule.
//
// PluginLookup — отдельный интерфейс: composite не зависит от pluginhost-а,
// связывание происходит в cmd/soul wire-up.
type CompositeRegistry struct {
	core   BeaconLookup
	plugin BeaconLookup
	logger *slog.Logger
}

// NewCompositeRegistry собирает реестр. Любая из веток может быть nil (например,
// в push-режиме plugin-discovery нет — передаётся nil); Lookup пропустит ветку.
//
// Конфликт имён core ↔ plugin не возможен в норме: plugin-имена резолвятся как
// `<namespace>.<name>` (например `community.zfs-degraded`), core — как
// `core.beacon.<name>`. Но защита в порядке проверки оставлена: core всегда
// первым, даже при ручном тиражировании имени.
func NewCompositeRegistry(core, plugin BeaconLookup, logger *slog.Logger) *CompositeRegistry {
	if logger == nil {
		logger = slog.Default()
	}
	return &CompositeRegistry{core: core, plugin: plugin, logger: logger}
}

// Lookup — последовательный поиск по слоям. Возвращает первый match.
func (c *CompositeRegistry) Lookup(name string) (Beacon, bool) {
	if c.core != nil {
		if b, ok := c.core.Lookup(name); ok {
			return b, true
		}
	}
	if c.plugin != nil {
		if b, ok := c.plugin.Lookup(name); ok {
			return b, true
		}
	}
	return nil, false
}
