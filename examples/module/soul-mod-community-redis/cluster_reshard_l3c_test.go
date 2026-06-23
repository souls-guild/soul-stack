//go:build e2e_live

// L3c-skeleton: e2e-live reshard против НАСТОЯЩЕГО Redis-кластера. Build-tag
// e2e_live (как остальные live-харнессы репо) + t.Skip: каркас компилируется в
// общем гейте, но реально не гоняется без поднятого кластера и явной разблокировки
// (отдельный slice — propose-and-wait по harness-сущности «live redis cluster»).
//
// L0 (cluster_test.go) доказывает ПОСЛЕДОВАТЕЛЬНОСТЬ команд (SETSLOT IMPORTING/
// MIGRATING/MIGRATE/SETSLOT NODE) и лосслесс на fake-conn. L0 НЕ доказывает, что
// слоты реально сменили владельца, ключи (вкл. whitespace+TTL) переехали без
// потерь, и DBSIZE сошёлся — это и есть зона L3c на живом кластере.
//
// ★ reshard ИМПЕРАТИВЕН и НЕ идемпотентен: тест зовёт его ОДИН раз и проверяет
// результат именно этого одного переноса. Повторный прогон сдвинул бы ещё N
// слотов — это by design (exec-style day-2, не converge), не баг.
package main

import (
	"context"
	"testing"
)

// TestL3cReshard_LiveLossless — e2e-live: reshard N слотов from→to на настоящем
// кластере с проверкой лосслесс-переноса и корректной смены владельца слотов.
//
// TODO(L3c-future, нужна harness-сущность «live redis cluster», propose-and-wait):
//   1. Поднять РЕАЛЬНЫЙ Redis-кластер минимум из 2 master-ов (testcontainers /
//      docker-compose redis:7 cluster, --cluster-enabled yes) → endpoint-ы
//      from/to + node-id обоих master-ов.
//   2. Записать в слоты, принадлежащие source master (from), набор ключей,
//      ОБЯЗАТЕЛЬНО включая:
//        - whitespace-имена ("user 42", "a\tb", "c\nd") — лосслесс-инвариант
//          типизированного GetKeysInSlot (см. brief P3/remove-node-фикс);
//        - ключи с TTL (SET k v EX 3600) — проверить, что TTL переехал (MIGRATE
//          переносит TTL вместе со значением), TTL на target > 0 и близок к 3600;
//        - обычные ключи для контраста.
//      Зафиксировать множество ключей source и суммарный DBSIZE до reshard.
//   3. Вызвать m.Apply(state=cluster, action=reshard, from, to, slots=N) ОДИН раз
//      (императивно). slots <= числу слотов source.
//   4. Инварианты ПОСЛЕ:
//        - CLUSTER NODES: перенесённые N слотов теперь принадлежат to (node-id
//          target), у from их больше нет; остальные слоты не тронуты.
//        - cluster_state:ok (CLUSTER INFO), 16384 слота покрыты, нет дыр.
//        - все ключи перенесённых слотов читаются С TARGET (вкл. whitespace+TTL),
//          и НЕ читаются с source → лосслесс, ни одного потерянного ключа.
//        - DBSIZE(from) + DBSIZE(to) == исходная сумма (ничего не пропало и не
//          задвоилось).
//   5. ★ Проверить НЕ-идемпотентность отдельным под-кейсом: повторный reshard
//      тех же from→to slots=N сдвигает ЕЩЁ N слотов (a НЕ no-op) — это
//      зафиксированная семантика, тест её утверждает, а не ловит как регресс.
func TestL3cReshard_LiveLossless(t *testing.T) {
	t.Skip("L3c-skeleton: нужен живой Redis-кластер (harness-сущность «live redis cluster», " +
		"propose-and-wait). L0 cluster_test.go доказывает последовательность команд + лосслесс " +
		"на fake-conn; здесь проверяются реальная смена владельца слотов, перенос whitespace+TTL " +
		"ключей и сходимость DBSIZE на настоящем кластере.")

	// Каркас под будущую разблокировку: контекст + точка вызова Apply. Реальное
	// поднятие кластера/запись ключей/ассерты владельца добавит L3c-future-slice.
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	// from, to := <endpoint source master>, <endpoint target master>
	// keys := []string{"user 42", "a\tb", "c\nd", "plain", "withttl"}
	// ... SET ключей в слоты source (withttl с EX 3600) ...
	// m := &RedisModule{} // реальный connect (defaultConnect к живым нодам)
	// stream := &applyStream{}
	// _ = m.Apply(&pluginv1.ApplyRequest{State: "cluster", Params: ...reshard...}, stream)
	// ... assert владелец слотов / лосслесс ключей / TTL / DBSIZE ...
}
