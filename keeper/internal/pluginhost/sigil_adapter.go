package pluginhost

import (
	"context"
	"log/slog"

	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// SigilRecordLister — поверхность чтения активных допусков в verify-форме,
// нужная адаптеру. Возвращает уже спроецированные [sharedhost.SigilRecord]
// (single source of маппинга sigil.Sigil → SigilRecord держит call-site —
// `keeper run`), чтобы keeper/internal/pluginhost НЕ импортировал
// keeper/internal/sigil: sigil уже импортирует pluginhost (ReadSlot/SlotContents),
// прямой импорт обратно дал бы import-цикл.
//
// keeper читает plugin_sigils НАПРЯМУЮ из своей БД (pool), в отличие от Soul-а,
// который получает допуски broadcast-ом по EventStream и держит in-memory кеш.
type SigilRecordLister interface {
	ListActive(ctx context.Context) ([]*sharedhost.SigilRecord, error)
}

// SigilLookupAdapter мостит keeper-side реестр plugin_sigils (читается из
// Postgres) к verify-контракту shared/pluginhost.SigilLookup. keeper-host сам
// верифицирует СВОИ плагины (CloudDriver / SshProvider) против печатей доверия,
// которые сам же и подписал (ADR-026(f)): trust-anchor — публичный ключ
// keeper-Signer-а, источник допусков — тот же реестр plugin_sigils, что
// раздаётся Soul-ам.
type SigilLookupAdapter struct {
	lister SigilRecordLister
	logger *slog.Logger
}

// NewSigilLookupAdapter оборачивает lister реестра plugin_sigils в
// shared-совместимый SigilLookup. nil-lister → адаптер всегда возвращает
// nil-запись (verify fail-closed по no_sigil): защита от nil-разыменования при
// неполном wire-up (Sigil выключен). logger может быть nil — тогда ошибки
// чтения проглатываются молча (fail-closed verify и так защитит).
func NewSigilLookupAdapter(lister SigilRecordLister, logger *slog.Logger) *SigilLookupAdapter {
	return &SigilLookupAdapter{lister: lister, logger: logger}
}

// Get резолвит активный допуск по (namespace, name) из реестра plugin_sigils.
// nil (допуска нет / ошибка чтения) → nil (verify трактует как no_sigil,
// fail-closed).
//
// Single-slot per пара (ADR-026(g), Вариант C): на (namespace, name) допущен
// ровно один бинарь, ref — operator-asserted метка внутри записи, в lookup НЕ
// участвует. partial unique index допускает несколько активных записей с
// разными ref на одну пару; при коллизии выбирается новейшая (ListActive
// сортирует allowed_at DESC, id DESC — первая совпавшая = последний allow).
//
// Manifest — byte-exact СЫРЫЕ байты manifest.yaml (call-site проецирует из
// sigil.Sigil.ManifestRaw, НЕ JSONB-проекции); verify прогоняет их через
// NormalizeManifestBytes (S3↔S6-инвариант).
func (a *SigilLookupAdapter) Get(namespace, name string) *sharedhost.SigilRecord {
	if a.lister == nil {
		return nil
	}
	recs, err := a.lister.ListActive(context.Background())
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("pluginhost: sigil lookup failed — verify fail-closed",
				slog.String("namespace", namespace),
				slog.String("name", name),
				slog.Any("error", err),
			)
		}
		return nil
	}
	for _, rec := range recs {
		if rec.Namespace == namespace && rec.Name == name {
			return rec
		}
	}
	return nil
}
