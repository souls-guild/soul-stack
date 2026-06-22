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
	cfg    connConfig
	id     string // ответ на CLUSTER MYID
	info   string // ответ на CLUSTER INFO (пусто → форма «не сформирован»)
	nodes  string // ответ на CLUSTER NODES (пусто → ровно nodesCount строк)
	calls  [][]any
	closed bool
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
				return c.nodes, nil
			}
		}
	}
	return "OK", nil
}

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
// на ноду. countClusterNodes считает строки, нам важно только их число.
func (fl *clusterFleet) setConvergedNodesView() {
	lines := make([]string, 0, len(fl.byAddr))
	for addr := range fl.byAddr {
		lines = append(lines, "id "+addr+" myself,master - 0 0 connected")
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
		t.Fatal("ждали Ok=false на нереализованный action (только create)")
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
