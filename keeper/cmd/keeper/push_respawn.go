package main

// push_respawn.go — реализация [push.ProviderRespawner] для daemon-wire-up
// (S7-2 hot-reload, ADR-032 amendment 2026-05-27).
//
// Контекст: setupPushProviderSvc поднимает Redis pub/sub subscription на
// `push-providers:changed`; при мутации `push_providers` через REST/MCP-фасад
// каждая нода кластера получает имя изменённого провайдера. Daemon-listener
// (runPushProviderInvalidationListener) делегирует фактический re-spawn
// `SshDispatcher.RefreshProvider`, которому, в свою очередь, нужен respawner
// — компонент, знающий, КАК поднять новый plugin-handle с обновлёнными
// env-payload. Этот файл содержит такую реализацию: она держит ссылки на
// pluginhost.Host + discovered + PGFallbackProviderResolver, и при вызове
// закрывает старый handle и spawn-ит новый.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
)

// pushProviderRespawner — push.ProviderRespawner поверх pluginhost.Host и
// PGFallbackProviderResolver. Хранит discovered-список ровно для того, чтобы
// найти manifest+бинарь по имени плагина (multi-provider routing — пост-S7,
// сейчас список из одного элемента).
type pushProviderRespawner struct {
	host       *pluginhost.Host
	discovered []pluginhost.Discovered
	resolver   *push.PGFallbackProviderResolver
	logger     *slog.Logger
}

// newPushProviderRespawner собирает respawner. Возвращает nil без ошибки, если
// какой-то из required-компонентов отсутствует (push выключен / Redis выключен
// / pilot single-instance dev) — `SshDispatcher.RefreshProvider` тогда вернёт
// ErrRespawnNotSupported, listener просто залогирует и продолжит.
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

// RespawnProvider — реализация [push.ProviderRespawner].
//
// Шаги:
//  1. Найти Discovered по имени (manifest.Name);
//  2. Закрыть oldCloser, если передан (плагин держит unix-socket + child-
//     process — оба нужно прибрать ДО spawn-а нового, чтобы не плодить
//     orphan-сокеты при ошибке Spawn);
//  3. Резолвить свежие params через resolver (PG → legacy-fallback);
//  4. Собрать env-payload и Spawn новый plugin-handle;
//  5. Обернуть BasePlugin в SshProviderPlugin (типовая защита kind=ssh_provider).
//
// Возврат:
//   - (SshProvider, io.Closer, nil) — handle готов к Authorize/Sign.
//   - (nil, nil, error) — diagnostic; caller (SshDispatcher) перейдёт в
//     degraded state.
func (r *pushProviderRespawner) RespawnProvider(ctx context.Context, providerName string, oldCloser io.Closer) (push.SshProvider, io.Closer, error) {
	d := r.findDiscoveredByName(providerName)
	if d == nil {
		return nil, nil, fmt.Errorf("respawn: no discovered SshProvider %q", providerName)
	}

	if oldCloser != nil {
		if cerr := oldCloser.Close(); cerr != nil {
			// Не fatal: спавнить новый всё равно надо, иначе провайдер
			// останется недоступен. Warning достаточно — старый процесс
			// либо доумрёт сам, либо станет zombie до Reaper-а ОС.
			r.logger.Warn("respawn: close old plugin-handle returned error",
				slog.String("provider", providerName), slog.Any("error", cerr))
		}
	}

	params, resolveErr := r.resolver.ResolveParams(ctx, providerName)
	// resolveErr=ErrPushProviderNotConfigured допустимо: плагин просто
	// стартует без env-payload (как pilot S6). Real PG/transport-ошибки —
	// fail (если респаунить не на чем — оставляем dispatcher degraded).
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

// findDiscoveredByName — линейный поиск по discovered (single-provider pilot
// держит обычно 1 элемент; multi-provider — единицы, мапу строить нет смысла).
func (r *pushProviderRespawner) findDiscoveredByName(name string) *pluginhost.Discovered {
	for i := range r.discovered {
		if r.discovered[i].Manifest != nil && r.discovered[i].Manifest.Name == name {
			return &r.discovered[i]
		}
	}
	return nil
}
