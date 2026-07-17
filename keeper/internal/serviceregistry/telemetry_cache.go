package serviceregistry

import (
	"context"
	"sync"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// TelemetryTTL — окно валидности кешированного telemetry-конфига одного
// `(name, ref)`. Конфиг immutable на git-ref снапшота, но git-URL сервиса может
// смениться под тем же ref-именем (Update); 60s — парный DirectivesTTL баланс.
const TelemetryTTL = 60 * time.Second

// TelemetryCatalog — снапшот-результат lister-а /telemetry: SHA1 материализованного
// снапшота (служит ETag-ом) + эффективный per-service telemetry-конфиг (манифест-
// дефолты, без essence). Форма результата lister-а /telemetry (parity DirectiveCatalog).
type TelemetryCatalog struct {
	SHA1      string
	Telemetry *keeperv1.TelemetryConfig
}

// TelemetryLister — поверхность чтения дефолтного (per-service, без essence)
// telemetry-конфига сервиса + SHA1 снапшота (для ETag) из материализованного
// снапшота Service-репо для `(name, ref)`. Parity [DirectiveLister]. При nil
// `GET /v1/services/{name}/telemetry` отвечает 500 «not configured».
type TelemetryLister interface {
	ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error)
}

// TelemetryListerFunc — функциональная реализация [TelemetryLister] (parity
// DirectiveListerFunc для wire-up без именованного типа).
type TelemetryListerFunc func(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error)

// ListServiceTelemetry делает функцию реализующей [TelemetryLister].
func (f TelemetryListerFunc) ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error) {
	return f(ctx, name, gitURL, ref)
}

// TelemetryCache — in-process TTL-кеш ответа [TelemetryLister.ListServiceTelemetry]
// по ключу `(name, ref)`. Per-Keeper, не cluster-wide (read-only каталог). Безопасен
// для конкурентного использования; per-ключ Mutex сериализует «один in-flight loader
// на ключ» (parity DirectivesCache).
type TelemetryCache struct {
	lister TelemetryLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[telemetryKey]*telemetryEntry
}

// telemetryKey — композитный ключ кеша (name+ref раздельно для инвалидации по name).
type telemetryKey struct {
	name string
	ref  string
}

// telemetryEntry — одна запись кеша: lock сериализует concurrent loader одного
// ключа; catalog/expires — закешированный ответ.
type telemetryEntry struct {
	lock    sync.Mutex
	catalog *TelemetryCatalog
	expires time.Time
}

// NewTelemetryCache собирает кеш поверх lister-а. lister обязателен (паника при
// nil — симметрично NewDirectivesCache); ttl ≤ 0 нормализуется в [TelemetryTTL].
func NewTelemetryCache(lister TelemetryLister, ttl time.Duration) *TelemetryCache {
	if lister == nil {
		panic("serviceregistry.NewTelemetryCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = TelemetryTTL
	}
	return &TelemetryCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[telemetryKey]*telemetryEntry),
	}
}

// ListServiceTelemetry возвращает telemetry-конфиг для (name, gitURL, ref). Hit — из
// кеша; miss/истёкший TTL — один loader-call под per-ключ lock-ом. Кешируется только
// success (ошибка не кешируется; parity DirectivesCache).
func (c *TelemetryCache) ListServiceTelemetry(ctx context.Context, name, gitURL, ref string) (*TelemetryCatalog, error) {
	entry := c.entryFor(telemetryKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.catalog != nil {
		return cloneTelemetryCatalog(entry.catalog), nil
	}

	catalog, err := c.lister.ListServiceTelemetry(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.catalog = catalog
	entry.expires = c.now().Add(c.ttl)
	return cloneTelemetryCatalog(catalog), nil
}

// Invalidate сбрасывает все записи кеша для name (все варианты ref). Парная
// семантика с DirectivesCache.Invalidate: после Update/Deregister Service-а
// устаревший конфиг исчезает, следующий запрос вернёт конфиг нового git-источника.
func (c *TelemetryCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) telemetryEntry для key.
func (c *TelemetryCache) entryFor(key telemetryKey) *telemetryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &telemetryEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneTelemetryCatalog — глубокая копия каталога (новый proto-message + копия среза
// Collectors): защищает кеш от мутации caller-ом. Скалярные поля — по значению.
func cloneTelemetryCatalog(in *TelemetryCatalog) *TelemetryCatalog {
	if in == nil {
		return nil
	}
	out := &TelemetryCatalog{SHA1: in.SHA1}
	if in.Telemetry != nil {
		out.Telemetry = &keeperv1.TelemetryConfig{
			Enabled:     in.Telemetry.Enabled,
			IntervalSec: in.Telemetry.IntervalSec,
			Collectors:  append([]string(nil), in.Telemetry.Collectors...),
		}
	}
	return out
}
