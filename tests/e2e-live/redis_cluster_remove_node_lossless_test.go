//go:build e2e_live

// L3c live-verification: remove-node слот-миграция ЛОССЛЕСС на РЕАЛЬНОМ Redis
// Cluster (НЕ L0-fake). Закрывает trust-gap MAJOR-фикса 2026-06-22:
// community.redis remove-node переносил ключи слота через CLUSTER GETKEYSINSLOT →
// стрингификация (join пробелом) → strings.Fields. Ключ Redis — произвольная
// байт-строка и может содержать пробел/\t/\n; "user 42" рвался на два токена →
// MIGRATE по несуществующим ключам → ключ НЕ переносился, а SETSLOT NODE всё
// равно отдавал слот → ПОТЕРЯ ДАННЫХ. Фикс — типизированный
// redisConn.GetKeysInSlot ([]string) поверх go-redis ClusterGetKeysInSlot.
//
// L0-fake (cluster_test.go::TestApplyClusterRemoveNode_WhitespaceKeysLossless)
// доказывает ТОЛЬКО последовательность команд: что "user 42" уходит в MIGRATE
// одним KEYS-аргументом. Он НЕ доказывает лосслесс РЕАЛЬНЫХ данных — что после
// remove-node ключ физически доступен на новом владельце и DBSIZE сходится. Это
// — задача L3c (живой Redis Cluster, independent verify через redis-cli).
//
// ★ ИНВАРИАНТ (что обязан проверить разблокированный тест):
//  1. Поднять РЕАЛЬНЫЙ Redis Cluster через community.redis scenario `create`
//     (examples/service/redis, redis_type=cluster) на soul-контейнерах —
//     ≥3 master со слотами + ≥1 удаляемый master со слотами.
//  2. Записать N ключей в слоты УДАЛЯЕМОГО master-а, ОБЯЗАТЕЛЬНО включая:
//     - ключ с ПРОБЕЛОМ в имени ("user 42") — ровно дефектный кейс;
//     - ключ с TTL (PSETEX) — MIGRATE обязан перенести и оставшийся TTL;
//     - обычный ключ для контраста.
//     Зафиксировать DBSIZE по всему кластеру (сумма по master-ам) ДО.
//  3. Выполнить scenario `remove_node` (remove_node_sid = удаляемый master,
//     seed_sid = любой оставшийся). WaitApplySuccess.
//  4. ★ ASSERT лосслесс (independent redis-cli, НЕ через плагин):
//     - КАЖДЫЙ записанный ключ доступен (GET / EXISTS) на НОВОМ владельце слота
//       (redis-cli -c автоматически следует MOVED), включая "user 42";
//     - ключ с TTL сохранил TTL (> 0, в разумном окне);
//     - суммарный DBSIZE кластера ПОСЛЕ == ДО (ни одного потерянного ключа);
//     - удаляемый master отсутствует в CLUSTER NODES (FORGET сошёлся).
//
// Pre-requisites (почему пока t.Skip):
//   - examples/service/redis scenario `create` для redis_type=cluster в L3b-live
//     ещё не доказан end-to-end (см. redis_cluster_create_test.go::t.Skip —
//     host-вариативный flow-control в destiny блокирует cluster-create live, а
//     community.redis cluster-bootstrap поверх soul-контейнеров harness-ом ещё не
//     обвязан: нет helper-а поднять cluster-mode redis на N контейнерах + дождаться
//     cluster_state:ok через плагин).
//   - harness не несёт helper-а записи ключей с whitespace/TTL в конкретный слот
//     и кластер-aware DBSIZE-агрегатора (redis-cli -c GET по MOVED).
//
// L3c НЕ закрывает этот harness-инфра-пробел — это каркас с t.Skip и понятной
// диагностикой; реальный прогон — отдельный slice, когда (a) cluster-create live
// разблокирован (per-роль scenario-steps ЛИБО per-host destiny-dispatch) и
// (b) harness обзаведётся cluster-aware write/verify helper-ами.
package e2e_live_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// remRedisPassword — requirepass redis для remove-node lossless-прогона
// (засевается в Vault scoped vault:-ref, как redis_cluster_create_test.go).
const remRedisPassword = "remove-node-redis-secret-32b"

// losslessKeys — ключи, записываемые в слоты удаляемого master-а ДО remove-node.
// "user 42" — РОВНО дефектный кейс (пробел в имени). Каждый обязан пережить
// миграцию слота лосслесс.
var losslessKeys = []string{
	"user 42",  // ★ пробел в имени — дефектный кейс key-loss
	"a\tb",     // таб
	"plain-key", // обычный — контраст
}

func TestL3cRedisClusterRemoveNode_SlotMigrationLossless(t *testing.T) {
	t.Skip("L3c заблокирован (harness-инфра): community.redis cluster-create live ещё не доказан end-to-end (см. redis_cluster_create_test.go::t.Skip — host-вариативный flow-control в cluster-create destiny) + нет harness-helper-ов записи ключей с whitespace/TTL в конкретный слот и кластер-aware DBSIZE-сверки. Разблокировать вместе с cluster-create live (per-роль scenario-steps ЛИБО per-host destiny-dispatch) + cluster-aware write/verify helper-ы в harness. L0-fake (TestApplyClusterRemoveNode_WhitespaceKeysLossless) доказывает порядок команд; этот тест — лосслесс реальных данных.")

	// ── Каркас остаётся для будущей разблокировки ──────────────────────────────
	// Когда cluster-create станет применим live и harness обзаведётся
	// cluster-aware helper-ами, тело ниже станет boilerplate.
	const (
		incName    = "redis-remove-node-lossless"
		rcService  = "redis"
		rcExample  = "examples/service/redis"
		removeSID  = "soul-live-c.example.com" // удаляемый master со слотами
		seedSID    = "soul-live-a.example.com" // оставшийся master (контакт seed)
	)

	stack := harness.NewStack(t, harness.Config{
		ExamplePath: rcExample,
		ServiceName: rcService,
		Souls:       3,
	})
	defer stack.Cleanup()

	for i := range stack.SoulContainers {
		stack.AddSoulToCoven(t, i, incName)
		stack.WaitSoulprintReported(t, i, 60)
	}
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	harness.SeedVaultKV(t, stack, "redis/"+incName, map[string]any{"password": remRedisPassword})

	// (1) bootstrap cluster-mode redis через scenario create (redis_type=cluster).
	//     TODO(L3c-future): нужен helper, гарантирующий cluster_state:ok через
	//     community.redis (плагинный cluster-bootstrap, не redis-cli --cluster create).
	createID := stack.RunScenario(t, incName, "create", map[string]any{
		"redis_type":     "cluster",
		"redis_password": "vault:secret/redis/" + incName + "#password",
	})
	stack.WaitApplySuccess(t, createID, 600)
	stack.WaitIncarnationReady(t, incName, 60)

	// (2) записать ключи (вкл. "user 42" + TTL-ключ) в слоты УДАЛЯЕМОГО master-а и
	//     зафиксировать суммарный DBSIZE кластера ДО.
	//     TODO(L3c-future): cluster-aware write-helper — redis-cli -c -a <pw> SET,
	//     PSETEX для TTL-ключа; адресовать слоты именно removeSID.
	for _, k := range losslessKeys {
		writeClusterKey(t, stack, seedSID, k, "v-"+k)
	}
	const ttlKey = "session ttl-key" // пробел + TTL одновременно
	writeClusterKeyWithTTL(t, stack, seedSID, ttlKey, "v-ttl", 600_000)
	dbsizeBefore := clusterDBSize(t, stack)

	// (3) remove_node удаляемого master-а.
	removeID := stack.RunScenario(t, incName, "remove_node", map[string]any{
		"remove_node_sid": removeSID,
		"seed_sid":        seedSID,
	})
	stack.WaitApplySuccess(t, removeID, 300)
	stack.WaitIncarnationReady(t, incName, 60)

	// (4) ★ лосслесс independent verify (redis-cli -c, следует MOVED):
	//     каждый ключ доступен на новом владельце; TTL-ключ сохранил TTL; DBSIZE
	//     сошёлся; удаляемый master forgotten.
	for _, k := range append(append([]string(nil), losslessKeys...), ttlKey) {
		if !clusterKeyExists(t, stack, seedSID, k) {
			t.Fatalf("★ ключ %q ПОТЕРЯН после remove-node (slot-migration не лосслесс)", k)
		}
	}
	assertClusterKeyTTLPositive(t, stack, seedSID, ttlKey)
	if after := clusterDBSize(t, stack); after != dbsizeBefore {
		t.Fatalf("★ DBSIZE не сошёлся: до=%d после=%d (потеря/дублирование ключей)", dbsizeBefore, after)
	}
	assertNodeForgotten(t, stack, seedSID, removeSID)
}

// --- TODO(L3c-future) harness cluster-aware helper-ы (заглушки каркаса) -------
// Реальные реализации появятся при разблокировке: redis-cli -c -a <pw> внутри
// soul-контейнера seedSID (-c → follow MOVED по всему кластеру).

func writeClusterKey(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key, _ /*val*/ string) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware SET helper для ключа %q (redis-cli -c -a <pw> SET)", key)
}

func writeClusterKeyWithTTL(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key, _ /*val*/ string, _ /*ttlMs*/ int) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware PSETEX helper для TTL-ключа %q", key)
}

func clusterDBSize(t *testing.T, _ *harness.Stack) int {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware DBSIZE-агрегатор (сумма DBSIZE по master-ам)")
	return 0
}

func clusterKeyExists(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key string) bool {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware EXISTS helper для ключа %q (redis-cli -c)", key)
	return false
}

func assertClusterKeyTTLPositive(t *testing.T, _ *harness.Stack, _ /*seedSID*/, key string) {
	t.Helper()
	t.Fatalf("TODO(L3c-future): cluster-aware PTTL>0 verify для ключа %q", key)
}

// assertNodeForgotten — удаляемый SID отсутствует в CLUSTER NODES seed-а (FORGET
// сошёлся). Реализуемо уже сейчас (redis-cli cluster nodes), но завязано на живой
// cluster из шага (1) — остаётся каркасом до разблокировки create.
func assertNodeForgotten(t *testing.T, stack *harness.Stack, seedSID, removeSID string) {
	t.Helper()
	idx := -1
	for i, sc := range stack.SoulContainers {
		if sc.SID == seedSID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("seed SID %q не найден среди soul-контейнеров", seedSID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, code, err := stack.SoulContainers[idx].Exec(ctx,
		[]string{"redis-cli", "-a", remRedisPassword, "cluster", "nodes"})
	if err != nil || code != 0 {
		t.Fatalf("cluster nodes на seed: code=%d err=%v out=%s", code, err, out)
	}
	// removeSID → ip:port. TODO(L3c-future): резолв SID→ip из soulprint; пока
	// проверяем, что удаляемый адрес физически исчез из топологии.
	if strings.Contains(out, removeSID) {
		t.Fatalf("★ удаляемый узел %q всё ещё в CLUSTER NODES (FORGET не сошёлся):\n%s", removeSID, out)
	}
}
