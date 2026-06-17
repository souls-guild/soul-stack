// Package pluginhost — Soul-side wrapper над `shared/pluginhost` для запуска
// плагинов kind ∈ {soul_module, soul_beacon} (ADR-020 + ADR-030 V5-2,
// docs/keeper/plugins.md).
//
// Общая kind-agnostic часть (Spawn / handshake / Close / discovery / tailBuffer)
// живёт в [sharedhost]. Этот пакет добавляет:
//
//   - kind-specific обёртки: [Plugin] (gRPC-клиент SoulModule) и
//     [BeaconPlugin] (gRPC-клиент SoulBeacon);
//   - kind-specific дефолт SocketDir (`/var/run/soul-stack/plugins`);
//   - фильтр Discover-результатов: на Soul-host принимаются kind=soul_module
//     и kind=soul_beacon (cloud/ssh — Keeper-side, отсекаются в warnings).
//
// Парсинг manifest.yaml — в `shared/plugin`; этот пакет публикует только
// type-alias-ы, чтобы не ломать existing call-sites.
package pluginhost

import (
	"context"
	"crypto/ed25519"

	"github.com/souls-guild/soul-stack/shared/config"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// DefaultSocketDir — Soul-host дефолт каталога Unix-сокетов плагинов.
// Отличается от Keeper-host-дефолта: keeper-сервис работает под отдельным
// пользователем (docs/keeper/plugins.md → Расположение сокета).
const DefaultSocketDir = "/var/run/soul-stack/plugins"

// Defaults перевыставляются из shared для удобства call-sites (старые тесты
// ссылались на `pluginhost.DefaultStartupTimeout`).
const (
	DefaultStartupTimeout = sharedhost.DefaultStartupTimeout
	DefaultShutdownGrace  = sharedhost.DefaultShutdownGrace
)

// Re-export типов из shared. Алиасы намеренно: они дают call-sites привычные
// короткие имена (`pluginhost.Discovered`, `pluginhost.Manifest`) и стабильную
// поверхность контракта Soul-host-а.
type (
	Discovered = sharedhost.Discovered
	Manifest   = sharedplugin.Manifest
)

// Host — Soul-side runtime для плагинов kind=soul_module. Тонкая обёртка над
// [sharedhost.Host]: переопределяет Spawn так, чтобы возвращать [Plugin]
// вместо generic [sharedhost.BasePlugin] и попутно отсекать kind != soul_module.
type Host struct {
	*sharedhost.Host
}

// Spawn — fork плагина kind=soul_module и оборачивание [sharedhost.BasePlugin]
// в kind-specific [Plugin] (SoulModule-клиент). Возвращает ошибку, если
// Discovered.Manifest.Kind != soul_module (защита от kind-mismatch при ручной
// конструкции Discovered в тестах). Для kind=soul_beacon — отдельный
// конструктор [Host.SpawnBeacon].
func (h *Host) Spawn(ctx context.Context, d Discovered) (*Plugin, error) {
	if d.Manifest != nil && d.Manifest.Kind != KindSoulModule {
		return nil, errKindMismatch(KindSoulModule, d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return newPluginFromBase(base), nil
}

// SpawnBeacon — fork плагина kind=soul_beacon и оборачивание
// [sharedhost.BasePlugin] в [BeaconPlugin] (SoulBeacon-клиент). Параллель
// [Host.Spawn] для второго kind-а Soul-host-а (ADR-030 V5-2).
//
// Защита от kind-mismatch: если manifest.kind != soul_beacon — ошибка до
// fork-а (симметрия Keeper-host-а: разные kind-ы оборачиваются разными
// wrap-функциями).
func (h *Host) SpawnBeacon(ctx context.Context, d Discovered) (*BeaconPlugin, error) {
	if d.Manifest != nil && d.Manifest.Kind != KindSoulBeacon {
		return nil, errKindMismatch(KindSoulBeacon, d.Manifest.Kind)
	}
	base, err := h.Host.Spawn(ctx, d)
	if err != nil {
		return nil, err
	}
	return newBeaconFromBase(base), nil
}

// Kind-константы для Soul-host. Soul принимает kind=soul_module и
// kind=soul_beacon (ADR-030 V5-2); остальные re-export-ятся для
// конструктивного использования в тестах / проверочной логике.
const (
	KindSoulModule  = sharedplugin.KindSoulModule
	KindCloudDriver = sharedplugin.KindCloudDriver
	KindSSHProvider = sharedplugin.KindSSHProvider
	KindSoulBeacon  = sharedplugin.KindSoulBeacon
)

// SupportedProtocolVersions — версии plugin-протокола, понятные Soul-host-у.
// Делегируется в shared/plugin как single source of truth.
var SupportedProtocolVersions = sharedplugin.SupportedProtocolVersions

// NewHost конструирует Soul-host. Принимает `soul.yml::plugin_runtime` и
// подставляет [DefaultSocketDir] если cfg.SocketDir пуст.
//
// anchors — НАБОР trust-anchor-ов verify Sigil (ADR-026(h), R3 multi-anchor)
// из SoulSeed-а (распарсенный sigil_pubkey.pem может нести несколько PEM-блоков,
// см. [seed.ParseSigilPubKeys]). Пустой набор = Sigil не настроен на Keeper →
// verify любого custom-плагина fail-closed (no_trust_anchor). OR-проверка по
// набору даёт безразрывную ротацию ключа подписи. sigils — поверхность чтения
// runtime-кеша допусков ([SigilLookupAdapter]); nil = допусков нет → fail-closed
// (no_sigil). Оба пробрасываются в [sharedhost.Host] как DI; набор обёрнут в
// атомарный [sharedhost.AnchorSet] (S6 заменит его в рантайме сообщением
// SigilTrustAnchors без перезапуска Soul-а).
func NewHost(cfg *config.PluginRuntime, anchors []ed25519.PublicKey, sigils sharedhost.SigilLookup) (*Host, error) {
	base, err := sharedhost.NewHost(cfg, DefaultSocketDir)
	if err != nil {
		return nil, err
	}
	base.SigilAnchors = sharedhost.NewAnchorSet(anchors)
	base.Sigils = sigils
	return &Host{Host: base}, nil
}

// Discover — Soul-host discovery: ищет плагины в modulesRoot и оставляет
// только kind=soul_module и kind=soul_beacon (ADR-030 V5-2). Cloud/SSH-плагины
// и невалидные записи попадают в warnings (caller их логирует).
//
// Раскладка кеша (docs/soul/modules.md):
//
//	/var/lib/soul-stack/modules/
//	  <namespace>-<name>/
//	    manifest.yaml
//	    soul-mod-<name>        # для kind=soul_module
//	    soul-beacon-<name>     # для kind=soul_beacon
//
// Внешний контракт идентичен предыдущему: `[]Discovered, []string, error`.
// Caller сам разделяет результат по kind (через `d.Manifest.Kind`) и
// регистрирует в нужный реестр (module-registry / beacon-registry).
func Discover(modulesRoot string) ([]Discovered, []string, error) {
	all, warnings, err := sharedhost.Discover(modulesRoot)
	if err != nil {
		return nil, nil, err
	}
	soulOnly, filterWarns := sharedhost.FilterByKinds(all, []string{KindSoulModule, KindSoulBeacon})
	return soulOnly, append(warnings, filterWarns...), nil
}
