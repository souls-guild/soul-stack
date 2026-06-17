// Package pluginhost — Keeper-side wrapper над `shared/pluginhost` для запуска
// плагинов kind=cloud_driver и kind=ssh_provider (ADR-020, docs/keeper/plugins.md).
//
// Общая kind-agnostic часть (Spawn / handshake / Close / discovery / tailBuffer)
// живёт в [sharedhost]. Этот пакет добавляет:
//
//   - kind-specific обёртки [CloudDriverPlugin], [SshProviderPlugin], общий
//     приватный [Plugin] с gRPC-conn;
//   - kind-specific дефолт SocketDir (`/var/run/soul-stack-keeper/plugins`);
//   - фильтр Discover-результатов: на Keeper-host принимаются только
//     cloud_driver и ssh_provider;
//   - [FilterByCatalog] для cross-check найденных плагинов с реестром
//     `keeper.yml::plugins.{cloud_drivers,ssh_providers}`.
package pluginhost

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// DefaultSocketDir — Keeper-host дефолт каталога Unix-сокетов плагинов.
// Отличается от Soul-host-дефолта: keeper-сервис работает под отдельным
// пользователем (docs/keeper/plugins.md → Расположение сокета).
const DefaultSocketDir = "/var/run/soul-stack-keeper/plugins"

// DefaultCacheRoot — конвенция директории-кеша Keeper-side плагинов
// (ADR-020(a), симметрия с [DefaultSocketDir]). Используется wire-up-ом
// main-а, когда `keeper.yml` не задаёт явный путь (поле кеша в схеме
// конфига пока не введено — git-резолв `plugins.{cloud_drivers,ssh_providers}`
// отдельной задачей).
const DefaultCacheRoot = "/var/lib/soul-stack-keeper/plugins"

// Defaults перевыставляются из shared для удобства call-sites.
const (
	DefaultStartupTimeout = sharedhost.DefaultStartupTimeout
	DefaultShutdownGrace  = sharedhost.DefaultShutdownGrace
)

// Re-export типов из shared. Алиасы намеренно: они дают call-sites привычные
// короткие имена и стабильную поверхность контракта Keeper-host-а.
type (
	Discovered = sharedhost.Discovered
	Manifest   = sharedplugin.Manifest
)

// Kind-константы для Keeper-host.
const (
	KindSoulModule  = sharedplugin.KindSoulModule
	KindCloudDriver = sharedplugin.KindCloudDriver
	KindSSHProvider = sharedplugin.KindSSHProvider
)

// SupportedProtocolVersions — версии plugin-протокола, понятные Keeper-host-у.
// Делегируется в shared/plugin как single source of truth.
var SupportedProtocolVersions = sharedplugin.SupportedProtocolVersions

// Host — Keeper-side runtime для плагинов kind ∈ {cloud_driver, ssh_provider}.
// Тонкая обёртка над [sharedhost.Host] с kind-specific Spawn-методами.
type Host struct {
	*sharedhost.Host
}

// NewHost конструирует Keeper-host. Принимает `keeper.yml::plugin_runtime` и
// подставляет [DefaultSocketDir] если cfg.SocketDir пуст.
//
// anchors — НАБОР trust-anchor-ов verify Sigil (ADR-026(h), R3 multi-anchor):
// публичные ключи всех active-ключей keeper-Signer-а ([sigil.Signer.AnchorSet]).
// keeper-host верифицирует СВОИ плагины против печатей, которые сам же подписал
// (ADR-026(f)), поэтому набор якорей — это active-набор подписи; OR-проверка
// даёт безразрывную ротацию ключа. Пустой набор = Sigil не настроен на Keeper →
// verify любого плагина fail-closed (no_trust_anchor): оператор с cloud/ssh
// обязан настроить Sigil + allow. sigils — поверхность чтения активных допусков
// ([SigilLookupAdapter] поверх реестра plugin_sigils); nil = допусков нет →
// fail-closed (no_sigil). Оба пробрасываются в [sharedhost.Host] как DI;
// набор обёрнут в атомарный [sharedhost.AnchorSet] (S6 заменит его в рантайме).
func NewHost(cfg *config.PluginRuntime, anchors []ed25519.PublicKey, sigils sharedhost.SigilLookup) (*Host, error) {
	base, err := sharedhost.NewHost(cfg, DefaultSocketDir)
	if err != nil {
		return nil, err
	}
	base.SigilAnchors = sharedhost.NewAnchorSet(anchors)
	base.Sigils = sigils
	return &Host{Host: base}, nil
}

// SpawnOption — алиас на [sharedhost.SpawnOption] для удобства call-site
// (keeper-side caller не таскает shared-импорт ради одного типа).
type SpawnOption = sharedhost.SpawnOption

// WithEnv — re-export [sharedhost.WithEnv] (см. doc там). Используется push-S6
// wire-up-ом SshDispatcher для env-payload params SshProvider-плагина
// (ADR-020 amendment l).
func WithEnv(env []string) SpawnOption { return sharedhost.WithEnv(env) }

// Spawn — fork плагина и возврат generic [sharedhost.BasePlugin]. Caller
// оборачивает результат в kind-specific [CloudDriverPlugin] / [SshProviderPlugin]
// через [NewCloudDriverPlugin] / [NewSshProviderPlugin] — Keeper-host
// различает два kind-а, поэтому промежуточный generic Plugin делает выбор
// явным, а не неявным.
//
// Защита от kind-mismatch: если manifest.kind не в {cloud_driver, ssh_provider},
// Spawn возвращает ошибку до fork-а.
//
// opts — опц. SpawnOption-ы ([WithEnv] и т.п.); пробрасываются в
// [sharedhost.Host.Spawn] без изменений.
func (h *Host) Spawn(ctx context.Context, d Discovered, opts ...SpawnOption) (*Plugin, error) {
	if d.Manifest != nil &&
		d.Manifest.Kind != KindCloudDriver &&
		d.Manifest.Kind != KindSSHProvider {
		return nil, fmt.Errorf("pluginhost: expected kind=cloud_driver|ssh_provider, got %q", d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d, opts...)
	if err != nil {
		return nil, err
	}
	return &Plugin{BasePlugin: base}, nil
}

// Plugin — Keeper-side generic handle. Не содержит kind-specific gRPC-клиента
// (в отличие от Soul-host-а, где kind ровно один): caller оборачивает Plugin в
// [CloudDriverPlugin] / [SshProviderPlugin] через NewCloudDriverPlugin /
// NewSshProviderPlugin.
type Plugin struct {
	*sharedhost.BasePlugin
}

// Discover — Keeper-host discovery: ищет плагины в cacheRoot и оставляет
// только kind ∈ {cloud_driver, ssh_provider}. Soul-плагины и невалидные
// записи попадают в warnings.
//
// Раскладка кеша (R-nested layout, A1-S1 — git-резолвер наполняет слоты):
//
//	<cacheRoot>/
//	  <namespace>-<name>/
//	    current -> <commit_sha>       # symlink на активный слот
//	    <commit_sha>/
//	      manifest.yaml
//	      soul-cloud-<name>           # для kind=cloud_driver
//	      soul-ssh-<name>             # для kind=ssh_provider
//
// Discovery идёт через `current` (одноуровневый резолв символа): для каждого
// каталога `<ns>-<name>` дискаверится `<ns>-<name>/current/`. Каталоги без
// валидного `current` (резолвер ещё не наполнил слот) попадают в warnings.
//
// Наполнение кеша git-резолвером (`plugins.{cloud_drivers,ssh_providers}` →
// commit_sha-слот) делает [plugingit.Resolver] ДО Discover при старте Keeper-а;
// [FilterByCatalog] фильтрует найденное по реестру.
func Discover(cacheRoot string) ([]Discovered, []string, error) {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost: read plugin cache root %q: %w", cacheRoot, err)
	}
	var (
		all      []sharedhost.Discovered
		warnings []string
	)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Активный слот плагина — через current-symlink. sharedhost.Discover
		// читает manifest+бинарь из переданного каталога; current указывает на
		// commit_sha-слот с этой раскладкой.
		current := filepath.Join(cacheRoot, e.Name(), CurrentLink)
		if _, statErr := os.Stat(current); statErr != nil {
			warnings = append(warnings, fmt.Sprintf("skip %s: no active slot (current): %v",
				filepath.Join(cacheRoot, e.Name()), statErr))
			continue
		}
		found, warns := sharedhost.DiscoverSlot(current)
		all = append(all, found...)
		warnings = append(warnings, warns...)
	}
	keeperOnly, filterWarns := sharedhost.FilterByKinds(all, []string{KindCloudDriver, KindSSHProvider})
	return keeperOnly, append(warnings, filterWarns...), nil
}

// FilterByCatalog оставляет в `found` только те плагины, чьё `manifest.name`
// упомянуто в каталоге `keeper.yml::plugins.{cloud_drivers,ssh_providers}`.
// Сравнение идёт по полю `name` (PluginCatalogEntry.Name) — это тот же
// kebab-case, что и `manifest.name`.
//
// Возвращает отфильтрованный список и список warnings:
//
//   - запись каталога без найденного плагина → warning;
//   - найденный плагин без записи в каталоге → warning.
//
// Сами `source`/`ref` каталога этим filter-ом не используются — git-резолв
// отдельная задача (см. [Discover]).
func FilterByCatalog(found []Discovered, plugins *config.KeeperPlugins) ([]Discovered, []string) {
	if plugins == nil {
		return nil, nil
	}
	// Индексируем декларированные имена по kind, чтобы один проход по
	// found валидировал оба списка одновременно. set-ы пустые если nil-блок.
	wantCloud := indexEntries(plugins.CloudDrivers)
	wantSSH := indexEntries(plugins.SSHProviders)

	var (
		out      []Discovered
		warnings []string
	)
	seenCloud := make(map[string]bool, len(wantCloud))
	seenSSH := make(map[string]bool, len(wantSSH))
	for _, d := range found {
		switch d.Manifest.Kind {
		case KindCloudDriver:
			if _, ok := wantCloud[d.Manifest.Name]; ok {
				out = append(out, d)
				seenCloud[d.Manifest.Name] = true
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"plugin %s (kind=cloud_driver) not declared in keeper.yml::plugins.cloud_drivers",
					d.Manifest.Address()))
			}
		case KindSSHProvider:
			if _, ok := wantSSH[d.Manifest.Name]; ok {
				out = append(out, d)
				seenSSH[d.Manifest.Name] = true
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"plugin %s (kind=ssh_provider) not declared in keeper.yml::plugins.ssh_providers",
					d.Manifest.Address()))
			}
		}
	}
	for name := range wantCloud {
		if !seenCloud[name] {
			warnings = append(warnings, fmt.Sprintf(
				"keeper.yml::plugins.cloud_drivers[name=%s] declared but binary not found in cache", name))
		}
	}
	for name := range wantSSH {
		if !seenSSH[name] {
			warnings = append(warnings, fmt.Sprintf(
				"keeper.yml::plugins.ssh_providers[name=%s] declared but binary not found in cache", name))
		}
	}
	return out, warnings
}

func indexEntries(entries []config.PluginCatalogEntry) map[string]struct{} {
	if len(entries) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		m[e.Name] = struct{}{}
	}
	return m
}
