package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// clusterConn — fake redisConn для cluster-тестов: пишет каждый вызов и отвечает
// на CLUSTER-подкоманды по args[1] (MYID/INFO/NODES). На MEET/ADDSLOTS/REPLICATE
// отдаёт "OK". id уникален на ноду — REPLICATE-asserts проверяют правильный мастер.
type clusterConn struct {
	cfg   connConfig
	id    string // ответ на CLUSTER MYID
	info  string // ответ на CLUSTER INFO (пусто → форма «не сформирован»)
	nodes string // ответ на CLUSTER NODES (пусто → ровно nodesCount строк)
	// nodesSeq — последовательные ответы на CLUSTER NODES (add-node: до/после MEET
	// топология разная). i-й вызов отдаёт nodesSeq[i]; за пределами — последний
	// элемент (или nodes, если seq пуст). nodesCalls — счётчик вызовов NODES.
	nodesSeq   []string
	nodesCalls int
	calls      [][]any
	closed     bool

	// keysInSlot — ключи слота для CLUSTER GETKEYSINSLOT (remove-node миграция).
	// slot -> single-batch: первый GetKeysInSlot отдаёт весь срез разом, затем пусто.
	keysInSlot map[int][]string
	// keysInSlotBatches — multi-batch модель: slot -> ОЧЕРЕДЬ порций. Каждый
	// GetKeysInSlot снимает с головы следующую порцию (имитируя, что предыдущий
	// MIGRATE удалил её ключи с источника), пустая очередь → nil (цикл завершается).
	// Покрывает слот с >migrateBatch ключей (несколько итераций цикла migrateOneSlot).
	// keysInSlot и keysInSlotBatches на ОДНОМ слоте одновременно не задаются.
	keysInSlotBatches map[int][][]string

	// infoRepl — ответ на INFO replication (failover-takeover sync-gate читает
	// master_link_status/role). Пусто → "" (parseInfoSection вернёт пустой map →
	// role/master_link_status отсутствуют, sync-gate трактует как нештатный INFO).
	infoRepl string

	// forgetErr — ошибка на CLUSTER FORGET (forget-external идемпотентность: нода
	// уже забыла старого → "Unknown node", глотается). nil → FORGET успешен ("OK").
	forgetErr error
}

func (c *clusterConn) Do(_ context.Context, args ...any) (string, error) {
	c.calls = append(c.calls, args)
	if len(args) >= 2 {
		v0, _ := args[0].(string)
		v1, _ := args[1].(string)
		if strings.EqualFold(v0, "CLUSTER") {
			switch strings.ToUpper(v1) {
			case "MYID":
				return c.id, nil
			case "INFO":
				return c.info, nil
			case "NODES":
				return c.nodesResponse(), nil
			case "FORGET":
				if c.forgetErr != nil {
					return "", c.forgetErr
				}
				return "OK", nil
			}
		}
		// INFO replication — sync-gate failover-takeover (master_link_status/role).
		if strings.EqualFold(v0, "INFO") && strings.EqualFold(v1, "replication") {
			return c.infoRepl, nil
		}
	}
	return "OK", nil
}

// GetKeysInSlot моделирует CLUSTER GETKEYSINSLOT go-redis: миграционный цикл
// migrateOneSlot крутится, пока срез непуст. Ключи возвращаются БЕЗ потери
// разделителей (имя с пробелом остаётся одним элементом) — это проверяет
// whitespace-key lossless тест. Две формы источника на слот (взаимоисключающие):
//
//   - keysInSlot[slot]        — single-batch: весь срез разом, затем nil;
//   - keysInSlotBatches[slot] — multi-batch: ОЧЕРЕДЬ порций, по одной за вызов
//     (имитация того, что предыдущий MIGRATE удалил порцию с источника), затем nil.
//
// Multi-batch покрывает слот с >migrateBatch ключей: несколько итераций цикла,
// где каждая порция — отдельный MIGRATE. Объединение всех порций обязано переехать.
func (c *clusterConn) GetKeysInSlot(_ context.Context, slot, _ int) ([]string, error) {
	if q := c.keysInSlotBatches[slot]; len(q) > 0 {
		batch := q[0]
		c.keysInSlotBatches[slot] = q[1:]
		return batch, nil
	}
	if c.keysInSlot == nil {
		return nil, nil
	}
	batch := c.keysInSlot[slot]
	if len(batch) == 0 {
		return nil, nil
	}
	// Отдаём весь оставшийся батч разом (single-batch тест держит ≤ migrateBatch
	// ключей) и опустошаем — следующий вызов вернёт nil → цикл завершается.
	c.keysInSlot[slot] = nil
	return batch, nil
}

// nodesResponse отдаёт текущий ответ на CLUSTER NODES с учётом nodesSeq.
func (c *clusterConn) nodesResponse() string {
	idx := c.nodesCalls
	c.nodesCalls++
	if len(c.nodesSeq) == 0 {
		return c.nodes
	}
	if idx >= len(c.nodesSeq) {
		return c.nodesSeq[len(c.nodesSeq)-1]
	}
	return c.nodesSeq[idx]
}

// ConfigGet — cluster-state не вызывает CONFIG GET, стаб под интерфейс redisConn.
func (c *clusterConn) ConfigGet(_ context.Context, param string) (map[string]string, error) {
	return map[string]string{param: ""}, nil
}

// AclList — cluster-state ACL не трогает, стаб под интерфейс redisConn.
func (c *clusterConn) AclList(_ context.Context) ([]string, error) { return nil, nil }

func (c *clusterConn) Close() error { c.closed = true; return nil }

// clusterFleet — набор fake-нод, раздаваемых по addr. registry фиксирует, к какой
// ноде ушёл каждый коннект (для per-node assert ADDSLOTS/REPLICATE).
type clusterFleet struct {
	byAddr map[string]*clusterConn
}

func newFleet(addrs ...string) *clusterFleet {
	fl := &clusterFleet{byAddr: make(map[string]*clusterConn, len(addrs))}
	for i, a := range addrs {
		fl.byAddr[a] = &clusterConn{id: fmt.Sprintf("nodeid-%d", i)}
	}
	return fl
}

// nodesView — общий для всех нод вывод CLUSTER NODES (gossip сошёлся): по строке
// на ноду. Строка несёт РЕАЛЬНЫЙ node-id ноды (тот, что отдаёт её CLUSTER MYID) —
// это нужно gossip-gate перед REPLICATE (узел-реплика обязан увидеть node-id
// своего мастера в локальном CLUSTER NODES). countClusterNodes считает строки.
func (fl *clusterFleet) setConvergedNodesView() {
	lines := make([]string, 0, len(fl.byAddr))
	for addr, c := range fl.byAddr {
		lines = append(lines, c.id+" "+addr+"@16379 master - 0 0 0 connected")
	}
	view := strings.Join(lines, "\n")
	for _, c := range fl.byAddr {
		c.nodes = view
	}
}

func (fl *clusterFleet) module() *RedisModule {
	return &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			c, ok := fl.byAddr[cfg.addr]
			if !ok {
				return nil, fmt.Errorf("no fake node for addr %q", cfg.addr)
			}
			c.cfg = cfg
			return c, nil
		},
	}
}

// clusterNodesParam строит params.nodes-map из набора addr (ключ = "node-<i>").
func clusterNodesParam(addrs ...string) map[string]any {
	nodes := map[string]any{}
	for i, a := range addrs {
		nodes[fmt.Sprintf("node-%d", i)] = map[string]any{"addr": a}
	}
	return nodes
}

// --- Validate: cluster ---

func TestValidate_ClusterRejectsEmptyNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "cluster",
		Params: mustStruct(t, map[string]any{"action": "create", "nodes": map[string]any{}}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой nodes")
	}
}

func TestValidate_ClusterRejectsNonCreateAction(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"nodes":  clusterNodesParam("127.0.0.1:6379"),
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на нереализованный action reshard (только create / add-node / remove-node)")
	}
}

func TestValidate_ClusterRejectsIndivisibleNodes(t *testing.T) {
	m := &RedisModule{}
	// 5 нод не делится на shardSize=2 (1 master + 1 replica).
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"replicas_per_shard": 1,
			"nodes": clusterNodesParam(
				"10.0.0.1:6379", "10.0.0.2:6379", "10.0.0.3:6379",
				"10.0.0.4:6379", "10.0.0.5:6379"),
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false: 5 нод не делится на размер шарда 2")
	}
}

func TestValidate_ClusterHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"replicas_per_shard": 1,
			"nodes":              clusterNodesParam("10.0.0.1:6379", "10.0.0.2:6379"),
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- slot-allocation (детерминизм деления 16384) ---

func TestAllocateSlots_FullCoverageAndRemainder(t *testing.T) {
	for _, shards := range []int{1, 2, 3, 6, 7} {
		ranges := allocateSlots(shards)
		if len(ranges) != shards {
			t.Fatalf("shards=%d: ждали %d диапазонов, got %d", shards, shards, len(ranges))
		}
		// Непрерывное покрытие 0..16383 без дыр и пересечений.
		expect := 0
		total := 0
		for i, r := range ranges {
			if r.from != expect {
				t.Fatalf("shards=%d диапазон[%d].from=%d, ждали %d", shards, i, r.from, expect)
			}
			if r.to < r.from {
				t.Fatalf("shards=%d диапазон[%d] пуст: [%d-%d]", shards, i, r.from, r.to)
			}
			expect = r.to + 1
			total += r.to - r.from + 1
		}
		if total != totalSlots {
			t.Fatalf("shards=%d: покрыто %d слотов, ждали %d", shards, total, totalSlots)
		}
		// Остаток — первым мастерам: размеры монотонно невозрастающие.
		for i := 1; i < len(ranges); i++ {
			prev := ranges[i-1].to - ranges[i-1].from + 1
			cur := ranges[i].to - ranges[i].from + 1
			if cur > prev {
				t.Fatalf("shards=%d: размер диапазона[%d]=%d > [%d]=%d (остаток не первым)", shards, i, cur, i-1, prev)
			}
		}
	}
}

// --- Apply create: happy-path (MEET/ADDSLOTS/REPLICATE + полное покрытие) ---

func TestApplyClusterCreate_HappyPath(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379", "10.0.0.3:6379", "10.0.0.4:6379"}
	fl := newFleet(addrs...)
	fl.setConvergedNodesView() // gossip сходится сразу
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"password":           secretPass,
			"replicas_per_shard": 1,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// node-0/node-1 — мастера (первые shards=2 по sort ключей), node-2/node-3 — реплики.
	master0 := fl.byAddr[addrs[0]]
	master1 := fl.byAddr[addrs[1]]
	replica2 := fl.byAddr[addrs[2]]
	replica3 := fl.byAddr[addrs[3]]

	// Hub (первая нода = master0) шлёт MEET всех остальных по ip:port.
	assertMeetTargets(t, master0, []string{"10.0.0.2:6379", "10.0.0.3:6379", "10.0.0.4:6379"})

	// ADDSLOTS только мастерам, диапазоны детерминированы и полностью покрывают 16384.
	r0 := assertAddSlots(t, master0)
	r1 := assertAddSlots(t, master1)
	assertNoAddSlots(t, replica2)
	assertNoAddSlots(t, replica3)
	assertFullSlotCoverage(t, r0, r1)

	// REPLICATE: реплики привязаны к своему мастеру (round-robin j%shards).
	// node-2 (j=0) -> master0, node-3 (j=1) -> master1.
	assertReplicateTo(t, replica2, master0.id)
	assertReplicateTo(t, replica3, master1.id)

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// --- Apply create: идемпотентность (уже сформирован → changed=false) ---

func TestApplyClusterCreate_AlreadyFormedNoOp(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379"}
	fl := newFleet(addrs...)
	// Первый мастер рапортует сформированный кластер: state ok, все ноды, все слоты.
	fl.byAddr[addrs[0]].info = "cluster_state:ok\n" +
		"cluster_slots_assigned:16384\n" +
		"cluster_known_nodes:2\n"
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"replicas_per_shard": 0,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false на уже сформированном кластере (no-op)")
	}
	// No-op: ни MEET, ни ADDSLOTS не должны вызываться.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "MEET") || isClusterSub(call, "ADDSLOTS") {
				t.Errorf("нода %s: no-op нарушен, вызвана %v", addr, call)
			}
		}
	}
}

// convergedNodesViewMastersOnly строит CLUSTER NODES, где ВСЕ ноды fleet-а —
// master (gossip сошёлся, slots могут быть назначены, но реплики НЕ настроены).
// rows: addr -> диапазон слотов (nil → master без слотов). node-id берётся из
// fake-ноды (её CLUSTER MYID), что нужно gossip-gate перед REPLICATE.
func (fl *clusterFleet) convergedNodesViewMastersOnly(slotsByAddr map[string][2]int) string {
	lines := make([]string, 0, len(fl.byAddr))
	for addr, c := range fl.byAddr {
		line := c.id + " " + addr + "@16379 master - 0 0 0 connected"
		if r, ok := slotsByAddr[addr]; ok {
			line += fmt.Sprintf(" %d-%d", r[0], r[1])
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// --- Apply create: PARTIAL topology (6 master, реплики не настроены) → доделать REPLICATE ---
//
// LIVE-БАГ (доказан на стенде): первый REPLICATE упал на gossip-timing («Unknown
// node»), кластер застыл как N master (slots полны, cluster_state:ok), а idempotent-
// гейт clusterAlreadyFormed смотрел ТОЛЬКО cluster_state/known_nodes/slots_assigned
// → рапортовал «уже сформирован» → no-op → реплики НИКОГДА не доделывались. Здесь
// 4 ноды (план: 2 master + 2 replica), live-кластер сошёлся как 4 master без реплик:
// плагин ОБЯЗАН доделать REPLICATE недостающих реплик (changed=true), НЕ трогая
// MEET/ADDSLOTS (slots уже на месте).
func TestApplyClusterCreate_PartialTopologyCompletesReplicas(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379", "10.0.0.3:6379", "10.0.0.4:6379"}
	fl := newFleet(addrs...)
	// План (sort ключей node-0..node-3): master node-0/node-1, replica node-2/node-3.
	// Реплики round-robin: node-2 (j=0) -> master0, node-3 (j=1) -> master1.
	master0 := fl.byAddr[addrs[0]]
	master1 := fl.byAddr[addrs[1]]
	replica2 := fl.byAddr[addrs[2]]
	replica3 := fl.byAddr[addrs[3]]

	// Live-топология: ВСЕ 4 ноды — master (реплики не настроены). slots раскиданы
	// между первыми двумя (как если бы ADDSLOTS прошёл, а REPLICATE — нет).
	view := fl.convergedNodesViewMastersOnly(map[string][2]int{
		addrs[0]: {0, 8191},
		addrs[1]: {8192, 16383},
	})
	for _, c := range fl.byAddr {
		c.nodes = view
	}
	// CLUSTER INFO первого мастера: state ok, все ноды, все слоты — ровно те три
	// условия, на которых старый гейт ошибочно говорил «сформирован».
	master0.info = "cluster_state:ok\ncluster_slots_assigned:16384\ncluster_known_nodes:4\n"

	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"password":           secretPass,
			"replicas_per_shard": 1,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if !fin.Changed {
		t.Fatal("ждали changed=true: partial-topology обязана доделать REPLICATE")
	}

	// REPLICATE доделан на ОБОИХ узлах-репликах к их мастерам (по live node-id).
	assertReplicateTo(t, replica2, master0.id)
	assertReplicateTo(t, replica3, master1.id)

	// ADDSLOTS НЕ переназначаются (slots уже на месте — иначе «Slot is already busy»).
	assertNoAddSlots(t, master0)
	assertNoAddSlots(t, master1)
	// REPLICATE НЕ вызывается на мастерах.
	for _, c := range []*clusterConn{master0, master1} {
		for _, call := range c.calls {
			if isClusterSub(call, "REPLICATE") {
				t.Errorf("мастер не должен получать REPLICATE: %v", call)
			}
		}
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// --- Apply create: ПОЛНАЯ topology (master+replica настроены) → no-op ---
//
// Guard против over-fix: если live-кластер уже полон (реплики на месте) — гейт
// обязан остаться идемпотентным (changed=false, никаких REPLICATE/ADDSLOTS).
func TestApplyClusterCreate_FullyFormedWithReplicasNoOp(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379", "10.0.0.3:6379", "10.0.0.4:6379"}
	fl := newFleet(addrs...)
	master0 := fl.byAddr[addrs[0]]
	master1 := fl.byAddr[addrs[1]]
	replica2 := fl.byAddr[addrs[2]]
	replica3 := fl.byAddr[addrs[3]]

	// Live-топология ПОЛНАЯ: node-2 — реплика master0, node-3 — реплика master1.
	view := strings.Join([]string{
		master0.id + " " + addrs[0] + "@16379 master - 0 0 0 connected 0-8191",
		master1.id + " " + addrs[1] + "@16379 master - 0 0 0 connected 8192-16383",
		replica2.id + " " + addrs[2] + "@16379 slave " + master0.id + " 0 0 0 connected",
		replica3.id + " " + addrs[3] + "@16379 slave " + master1.id + " 0 0 0 connected",
	}, "\n")
	for _, c := range fl.byAddr {
		c.nodes = view
	}
	master0.info = "cluster_state:ok\ncluster_slots_assigned:16384\ncluster_known_nodes:4\n"

	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"replicas_per_shard": 1,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: кластер уже полностью сформирован (no-op)")
	}
	// No-op: ни MEET, ни ADDSLOTS, ни REPLICATE.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "MEET") || isClusterSub(call, "ADDSLOTS") || isClusterSub(call, "REPLICATE") {
				t.Errorf("нода %s: no-op нарушен, вызвана %v", addr, call)
			}
		}
	}
}

// --- Apply create: gossip-gate перед REPLICATE (master-id ещё не виден реплике) ---
//
// Корень live-«Unknown node»: формирование с нуля шлёт REPLICATE на узле-реплике
// сразу после ADDSLOTS, но узел мог ещё не получить gossip о мастере → его
// локальный CLUSTER NODES не содержит master node-id → REPLICATE падает. Фикс
// ждёт (bounded retry), пока узел-реплика увидит master-id. Здесь узел-реплика
// первые вызовы CLUSTER NODES отдаёт БЕЗ строки мастера, затем — со строкой:
// плагин обязан дождаться и не упасть.
func TestApplyClusterCreate_GossipGateBeforeReplicate(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379"}
	fl := newFleet(addrs...)
	fl.setConvergedNodesView() // hub видит обе ноды (число) — MEET-gate проходит

	master0 := fl.byAddr[addrs[0]]
	replica1 := fl.byAddr[addrs[1]]

	// Узел-реплика (node-1) сперва НЕ видит мастера в локальной топологии (только
	// себя), затем gossip доносит мастера. master0.id — то, что вернёт CLUSTER MYID
	// мастера и что обязано появиться в NODES реплики до REPLICATE.
	selfOnly := replica1.id + " " + addrs[1] + "@16379 myself,master - 0 0 0 connected"
	withMaster := selfOnly + "\n" + master0.id + " " + addrs[0] + "@16379 master - 0 0 0 connected 0-16383"
	// Первые 2 ответа NODES реплики — без мастера, 3-й и далее — с мастером. hub
	// (master0) свою converged-view уже имеет (setConvergedNodesView выше).
	replica1.nodesSeq = []string{selfOnly, selfOnly, withMaster}

	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"password":           secretPass,
			"replicas_per_shard": 1,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (gossip-gate дождался мастера), got %+v", fin)
	}
	if !fin.Changed {
		t.Fatal("ждали changed=true")
	}
	// REPLICATE состоялся к master0 ПОСЛЕ того, как master-id стал виден реплике.
	assertReplicateTo(t, replica1, master0.id)
	// Узел-реплика опросил локальный CLUSTER NODES минимум трижды (2 пустых + 1 с
	// мастером) — gossip-gate реально ждал, а не выстрелил вслепую.
	if replica1.nodesCalls < 3 {
		t.Errorf("ждали >=3 опросов CLUSTER NODES реплики (gossip-gate), got %d", replica1.nodesCalls)
	}
}

// --- Детерминизм: один вход → одна раскладка (несколько прогонов) ---

func TestApplyClusterCreate_LayoutDeterministic(t *testing.T) {
	addrs := []string{"10.0.0.4:6379", "10.0.0.1:6379", "10.0.0.3:6379", "10.0.0.2:6379", "10.0.0.6:6379", "10.0.0.5:6379"}

	var first string
	for run := 0; run < 5; run++ {
		fl := newFleet(addrs...)
		fl.setConvergedNodesView()
		m := fl.module()
		stream := &applyStream{}
		err := m.Apply(&pluginv1.ApplyRequest{
			State: "cluster",
			Params: mustStruct(t, map[string]any{
				"action":             "create",
				"replicas_per_shard": 1,
				"nodes":              clusterNodesParam(addrs...),
			}),
		}, stream)
		if err != nil {
			t.Fatalf("run %d Apply: %v", run, err)
		}
		got := stream.final().GetOutput().GetFields()["layout"].GetStringValue()
		if got == "" {
			t.Fatalf("run %d: пустой layout", run)
		}
		if run == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("раскладка недетерминирована: run0=%q run%d=%q", first, run, got)
		}
	}
}

// --- Детерминизм по ключу, не по addr-строке: одинаковые ключи → одинаковая роль ---

func TestBuildClusterPlan_RoleByKeyOrder(t *testing.T) {
	// Ключи задаются явно — раскладка обязана идти по СОРТИРОВКЕ ключей.
	nodes := []clusterNode{
		{key: "z", addr: "10.0.0.9:6379", ip: "10.0.0.9", port: 6379},
		{key: "a", addr: "10.0.0.1:6379", ip: "10.0.0.1", port: 6379},
	}
	sortNodesByKey(nodes)
	plan, err := buildClusterPlan(nodes, 1)
	if err != nil {
		t.Fatalf("buildClusterPlan: %v", err)
	}
	if len(plan.masters) != 1 || plan.masters[0].key != "a" {
		t.Fatalf("master должен быть ключ 'a' (первый по sort), got %+v", plan.masters)
	}
	if len(plan.replicas) != 1 || plan.replicas[0].key != "z" {
		t.Fatalf("replica должна быть ключ 'z', got %+v", plan.replicas)
	}
}

// --- Негатив Validate: дублируем happy-обвязку для пустого/неверного входа ---

func TestValidate_ClusterRejectsNegativeReplicas(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"replicas_per_shard": -1,
			"nodes":              clusterNodesParam("10.0.0.1:6379"),
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на отрицательный replicas_per_shard")
	}
}

// --- Пароль не утекает (Apply create) ---

func TestApplyClusterCreate_NoSecretLeak(t *testing.T) {
	addrs := []string{"10.0.0.1:6379", "10.0.0.2:6379"}
	fl := newFleet(addrs...)
	fl.setConvergedNodesView()
	m := fl.module()
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"password":           secretPass,
			"replicas_per_shard": 0,
			"nodes":              clusterNodesParam(addrs...),
		}),
	}, stream)

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		// Пароль ушёл в коннект.
		if c.cfg.password != secretPass {
			t.Errorf("нода %s: пароль не доехал до коннекта", addr)
		}
		assertNoClusterSecret(t, addr, c)
	}
}

func TestApplyClusterCreate_ConnectFailureNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"password":           secretPass,
			"replicas_per_shard": 0,
			"nodes":              clusterNodesParam("10.0.0.1:6379", "10.0.0.2:6379"),
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// =============================== add-node ====================================

// clusterNodesTable строит реалистичный вывод CLUSTER NODES из спецификаций строк.
// Каждая строка: "<id> <ipPort>@<cport> <flags> <masterID|-> 0 0 0 connected".
type nodeRowSpec struct {
	id       string
	ipPort   string
	master   bool
	masterID string // для реплики
}

func clusterNodesTable(rows ...nodeRowSpec) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		flags := "slave"
		mid := r.masterID
		if r.master {
			flags = "master"
			mid = "-"
		}
		if mid == "" {
			mid = "-"
		}
		cport := r.ipPort // @cport не парсится — годится любой суффикс
		lines = append(lines, fmt.Sprintf("%s %s@%s %s %s 0 0 0 connected", r.id, r.ipPort, cport, flags, mid))
	}
	return strings.Join(lines, "\n")
}

// formedTwoMasterSeed — seed с двумя мастерами m0/m1 (для add-node-сценариев).
// До MEET: 2 строки; после MEET: + строка новичка (gossip сошёлся).
func formedTwoMasterSeed(newIPPort string) (before, after string) {
	before = clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "m1id", ipPort: "10.0.0.2:6379", master: true},
	)
	after = clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "m1id", ipPort: "10.0.0.2:6379", master: true},
		nodeRowSpec{id: "newid", ipPort: newIPPort, master: true},
	)
	return before, after
}

func nodeMapParam(addr string) map[string]any { return map[string]any{"addr": addr} }

// --- Validate: add-node ---

func TestValidate_AddNodeRequiresNewNodeAndSeed(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "cluster",
		Params: mustStruct(t, map[string]any{"action": "add-node"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false без new_node/seed")
	}
}

func TestValidate_AddNodeRejectsBadRole(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"new_node": nodeMapParam("10.0.0.9:6379"),
			"seed":     nodeMapParam("10.0.0.1:6379"),
			"role":     "arbiter",
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на неизвестную role")
	}
}

func TestValidate_AddNodeHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"new_node": nodeMapParam("10.0.0.9:6379"),
			"seed":     nodeMapParam("10.0.0.1:6379"),
			"role":     "replica",
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply add-node: replica, авто-выбор master (наименее загруженный) ---

func TestApplyClusterAddNode_ReplicaAutoMaster(t *testing.T) {
	newAddr, seedAddr := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(newAddr, seedAddr)
	// m1 уже несёт одну реплику → авто-выбор обязан пасть на m0 (0 реплик).
	before := clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "m1id", ipPort: "10.0.0.2:6379", master: true},
		nodeRowSpec{id: "r1id", ipPort: "10.0.0.3:6379", masterID: "m1id"},
	)
	// after — gossip сошёлся: +новичок (len(before)+1 = 4 строки).
	after := clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "m1id", ipPort: "10.0.0.2:6379", master: true},
		nodeRowSpec{id: "r1id", ipPort: "10.0.0.3:6379", masterID: "m1id"},
		nodeRowSpec{id: "newid", ipPort: "10.0.0.9:6379", masterID: "m0id"},
	)
	fl.byAddr[seedAddr].nodesSeq = []string{before, after}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"password": secretPass,
			"new_node": nodeMapParam(newAddr),
			"seed":     nodeMapParam(seedAddr),
			"role":     "replica",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// seed шлёт MEET новичку по ip:port.
	assertMeetTargets(t, fl.byAddr[seedAddr], []string{"10.0.0.9:6379"})
	// REPLICATE исполнен НА НОВИЧКЕ к m0id (наименее загруженный мастер).
	assertReplicateTo(t, fl.byAddr[newAddr], "m0id")
	if got := fin.GetOutput().GetFields()["master_id"].GetStringValue(); got != "m0id" {
		t.Errorf("master_id=%q, ждали m0id (авто-выбор наименее загруженного)", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// --- Apply add-node: replica, явный master ---

func TestApplyClusterAddNode_ReplicaExplicitMaster(t *testing.T) {
	newAddr, seedAddr := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(newAddr, seedAddr)
	before, after := formedTwoMasterSeed("10.0.0.9:6379")
	fl.byAddr[seedAddr].nodesSeq = []string{before, after}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"new_node": nodeMapParam(newAddr),
			"seed":     nodeMapParam(seedAddr),
			"role":     "replica",
			"master":   nodeMapParam("10.0.0.2:6379"), // m1
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	// Явный master 10.0.0.2 → m1id, несмотря на то что m0 менее загружен.
	assertReplicateTo(t, fl.byAddr[newAddr], "m1id")
}

// Явный master не из кластера → ошибка (failed=true), пароль не течёт.
func TestApplyClusterAddNode_ReplicaUnknownMaster(t *testing.T) {
	newAddr, seedAddr := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(newAddr, seedAddr)
	before, _ := formedTwoMasterSeed("10.0.0.9:6379")
	fl.byAddr[seedAddr].nodesSeq = []string{before}
	m := fl.module()
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"password": secretPass,
			"new_node": nodeMapParam(newAddr),
			"seed":     nodeMapParam(seedAddr),
			"role":     "replica",
			"master":   nodeMapParam("10.0.0.7:6379"), // нет в кластере
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на master вне кластера, got %+v", fin)
	}
	// MEET не должен был выполниться (мастер резолвится ДО MEET).
	for _, call := range fl.byAddr[seedAddr].calls {
		if isClusterSub(call, "MEET") {
			t.Error("MEET не должен вызываться при нерезолвимом master")
		}
	}
	assertEventsNoSecret(t, stream)
}

// --- Apply add-node: master (пустой, без слотов) ---

func TestApplyClusterAddNode_EmptyMaster(t *testing.T) {
	newAddr, seedAddr := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(newAddr, seedAddr)
	before, after := formedTwoMasterSeed("10.0.0.9:6379")
	fl.byAddr[seedAddr].nodesSeq = []string{before, after}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"new_node": nodeMapParam(newAddr),
			"seed":     nodeMapParam(seedAddr),
			"role":     "master",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["role"].GetStringValue(); got != "master" {
		t.Errorf("role=%q, ждали master", got)
	}
	// add-node master НЕ двигает слоты: ни ADDSLOTS, ни REPLICATE.
	assertNoAddSlots(t, fl.byAddr[newAddr])
	for _, call := range fl.byAddr[newAddr].calls {
		if isClusterSub(call, "REPLICATE") {
			t.Error("empty master не должен REPLICATE")
		}
	}
}

// --- Apply add-node: идемпотентность (нода уже в кластере → no-op) ---

func TestApplyClusterAddNode_AlreadyMemberNoOp(t *testing.T) {
	newAddr, seedAddr := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(newAddr, seedAddr)
	// Топология seed-а УЖЕ содержит новичка 10.0.0.9.
	fl.byAddr[seedAddr].nodes = clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "newid", ipPort: "10.0.0.9:6379", master: true},
	)
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"new_node": nodeMapParam(newAddr),
			"seed":     nodeMapParam(seedAddr),
			"role":     "replica",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: нода уже в кластере (no-op)")
	}
	// No-op: ни MEET, ни REPLICATE.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "MEET") || isClusterSub(call, "REPLICATE") {
				t.Errorf("нода %s: no-op нарушен, вызвана %v", addr, call)
			}
		}
	}
}

// --- Apply add-node: коннект-фейл к seed не течёт паролем ---

func TestApplyClusterAddNode_SeedConnectFailNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "add-node",
			"password": secretPass,
			"new_node": nodeMapParam("10.0.0.9:6379"),
			"seed":     nodeMapParam("10.0.0.1:6379"),
			"role":     "replica",
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// --- parseClusterNodesTable: разбор строк топологии ---

func TestParseClusterNodesTable(t *testing.T) {
	table := clusterNodesTable(
		nodeRowSpec{id: "m0id", ipPort: "10.0.0.1:6379", master: true},
		nodeRowSpec{id: "r0id", ipPort: "10.0.0.3:6379", masterID: "m0id"},
	)
	rows := parseClusterNodesTable(table)
	if len(rows) != 2 {
		t.Fatalf("ждали 2 строки, got %d", len(rows))
	}
	if rows[0].id != "m0id" || rows[0].ipPort != "10.0.0.1:6379" || !rows[0].isMaster {
		t.Errorf("строка master разобрана неверно: %+v", rows[0])
	}
	if rows[1].isMaster || rows[1].masterID != "m0id" {
		t.Errorf("строка replica разобрана неверно: %+v", rows[1])
	}
	// @cport должен быть отрезан.
	if strings.Contains(rows[0].ipPort, "@") {
		t.Errorf("ipPort несёт @cport: %q", rows[0].ipPort)
	}
}

// --- assert-хелперы (cluster) ---

func sortNodesByKey(nodes []clusterNode) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].key < nodes[j].key })
}

func isClusterSub(call []any, sub string) bool {
	if len(call) < 2 {
		return false
	}
	v0, _ := call[0].(string)
	v1, _ := call[1].(string)
	return strings.EqualFold(v0, "CLUSTER") && strings.EqualFold(v1, sub)
}

// assertMeetTargets проверяет множество ip:port, которым нода слала CLUSTER MEET.
func assertMeetTargets(t *testing.T, c *clusterConn, want []string) {
	t.Helper()
	var got []string
	for _, call := range c.calls {
		if isClusterSub(call, "MEET") {
			ip, _ := call[2].(string)
			port, _ := call[3].(string)
			got = append(got, ip+":"+port)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("MEET targets=%v, ждали %v", got, want)
	}
}

// assertAddSlots извлекает диапазон слотов из CLUSTER ADDSLOTS на ноде; требует
// ровно один такой вызов.
func assertAddSlots(t *testing.T, c *clusterConn) slotRange {
	t.Helper()
	var found *slotRange
	for _, call := range c.calls {
		if !isClusterSub(call, "ADDSLOTS") {
			continue
		}
		if found != nil {
			t.Fatal("ждали ровно один ADDSLOTS на мастере")
		}
		slots := make([]int, 0, len(call)-2)
		for _, a := range call[2:] {
			s, _ := a.(string)
			n, err := strconv.Atoi(s)
			if err != nil {
				t.Fatalf("ADDSLOTS аргумент не число: %v", a)
			}
			slots = append(slots, n)
		}
		if len(slots) == 0 {
			t.Fatal("ADDSLOTS без слотов")
		}
		// Слоты непрерывны и возрастают.
		for i := 1; i < len(slots); i++ {
			if slots[i] != slots[i-1]+1 {
				t.Fatalf("ADDSLOTS слоты не непрерывны: %v", slots)
			}
		}
		r := slotRange{from: slots[0], to: slots[len(slots)-1]}
		found = &r
	}
	if found == nil {
		t.Fatal("на мастере не было ADDSLOTS")
	}
	return *found
}

func assertNoAddSlots(t *testing.T, c *clusterConn) {
	t.Helper()
	for _, call := range c.calls {
		if isClusterSub(call, "ADDSLOTS") {
			t.Errorf("на реплике не должно быть ADDSLOTS, got %v", call)
		}
	}
}

func assertFullSlotCoverage(t *testing.T, ranges ...slotRange) {
	t.Helper()
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].from < ranges[j].from })
	expect := 0
	for _, r := range ranges {
		if r.from != expect {
			t.Fatalf("дыра/перекрытие слотов: ждали from=%d, got %d", expect, r.from)
		}
		expect = r.to + 1
	}
	if expect != totalSlots {
		t.Fatalf("покрыто %d слотов, ждали %d", expect, totalSlots)
	}
}

func assertReplicateTo(t *testing.T, c *clusterConn, masterID string) {
	t.Helper()
	for _, call := range c.calls {
		if isClusterSub(call, "REPLICATE") {
			got, _ := call[2].(string)
			if got != masterID {
				t.Errorf("REPLICATE -> %q, ждали %q", got, masterID)
			}
			return
		}
	}
	t.Errorf("на реплике не было CLUSTER REPLICATE")
}

func assertNoClusterSecret(t *testing.T, addr string, c *clusterConn) {
	t.Helper()
	for i, call := range c.calls {
		for _, a := range call {
			if s, ok := a.(string); ok && strings.Contains(s, secretPass) {
				t.Errorf("нода %s команда[%d] несёт пароль: %v", addr, i, call)
			}
		}
	}
}

// ============================= remove-node ===================================

// nodesTableWithSlots собирает вывод CLUSTER NODES из готовых строк (clusterNodesTable
// слоты не несёт, а remove-node их разбирает — строки строим явно).
func nodesTableWithSlots(rows ...string) string { return strings.Join(rows, "\n") }

// masterRowSlots строит строку CLUSTER NODES master-а с диапазоном слотов.
func masterRowSlots(id, ipPort string, from, to int) string {
	return fmt.Sprintf("%s %s@%s master - 0 0 0 connected %d-%d", id, ipPort, ipPort, from, to)
}

// masterRowNoSlots — master без слотов (пустой master).
func masterRowNoSlots(id, ipPort string) string {
	return fmt.Sprintf("%s %s@%s master - 0 0 0 connected", id, ipPort, ipPort)
}

// replicaRow — реплика (slave) master-а masterID, без слотов.
func replicaRow(id, ipPort, masterID string) string {
	return fmt.Sprintf("%s %s@%s slave %s 0 0 0 connected", id, ipPort, ipPort, masterID)
}

func clusterForgetTargets(c *clusterConn) []string {
	var got []string
	for _, call := range c.calls {
		if isClusterSub(call, "FORGET") {
			id, _ := call[2].(string)
			got = append(got, id)
		}
	}
	return got
}

func hasClusterForget(c *clusterConn, removeID string) bool {
	for _, id := range clusterForgetTargets(c) {
		if id == removeID {
			return true
		}
	}
	return false
}

// --- Validate: remove-node ---

func TestValidate_RemoveNodeRequiresNodeAndSeed(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "cluster",
		Params: mustStruct(t, map[string]any{"action": "remove-node"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false без node/seed")
	}
}

func TestValidate_RemoveNodeHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "remove-node",
			"node":   nodeMapParam("10.0.0.3:6379"),
			"seed":   nodeMapParam("10.0.0.1:6379"),
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply remove-node: replica (просто FORGET на всех оставшихся) ---

func TestApplyClusterRemoveNode_ReplicaForgetOnly(t *testing.T) {
	removeAddr, seedAddr := "10.0.0.3:6379", "10.0.0.1:6379"
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	fl := newFleet(m0Addr, m1Addr, removeAddr)
	// Топология: m0/m1 — мастера со слотами, r1 (10.0.0.3) — реплика m1.
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8191),
		masterRowSlots("m1id", "10.0.0.2:6379", 8192, 16383),
		replicaRow("r1id", "10.0.0.3:6379", "m1id"),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "remove-node",
			"password": secretPass,
			"node":     nodeMapParam(removeAddr),
			"seed":     nodeMapParam(seedAddr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// Реплику не мигрируют: ни SETSLOT, ни MIGRATE.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "SETSLOT") {
				t.Errorf("нода %s: SETSLOT при удалении РЕПЛИКИ не должен вызываться: %v", addr, call)
			}
			if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
				t.Errorf("нода %s: MIGRATE при удалении РЕПЛИКИ не должен вызываться: %v", addr, call)
			}
		}
	}
	// FORGET r1id на ОБОИХ оставшихся мастерах, НЕ на самой удаляемой.
	if !hasClusterForget(fl.byAddr[m0Addr], "r1id") {
		t.Error("m0: ждали CLUSTER FORGET r1id")
	}
	if !hasClusterForget(fl.byAddr[m1Addr], "r1id") {
		t.Error("m1: ждали CLUSTER FORGET r1id")
	}
	if len(clusterForgetTargets(fl.byAddr[removeAddr])) != 0 {
		t.Error("удаляемая нода не должна получать FORGET")
	}
	if got := fin.GetOutput().GetFields()["forgotten_on"].GetNumberValue(); got != 2 {
		t.Errorf("forgotten_on=%v, ждали 2", got)
	}
	if got := fin.GetOutput().GetFields()["slots_migrated"].GetNumberValue(); got != 0 {
		t.Errorf("slots_migrated=%v, ждали 0 (реплика)", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// --- Apply remove-node: master СО слотами (миграция слотов + FORGET) ---

func TestApplyClusterRemoveNode_MasterWithSlotsMigrates(t *testing.T) {
	removeAddr := "10.0.0.3:6379" // m2 — удаляемый master со слотами 16380-16383 (4 слота)
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	seedAddr := m0Addr
	fl := newFleet(m0Addr, m1Addr, removeAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8189),
		masterRowSlots("m1id", "10.0.0.2:6379", 8190, 16379),
		masterRowSlots("m2id", "10.0.0.3:6379", 16380, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	// На слоте 16380 у источника лежит один ключ → миграция реально шлёт MIGRATE.
	fl.byAddr[removeAddr].keysInSlot = map[int][]string{16380: {"key-a"}}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "remove-node",
			"password": secretPass,
			"node":     nodeMapParam(removeAddr),
			"seed":     nodeMapParam(seedAddr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	// 4 слота (16380..16383) перенесены.
	if got := fin.GetOutput().GetFields()["slots_migrated"].GetNumberValue(); got != 4 {
		t.Errorf("slots_migrated=%v, ждали 4", got)
	}

	src := fl.byAddr[removeAddr]

	// Источник: MIGRATING на КАЖДЫЙ слот + GETKEYSINSLOT + SETSLOT NODE.
	migratingSlots := setslotSlots(src, "MIGRATING")
	if len(migratingSlots) != 4 {
		t.Errorf("источник: ждали 4 SETSLOT MIGRATING, got %d (%v)", len(migratingSlots), migratingSlots)
	}
	// Слоты round-robin между двумя destination-мастерами (16380→m0, 16381→m1, ...).
	assertSetslotImporting(t, fl.byAddr[m0Addr], "m2id") // m0 импортирует слоты из m2
	assertSetslotImporting(t, fl.byAddr[m1Addr], "m2id")

	// На слоте 16380 был ключ → MIGRATE действительно выполнен (с AUTH ***).
	if !hasMigrate(src) {
		t.Error("источник: ждали MIGRATE для слота с ключом")
	}

	// После миграции — FORGET m2id на обоих оставшихся мастерах.
	if !hasClusterForget(fl.byAddr[m0Addr], "m2id") {
		t.Error("m0: ждали CLUSTER FORGET m2id после миграции")
	}
	if !hasClusterForget(fl.byAddr[m1Addr], "m2id") {
		t.Error("m1: ждали CLUSTER FORGET m2id после миграции")
	}

	// ИБ-инвариант: пароль НЕ в событиях/ошибках (логи/OTel/RunResult). На ПРОВОДЕ
	// он неизбежен ровно в одном месте — MIGRATE ... AUTH <pass> (AUTH к
	// password-protected destination; ровно так шлёт и сам go-redis). Везде, КРОМЕ
	// этого AUTH-аргумента, пароля быть не должно.
	assertEventsNoSecret(t, stream)
	assertSecretOnlyInMigrateAuth(t, src)
	for addr, c := range fl.byAddr {
		if addr == removeAddr {
			continue // источник MIGRATE — проверен отдельно выше
		}
		assertNoClusterSecret(t, addr, c)
	}
}

// --- Apply remove-node: ключи с whitespace в имени мигрируют лосслесс ---

// TestApplyClusterRemoveNode_WhitespaceKeysLossless фиксирует MAJOR-дефект:
// Redis-ключ — произвольная байт-строка и может содержать пробел/\t/\n. Раньше
// GETKEYSINSLOT стрингифицировался join-ом через пробел + strings.Fields →
// ключ "user 42" рвался на два токена → MIGRATE по несуществующим ключам → ключ
// НЕ переносился, а SETSLOT NODE всё равно отдавал слот → ПОТЕРЯ ДАННЫХ.
//
// Типизированный GetKeysInSlot ([]string) сохраняет ключи целиком. Тест требует:
// каждый ключ слота (включая whitespace-имена) попал в MIGRATE как ОДИН KEYS-
// аргумент, и множество перенесённых ключей в точности равно исходному (лосслесс).
func TestApplyClusterRemoveNode_WhitespaceKeysLossless(t *testing.T) {
	removeAddr := "10.0.0.3:6379" // m2 — удаляемый master со слотом 16380
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	seedAddr := m0Addr
	fl := newFleet(m0Addr, m1Addr, removeAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8189),
		masterRowSlots("m1id", "10.0.0.2:6379", 8190, 16379),
		masterRowSlots("m2id", "10.0.0.3:6379", 16380, 16380), // ровно один слот
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	// Слот 16380 содержит ключи С разделителями в имени: пробел, таб, перевод
	// строки — и обычный ключ для контраста. Все должны переехать как есть.
	keys := []string{"user 42", "a\tb", "c\nd", "plain"}
	fl.byAddr[removeAddr].keysInSlot = map[int][]string{16380: append([]string(nil), keys...)}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "remove-node",
			"password": secretPass,
			"node":     nodeMapParam(removeAddr),
			"seed":     nodeMapParam(seedAddr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// Множество ключей, реально ушедших в MIGRATE … KEYS …, должно совпасть с
	// исходным БЕЗ потерь и БЕЗ расщепления (никаких "user"/"42" по отдельности).
	got := migrateKeyArgs(t, fl.byAddr[removeAddr])
	if !equalStringSets(got, keys) {
		t.Fatalf("MIGRATE KEYS лоссово: got %q, ждали %q (ключи с whitespace расщеплены/потеряны)", got, keys)
	}
	// Точечно: "user 42" обязан быть ОДНИМ аргументом, не двумя токенами.
	if !containsString(got, "user 42") {
		t.Errorf(`ключ "user 42" не передан как один аргумент MIGRATE: %q`, got)
	}
}

// --- Apply remove-node: слот с >1 непустым батчем (multi-batch цикл) ---

// TestApplyClusterRemoveNode_MultiBatchSlotLossless фиксирует DATA-RISK путь:
// migrateOneSlot циклит GETKEYSINSLOT+MIGRATE ПОКА слот не пуст. Слот с >migrateBatch
// ключей отдаёт несколько порций → несколько итераций цикла. Если цикл прервётся
// после первой порции (или fake-источник отдаст всё разом и спрячет баг), ключи
// сверх первого батча ПОТЕРЯЮТСЯ при SETSLOT NODE. Тест держит на удаляемом
// master-е слот с очередью из 3 порций (whitespace-ключ — во ВТОРОЙ, не первой):
// требует, чтобы объединение ВСЕХ порций ушло в MIGRATE, цикл завершился, а
// порядок фаз слота был IMPORTING→MIGRATING→(N MIGRATE)→SETSLOT NODE.
func TestApplyClusterRemoveNode_MultiBatchSlotLossless(t *testing.T) {
	removeAddr := "10.0.0.3:6379" // m2 — удаляемый master со слотом 16380
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	seedAddr := m0Addr
	fl := newFleet(m0Addr, m1Addr, removeAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8189),
		masterRowSlots("m1id", "10.0.0.2:6379", 8190, 16379),
		masterRowSlots("m2id", "10.0.0.3:6379", 16380, 16380), // ровно один слот
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	// Слот 16380 опустошается за 3 порции (по >1 итерации цикла). whitespace-ключ
	// "user 42" — во ВТОРОЙ порции (не в первой): доказывает, что lossless держится
	// и за пределами первого батча. Round-robin: 16380 — первый и единственный слот
	// удаляемого master-а → уходит на m0 (di=0).
	batch1 := []string{"k1", "k2"}
	batch2 := []string{"user 42", "k3"}
	batch3 := []string{"k4"}
	fl.byAddr[removeAddr].keysInSlotBatches = map[int][][]string{
		16380: {batch1, batch2, batch3},
	}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "remove-node",
			"password": secretPass,
			"node":     nodeMapParam(removeAddr),
			"seed":     nodeMapParam(seedAddr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["slots_migrated"].GetNumberValue(); got != 1 {
		t.Errorf("slots_migrated=%v, ждали 1", got)
	}

	src := fl.byAddr[removeAddr]
	wantKeys := append(append(append([]string(nil), batch1...), batch2...), batch3...)

	// ВСЕ ключи всех порций ушли в MIGRATE (объединение), ничего не потеряно.
	got := migrateKeyArgs(t, src)
	if !equalStringSets(got, wantKeys) {
		t.Fatalf("multi-batch лоссово: MIGRATE KEYS=%q, ждали объединение порций %q", got, wantKeys)
	}
	if !containsString(got, "user 42") {
		t.Errorf(`whitespace-ключ "user 42" из 2-й порции не переехал/расщеплён: %q`, got)
	}
	// Три порции → ровно три MIGRATE-вызова (по одному на непустой батч).
	if n := countMigrate(src); n != 3 {
		t.Errorf("ждали 3 MIGRATE (по порции на батч), got %d", n)
	}
	// Цикл завершился (не зациклился): очередь слота опустошена.
	if q := src.keysInSlotBatches[16380]; len(q) != 0 {
		t.Errorf("очередь слота 16380 не исчерпана (цикл недокрутил): осталось %d порций", len(q))
	}
	// Порядок фаз слота 16380: IMPORTING(цель)→MIGRATING(источник)→3×MIGRATE→
	// SETSLOT NODE. Источник несёт MIGRATING, 3 MIGRATE и SETSLOT NODE в этом порядке.
	assertSlotPhaseOrder(t, src, 16380)
	// IMPORTING на цели (m0, di=0) указывает на источник m2id.
	assertSetslotImporting(t, fl.byAddr[m0Addr], "m2id")

	assertEventsNoSecret(t, stream)
	assertSecretOnlyInMigrateAuth(t, src)
}

// migrateKeyArgs собирает все ключи, переданные в MIGRATE … KEYS k1 k2 … (всё
// после литерала "KEYS"). Каждый ключ — ОТДЕЛЬНЫЙ аргумент команды.
func migrateKeyArgs(t *testing.T, c *clusterConn) []string {
	t.Helper()
	var out []string
	for _, call := range c.calls {
		if v0, _ := call[0].(string); !strings.EqualFold(v0, "MIGRATE") {
			continue
		}
		keysAt := -1
		for j, a := range call {
			if s, _ := a.(string); strings.EqualFold(s, "KEYS") {
				keysAt = j
				break
			}
		}
		if keysAt < 0 {
			t.Fatalf("MIGRATE без KEYS-секции: %v", call)
		}
		for _, a := range call[keysAt+1:] {
			s, _ := a.(string)
			out = append(out, s)
		}
	}
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

// --- Apply remove-node: master БЕЗ слотов (просто FORGET) ---

func TestApplyClusterRemoveNode_EmptyMasterForgetOnly(t *testing.T) {
	removeAddr := "10.0.0.3:6379"
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	fl := newFleet(m0Addr, m1Addr, removeAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8191),
		masterRowSlots("m1id", "10.0.0.2:6379", 8192, 16383),
		masterRowNoSlots("m2id", "10.0.0.3:6379"), // пустой master, слотов нет
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "remove-node",
			"node":   nodeMapParam(removeAddr),
			"seed":   nodeMapParam(m0Addr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["slots_migrated"].GetNumberValue(); got != 0 {
		t.Errorf("slots_migrated=%v, ждали 0 (пустой master)", got)
	}
	// Никакой миграции у пустого master-а.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "SETSLOT") {
				t.Errorf("нода %s: SETSLOT при удалении ПУСТОГО master не должен вызываться: %v", addr, call)
			}
		}
	}
	if !hasClusterForget(fl.byAddr[m0Addr], "m2id") || !hasClusterForget(fl.byAddr[m1Addr], "m2id") {
		t.Error("ждали CLUSTER FORGET m2id на обоих оставшихся мастерах")
	}
}

// --- Apply remove-node: идемпотентность (ноды уже нет → no-op) ---

func TestApplyClusterRemoveNode_AbsentNoOp(t *testing.T) {
	removeAddr := "10.0.0.9:6379" // нет в топологии
	m0Addr := "10.0.0.1:6379"
	m1Addr := "10.0.0.2:6379"
	fl := newFleet(m0Addr, m1Addr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8191),
		masterRowSlots("m1id", "10.0.0.2:6379", 8192, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "remove-node",
			"node":   nodeMapParam(removeAddr),
			"seed":   nodeMapParam(m0Addr),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: ноды уже нет (no-op)")
	}
	// No-op: ни FORGET, ни SETSLOT, ни MIGRATE.
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "FORGET") || isClusterSub(call, "SETSLOT") {
				t.Errorf("нода %s: no-op нарушен, вызвана %v", addr, call)
			}
		}
	}
}

// --- Apply remove-node: коннект-фейл к seed не течёт паролем ---

func TestApplyClusterRemoveNode_SeedConnectFailNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "remove-node",
			"password": secretPass,
			"node":     nodeMapParam("10.0.0.3:6379"),
			"seed":     nodeMapParam("10.0.0.1:6379"),
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// ================================ reshard ====================================

// --- Validate: reshard ---

func TestValidate_ReshardRequiresFromToSlots(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "cluster",
		Params: mustStruct(t, map[string]any{"action": "reshard"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false без from/to/slots")
	}
}

func TestValidate_ReshardRejectsSameFromTo(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"from":   nodeMapParam("10.0.0.1:6379"),
			"to":     nodeMapParam("10.0.0.1:6379"),
			"slots":  10,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на from == to")
	}
}

// {addr} и {ip,port}-форма ОДНОГО узла обязаны распознаваться как совпадение.
func TestValidate_ReshardRejectsSameFromToMixedForm(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"from":   nodeMapParam("10.0.0.1:6379"),
			"to":     map[string]any{"ip": "10.0.0.1", "port": 6379},
			"slots":  10,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на from == to (addr vs ip+port одного узла)")
	}
}

func TestValidate_ReshardRejectsZeroSlots(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"from":   nodeMapParam("10.0.0.1:6379"),
			"to":     nodeMapParam("10.0.0.2:6379"),
			"slots":  0,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на slots < 1")
	}
}

func TestValidate_ReshardHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"from":   nodeMapParam("10.0.0.1:6379"),
			"to":     nodeMapParam("10.0.0.2:6379"),
			"slots":  100,
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply reshard: happy-path (перенос N слотов from→to, последовательность) ---

func TestApplyClusterReshard_HappyPathSequence(t *testing.T) {
	fromAddr := "10.0.0.1:6379" // m0 — источник, слоты 0..8191
	toAddr := "10.0.0.2:6379"   // m1 — получатель, слоты 8192..16383
	fl := newFleet(fromAddr, toAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8191),
		masterRowSlots("m1id", "10.0.0.2:6379", 8192, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	// На первом из переносимых слотов (0) лежит ключ → MIGRATE реально вызывается.
	fl.byAddr[fromAddr].keysInSlot = map[int][]string{0: {"key-a"}}
	m := fl.module()
	stream := &applyStream{}

	const moveN = 3
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "reshard",
			"password": secretPass,
			"from":     nodeMapParam(fromAddr),
			"to":       nodeMapParam(toAddr),
			"slots":    moveN,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["slots_moved"].GetNumberValue(); got != moveN {
		t.Errorf("slots_moved=%v, ждали %d", got, moveN)
	}

	src := fl.byAddr[fromAddr]
	dst := fl.byAddr[toAddr]

	// Переносятся ПЕРВЫЕ N слотов источника по возрастанию: 0, 1, 2.
	wantSlots := []int{0, 1, 2}

	// Цель: SETSLOT <slot> IMPORTING <src-id> на каждый перенесённый слот.
	if got := setslotSlots(dst, "IMPORTING"); !equalIntSlices(got, wantSlots) {
		t.Errorf("цель: SETSLOT IMPORTING слоты=%v, ждали %v", got, wantSlots)
	}
	// Источник: SETSLOT <slot> MIGRATING <dst-id> на каждый слот.
	if got := setslotSlots(src, "MIGRATING"); !equalIntSlices(got, wantSlots) {
		t.Errorf("источник: SETSLOT MIGRATING слоты=%v, ждали %v", got, wantSlots)
	}
	// SETSLOT NODE <dst-id> на ОБЕИХ нодах для каждого слота (фиксация владельца).
	if got := setslotSlots(src, "NODE"); !equalIntSlices(got, wantSlots) {
		t.Errorf("источник: SETSLOT NODE слоты=%v, ждали %v", got, wantSlots)
	}
	if got := setslotSlots(dst, "NODE"); !equalIntSlices(got, wantSlots) {
		t.Errorf("цель: SETSLOT NODE слоты=%v, ждали %v", got, wantSlots)
	}
	// На слоте 0 был ключ → MIGRATE действительно выполнен.
	if !hasMigrate(src) {
		t.Error("источник: ждали MIGRATE для слота с ключом")
	}
	// IMPORTING/MIGRATING указывают на правильные node-id (m1id импортирует у m0id).
	assertSetslotImporting(t, dst, "m0id")

	// ИБ: пароль только в MIGRATE AUTH (на проводе неизбежно), больше нигде.
	assertEventsNoSecret(t, stream)
	assertSecretOnlyInMigrateAuth(t, src)
	assertNoClusterSecret(t, toAddr, dst)
}

// --- Apply reshard: лосслесс ключей с whitespace в имени ---

func TestApplyClusterReshard_WhitespaceKeysLossless(t *testing.T) {
	fromAddr := "10.0.0.1:6379"
	toAddr := "10.0.0.2:6379"
	fl := newFleet(fromAddr, toAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 0), // ровно один слот у источника
		masterRowSlots("m1id", "10.0.0.2:6379", 1, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	// Слот 0 содержит ключи С разделителями: пробел, таб, перевод строки + обычный.
	keys := []string{"user 42", "a\tb", "c\nd", "plain"}
	fl.byAddr[fromAddr].keysInSlot = map[int][]string{0: append([]string(nil), keys...)}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "reshard",
			"password": secretPass,
			"from":     nodeMapParam(fromAddr),
			"to":       nodeMapParam(toAddr),
			"slots":    1,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// Все ключи (вкл. whitespace-имена) ушли в MIGRATE … KEYS … как ОТДЕЛЬНЫЕ
	// аргументы, множество совпадает с исходным (типизированный GetKeysInSlot).
	got := migrateKeyArgs(t, fl.byAddr[fromAddr])
	if !equalStringSets(got, keys) {
		t.Fatalf("MIGRATE KEYS лоссово: got %q, ждали %q (whitespace-ключи расщеплены/потеряны)", got, keys)
	}
	if !containsString(got, "user 42") {
		t.Errorf(`ключ "user 42" не передан как один аргумент MIGRATE: %q`, got)
	}
}

// --- Apply reshard: слот с >1 непустым батчем (multi-batch цикл) ---

// TestApplyClusterReshard_MultiBatchSlotLossless — reshard-зеркало multi-batch
// guard: единственный переносимый слот (0) отдаёт ключи тремя порциями, цикл
// migrateOneSlot обязан опустошить слот полностью (3 MIGRATE) и зафиксировать
// владельца только после. whitespace-ключ — в 3-й (последней) порции.
func TestApplyClusterReshard_MultiBatchSlotLossless(t *testing.T) {
	fromAddr := "10.0.0.1:6379"
	toAddr := "10.0.0.2:6379"
	fl := newFleet(fromAddr, toAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 0), // ровно один слот у источника
		masterRowSlots("m1id", "10.0.0.2:6379", 1, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	batch1 := []string{"k1", "k2"}
	batch2 := []string{"k3"}
	batch3 := []string{"c\nd", "user 42"} // whitespace-ключи в последней порции
	fl.byAddr[fromAddr].keysInSlotBatches = map[int][][]string{
		0: {batch1, batch2, batch3},
	}
	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "reshard",
			"password": secretPass,
			"from":     nodeMapParam(fromAddr),
			"to":       nodeMapParam(toAddr),
			"slots":    1,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	src := fl.byAddr[fromAddr]
	dst := fl.byAddr[toAddr]
	wantKeys := append(append(append([]string(nil), batch1...), batch2...), batch3...)

	got := migrateKeyArgs(t, src)
	if !equalStringSets(got, wantKeys) {
		t.Fatalf("multi-batch лоссово: MIGRATE KEYS=%q, ждали объединение порций %q", got, wantKeys)
	}
	if !containsString(got, "user 42") {
		t.Errorf(`whitespace-ключ "user 42" из 3-й порции не переехал/расщеплён: %q`, got)
	}
	if n := countMigrate(src); n != 3 {
		t.Errorf("ждали 3 MIGRATE (по порции на батч), got %d", n)
	}
	if q := src.keysInSlotBatches[0]; len(q) != 0 {
		t.Errorf("очередь слота 0 не исчерпана (цикл недокрутил): осталось %d порций", len(q))
	}
	// Порядок фаз слота 0: IMPORTING(цель)→MIGRATING(источник)→3×MIGRATE→SETSLOT NODE.
	assertSlotPhaseOrder(t, src, 0)
	assertSetslotImporting(t, dst, "m0id")

	assertEventsNoSecret(t, stream)
	assertSecretOnlyInMigrateAuth(t, src)
}

// --- Apply reshard: from не master в кластере → failed, пароль не течёт ---

func TestApplyClusterReshard_UnknownFromMaster(t *testing.T) {
	fromAddr := "10.0.0.9:6379" // нет в топологии
	toAddr := "10.0.0.2:6379"
	fl := newFleet(fromAddr, toAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 8191),
		masterRowSlots("m1id", "10.0.0.2:6379", 8192, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	m := fl.module()
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "reshard",
			"password": secretPass,
			"from":     nodeMapParam(fromAddr),
			"to":       nodeMapParam(toAddr),
			"slots":    5,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на from вне кластера, got %+v", fin)
	}
	// Никаких SETSLOT/MIGRATE до резолва топологии.
	for _, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "SETSLOT") {
				t.Error("SETSLOT не должен вызываться при нерезолвимом from")
			}
		}
	}
	assertEventsNoSecret(t, stream)
}

// --- Apply reshard: slots > имеющихся у источника → failed ---

func TestApplyClusterReshard_SlotsExceedOwned(t *testing.T) {
	fromAddr := "10.0.0.1:6379" // m0 владеет ровно 4 слотами 0..3
	toAddr := "10.0.0.2:6379"
	fl := newFleet(fromAddr, toAddr)
	table := nodesTableWithSlots(
		masterRowSlots("m0id", "10.0.0.1:6379", 0, 3),
		masterRowSlots("m1id", "10.0.0.2:6379", 4, 16383),
	)
	for _, c := range fl.byAddr {
		c.nodes = table
	}
	m := fl.module()
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "reshard",
			"from":   nodeMapParam(fromAddr),
			"to":     nodeMapParam(toAddr),
			"slots":  5, // больше, чем 4 имеющихся
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на slots > имеющихся, got %+v", fin)
	}
	// Перенос не начат: ни одного SETSLOT.
	for _, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "SETSLOT") {
				t.Error("SETSLOT не должен вызываться при slots > имеющихся")
			}
		}
	}
}

// --- Apply reshard: коннект-фейл к from не течёт паролем ---

func TestApplyClusterReshard_FromConnectFailNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "reshard",
			"password": secretPass,
			"from":     nodeMapParam("10.0.0.1:6379"),
			"to":       nodeMapParam("10.0.0.2:6379"),
			"slots":    5,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// equalIntSlices — точное поэлементное сравнение (порядок важен: слоты в порядке
// переноса). setslotSlots уже сортирует, поэтому сравнение детерминировано.
func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- assert-хелперы (remove-node) ---

// setslotSlots — слоты, которым нода слала CLUSTER SETSLOT <slot> <sub>.
func setslotSlots(c *clusterConn, sub string) []int {
	var got []int
	for _, call := range c.calls {
		if !isClusterSub(call, "SETSLOT") || len(call) < 4 {
			continue
		}
		s, _ := call[3].(string)
		if !strings.EqualFold(s, sub) {
			continue
		}
		slot, _ := strconv.Atoi(call[2].(string))
		got = append(got, slot)
	}
	sort.Ints(got)
	return got
}

// assertSetslotImporting — нода получила хотя бы один SETSLOT … IMPORTING <srcID>.
func assertSetslotImporting(t *testing.T, c *clusterConn, srcID string) {
	t.Helper()
	for _, call := range c.calls {
		if !isClusterSub(call, "SETSLOT") || len(call) < 5 {
			continue
		}
		if s, _ := call[3].(string); strings.EqualFold(s, "IMPORTING") {
			if got, _ := call[4].(string); got == srcID {
				return
			}
		}
	}
	t.Errorf("ждали SETSLOT IMPORTING %s, не найдено", srcID)
}

// hasMigrate — нода исполнила хотя бы один MIGRATE.
func hasMigrate(c *clusterConn) bool {
	for _, call := range c.calls {
		if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
			return true
		}
	}
	return false
}

// countMigrate — сколько MIGRATE-вызовов исполнила нода (в multi-batch цикле —
// один на непустую порцию ключей слота).
func countMigrate(c *clusterConn) int {
	n := 0
	for _, call := range c.calls {
		if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
			n++
		}
	}
	return n
}

// assertSlotPhaseOrder проверяет на ИСТОЧНИКЕ корректный порядок фаз миграции
// конкретного слота: SETSLOT <slot> MIGRATING (ровно раз) → один или несколько
// MIGRATE → SETSLOT <slot> NODE (фиксация владельца ПОСЛЕ всех MIGRATE). Это
// гарантирует, что владелец слота не зафиксирован раньше, чем перенесены все
// порции ключей (иначе — потеря данных в multi-batch). MIGRATE на источнике
// относятся к текущему слоту (за раз мигрируется один слот).
func assertSlotPhaseOrder(t *testing.T, c *clusterConn, slot int) {
	t.Helper()
	slotArg := strconv.Itoa(slot)
	migratingAt, nodeAt := -1, -1
	var migrateIdx []int
	for i, call := range c.calls {
		if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
			migrateIdx = append(migrateIdx, i)
			continue
		}
		if !isClusterSub(call, "SETSLOT") || len(call) < 4 {
			continue
		}
		if s, _ := call[2].(string); s != slotArg {
			continue
		}
		switch sub, _ := call[3].(string); {
		case strings.EqualFold(sub, "MIGRATING"):
			if migratingAt != -1 {
				t.Errorf("слот %d: более одного SETSLOT MIGRATING", slot)
			}
			migratingAt = i
		case strings.EqualFold(sub, "NODE"):
			nodeAt = i
		}
	}
	if migratingAt < 0 {
		t.Fatalf("слот %d: нет SETSLOT MIGRATING на источнике", slot)
	}
	if nodeAt < 0 {
		t.Fatalf("слот %d: нет SETSLOT NODE на источнике", slot)
	}
	if len(migrateIdx) == 0 {
		t.Fatalf("слот %d: нет ни одного MIGRATE между MIGRATING и NODE", slot)
	}
	if nodeAt < migratingAt {
		t.Errorf("слот %d: SETSLOT NODE (idx %d) раньше MIGRATING (idx %d)", slot, nodeAt, migratingAt)
	}
	for _, mi := range migrateIdx {
		if mi < migratingAt {
			t.Errorf("слот %d: MIGRATE (idx %d) раньше SETSLOT MIGRATING (idx %d)", slot, mi, migratingAt)
		}
		if mi > nodeAt {
			t.Errorf("слот %d: MIGRATE (idx %d) ПОСЛЕ SETSLOT NODE (idx %d) — владелец зафиксирован до переноса", slot, mi, nodeAt)
		}
	}
}

// assertSecretOnlyInMigrateAuth допускает пароль ТОЛЬКО как AUTH-аргумент MIGRATE
// (неизбежно на проводе для password-protected destination); любое другое
// появление пароля в командах — утечка.
func assertSecretOnlyInMigrateAuth(t *testing.T, c *clusterConn) {
	t.Helper()
	for i, call := range c.calls {
		isMigrate := false
		if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
			isMigrate = true
		}
		for j, a := range call {
			s, ok := a.(string)
			if !ok || !strings.Contains(s, secretPass) {
				continue
			}
			// MIGRATE host port "" db timeout AUTH <pass> KEYS … → пароль на позиции
			// сразу после "AUTH".
			if isMigrate && j > 0 {
				if prev, _ := call[j-1].(string); strings.EqualFold(prev, "AUTH") {
					continue
				}
			}
			t.Errorf("команда[%d] несёт пароль вне MIGRATE AUTH: %v", i, call)
		}
	}
}

// ============================= explicit topology =============================
//
// Опциональный params.topology задаёт ЯВНУЮ раскладку шардов (оператор сам распихал
// VM по зонам / закодировал anti-affinity в списке). buildClusterPlanExplicit —
// зеркало buildClusterPlan: masters[i]=nodes[topology[i][0]], реплики из хвостов
// (replicaOf=i), слоты — та же allocateSlots(len(topology)). Сборка (MEET/ADDSLOTS/
// REPLICATE), идемпотентность и no-secret-leak переиспользуются (общий clusterPlan).

// topologyParam строит params.topology — список шардов из списков SID.
func topologyParam(shards ...[]string) []any {
	out := make([]any, 0, len(shards))
	for _, shard := range shards {
		inner := make([]any, 0, len(shard))
		for _, sid := range shard {
			inner = append(inner, sid)
		}
		out = append(out, inner)
	}
	return out
}

// namedNodesParam строит params.nodes-map с ЯВНЫМИ ключами (SID) — topology
// ссылается на ноды по ключу, а не по индексу (в отличие от clusterNodesParam).
func namedNodesParam(byKey map[string]string) map[string]any {
	nodes := map[string]any{}
	for key, addr := range byKey {
		nodes[key] = map[string]any{"addr": addr}
	}
	return nodes
}

// --- Validate: topology ---

func TestValidate_ClusterTopology(t *testing.T) {
	nodes := map[string]any{
		"node-a": map[string]any{"addr": "10.0.0.1:6379"},
		"node-b": map[string]any{"addr": "10.0.0.2:6379"},
		"node-c": map[string]any{"addr": "10.0.0.3:6379"},
		"node-d": map[string]any{"addr": "10.0.0.4:6379"},
	}
	cases := []struct {
		name     string
		params   map[string]any
		wantOk   bool
		errMatch string // подстрока в одной из ошибок (если wantOk=false)
	}{
		{
			name: "happy: 2 shards x 1 replica covers all nodes",
			params: map[string]any{
				"action":   "create",
				"nodes":    nodes,
				"topology": topologyParam([]string{"node-a", "node-c"}, []string{"node-b", "node-d"}),
			},
			wantOk: true,
		},
		{
			name: "happy: master-only shard (no replicas) is allowed without replicas_per_shard",
			params: map[string]any{
				"action": "create",
				"nodes": map[string]any{
					"node-a": map[string]any{"addr": "10.0.0.1:6379"},
					"node-b": map[string]any{"addr": "10.0.0.2:6379"},
				},
				"topology": topologyParam([]string{"node-a"}, []string{"node-b"}),
			},
			wantOk: true,
		},
		{
			name: "duplicate SID across shards rejected",
			params: map[string]any{
				"action":   "create",
				"nodes":    nodes,
				"topology": topologyParam([]string{"node-a", "node-c"}, []string{"node-a", "node-d"}),
			},
			wantOk:   false,
			errMatch: "appears 2 times",
		},
		{
			name: "non-existent SID rejected",
			params: map[string]any{
				"action": "create",
				"nodes": map[string]any{
					"node-a": map[string]any{"addr": "10.0.0.1:6379"},
					"node-b": map[string]any{"addr": "10.0.0.2:6379"},
				},
				"topology": topologyParam([]string{"node-a", "node-ZZZ"}, []string{"node-b"}),
			},
			wantOk:   false,
			errMatch: "not found in nodes",
		},
		{
			name: "empty shard rejected",
			params: map[string]any{
				"action": "create",
				"nodes": map[string]any{
					"node-a": map[string]any{"addr": "10.0.0.1:6379"},
				},
				"topology": topologyParam([]string{"node-a"}, []string{}),
			},
			wantOk:   false,
			errMatch: "at least a master SID",
		},
		{
			name: "unused node (not covered by topology) rejected",
			params: map[string]any{
				"action":   "create",
				"nodes":    nodes, // 4 ноды, но topology покрывает только 3
				"topology": topologyParam([]string{"node-a", "node-c"}, []string{"node-b"}),
			},
			wantOk:   false,
			errMatch: "not assigned to any shard",
		},
		{
			name: "replicas_per_shard conflicting with shard size rejected (fail-fast)",
			params: map[string]any{
				"action":             "create",
				"nodes":              nodes,
				"replicas_per_shard": 2, // ждёт шарды по 3 ноды, а они по 2
				"topology":           topologyParam([]string{"node-a", "node-c"}, []string{"node-b", "node-d"}),
			},
			wantOk:   false,
			errMatch: "replicas_per_shard=2 requires 3",
		},
		{
			name: "replicas_per_shard matching shard size accepted",
			params: map[string]any{
				"action":             "create",
				"nodes":              nodes,
				"replicas_per_shard": 1, // шарды по 2 = 1 master + 1 replica → ок
				"topology":           topologyParam([]string{"node-a", "node-c"}, []string{"node-b", "node-d"}),
			},
			wantOk: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &RedisModule{}
			reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
				State:  "cluster",
				Params: mustStruct(t, tc.params),
			})
			if reply.Ok != tc.wantOk {
				t.Fatalf("Ok=%v, ждали %v (errors=%v)", reply.Ok, tc.wantOk, reply.Errors)
			}
			if tc.wantOk {
				return
			}
			joined := strings.Join(reply.Errors, " | ")
			if !strings.Contains(joined, tc.errMatch) {
				t.Fatalf("ошибка не содержит %q: %v", tc.errMatch, reply.Errors)
			}
		})
	}
}

// --- buildClusterPlanExplicit: раскладка из явной топологии ---

func TestBuildClusterPlanExplicit_RolesAndSlots(t *testing.T) {
	// Ключи расставлены так, что АВТО-раскладка (sort) дала бы мастерами node-a/node-b;
	// topology же делает мастерами node-b/node-d — доказывает, что topology перекрывает
	// sort, а не совпадает с ним случайно.
	nodes := []clusterNode{
		{key: "node-a", addr: "10.0.0.1:6379", ip: "10.0.0.1", port: 6379},
		{key: "node-b", addr: "10.0.0.2:6379", ip: "10.0.0.2", port: 6379},
		{key: "node-c", addr: "10.0.0.3:6379", ip: "10.0.0.3", port: 6379},
		{key: "node-d", addr: "10.0.0.4:6379", ip: "10.0.0.4", port: 6379},
	}
	topology := [][]string{
		{"node-b", "node-a"}, // shard 0: master node-b, replica node-a
		{"node-d", "node-c"}, // shard 1: master node-d, replica node-c
	}

	plan, err := buildClusterPlanExplicit(nodes, topology)
	if err != nil {
		t.Fatalf("buildClusterPlanExplicit: %v", err)
	}

	// Мастера — ПЕРВЫЕ SID шардов в порядке topology (не sort).
	if len(plan.masters) != 2 || plan.masters[0].key != "node-b" || plan.masters[1].key != "node-d" {
		t.Fatalf("masters=%v, ждали [node-b node-d]", masterKeys(plan))
	}
	// Реплики — хвосты шардов, replicaOf указывает на их шард.
	if len(plan.replicas) != 2 || len(plan.replicaOf) != 2 {
		t.Fatalf("replicas=%v replicaOf=%v, ждали по 2", replicaKeys(plan), plan.replicaOf)
	}
	if plan.replicas[0].key != "node-a" || plan.replicaOf[0] != 0 {
		t.Errorf("replica[0]=%q->shard%d, ждали node-a->shard0", plan.replicas[0].key, plan.replicaOf[0])
	}
	if plan.replicas[1].key != "node-c" || plan.replicaOf[1] != 1 {
		t.Errorf("replica[1]=%q->shard%d, ждали node-c->shard1", plan.replicas[1].key, plan.replicaOf[1])
	}
	// Слоты — та же равномерная allocateSlots(2): полное покрытие 16384 без дыр.
	if len(plan.slots) != 2 {
		t.Fatalf("slots=%v, ждали 2 диапазона", plan.slots)
	}
	assertFullSlotCoverage(t, plan.slots...)
}

func TestBuildClusterPlanExplicit_MasterOnlyShards(t *testing.T) {
	// Шарды без реплик (master-only) — реплик нет, replicaOf пуст, слоты полны.
	nodes := []clusterNode{
		{key: "node-a", addr: "10.0.0.1:6379", ip: "10.0.0.1", port: 6379},
		{key: "node-b", addr: "10.0.0.2:6379", ip: "10.0.0.2", port: 6379},
		{key: "node-c", addr: "10.0.0.3:6379", ip: "10.0.0.3", port: 6379},
	}
	plan, err := buildClusterPlanExplicit(nodes, [][]string{{"node-a"}, {"node-b"}, {"node-c"}})
	if err != nil {
		t.Fatalf("buildClusterPlanExplicit: %v", err)
	}
	if len(plan.masters) != 3 {
		t.Fatalf("masters=%v, ждали 3", masterKeys(plan))
	}
	if len(plan.replicas) != 0 || len(plan.replicaOf) != 0 {
		t.Fatalf("ждали 0 реплик, got replicas=%v replicaOf=%v", replicaKeys(plan), plan.replicaOf)
	}
	assertFullSlotCoverage(t, plan.slots...)
}

func masterKeys(p clusterPlan) []string {
	out := make([]string, 0, len(p.masters))
	for _, m := range p.masters {
		out = append(out, m.key)
	}
	return out
}

func replicaKeys(p clusterPlan) []string {
	out := make([]string, 0, len(p.replicas))
	for _, r := range p.replicas {
		out = append(out, r.key)
	}
	return out
}

// --- Apply create с явной топологией: MEET/ADDSLOTS/REPLICATE по списку оператора ---

func TestApplyClusterCreate_ExplicitTopology(t *testing.T) {
	addrByKey := map[string]string{
		"node-a": "10.0.0.1:6379",
		"node-b": "10.0.0.2:6379",
		"node-c": "10.0.0.3:6379",
		"node-d": "10.0.0.4:6379",
	}
	addrs := []string{addrByKey["node-a"], addrByKey["node-b"], addrByKey["node-c"], addrByKey["node-d"]}
	fl := newFleet(addrs...)
	fl.setConvergedNodesView()
	m := fl.module()
	stream := &applyStream{}

	// topology: мастера node-b/node-d (НЕ первые по sort), реплики node-a/node-c.
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "create",
			"password": secretPass,
			"nodes":    namedNodesParam(addrByKey),
			"topology": topologyParam([]string{"node-b", "node-a"}, []string{"node-d", "node-c"}),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	masterB := fl.byAddr[addrByKey["node-b"]]
	masterD := fl.byAddr[addrByKey["node-d"]]
	replicaA := fl.byAddr[addrByKey["node-a"]]
	replicaC := fl.byAddr[addrByKey["node-c"]]

	// ADDSLOTS — только мастерам из topology (node-b/node-d), полное покрытие 16384.
	rB := assertAddSlots(t, masterB)
	rD := assertAddSlots(t, masterD)
	assertNoAddSlots(t, replicaA)
	assertNoAddSlots(t, replicaC)
	assertFullSlotCoverage(t, rB, rD)

	// REPLICATE — реплики привязаны к мастеру СВОЕГО шарда (а не round-robin по sort).
	assertReplicateTo(t, replicaA, masterB.id)
	assertReplicateTo(t, replicaC, masterD.id)

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}
