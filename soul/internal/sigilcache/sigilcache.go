// Package sigilcache — runtime-level in-memory кеш печатей доверия Sigil
// (ADR-026) на Soul-стороне, slice S6a / S6c.
//
// Авторитетный источник active-набора — SigilSnapshot (ADR-026(h), Вариант A):
// Keeper шлёт полный набор допусков по EventStream-у при подключении и на каждый
// cluster-wide invalidate. Soul применяет его как ReplaceAll ([Cache.ReplaceAll]),
// заменяя ВЕСЬ локальный набор. Допуск, отсутствующий в snapshot, забывается —
// так срабатывает near-instant revoke (S6c): после revoke Архонтом доступ
// закрывается без перезапуска Soul-а. Пустой snapshot = ни один плагин не
// допущен. Verify против кеша — S6b (shared/pluginhost).
//
// Ключ — пара (namespace, name), НЕ ref: на пару допущен ровно один активный
// Sigil (single-slot), его ref хранится внутри значения.
//
// Кеш живёт на runtime-уровне Soul (создаётся при старте демона, вне
// reconnect-loop) и НЕ пере-создаётся при переподключении стрима — допуски
// переживают разрыв EventStream-а (следующая сессия первым делом получит свежий
// snapshot и применит ReplaceAll). На диск НЕ персистится: stale-допуск на диске
// был бы дырой в trust-модели.
package sigilcache

import (
	"sync"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// key — составной ключ кеша. namespace+name однозначно адресуют один активный
// Sigil (single-slot per пара, см. doc пакета).
type key struct {
	namespace string
	name      string
}

// Cache — потокобезопасный кеш Sigil-ов. Записи идёт recv-loop (один writer),
// чтения — будущий verify (S6b); RWMutex покрывает оба сценария. Нулевое
// значение не готово к использованию — создавать через New.
type Cache struct {
	mu    sync.RWMutex
	items map[key]*keeperv1.PluginSigil
}

// New создаёт пустой кеш.
func New() *Cache {
	return &Cache{items: make(map[key]*keeperv1.PluginSigil)}
}

// ReplaceAll атомарно заменяет ВЕСЬ набор допусков переданным snapshot-ом
// (ADR-026(h), Вариант A: SigilSnapshot — единственный источник истины,
// применяется как ReplaceAll). Допуск, отсутствующий в snapshot, забывается —
// это и есть near-instant revoke (S6c). Пустой/nil snapshot → пустой кеш (ни
// один плагин не допущен).
//
// nil-элементы внутри snapshot пропускаются (битый payload не должен ронять
// замену). Под единым Lock-ом: читатели verify-фазы (S6b) видят либо старый,
// либо новый набор целиком, без промежуточного состояния.
func (c *Cache) ReplaceAll(snapshot []*keeperv1.PluginSigil) {
	next := make(map[key]*keeperv1.PluginSigil, len(snapshot))
	for _, sig := range snapshot {
		if sig == nil {
			continue
		}
		next[key{namespace: sig.GetNamespace(), name: sig.GetName()}] = sig
	}
	c.mu.Lock()
	c.items = next
	c.mu.Unlock()
}

// Get возвращает активный Sigil для пары (namespace, name) или nil, если допуска
// нет. Возвращается хранимый указатель — вызывающий не должен мутировать
// PluginSigil (proto-сообщение read-only после приёма).
func (c *Cache) Get(namespace, name string) *keeperv1.PluginSigil {
	k := key{namespace: namespace, name: name}
	c.mu.RLock()
	sig := c.items[k]
	c.mu.RUnlock()
	return sig
}
