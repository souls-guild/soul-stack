package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// --- INFO replication / CLUSTER NODES фикстуры для failover-takeover ---

// replicaInfoSynced — INFO replication синхронной реплики (линк up): sync-gate
// пропускает, failover можно запускать.
func replicaInfoSynced() string {
	return "role:slave\nmaster_link_status:up\nmaster_host:10.0.0.1\nmaster_port:6379\n"
}

// replicaInfoNotSynced — INFO replication реплики с НЕ-up линком (ещё догоняет):
// sync-gate обязан отказать ДО первого failover.
func replicaInfoNotSynced() string {
	return "role:slave\nmaster_link_status:down\nmaster_host:10.0.0.1\nmaster_port:6379\n"
}

// masterInfo — INFO replication уже промоутнутого master-а (нет master_link_status):
// идемпотентность failover-takeover → no-op.
func masterInfo() string {
	return "role:master\nconnected_slaves:1\n"
}

// promotedNodeRow — строка CLUSTER NODES самого узла как master СО слотами (после
// graceful failover узел взял слоты). waitNodePromoted сходится на ней.
func promotedNodeRow(id, ipPort string, from, to int) string {
	return masterRowWithSlots(id, ipPort, from, to)
}

// hasClusterFailover — нода исполнила хотя бы один CLUSTER FAILOVER.
func hasClusterFailover(c *clusterConn) bool {
	for _, call := range c.calls {
		if isClusterSub(call, "FAILOVER") {
			return true
		}
	}
	return false
}

// assertGracefulFailoverOnly требует, чтобы все CLUSTER FAILOVER на ноде были
// GRACEFUL (ровно "CLUSTER FAILOVER", без аргумента FORCE/TAKEOVER). Это ИБ-ядро
// fail-closed: эскалация на FORCE/TAKEOVER — split-brain, запрещена.
func assertGracefulFailoverOnly(t *testing.T, c *clusterConn) {
	t.Helper()
	for _, call := range c.calls {
		if !isClusterSub(call, "FAILOVER") {
			continue
		}
		for _, a := range call[2:] {
			s, _ := a.(string)
			if strings.EqualFold(s, "FORCE") || strings.EqualFold(s, "TAKEOVER") {
				t.Errorf("CLUSTER FAILOVER эскалирован на %q (split-brain): %v", s, call)
			}
		}
	}
}

// =========================== Validate: failover-takeover =====================

func TestValidate_FailoverTakeoverRejectsEmptyNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "failover-takeover",
			"nodes":  map[string]any{},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой nodes")
	}
}

func TestValidate_FailoverTakeoverHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "failover-takeover",
			"nodes":  clusterNodesParam("10.1.0.1:6379", "10.1.0.2:6379"),
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// =========================== Apply: failover-takeover ========================

// TestApplyFailoverTakeover_HappyPath — две синхронные новые реплики промоутятся
// в мастера: sync-gate проходит (обе master_link_status:up), на каждой GRACEFUL
// CLUSTER FAILOVER, poll видит её master-ом со слотами. changed=true.
func TestApplyFailoverTakeover_HappyPath(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	fl := newFleet(new0, new1)

	// sync-gate: обе реплики синхронны.
	fl.byAddr[new0].infoRepl = replicaInfoSynced()
	fl.byAddr[new1].infoRepl = replicaInfoSynced()
	// После CLUSTER FAILOVER первый же CLUSTER NODES узла показывает его master-ом
	// со слотами (waitNodePromoted сходится сразу).
	fl.byAddr[new0].nodes = promotedNodeRow("n0", new0, 0, 8191)
	fl.byAddr[new1].nodes = promotedNodeRow("n1", new1, 8192, 16383)

	m := fl.module()
	stream := &applyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "failover-takeover",
			"password": secretPass,
			"nodes":    clusterNodesParam(new0, new1),
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// Обе реплики получили GRACEFUL CLUSTER FAILOVER (без FORCE/TAKEOVER).
	if !hasClusterFailover(fl.byAddr[new0]) || !hasClusterFailover(fl.byAddr[new1]) {
		t.Error("ждали CLUSTER FAILOVER на обеих новых репликах")
	}
	assertGracefulFailoverOnly(t, fl.byAddr[new0])
	assertGracefulFailoverOnly(t, fl.byAddr[new1])

	if got := fin.GetOutput().GetFields()["promoted"].GetNumberValue(); got != 2 {
		t.Errorf("promoted=%v, ждали 2", got)
	}
	if got := fin.GetOutput().GetFields()["per_node"].GetStringValue(); !strings.Contains(got, "promoted") {
		t.Errorf("per_node=%q, ждали статус promoted", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// TestApplyFailoverTakeover_SyncGateBlocksBeforeFailover — ОДНА из двух реплик
// ещё не догнала (master_link_status:down). ★sync-gate обязан отказать ДО ЛЮБОГО
// CLUSTER FAILOVER (ранний failover на недогнанной реплике теряет хвост). Ни одна
// нода (включая синхронную) не должна получить FAILOVER.
func TestApplyFailoverTakeover_SyncGateBlocksBeforeFailover(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	fl := newFleet(new0, new1)

	// new0 синхронна, new1 ещё догоняет (link down) → отказ ДО failover.
	fl.byAddr[new0].infoRepl = replicaInfoSynced()
	fl.byAddr[new1].infoRepl = replicaInfoNotSynced()
	// Слоты бы появились, но до них не дойдёт (fail на sync-gate).
	fl.byAddr[new0].nodes = promotedNodeRow("n0", new0, 0, 8191)
	fl.byAddr[new1].nodes = promotedNodeRow("n1", new1, 8192, 16383)

	m := fl.module()
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "failover-takeover",
			"password": secretPass,
			"nodes":    clusterNodesParam(new0, new1),
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на несинхронной реплике, got %+v", fin)
	}
	if !strings.Contains(fin.GetMessage(), "master_link_status") {
		t.Errorf("ждали понятную причину про master_link_status, got %q", fin.GetMessage())
	}
	// КРИТ: НИ ОДНА нода не получила FAILOVER (sync-gate отрабатывает ДО first failover).
	for _, addr := range []string{new0, new1} {
		if hasClusterFailover(fl.byAddr[addr]) {
			t.Errorf("узел %s получил CLUSTER FAILOVER несмотря на несинхронность кластера", addr)
		}
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyFailoverTakeover_FailClosedNoEscalation — graceful CLUSTER FAILOVER НЕ
// завершился: poll CLUSTER NODES всё время видит узел РЕПЛИКОЙ (не стал master).
// ★FAIL-CLOSED: ошибка + НИ ОДНОГО CLUSTER FAILOVER FORCE/TAKEOVER (эскалация
// запрещена — split-brain).
func TestApplyFailoverTakeover_FailClosedNoEscalation(t *testing.T) {
	new0 := "10.1.0.1:6379"
	fl := newFleet(new0)

	fl.byAddr[new0].infoRepl = replicaInfoSynced() // sync-gate проходит
	// CLUSTER NODES узла ВСЕГДА показывает его репликой (graceful не сошёлся).
	fl.byAddr[new0].nodes = clusterNodesTable(
		masterRowSpecForFailover("oldm0", "10.0.0.1:6379"),
		nodeRowSpec{id: "n0", ipPort: new0, masterID: "oldm0"},
	)

	m := fl.module()
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "failover-takeover",
			"password": secretPass,
			"nodes":    clusterNodesParam(new0),
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на незавершённом graceful failover, got %+v", fin)
	}
	// graceful CLUSTER FAILOVER был отправлен (попытка), но эскалации НЕТ.
	if !hasClusterFailover(fl.byAddr[new0]) {
		t.Error("ждали хотя бы один (graceful) CLUSTER FAILOVER")
	}
	assertGracefulFailoverOnly(t, fl.byAddr[new0]) // ★ ни FORCE, ни TAKEOVER
	if !strings.Contains(fin.GetMessage(), "FORCE") && !strings.Contains(fin.GetMessage(), "manually") {
		t.Errorf("ждали сообщение про отказ эскалации / ручное вмешательство, got %q", fin.GetMessage())
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyFailoverTakeover_AlreadyMasterNoOp — оба узла УЖЕ мастера (повторный
// apply, INFO replication role:master) → changed=false, ни одного CLUSTER FAILOVER.
func TestApplyFailoverTakeover_AlreadyMasterNoOp(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	fl := newFleet(new0, new1)
	fl.byAddr[new0].infoRepl = masterInfo()
	fl.byAddr[new1].infoRepl = masterInfo()

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "failover-takeover",
			"nodes":  clusterNodesParam(new0, new1),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: оба узла уже мастера (no-op)")
	}
	// No-op: ни одного FAILOVER.
	for _, addr := range []string{new0, new1} {
		if hasClusterFailover(fl.byAddr[addr]) {
			t.Errorf("узел %s (уже master): FAILOVER не должен вызываться", addr)
		}
	}
	if got := fin.GetOutput().GetFields()["per_node"].GetStringValue(); !strings.Contains(got, "already") {
		t.Errorf("per_node=%q, ждали статус already", got)
	}
}

// TestApplyFailoverTakeover_PartialIdempotent — один узел уже master (no-op),
// второй синхронная реплика (промоутится): changed=true, FAILOVER только на втором.
func TestApplyFailoverTakeover_PartialIdempotent(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	fl := newFleet(new0, new1)
	// new0 уже master (no-op), new1 синхронная реплика (промоутится).
	fl.byAddr[new0].infoRepl = masterInfo()
	fl.byAddr[new1].infoRepl = replicaInfoSynced()
	fl.byAddr[new1].nodes = promotedNodeRow("n1", new1, 8192, 16383)

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action": "failover-takeover",
			"nodes":  clusterNodesParam(new0, new1),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (new1 промоутнут), got %+v", fin)
	}
	if hasClusterFailover(fl.byAddr[new0]) {
		t.Error("new0 (уже master): FAILOVER не должен вызываться")
	}
	if !hasClusterFailover(fl.byAddr[new1]) {
		t.Error("new1 (синхронная реплика): ждали CLUSTER FAILOVER")
	}
	if got := fin.GetOutput().GetFields()["promoted"].GetNumberValue(); got != 1 {
		t.Errorf("promoted=%v, ждали 1", got)
	}
}

// TestApplyFailoverTakeover_ConnectFailNoLeak — коннект к новому узлу падает с
// паролем в тексте → failed, пароль не утекает.
func TestApplyFailoverTakeover_ConnectFailNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":   "failover-takeover",
			"password": secretPass,
			"nodes":    clusterNodesParam("10.1.0.1:6379"),
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// =========================== Validate: forget-external =======================

func TestValidate_ForgetExternalRejectsEmptyNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        map[string]any{},
			"source_nodes": []any{"10.0.0.1:6379"},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой nodes")
	}
}

func TestValidate_ForgetExternalRejectsEmptySourceNodes(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam("10.1.0.1:6379"),
			"source_nodes": []any{},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой source_nodes")
	}
}

func TestValidate_ForgetExternalHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam("10.1.0.1:6379", "10.1.0.2:6379"),
			"source_nodes": []any{"10.0.0.1:6379"},
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// =========================== Apply: forget-external ==========================

// oldClusterTwoMastersOneReplica — топология СТАРОГО кластера: 2 мастера + 1
// реплика (forget-external выкидывает ВСЕХ — и мастеров, и реплик).
func oldClusterTwoMastersOneReplica() string {
	return strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 8191),
		masterRowWithSlots("oldm1", "10.0.0.2:6379", 8192, 16383),
		replicaRow("oldr0", "10.0.0.3:6379", "oldm0"),
	}, "\n")
}

// TestApplyForgetExternal_ForgetsAllOldOnEachNode — два новых узла исполняют
// CLUSTER FORGET всех трёх старых node-id (2 мастера + 1 реплика). changed=true.
func TestApplyForgetExternal_ForgetsAllOldOnEachNode(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)
	fl.byAddr[seed].nodes = oldClusterTwoMastersOneReplica()

	m := fl.module()
	stream := &applyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}

	// КАЖДЫЙ новый узел исполнил FORGET для ВСЕХ трёх старых id.
	for _, addr := range []string{new0, new1} {
		for _, oldID := range []string{"oldm0", "oldm1", "oldr0"} {
			if !hasClusterForget(fl.byAddr[addr], oldID) {
				t.Errorf("узел %s: ждали CLUSTER FORGET %s", addr, oldID)
			}
		}
	}
	// Source-seed сам FORGET не получает (он только источник id).
	if len(clusterForgetTargets(fl.byAddr[seed])) != 0 {
		t.Error("source seed не должен получать CLUSTER FORGET")
	}

	if got := fin.GetOutput().GetFields()["old_nodes"].GetNumberValue(); got != 3 {
		t.Errorf("old_nodes=%v, ждали 3", got)
	}
	// 2 новых × 3 старых = 6 реально забытых пар.
	if got := fin.GetOutput().GetFields()["forgotten"].GetNumberValue(); got != 6 {
		t.Errorf("forgotten=%v, ждали 6 (2 узла × 3 старых)", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// TestApplyForgetExternal_SharedTopologyNoSelfForget — ★пост-join реальность:
// после join-external + failover-takeover новые ноды — члены ТОГО ЖЕ кластера, и
// CLUSTER NODES старого seed перечисляет И старые, И новые ноды (по их РЕАЛЬНЫМ
// id). forget-external обязан забыть ТОЛЬКО старые id и НИ ОДНА новая нода не должна
// получить CLUSTER FORGET своего собственного id (Redis: "can't forget myself" —
// hard-fail). Изолированная old-cluster-фикстура прячет этот баг; здесь топология
// общая.
func TestApplyForgetExternal_SharedTopologyNoSelfForget(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, new1, seed)

	// SHARED-топология: старые мастера/реплика отдали слоты новым (после failover),
	// старые — пустые мастера; новые ноды (n0/n1) — члены кластера с РЕАЛЬНЫМИ id.
	// clusterNodesParam даёт ключи node-0/node-1, но id в топологии — собственные у
	// каждой ноды (как реальный CLUSTER NODES).
	const n0ID, n1ID = "newid0", "newid1"
	fl.byAddr[seed].nodes = strings.Join([]string{
		masterRowNoSlots("oldm0", "10.0.0.1:6379"), // старый мастер, слоты ушли
		masterRowNoSlots("oldm1", "10.0.0.2:6379"),
		replicaRow("oldr0", "10.0.0.4:6379", "oldm0"),
		masterRowWithSlots(n0ID, new0, 0, 8191), // новая нода — теперь master СО слотами
		masterRowWithSlots(n1ID, new1, 8192, 16383),
	}, "\n")

	m := fl.module()
	stream := &applyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (self-forget исключён), got %+v", fin)
	}
	if !fin.Changed {
		t.Error("ждали changed=true (старые забыты)")
	}

	// ★КРИТ: ни одна новая нода не форгетит СВОЙ id (и id соседней новой ноды).
	for _, addr := range []string{new0, new1} {
		for _, selfish := range []string{n0ID, n1ID} {
			if hasClusterForget(fl.byAddr[addr], selfish) {
				t.Errorf("узел %s форгетит НОВУЮ ноду %s (self-forget / forget-peer): %v",
					addr, selfish, clusterForgetTargets(fl.byAddr[addr]))
			}
		}
	}
	// Каждая новая нода забыла ровно старые id (3 старых).
	for _, addr := range []string{new0, new1} {
		for _, oldID := range []string{"oldm0", "oldm1", "oldr0"} {
			if !hasClusterForget(fl.byAddr[addr], oldID) {
				t.Errorf("узел %s: ждали CLUSTER FORGET старого %s", addr, oldID)
			}
		}
	}
	// oldIDs = 3 (новые отфильтрованы из топологии seed), forgotten = 2 узла × 3.
	if got := fin.GetOutput().GetFields()["old_nodes"].GetNumberValue(); got != 3 {
		t.Errorf("old_nodes=%v, ждали 3 (новые ноды отфильтрованы)", got)
	}
	if got := fin.GetOutput().GetFields()["forgotten"].GetNumberValue(); got != 6 {
		t.Errorf("forgotten=%v, ждали 6 (2 узла × 3 старых)", got)
	}

	assertEventsNoSecret(t, stream)
	for addr, c := range fl.byAddr {
		assertNoClusterSecret(t, addr, c)
	}
}

// TestApplyForgetExternal_CantForgetSelfSwallowed — defense-in-depth: даже если
// id новой ноды просочился в oldIDs (gossip-гонка между фильтром и FORGET, ip-форма
// разошлась), Redis-ответ "I can't forget myself" глотается как идемпотентность —
// прогон НЕ падает.
func TestApplyForgetExternal_CantForgetSelfSwallowed(t *testing.T) {
	new0 := "10.1.0.1:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, seed)
	fl.byAddr[seed].nodes = strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.2:6379", 0, 16383),
	}, "\n")
	// Нода на FORGET отвечает "can't forget myself" (как если бы id был её собственный).
	fl.byAddr[new0].forgetErr = errors.New("ERR I tried hard but I can't forget myself...")

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam(new0),
			"source_nodes": []any{seed},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (can't-forget-myself проглочен), got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["forgotten"].GetNumberValue(); got != 0 {
		t.Errorf("forgotten=%v, ждали 0 (self-forget проглочен)", got)
	}
}

// TestApplyForgetExternal_OnlyNewNodesLeftNoOp — старых в топологии seed уже не
// осталось (все забыты предыдущим apply), seed перечисляет ТОЛЬКО новые ноды:
// oldIDs пуст → changed=false, ни одного FORGET (steady-state no-op, не ошибка).
func TestApplyForgetExternal_OnlyNewNodesLeftNoOp(t *testing.T) {
	new0, new1 := "10.1.0.1:6379", "10.1.0.2:6379"
	seed := new0 // seed теперь сама новая нода (старые погашены, оператор дал новый seed)
	fl := newFleet(new0, new1)
	fl.byAddr[new0].nodes = strings.Join([]string{
		masterRowWithSlots("newid0", new0, 0, 8191),
		masterRowWithSlots("newid1", new1, 8192, 16383),
	}, "\n")

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam(new0, new1),
			"source_nodes": []any{seed},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (no-op), got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: старых не осталось (steady-state no-op)")
	}
	for _, addr := range []string{new0, new1} {
		if len(clusterForgetTargets(fl.byAddr[addr])) != 0 {
			t.Errorf("узел %s: ни одного FORGET не ждали (старых нет): %v", addr, clusterForgetTargets(fl.byAddr[addr]))
		}
	}
	if got := fin.GetOutput().GetFields()["old_nodes"].GetNumberValue(); got != 0 {
		t.Errorf("old_nodes=%v, ждали 0", got)
	}
}

// TestApplyForgetExternal_NoSlotMigration — forget-external НЕ мигрирует слоты
// (в отличие от remove-node): слоты уже у новых мастеров после failover. Ни
// SETSLOT, ни MIGRATE.
func TestApplyForgetExternal_NoSlotMigration(t *testing.T) {
	new0 := "10.1.0.1:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, seed)
	fl.byAddr[seed].nodes = oldClusterTwoMastersOneReplica()

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam(new0),
			"source_nodes": []any{seed},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	for addr, c := range fl.byAddr {
		for _, call := range c.calls {
			if isClusterSub(call, "SETSLOT") {
				t.Errorf("нода %s: SETSLOT в forget-external не должен вызываться: %v", addr, call)
			}
			if v0, _ := call[0].(string); strings.EqualFold(v0, "MIGRATE") {
				t.Errorf("нода %s: MIGRATE в forget-external не должен вызываться: %v", addr, call)
			}
		}
	}
}

// TestApplyForgetExternal_UnknownNodeIdempotent — старый id уже неизвестен новой
// ноде (повторный apply / gossip уже забыл): FORGET вернул "Unknown node" →
// глотается как идемпотентность, прогон успешен, changed=false (ничего не забыто).
func TestApplyForgetExternal_UnknownNodeIdempotent(t *testing.T) {
	new0 := "10.1.0.1:6379"
	seed := "10.0.0.1:6379"
	fl := newFleet(new0, seed)
	fl.byAddr[seed].nodes = strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 16383),
	}, "\n")
	// Новая нода на КАЖДЫЙ FORGET отвечает "Unknown node" (уже забыла старого).
	fl.byAddr[new0].forgetErr = errors.New("ERR Unknown node oldm0")

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam(new0),
			"source_nodes": []any{seed},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (Unknown node проглочен), got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: все старые уже забыты (no-op)")
	}
	if got := fin.GetOutput().GetFields()["forgotten"].GetNumberValue(); got != 0 {
		t.Errorf("forgotten=%v, ждали 0 (все Unknown node)", got)
	}
}

// TestApplyForgetExternal_SourceSeedFailoverToNext — первый source-seed недоступен,
// второй отвечает: старые id берутся со второго.
func TestApplyForgetExternal_SourceSeedFailoverToNext(t *testing.T) {
	new0 := "10.1.0.1:6379"
	seedDown, seedUp := "10.0.0.9:6379", "10.0.0.1:6379"
	fl := newFleet(new0, seedUp) // seedDown не заведён → коннект к нему фейлит
	fl.byAddr[seedUp].nodes = strings.Join([]string{
		masterRowWithSlots("oldm0", "10.0.0.1:6379", 0, 16383),
	}, "\n")

	m := fl.module()
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"nodes":        clusterNodesParam(new0),
			"source_nodes": []any{seedDown, seedUp},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех через второй seed, got %+v", fin)
	}
	if !hasClusterForget(fl.byAddr[new0], "oldm0") {
		t.Error("ждали CLUSTER FORGET oldm0 (id со второго seed)")
	}
	if got := fin.GetOutput().GetFields()["source_via"].GetStringValue(); got != seedUp {
		t.Errorf("source_via=%q, ждали %q (второй seed)", got, seedUp)
	}
}

// TestApplyForgetExternal_AllSourceSeedsDown — все старые seed недоступны (старый
// кластер уже погашен): id взять неоткуда → failed (не идемпотентный путь — мы не
// знаем, что забывать). Пароль не утекает.
func TestApplyForgetExternal_AllSourceSeedsDown(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":       "forget-external",
			"password":     secretPass,
			"nodes":        clusterNodesParam("10.1.0.1:6379"),
			"source_nodes": []any{"10.0.0.1:6379"},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на всех недоступных seed, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// masterRowSpecForFailover — мастер-строка nodeRowSpec для fail-closed-теста (узел
// остаётся репликой этого мастера). Локальный хелпер читаемости.
func masterRowSpecForFailover(id, ipPort string) nodeRowSpec {
	return nodeRowSpec{id: id, ipPort: ipPort, master: true}
}
