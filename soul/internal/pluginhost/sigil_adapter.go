package pluginhost

import (
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// sigilCache — поверхность чтения runtime-кеша Sigil-ов, нужная адаптеру.
// Реализуется *sigilcache.Cache; сужение до интерфейса держит soul/internal/
// pluginhost независимым от sigilcache-пакета напрямую (адаптер биндится в
// cmd/soul при wire-up).
type sigilCache interface {
	Get(namespace, name string) *keeperv1.PluginSigil
}

// SigilLookupAdapter мостит runtime-кеш Sigil-ов Soul-а
// (*sigilcache.Cache, ключ keeperv1.PluginSigil) к verify-контракту
// shared/pluginhost.SigilLookup. Здесь — единственная точка маппинга
// keeperv1.PluginSigil → shared.SigilRecord: shared НЕ тянет keeper-proto
// (verify-DTO), proto-зависимость остаётся на Soul-стороне.
type SigilLookupAdapter struct {
	cache sigilCache
}

// NewSigilLookupAdapter оборачивает кеш в shared-совместимый SigilLookup.
// nil-кеш → адаптер всегда возвращает nil-запись (verify fail-closed по
// no_sigil): защита от nil-разыменования при неполном wire-up.
func NewSigilLookupAdapter(cache sigilCache) *SigilLookupAdapter {
	return &SigilLookupAdapter{cache: cache}
}

// Get резолвит активный допуск по (namespace, name) и проецирует
// keeperv1.PluginSigil в shared.SigilRecord. nil (допуск не доехал) → nil
// (verify трактует как no_sigil).
//
// Manifest берётся из PluginSigil.Manifest — СЫРЫЕ байты manifest.yaml из
// транспорта (M1), которые verify прогоняет через NormalizeManifestBytes
// (S3↔S6-инвариант: не parsed-форма, не файл с диска).
func (a *SigilLookupAdapter) Get(namespace, name string) *sharedhost.SigilRecord {
	if a.cache == nil {
		return nil
	}
	sig := a.cache.Get(namespace, name)
	if sig == nil {
		return nil
	}
	return &sharedhost.SigilRecord{
		Namespace:       sig.GetNamespace(),
		Name:            sig.GetName(),
		Ref:             sig.GetRef(),
		BinarySHA256hex: sig.GetBinarySha256(),
		Signature:       sig.GetSignature(),
		Manifest:        sig.GetManifest(),
	}
}
