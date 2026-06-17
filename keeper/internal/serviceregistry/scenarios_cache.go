package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// ScenariosTTL — окно валидности кешированного scenario-listing-а одного
// Service-а. 60s — парный [RefsTTL]-выбор: тот же UX-баланс «дёрганий remote
// репо при открытии Run-modal в UI» vs. свежести listing-а (новый scenario,
// положенный в репо минуту назад, оператор увидит спустя ≤60s).
const ScenariosTTL = 60 * time.Second

// ScenarioLister — поверхность listing-а scenario из локально-материализованного
// снапшота Service-репо (`scenario/*/main.yml`). Объявлено интерфейсом, чтобы
// тесты могли подменить реальный loader fake-ом, а handler — принимать
// минимальную зависимость.
//
// Контракт: дёргается под per-(name+ref) lock-ом в [ScenariosCache]; реализация
// в production — [ScenariosLister.ListScenarios] поверх [artifact.ServiceLoader].
type ScenarioLister interface {
	ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)
}

// ScenarioListerFunc — функциональная реализация [ScenarioLister] (парный
// [artifact.RefsListerFunc]-у для handler-side wire-up без оборачивания в
// именованный тип).
type ScenarioListerFunc func(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error)

// ListScenarios делает функцию реализующей [ScenarioLister].
func (f ScenarioListerFunc) ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
	return f(ctx, name, gitURL, ref)
}

// ScenariosCache — in-process TTL-кеш ответа [ScenarioLister.ListScenarios] по
// ключу `(name, ref)` (не по `(name)`-only: один сервис может запрашиваться с
// разными ref-ами — UI dropdown показывает scenarios конкретной версии).
//
// Per-Keeper, не cluster-wide: scenario — read-only представление, отставание
// между инстансами не нарушает консистентность реестра. Cluster-wide Redis-кеш
// — отдельный slice по запросу (парный с [RefsCache]-обоснованием).
//
// Безопасен для конкурентного использования. Per-ключ Mutex сериализует
// «один in-flight loader на ключ» — параллельные клики «Run scenario» для
// одного (name,ref) не лупят git-clone N раз. На уровне самого loader-а
// `artifact.ServiceLoader` тоже несёт per-name lock + переиспользует
// snapshot-каталог по sha1 — но это другой уровень: handler-сторона должна
// иметь свой short-circuit, чтобы не вызывать loader N раз подряд для одного
// и того же ключа.
type ScenariosCache struct {
	lister ScenarioLister
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[scenariosKey]*scenariosEntry
}

// scenariosKey — композитный ключ кеша. name+ref хранятся раздельно для
// корректной инвалидации по name (Update/Deregister Service-а сбрасывает все
// ref-варианты под этим name).
type scenariosKey struct {
	name string
	ref  string
}

// scenariosEntry — одна запись кеша. lock сериализует concurrent loader-вызовы
// одного ключа; scenarios/expires — закешированный ответ.
type scenariosEntry struct {
	lock      sync.Mutex
	scenarios []artifact.Scenario
	expires   time.Time
}

// NewScenariosCache собирает кеш поверх lister-а. lister обязателен (паника
// при nil — симметрично [NewRefsCache]); ttl ≤ 0 нормализуется в [ScenariosTTL].
func NewScenariosCache(lister ScenarioLister, ttl time.Duration) *ScenariosCache {
	if lister == nil {
		panic("serviceregistry.NewScenariosCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = ScenariosTTL
	}
	return &ScenariosCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[scenariosKey]*scenariosEntry),
	}
}

// ListScenarios возвращает scenarios для (name, gitURL, ref). Hit — отдаём из
// кеша; miss или истекший TTL — один loader-call под per-ключ lock-ом.
//
// Возврат: либо успешный []Scenario (может быть пустым — сервис без сценариев
// валиден), либо ошибка lister-а «как есть» — caller (handler) маппит её в 502
// Bad Gateway. Кешируется ТОЛЬКО success-ответ: при ошибке следующий запрос
// снова попытается loader (best-effort + читаемость failure-ов в UI; парная
// семантика с [RefsCache.ListRefs]).
func (c *ScenariosCache) ListScenarios(ctx context.Context, name, gitURL, ref string) ([]artifact.Scenario, error) {
	entry := c.entryFor(scenariosKey{name: name, ref: ref})

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.scenarios != nil {
		return cloneScenarios(entry.scenarios), nil
	}

	scenarios, err := c.lister.ListScenarios(ctx, name, gitURL, ref)
	if err != nil {
		return nil, err
	}
	entry.scenarios = scenarios
	entry.expires = c.now().Add(c.ttl)
	return cloneScenarios(scenarios), nil
}

// Invalidate сбрасывает все записи кеша для данного name (все варианты ref).
// Семантика — парная с [RefsCache.Invalidate]: после Update/Deregister Service-а
// устаревшие закешированные scenarios должны исчезнуть, чтобы следующий запрос
// вернул listing нового git-источника. Идемпотентен.
func (c *ScenariosCache) Invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k.name == name {
			delete(c.entries, k)
		}
	}
}

// entryFor возвращает (создавая при необходимости) scenariosEntry для key.
// Не держит c.mu во время loader-вызова — это работа per-ключ lock-а внутри
// entry.
func (c *ScenariosCache) entryFor(key scenariosKey) *scenariosEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		e = &scenariosEntry{}
		c.entries[key] = e
	}
	return e
}

// cloneScenarios — мелкая копия slice-а, чтобы caller не мог изменить
// кешированный массив. Scenario.InputSchema — map (ссылочный тип); deep-copy
// его не делаем сознательно: handler сразу сериализует в JSON, а caller-у вне
// handler-а кеш не передаётся.
func cloneScenarios(in []artifact.Scenario) []artifact.Scenario {
	if in == nil {
		return nil
	}
	out := make([]artifact.Scenario, len(in))
	copy(out, in)
	return out
}
