package serviceregistry

import (
	"context"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// RefsTTL — окно валидности кешированного git-ls-remote ответа. Совпадает с
// рекомендацией ТЗ: 60s — баланс между «дёрганиями» remote-репо при открытии
// Upgrade-modal в UI и приемлемой свежестью списка ref-ов (тег, появившийся
// минуту назад, оператор увидит спустя ≤60s после рефреша страницы).
const RefsTTL = 60 * time.Second

// RefsCache — in-process TTL-кеш ответа [artifact.ListRefs] по имени Service-а
// (не по git-URL: имя устойчивее — переименование git-источника в реестре не
// должно унаследовать ref-ы старого репо).
//
// Per-Keeper, не cluster-wide: refs — read-only представление, отставание между
// инстансами не нарушает консистентность реестра. Если в кластере один из
// keeper-ов уже подтянул refs, остальные сделают свой ls-remote при первом
// запросе на их адрес — это пренебрежимый трафик к git-источнику (UI dropdown
// открывают редко). Cluster-wide Redis-кеш — отдельный slice по запросу.
//
// Безопасен для конкурентного использования. Per-name Mutex сериализует
// «один in-flight ls-remote на сервис» — параллельные клики «открыть Upgrade»
// для одного сервиса не лупят одинаковый ls-remote N раз.
type RefsCache struct {
	lister artifact.RefsLister
	ttl    time.Duration
	now    func() time.Time // для тестов

	mu      sync.Mutex
	entries map[string]*refsEntry
}

// refsEntry — одна запись кеша. lock сериализует concurrent ls-remote одного
// и того же сервиса; refs/expires — закешированный ответ (зашитый под lock-ом
// при write-е, читается атомарно после Lock/Unlock).
type refsEntry struct {
	lock    sync.Mutex
	refs    []artifact.GitRef
	expires time.Time
}

// NewRefsCache собирает кеш поверх lister-а. lister обязателен (паника при nil
// — единственная точка misconfiguration); ttl ≤0 нормализуется в [RefsTTL].
func NewRefsCache(lister artifact.RefsLister, ttl time.Duration) *RefsCache {
	if lister == nil {
		panic("serviceregistry.NewRefsCache: lister is nil")
	}
	if ttl <= 0 {
		ttl = RefsTTL
	}
	return &RefsCache{
		lister:  lister,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[string]*refsEntry),
	}
}

// ListRefs возвращает refs для (name, gitURL). Если в кеше валидная запись —
// сразу её. Иначе — один ls-remote через lister под per-name lock-ом (остальные
// горутины для того же name ждут результата).
//
// name — ключ кеша; gitURL — параметр запроса к lister-у (handler читает оба из
// записи реестра, мы их не валидируем повторно — это работа service-слоя).
//
// Возврат: либо успешный []GitRef (может быть пустым), либо ошибка lister-а
// «как есть» — caller (handler) маппит её в 502 Bad Gateway. Кешируется ТОЛЬКО
// success-ответ: при ошибке следующий запрос снова попытается ls-remote
// (best-effort + читаемость failure-ов в UI).
func (c *RefsCache) ListRefs(ctx context.Context, name, gitURL string) ([]artifact.GitRef, error) {
	entry := c.entryFor(name)

	entry.lock.Lock()
	defer entry.lock.Unlock()

	if c.now().Before(entry.expires) && entry.refs != nil {
		return cloneRefs(entry.refs), nil
	}

	refs, err := c.lister.ListRefs(ctx, gitURL)
	if err != nil {
		return nil, err
	}
	entry.refs = refs
	entry.expires = c.now().Add(c.ttl)
	return cloneRefs(refs), nil
}

// Invalidate сбрасывает кеш для name (после Update/Deregister реестра имеет
// смысл выкинуть устаревшую запись, чтобы следующий запрос вернул refs нового
// git-источника). Идемпотентен: нет записи — no-op.
func (c *RefsCache) Invalidate(name string) {
	c.mu.Lock()
	delete(c.entries, name)
	c.mu.Unlock()
}

// entryFor возвращает (создавая при необходимости) refsEntry для name. Не
// держит c.mu во время ls-remote — это работа per-name lock-а внутри entry.
func (c *RefsCache) entryFor(name string) *refsEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok {
		e = &refsEntry{}
		c.entries[name] = e
	}
	return e
}

// cloneRefs делает мелкую копию slice-а, чтобы caller не мог изменить
// кешированный массив (GitRef — value-type, deep-copy не требуется).
func cloneRefs(in []artifact.GitRef) []artifact.GitRef {
	if in == nil {
		return nil
	}
	out := make([]artifact.GitRef, len(in))
	copy(out, in)
	return out
}
