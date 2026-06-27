package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// masterRowWithSlots строит строку CLUSTER NODES для master-а СО слот-диапазоном
// (clusterNodesTable слоты не несёт, а join-external их разбирает для сортировки
// маппинга 1:1). Формат: <id> <ip:port@cport> master - 0 0 0 connected <from-to>.
func masterRowWithSlots(id, ipPort string, from, to int) string {
	return fmt.Sprintf("%s %s@%s master - 0 0 0 connected %d-%d", id, ipPort, ipPort, from, to)
}

// sourceClusterTwoMasters — топология старого кластера из двух мастеров со
// слотами (m0 владеет нижней половиной, m1 — верхней): база happy-path и проверки
// сортировки маппинга по возрастанию первого слота.
func sourceClusterTwoMasters() string {
	return strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 8191),
		masterRowWithSlots("oldm1", "10.0.0.2:6379", 8192, 16383),
	}, "\n")
}

// ownIsolated — CLUSTER NODES свежего изолированного нового узла (одна строка:
// он сам как master без слотов) → alreadyReplicaOf=false, идём вливать.
func ownIsolated(id, ipPort string) string {
	return clusterNodesTable(nodeRowSpec{id: id, ipPort: ipPort, master: true})
}

// --- Validate: join-external ---

func TestValidate_JoinExternalRejectsEmptyNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        map[string]any{},
			"source_nodes": []any{"10.0.0.1:6379"},
			"shards_dest":  1,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой nodes")
	}
}

func TestValidate_JoinExternalRejectsEmptySourceNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam("10.1.0.1:6379"),
			"source_nodes": []any{},
			"shards_dest":  1,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой source_nodes")
	}
}

func TestValidate_JoinExternalRejectsBadShardsDest(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam("10.1.0.1:6379"),
			"source_nodes": []any{"10.0.0.1:6379"},
			"shards_dest":  0,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на shards_dest < 1")
	}
}

func TestValidate_JoinExternalHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam("10.1.0.1:6379", "10.1.0.2:6379"),
			"source_nodes": []any{"10.0.0.1:6379"},
			"shards_dest":  2,
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply join-external: happy-path (NODES → MEET → REPLICATE, 1:1 по слотам) ---

// TestApplyJoinExternal_HappyPath — два новых узла вливаются в старый кластер из
// двух мастеров: каждый шлёт MEET на source-seed, ждёт сходимости, REPLICATE
// смаппленного мастера. Маппинг 1:1 ПО СЛОТАМ: новый узел с меньшим ключом ↔
// старый мастер с меньшим первым слотом.
func TestApplyJoinExternal_HappyPath(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)

	// Source-seed отдаёт топологию старого кластера (2 мастера со слотами).
	fl.byAddr[seed].nodes = sourceClusterTwoMasters()

	// Новый узел: первый CLUSTER NODES (до MEET) — изолированный (он сам, master
	// без master-id) → не реплика; после MEET — видит целевого старого мастера
	// (его id в топологии) → waitNodeKnows сходится.
	converged := sourceClusterTwoMasters()
	fl.byAddr[new0].nodesSeq = []string{ownIsolated("n0", new0), converged}
	fl.byAddr[new1].nodesSeq = []string{ownIsolated("n1", new1), converged}

	m := fl.module()
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
			"shards_dest":  2,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// Маппинг 1:1 по слотам: node-0 (clusterNodesParam ключ "node-0") ↔ oldm0
	// (слоты 0-8191), node-1 ↔ oldm1 (8192-16383).
	assertReplicateTo(t, fl.byAddr[new0], "oldm0")
	assertReplicateTo(t, fl.byAddr[new1], "oldm1")

	// Каждый новый узел слал MEET именно на source-seed ip:port.
	assertMeetTargets(t, fl.byAddr[new0], []string{seed})
	assertMeetTargets(t, fl.byAddr[new1], []string{seed})

	// Source-seed НЕ получает MEET/REPLICATE — он лишь источник топологии.
	for _, call := range fl.byAddr[seed].calls {
		if isClusterSub(call, "MEET") || isClusterSub(call, "REPLICATE") {
			t.Errorf("source seed не должен получать MEET/REPLICATE: %v", call)
		}
	}

	if got := fin.GetOutput().GetFields()["mapping"].GetStringValue(); got != "node-0->oldm0,node-1->oldm1" {
		t.Errorf("mapping=%q, ждали node-0->oldm0,node-1->oldm1", got)
	}
	if got := fin.GetOutput().GetFields()["shards"].GetNumberValue(); got != 2 {
		t.Errorf("shards=%v, ждали 2", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// TestApplyJoinExternal_MappingByFirstSlotNotID — порядок строк CLUSTER NODES и
// node-id НЕ влияют на маппинг: он строго по возрастанию первого слота. Старый
// мастер с алфавитно-БОЛЬШИМ id, но МЕНЬШИМ слотом обязан смаппиться на первый
// (по ключу) новый узел.
func TestApplyJoinExternal_MappingByFirstSlotNotID(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)

	// zzz владеет нижними слотами 0-8191, aaa — верхними 8192-16383. По id-сортировке
	// первым был бы aaa; по слот-сортировке (правильной) — zzz.
	src := strings.Join([]string{
		masterRowWithSlots("aaa", "10.0.0.2:6379", 8192, 16383),
		masterRowWithSlots("zzz", "10.0.0.1:6379", 0, 8191),
	}, "\n")
	fl.byAddr[seed].nodes = src
	fl.byAddr[new0].nodesSeq = []string{ownIsolated("n0", new0), src}
	fl.byAddr[new1].nodesSeq = []string{ownIsolated("n1", new1), src}

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
			"shards_dest":  2,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	// node-0 ↔ zzz (первый слот 0), node-1 ↔ aaa (первый слот 8192).
	assertReplicateTo(t, fl.byAddr[new0], "zzz")
	assertReplicateTo(t, fl.byAddr[new1], "aaa")
}

// --- Apply join-external: FAIL-FAST на shards-mismatch ---

// TestApplyJoinExternal_FailFastShardsMismatch — у источника 3 мастера, dest
// ждёт 2 шарда (и подаёт 2 новых узла) → 1:1 невозможен → failed, REPLICATE/MEET
// на новых узлах НЕ выполняется. Пароль не течёт.
func TestApplyJoinExternal_FailFastShardsMismatch(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)

	// Источник: 3 мастера со слотами (3 шарда).
	fl.byAddr[seed].nodes = strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 5460),
		masterRowWithSlots("oldm1", "10.0.0.2:6379", 5461, 10922),
		masterRowWithSlots("oldm2", "10.0.0.3:6379", 10923, 16383),
	}, "\n")

	m := fl.module()
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam(new0, new1), // 2 новых узла
			"source_nodes": []any{seed},
			"shards_dest":  2, // ждём 2, у источника 3
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на shards-mismatch (3 source masters vs 2 dest), got %+v", fin)
	}
	if !strings.Contains(fin.GetMessage(), "3 masters") || !strings.Contains(fin.GetMessage(), "2 shards") {
		t.Errorf("ждали понятную ошибку про 3 masters / 2 shards, got %q", fin.GetMessage())
	}
	// Новые узлы НЕ трогались: ни MEET, ни REPLICATE (fail до вливания).
	for _, addr := range []string{new0, new1} {
		for _, call := range fl.byAddr[addr].calls {
			if isClusterSub(call, "MEET") || isClusterSub(call, "REPLICATE") {
				t.Errorf("узел %s тронут при fail-fast: %v", addr, call)
			}
		}
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyJoinExternal_FailFastNodesCountMismatch — число новых узлов != shards_dest
// (статически непроверяемо относительно живого источника, но узлы↔shards_dest
// сверяются до коннекта) → failed, источник даже не опрашивается.
func TestApplyJoinExternal_FailFastNodesCountMismatch(t *testing.T) {
	new0 := "10.1.0.1:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, seed)
	fl.byAddr[seed].nodes = sourceClusterTwoMasters()

	m := fl.module()
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam(new0), // 1 узел
			"source_nodes": []any{seed},
			"shards_dest":  2, // ждём 2
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на nodes!=shards_dest, got %+v", fin)
	}
	// Источник не опрашивался — fail до коннекта к seed.
	if len(fl.byAddr[seed].calls) != 0 {
		t.Errorf("source seed не должен опрашиваться при nodes!=shards_dest: %v", fl.byAddr[seed].calls)
	}
}

// --- Apply join-external: идемпотентность (узел уже реплика нужного мастера) ---

// TestApplyJoinExternal_AlreadyReplicaNoOp — оба новых узла УЖЕ реплики своих
// смаппленных старых мастеров (повторный apply) → changed=false, ни MEET, ни
// REPLICATE не шлются.
func TestApplyJoinExternal_AlreadyReplicaNoOp(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)
	fl.byAddr[seed].nodes = sourceClusterTwoMasters()

	// CLUSTER NODES каждого узла УЖЕ содержит его строку как реплику нужного
	// мастера (n0 → oldm0, n1 → oldm1) → alreadyReplicaOf=true.
	n0Topo := strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 8191),
		clusterNodesTable(nodeRowSpec{id: "n0", ipPort: new0, masterID: "oldm0"}),
	}, "\n")
	n1Topo := strings.Join([]string{
		masterRowWithSlots("oldm1", "10.0.0.2:6379", 8192, 16383),
		clusterNodesTable(nodeRowSpec{id: "n1", ipPort: new1, masterID: "oldm1"}),
	}, "\n")
	fl.byAddr[new0].nodes = n0Topo
	fl.byAddr[new1].nodes = n1Topo

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
			"shards_dest":  2,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: оба узла уже реплики нужных мастеров (no-op)")
	}
	// No-op: ни MEET, ни REPLICATE на новых узлах.
	for _, addr := range []string{new0, new1} {
		for _, call := range fl.byAddr[addr].calls {
			if isClusterSub(call, "MEET") || isClusterSub(call, "REPLICATE") {
				t.Errorf("узел %s: no-op нарушен, вызвана %v", addr, call)
			}
		}
	}
	if got := fin.GetOutput().GetFields()["per_node"].GetStringValue(); !strings.Contains(got, "already") {
		t.Errorf("per_node=%q, ждали статус already", got)
	}
}

// TestApplyJoinExternal_PartialIdempotent — один узел уже реплика, второй ещё нет:
// changed=true (второй влит), первый не трогается.
func TestApplyJoinExternal_PartialIdempotent(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)
	fl.byAddr[seed].nodes = sourceClusterTwoMasters()

	// n0 уже реплика oldm0 (no-op), n1 ещё изолирован (вливается).
	fl.byAddr[new0].nodes = strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 8191),
		clusterNodesTable(nodeRowSpec{id: "n0", ipPort: new0, masterID: "oldm0"}),
	}, "\n")
	fl.byAddr[new1].nodesSeq = []string{ownIsolated("n1", new1), sourceClusterTwoMasters()}

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
			"shards_dest":  2,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (n1 влит), got %+v", fin)
	}
	// n0 не трогался.
	for _, call := range fl.byAddr[new0].calls {
		if isClusterSub(call, "MEET") || isClusterSub(call, "REPLICATE") {
			t.Errorf("n0 (уже реплика) не должен трогаться: %v", call)
		}
	}
	// n1 влит: MEET seed + REPLICATE oldm1.
	assertMeetTargets(t, fl.byAddr[new1], []string{seed})
	assertReplicateTo(t, fl.byAddr[new1], "oldm1")
}

// --- Apply join-external: коннект к source-seed падает не утекая паролем ---

func TestApplyJoinExternal_SourceConnectFailNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam("10.1.0.1:6379", "10.1.0.2:6379"),
			"source_nodes": []any{"10.0.0.1:6379"},
			"shards_dest":  2,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyJoinExternal_SourceSeedFailoverToNext — первый source-seed недоступен,
// второй отвечает: топология берётся со второго (как redis-cli с первым
// достижимым узлом).
func TestApplyJoinExternal_SourceSeedFailoverToNext(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seedDown, seedUp := "10.0.0.9:6379", "10.0.0.1:6379"
	// seedDown в fleet НЕ заводим → connect к нему вернёт ошибку, перебор идёт дальше.
	fl := newFleet(new0, new1, seedUp)
	fl.byAddr[seedUp].nodes = sourceClusterTwoMasters()
	conv := sourceClusterTwoMasters()
	fl.byAddr[new0].nodesSeq = []string{ownIsolated("n0", new0), conv}
	fl.byAddr[new1].nodesSeq = []string{ownIsolated("n1", new1), conv}

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "join-external",
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seedDown, seedUp},
			"shards_dest":  2,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех через второй seed, got %+v", fin)
	}
	// MEET ушёл на ВТОРОЙ (живой) seed, не на первый (мёртвый).
	assertMeetTargets(t, fl.byAddr[new0], []string{seedUp})
	if got := fin.GetOutput().GetFields()["source_via"].GetStringValue(); got != seedUp {
		t.Errorf("source_via=%q, ждали %q (второй seed)", got, seedUp)
	}
}
