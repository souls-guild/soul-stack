package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// StateSchemaTTL — окно валидности кешированного state-schema-ответа одного
// Service-а. 60s — парный [ScenariosTTL]-выбор: тот же UX-баланс «дёрганий
// remote репо при открытии UI Schema explorer-а» vs. свежести (новая
// миграция, положенная в репо минуту назад, оператор увидит спустя ≤60s).
const StateSchemaTTL = 60 * time.Second

// StateSchemaLister — поверхность listing-а state_schema-метаданных
// (`state_schema_version` + опц. декларация структуры + цепочка миграций) из
// локально-материализованного снапшота Service-репо. Объявлено интерфейсом
// для подмены fake-ом в тестах handler-а; production-реализация — функция
// поверх [artifact.ServiceLoader] + [artifact.ListStateSchema].
//
// Контракт: дёргается под per-(name+ref) lock-ом в [StateSchemaCache]; ref —
// явный, потому что разные версии одного сервиса могут иметь разный
// state_schema_version (UI Schema explorer показывает версию выбранного ref-а).
type StateSchemaLister interface {
	ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)
}

// StateSchemaListerFunc — функциональная реализация [StateSchemaLister]
// (парный [ScenarioListerFunc] для handler-side wire-up без обёрточного
// именованного типа).
type StateSchemaListerFunc func(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error)

// ListStateSchema делает функцию реализующей [StateSchemaLister].
func (f StateSchemaListerFunc) ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
	return f(ctx, name, gitURL, ref)
}

// StateSchemaCache — in-process TTL-кеш ответа
// [StateSchemaLister.ListStateSchema] по ключу `(name, ref)`. Per-Keeper, не
// cluster-wide: state-schema — read-only представление, отставание между
// инстансами не нарушает консистентность реестра (parity с [ScenariosCache]).
//
// Безопасен для конкурентного использования. Per-ключ Mutex сериализует «один
// in-flight loader на ключ» — параллельные клики «Open Schema explorer» не
// лупят git-clone N раз. На уровне самого loader-а [artifact.ServiceLoader]
// тоже несёт per-name lock + переиспользует snapshot-каталог по sha1.
type StateSchemaCache struct {
	lister StateSchemaLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[stateSchemaKey]*stateSchemaEntry
}

// stateSchemaKey — композитный ключ кеша. name+ref хранятся раздельно для
// корректной инвалидации по name (Update/Deregister Service-а сбрасывает все
// ref-варианты под этим name; parity с [scenariosKey]).
type stateSchemaKey struct {
	name string
	ref  string
}

// stateSchemaEntry — одна запись кеша. lock сериализует concurrent loader-
// вызовы одного ключа; info/expires — закешированный ответ.
type stateSchemaEntry struct {
	lock    sync.Mutex
	info    *artifact.StateSchemaInfo
	expires time.Time
}

// NewStateSchemaCache собирает кеш поверх lister-а. lister обязателен (паника
// при nil — симметрично [NewScenariosCache]); ttl ≤ 0 нормализуется в
// [StateSchemaTTL].
func NewStateSchemaCache(lister StateSchemaLister, ttl time.Duration) *StateSchemaCache {
	if lister == nil {
		panic("serviceregistry.NewStateSchemaCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = StateSchemaTTL
	}
	return &StateSchemaCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[stateSchemaKey]*stateSchemaEntry),
	}
}

// ListStateSchema возвращает state-schema info для (name, gitURL, ref). Hit —
// отдаём из кеша; miss или истекший TTL — один loader-call под per-ключ
// lock-ом.
//
// Кешируется ТОЛЬКО success-ответ: при ошибке следующий запрос снова попытается
// loader (best-effort + читаемость failure-ов в UI; parity с
// [ScenariosCache.ListScenarios]).
func (c *StateSchemaCache) ListStateSchema(ctx context.Context, name, gitURL, ref string) (*artifact.StateSchemaInfo, error) {
	entry := c.entryFor(stateSchemaKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.info != nil {
		return cloneStateSchemaInfo(entry.info), nil
	}

	info, err := c.lister.ListStateSchema(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.info = info
	entry.expires = c.now().Add(c.ttl)
	return cloneStateSchemaInfo(info), nil
}

// Invalidate сбрасывает все записи кеша для данного name (все варианты ref).
// Семантика — парная с [ScenariosCache.Invalidate]: после Update/Deregister
// Service-а устаревшие закешированные state-schema должны исчезнуть, чтобы
// следующий запрос вернул listing нового git-источника. Идемпотентен.
func (c *StateSchemaCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) stateSchemaEntry для key.
// Не держит c.mu во время loader-вызова — это работа per-ключ lock-а внутри
// entry.
func (c *StateSchemaCache) entryFor(key stateSchemaKey) *stateSchemaEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &stateSchemaEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneStateSchemaInfo — мелкая копия структуры, чтобы caller не мог изменить
// кешированную запись. Schema/Migrations — ссылочные типы; копируем slice
// миграций (handler сериализует в JSON и за рамки кеша не передаёт);
// Schema-map deep-copy НЕ делаем сознательно (parity с cloneScenarios:
// InputSchema-map тоже не клонируется — UI её только читает).
func cloneStateSchemaInfo(in *artifact.StateSchemaInfo) *artifact.StateSchemaInfo {
	if in == nil {
		return nil
	}
	out := *in
	if in.Migrations != nil {
		out.Migrations = make([]artifact.Migration, len(in.Migrations))
		copy(out.Migrations, in.Migrations)
	}
	return &out
}
