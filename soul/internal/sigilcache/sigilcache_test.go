package sigilcache

import (
	"sync"
	"testing"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func sig(ns, name, ref string) *keeperv1.PluginSigil {
	return &keeperv1.PluginSigil{Namespace: ns, Name: name, Ref: ref}
}

func TestReplaceAllGet(t *testing.T) {
	c := New()

	if got := c.Get("core", "pkg"); got != nil {
		t.Fatalf("Get на пустом кеше: ожидался nil, получено %v", got)
	}

	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "pkg", "v1")})
	got := c.Get("core", "pkg")
	if got == nil {
		t.Fatal("Get после ReplaceAll: ожидался Sigil, получен nil")
	}
	if got.GetRef() != "v1" {
		t.Fatalf("ref: ожидался v1, получен %q", got.GetRef())
	}
}

// TestKeyIsNamespaceAndName — пара (namespace, name) адресует разные слоты:
// одинаковый name в разных namespace не коллидирует.
func TestKeyIsNamespaceAndName(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		sig("community", "pkg", "v9"),
	})

	core := c.Get("core", "pkg")
	if core == nil || core.GetRef() != "v1" {
		t.Fatalf("ожидался core/pkg ref=v1, получено %v", core)
	}
	comm := c.Get("community", "pkg")
	if comm == nil || comm.GetRef() != "v9" {
		t.Fatalf("одинаковый name в разных namespace должен быть разными слотами: %v", comm)
	}
}

// TestSurvivesReconnect — структурная гарантия: кеш не привязан к стриму.
// Один и тот же *Cache переживает множество условных «сессий» (в проде каждая
// сессия — новый StreamSession, кеш создаётся в runDaemon вне reconnect-loop).
func TestSurvivesReconnect(t *testing.T) {
	c := New() // создан один раз на runtime-уровне Soul

	// «Сессия 1» применила snapshot и закрылась.
	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "file", "v1")})

	// «Сессия 2» (после reconnect) видит допуск, применённый в сессии 1 —
	// тот же *Cache не пере-создаётся.
	if got := c.Get("core", "file"); got == nil {
		t.Fatal("кеш потерял допуск после условного reconnect — он не должен пере-создаваться на сессию")
	}
}

// TestReplaceAllAddsAndRevokes — ReplaceAll атомарно добавляет новые допуски и
// забывает отсутствующие (near-instant revoke, ADR-026(h) S6c).
func TestReplaceAllAddsAndRevokes(t *testing.T) {
	c := New()

	// Первый snapshot: два допуска.
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		sig("core", "file", "v1"),
	})
	if c.Get("core", "pkg") == nil || c.Get("core", "file") == nil {
		t.Fatal("ReplaceAll должен был добавить оба допуска")
	}

	// Второй snapshot: core/pkg отозван (отсутствует), добавлен core/service,
	// core/file обновлён до v2.
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "file", "v2"),
		sig("core", "service", "v1"),
	})

	if c.Get("core", "pkg") != nil {
		t.Fatal("revoke: отсутствующий в snapshot допуск должен быть забыт")
	}
	if got := c.Get("core", "file"); got == nil || got.GetRef() != "v2" {
		t.Fatalf("update: core/file ожидался ref=v2, получено %v", got)
	}
	if c.Get("core", "service") == nil {
		t.Fatal("allow: новый допуск должен появиться")
	}
}

// TestReplaceAllEmptyClears — пустой snapshot очищает весь кеш (ни один плагин
// не допущен).
func TestReplaceAllEmptyClears(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{sig("core", "pkg", "v1")})
	if c.Get("core", "pkg") == nil {
		t.Fatal("предусловие: допуск должен быть в кеше")
	}

	c.ReplaceAll(nil)
	if c.Get("core", "pkg") != nil {
		t.Fatal("пустой/nil snapshot должен очистить кеш")
	}
}

// TestReplaceAllSkipsNilElements — nil-элементы внутри snapshot не роняют замену
// и не создают мусорных ключей.
func TestReplaceAllSkipsNilElements(t *testing.T) {
	c := New()
	c.ReplaceAll([]*keeperv1.PluginSigil{
		sig("core", "pkg", "v1"),
		nil,
		sig("core", "file", "v1"),
	})
	if c.Get("core", "pkg") == nil || c.Get("core", "file") == nil {
		t.Fatal("ReplaceAll должен пропустить nil и сохранить валидные допуски")
	}
	if c.Get("", "") != nil {
		t.Fatal("nil-элемент не должен создавать запись по пустому ключу")
	}
}

// TestConcurrentReplaceAllGet — конкурентные ReplaceAll (writer) + Get (readers)
// под -race: замена набора атомарна, читатель видит целостный набор, гонок нет.
func TestConcurrentReplaceAllGet(t *testing.T) {
	c := New()
	const writers = 8
	const readers = 8
	const iters = 500

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				c.ReplaceAll([]*keeperv1.PluginSigil{
					sig("core", "pkg", "v1"),
					sig("core", "file", "v1"),
				})
			}
		}()
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = c.Get("core", "pkg")
				_ = c.Get("core", "file")
			}
		}()
	}
	wg.Wait()
}
