package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// DirectivesTTL — окно валидности кешированного каталога директив одного
// `(name, ref)`. Каталог immutable на git-ref снапшота, но git-URL сервиса
// может смениться под тем же ref-именем (Update); 60s — парный ScenariosTTL/
// RefsTTL баланс «свежесть vs. дёрганье remote».
const DirectivesTTL = 60 * time.Second

// DirectiveLister — поверхность чтения ПОЛНОГО каталога директив (все серии) из
// материализованного снапшота Service-репо + SHA1 снапшота (для ETag). Version-
// сужение делает handler над результатом (artifact.FilterDirectivesByVersion),
// поэтому кеш version-agnostic (ключ (name,ref), как у sibling-каталогов).
type DirectiveLister interface {
	ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)
}

// DirectiveListerFunc — функциональная реализация [DirectiveLister] (парный
// ScenarioListerFunc для wire-up без именованного типа).
type DirectiveListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error)

// ListDirectives делает функцию реализующей [DirectiveLister].
func (f DirectiveListerFunc) ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error) {
	return f(ctx, name, gitURL, ref)
}

// DirectivesCache — in-process TTL-кеш ответа [DirectiveLister.ListDirectives]
// по ключу `(name, ref)`. Per-Keeper, не cluster-wide (read-only каталог,
// отставание между инстансами не нарушает консистентность реестра). Безопасен
// для конкурентного использования; per-ключ Mutex сериализует «один in-flight
// loader на ключ» (parity ScenariosCache).
type DirectivesCache struct {
	lister DirectiveLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[directivesKey]*directivesEntry
}

// directivesKey — композитный ключ кеша (name+ref раздельно для инвалидации по name).
type directivesKey struct {
	name string
	ref  string
}

// directivesEntry — одна запись кеша: lock сериализует concurrent loader одного
// ключа; catalog/expires — закешированный ответ.
type directivesEntry struct {
	lock    sync.Mutex
	catalog *artifact.DirectiveCatalog
	expires time.Time
}

// NewDirectivesCache собирает кеш поверх lister-а. lister обязателен (паника при
// nil — симметрично NewScenariosCache); ttl ≤ 0 нормализуется в [DirectivesTTL].
func NewDirectivesCache(lister DirectiveLister, ttl time.Duration) *DirectivesCache {
	if lister == nil {
		panic("serviceregistry.NewDirectivesCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = DirectivesTTL
	}
	return &DirectivesCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[directivesKey]*directivesEntry),
	}
}

// ListDirectives возвращает полный каталог для (name, gitURL, ref). Hit — из
// кеша; miss/истёкший TTL — один loader-call под per-ключ lock-ом. Кешируется
// только success (ошибка не кешируется — следующий запрос ретраит; parity
// ScenariosCache).
func (c *DirectivesCache) ListDirectives(ctx context.Context, name, gitURL, ref string) (*artifact.DirectiveCatalog, error) {
	entry := c.entryFor(directivesKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.catalog != nil {
		return cloneDirectiveCatalog(entry.catalog), nil
	}

	catalog, err := c.lister.ListDirectives(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.catalog = catalog
	entry.expires = c.now().Add(c.ttl)
	return cloneDirectiveCatalog(catalog), nil
}

// Invalidate сбрасывает все записи кеша для name (все варианты ref). Парная
// семантика с ScenariosCache.Invalidate: после Update/Deregister Service-а
// устаревший каталог исчезает, следующий запрос вернёт каталог нового git-источника.
func (c *DirectivesCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) directivesEntry для key.
func (c *DirectivesCache) entryFor(key directivesKey) *directivesEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &directivesEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneDirectiveCatalog — мелкая копия каталога (новый внешний map, срезы имён
// шарятся): срезы read-only после загрузки (отсортированы, не мутируются), потому
// shared безопасен; копия внешнего map защищает от мутации кеша caller-ом.
func cloneDirectiveCatalog(in *artifact.DirectiveCatalog) *artifact.DirectiveCatalog {
	if in == nil {
		return nil
	}
	out := &artifact.DirectiveCatalog{SHA1: in.SHA1}
	if in.Directives != nil {
		m := make(map[string][]string, len(in.Directives))
		for k, v := range in.Directives {
			m[k] = v
		}
		out.Directives = m
	}
	return out
}
