package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// DependenciesTTL — окно валидности кешированного dependencies-ответа одного
// Service-а. 60s — парный [StateSchemaTTL] / [ScenariosTTL]-выбор: тот же
// UX-баланс «дёрганий remote репо при открытии UI Service Detail» vs. свежести
// (изменённый `destiny:`/`modules:`-блок оператор увидит спустя ≤60s).
const DependenciesTTL = 60 * time.Second

// DependenciesLister — поверхность listing-а git-зависимостей (destiny/modules
// из `service.yml`) одного снапшота Service-репо. Объявлено интерфейсом для
// подмены fake-ом в тестах handler-а; production-реализация — функция поверх
// [artifact.ServiceLoader] + [artifact.ListDependencies].
//
// Контракт: дёргается под per-(name+ref) lock-ом в [DependenciesCache]; ref —
// явный, потому что разные версии одного сервиса могут декларировать разные
// зависимости (UI Service Detail показывает зависимости выбранного ref-а).
type DependenciesLister interface {
	ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)
}

// DependenciesListerFunc — функциональная реализация [DependenciesLister]
// (парный [StateSchemaListerFunc] для handler-side wire-up без обёрточного
// именованного типа).
type DependenciesListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error)

// ListDependencies делает функцию реализующей [DependenciesLister].
func (f DependenciesListerFunc) ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
	return f(ctx, name, gitURL, ref)
}

// DependenciesCache — in-process TTL-кеш ответа
// [DependenciesLister.ListDependencies] по ключу `(name, ref)`. Per-Keeper, не
// cluster-wide: dependencies — read-only представление, отставание между
// инстансами не нарушает консистентность реестра (parity с [StateSchemaCache]).
//
// Безопасен для конкурентного использования. Per-ключ Mutex сериализует «один
// in-flight loader на ключ» — параллельные открытия Service Detail не лупят
// git-clone N раз. На уровне самого loader-а [artifact.ServiceLoader] тоже
// несёт per-name lock + переиспользует snapshot-каталог по sha1.
type DependenciesCache struct {
	lister DependenciesLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[dependenciesKey]*dependenciesEntry
}

// dependenciesKey — композитный ключ кеша. name+ref хранятся раздельно для
// корректной инвалидации по name (Update/Deregister Service-а сбрасывает все
// ref-варианты под этим name; parity с [stateSchemaKey]).
type dependenciesKey struct {
	name string
	ref  string
}

// dependenciesEntry — одна запись кеша. lock сериализует concurrent loader-
// вызовы одного ключа; deps/expires — закешированный ответ.
type dependenciesEntry struct {
	lock    sync.Mutex
	deps    *artifact.ServiceDependencies
	expires time.Time
}

// NewDependenciesCache собирает кеш поверх lister-а. lister обязателен (паника
// при nil — симметрично [NewStateSchemaCache]); ttl ≤ 0 нормализуется в
// [DependenciesTTL].
func NewDependenciesCache(lister DependenciesLister, ttl time.Duration) *DependenciesCache {
	if lister == nil {
		panic("serviceregistry.NewDependenciesCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = DependenciesTTL
	}
	return &DependenciesCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[dependenciesKey]*dependenciesEntry),
	}
}

// ListDependencies возвращает dependencies для (name, gitURL, ref). Hit —
// отдаём из кеша; miss или истекший TTL — один loader-call под per-ключ lock-ом.
//
// Кешируется ТОЛЬКО success-ответ: при ошибке следующий запрос снова попытается
// loader (best-effort + читаемость failure-ов в UI; parity с
// [StateSchemaCache.ListStateSchema]).
func (c *DependenciesCache) ListDependencies(ctx context.Context, name, gitURL, ref string) (*artifact.ServiceDependencies, error) {
	entry := c.entryFor(dependenciesKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.deps != nil {
		return cloneDependencies(entry.deps), nil
	}

	deps, err := c.lister.ListDependencies(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.deps = deps
	entry.expires = c.now().Add(c.ttl)
	return cloneDependencies(deps), nil
}

// Invalidate сбрасывает все записи кеша для данного name (все варианты ref).
// Семантика — парная с [StateSchemaCache.Invalidate]: после Update/Deregister
// Service-а устаревшие закешированные dependencies должны исчезнуть, чтобы
// следующий запрос вернул listing нового git-источника. Идемпотентен.
func (c *DependenciesCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) dependenciesEntry для key.
// Не держит c.mu во время loader-вызова — это работа per-ключ lock-а внутри
// entry.
func (c *DependenciesCache) entryFor(key dependenciesKey) *dependenciesEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &dependenciesEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneDependencies — мелкая копия структуры, чтобы caller не мог изменить
// кешированную запись. Destiny/Modules — слайсы значений (Dependency без
// ссылочных полей), копируем оба (handler сериализует в JSON и за рамки кеша не
// передаёт; parity с cloneStateSchemaInfo).
func cloneDependencies(in *artifact.ServiceDependencies) *artifact.ServiceDependencies {
	if in == nil {
		return nil
	}
	out := &artifact.ServiceDependencies{}
	if in.Destiny != nil {
		out.Destiny = make([]artifact.Dependency, len(in.Destiny))
		copy(out.Destiny, in.Destiny)
	}
	if in.Modules != nil {
		out.Modules = make([]artifact.Dependency, len(in.Modules))
		copy(out.Modules, in.Modules)
	}
	return out
}
