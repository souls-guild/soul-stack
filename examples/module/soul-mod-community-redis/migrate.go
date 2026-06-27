// migrate-cluster (action=join-external) плагина community.redis — вступление
// НОВЫХ cluster-mode нод в УЖЕ существующий (старый) Redis-кластер с репликацией
// старых мастеров. Первый шаг миграции «старый кластер → новый кластер той же
// топологии»: новые ноды становятся репликами старых мастеров 1:1, догоняют их
// данные. Graceful failover (промоушен новых в мастера) + forget старых —
// ОТДЕЛЬНАЯ операция (следующий батч), здесь её НЕТ.
//
// ЦЕЛИКОМ через go-redis (CLUSTER NODES / MEET / REPLICATE), как create/add-node:
// никакого redis-cli/shell, capability остаётся network_outbound. ТА ЖЕ сеть и
// ТОТ ЖЕ пароль кластера (оператор выравнивает новый пароль == старый до запуска)
// — единый password/tls на старые seed-ноды и на новые узлы.
//
// Маппинг 1:1 ДЕТЕРМИНИРОВАН: новые узлы сортируются по ключу nodes-map (как
// buildClusterPlan), старые мастера — по возрастанию первого слот-диапазона.
// i-й новый узел реплицирует i-го старого мастера. Это требует РОВНО столько же
// новых узлов, сколько старых мастеров (shards_dest == shards_source) — иначе
// fail-fast (1:1 невозможен). shards_source render-фазе не виден (он в живой
// топологии старого кластера), поэтому проверка — runtime-assert в Apply.
//
// Идемпотентен: узел уже реплика нужного старого мастера (CLUSTER NODES узла) →
// для него no-op; повторный apply на сошедшемся входе → changed=false.
package main

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// validateClusterJoinExternal — статические проверки join-external: непустой
// nodes-map, непустой source_nodes, shards_dest >= 1. Соответствие shards_dest
// числу новых узлов И числу старых мастеров проверяется в Apply (число старых
// мастеров видно только в живой топологии). Тексты без пароля.
func validateClusterJoinExternal(f map[string]*structpb.Value) []string {
	var errs []string

	if len(nodeSpecs(f["nodes"])) == 0 {
		errs = append(errs, "params.nodes: must be a non-empty map (key -> {addr|ip+port}) of the NEW cluster nodes")
	}
	if len(stringList(f["source_nodes"])) == 0 {
		errs = append(errs, "params.source_nodes: must be a non-empty list of seed nodes (host:port) of the SOURCE cluster")
	}
	if intOrDefault(f["shards_dest"], 0) < 1 {
		errs = append(errs, "params.shards_dest: must be an integer >= 1")
	}

	return errs
}

// applyClusterJoinExternal вливает новые cluster-mode ноды в старый кластер и
// делает каждую репликой смаппленного старого мастера (day-2 migration step 1):
//
//  1. коннект к первой source-seed → CLUSTER NODES → старые мастера + их слоты;
//  2. fail-fast: число старых мастеров != shards_dest → 1:1 невозможен;
//  3. маппинг 1:1 — новый узел i (сортировка ключей nodes) ↔ старый мастер i
//     (сортировка по возрастанию слот-диапазона);
//  4. на каждом новом узле: MEET old-seed → waitGossipConverged → REPLICATE
//     old-master-id (идемпотентно: уже реплика нужного мастера → no-op).
//
// Финальный Output несёт маппинг (новый-ключ → старый-master-id) и per-node
// join-статус (joined|already). Пароль НЕ попадает в события (ИБ ADR-010).
func (m *RedisModule) applyClusterJoinExternal(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])
	shardsDest := intOrDefault(f["shards_dest"], 0)

	newNodes, err := parseClusterNodes(f["nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	sources, err := parseSourceSeeds(f["source_nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	// Число НОВЫХ узлов обязано совпасть с shards_dest (1:1 ровно по одной реплике
	// на старый мастер — пилот не делает >1 реплику и не оставляет узлы без пары).
	if len(newNodes) != shardsDest {
		return sendFailure(stream, fmt.Sprintf(
			"params.nodes: %d new nodes != shards_dest %d (join-external maps exactly one new node per source master)",
			len(newNodes), shardsDest))
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			// TLS-handshake-ошибка теоретически несёт PEM client-key — редактируем
			// его ПРЯМО тут (пароль редактируется caller-ом по тексту отдельно).
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	// Топология СТАРОГО кластера с первой доступной source-seed. Перебираем seed-ы
	// по порядку: первый ответивший CLUSTER NODES задаёт топологию (как redis-cli,
	// который берёт первый достижимый узел).
	srcMasters, seedEndpoint, err := sourceMasters(ctx, connect, sources, password)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	// FAIL-FAST: 1:1 маппинг возможен только при равном числе старых мастеров и
	// новых узлов (= shards_dest). shards_source render-фазе не виден (живая
	// топология), поэтому assert здесь.
	if len(srcMasters) != shardsDest {
		return sendFailure(stream, fmt.Sprintf(
			"source cluster has %d masters, dest expects %d shards — 1:1 mapping impossible (align shards_dest with source master count)",
			len(srcMasters), shardsDest))
	}

	// Маппинг 1:1: новый узел i ↔ старый мастер i. newNodes уже отсортированы по
	// ключу (parseClusterNodes), srcMasters — по возрастанию слот-диапазона.
	results := make([]joinResult, len(newNodes))
	mapping := make(map[string]any, len(newNodes))
	for i, node := range newNodes {
		master := srcMasters[i]
		res, err := joinNodeToMaster(ctx, connect, node, master, seedEndpoint, password)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		results[i] = res
		mapping[node.key] = master.id
	}

	joined := 0
	statuses := make([]string, len(results))
	for i, r := range results {
		statuses[i] = r.node.key + "=" + r.status + "->" + r.masterID
		if r.status == "joined" {
			joined++
		}
	}

	return sendOutcome(stream, joined > 0, fmt.Sprintf(
		"join-external: %d/%d new nodes joined source cluster (%d masters replicated 1:1)",
		joined, len(newNodes), len(srcMasters)),
		map[string]any{
			"shards":     int64(len(srcMasters)),
			"joined":     int64(joined),
			"nodes":      int64(len(newNodes)),
			"mapping":    mappingSummary(results),
			"per_node":   strings.Join(statuses, ","),
			"source_via": seedEndpoint,
		})
}

// joinResult — итог вступления одного нового узла: статус (joined|already) и
// node-id старого мастера, чьей репликой узел стал.
type joinResult struct {
	node     clusterNode
	masterID string
	status   string // "joined" (выполнили REPLICATE) | "already" (уже реплика)
}

// joinNodeToMaster вливает один новый узел в старый кластер и делает его репликой
// заданного старого мастера. Идемпотентно: узел уже реплика этого мастера
// (по его CLUSTER NODES) → status "already", REPLICATE НЕ шлём.
//
//	MEET old-seed (по ip:port) → waitGossipConverged (узел увидел старый кластер)
//	→ REPLICATE old-master-id (узел становится репликой смаппленного мастера).
func joinNodeToMaster(ctx context.Context, connect func(clusterNode) (redisConn, error), node clusterNode, master sourceMaster, seedEndpoint, password string) (joinResult, error) {
	conn, err := connect(node)
	if err != nil {
		return joinResult{}, fmt.Errorf("connect new node %s: %w", node.addr, err)
	}
	defer func() { _ = conn.Close() }()

	// Идемпотентность: узел уже реплика нужного мастера → no-op (его CLUSTER NODES
	// несёт строку самого узла с master-id == целевому). На изолированном свежем
	// узле своя строка master-id пуста → пойдём вливать.
	own, err := conn.Do(ctx, "CLUSTER", "NODES")
	if err != nil {
		return joinResult{}, fmt.Errorf("CLUSTER NODES on new node %s: %w", node.addr, err)
	}
	if alreadyReplicaOf(parseClusterNodesTable(own), node, master.id) {
		return joinResult{node: node, masterID: master.id, status: "already"}, nil
	}

	// MEET старого seed-а: узел знакомится с gossip старого кластера по ip:port.
	seedIP, seedPort, err := splitIPPort(seedEndpoint)
	if err != nil {
		return joinResult{}, fmt.Errorf("source seed %q: %w", seedEndpoint, err)
	}
	if _, err := conn.Do(ctx, "CLUSTER", "MEET", seedIP, strconv.Itoa(seedPort)); err != nil {
		return joinResult{}, fmt.Errorf("CLUSTER MEET %s from %s: %w",
			net.JoinHostPort(seedIP, strconv.Itoa(seedPort)), node.addr, err)
	}

	// Сходимость: узел обязан увидеть весь старый кластер + себя. Ждём, пока в его
	// CLUSTER NODES появится хотя бы целевой мастер (его id) — иначе REPLICATE
	// упрётся в неизвестный node-id.
	if err := waitNodeKnows(ctx, conn, master.id); err != nil {
		return joinResult{}, fmt.Errorf("new node %s: %w", node.addr, err)
	}

	if _, err := conn.Do(ctx, "CLUSTER", "REPLICATE", master.id); err != nil {
		return joinResult{}, fmt.Errorf("CLUSTER REPLICATE %s on %s: %w", master.id, node.addr, err)
	}
	return joinResult{node: node, masterID: master.id, status: "joined"}, nil
}

// sourceMaster — старый мастер из топологии источника: node-id + первый слот
// (ключ детерминированной сортировки маппинга 1:1).
type sourceMaster struct {
	id        string
	ipPort    string
	firstSlot int
}

// sourceMasters коннектится к source-seed-ам по порядку, берёт CLUSTER NODES с
// первой ответившей и возвращает её мастеров СО слотами, отсортированных по
// возрастанию первого слот-диапазона (детерминированная база маппинга 1:1).
// Возвращает также endpoint (ip:port) сработавшего seed-а для последующих MEET.
// Мастера БЕЗ слотов (свежие пустые) в маппинг не идут — реплицируем владельцев
// данных. Все seed-ы недоступны → ошибка.
func sourceMasters(ctx context.Context, connect func(clusterNode) (redisConn, error), seeds []clusterNode, password string) ([]sourceMaster, string, error) {
	var lastErr error
	for _, seed := range seeds {
		conn, err := connect(seed)
		if err != nil {
			lastErr = fmt.Errorf("connect source seed %s: %w", seed.addr, err)
			continue
		}
		topology, err := conn.Do(ctx, "CLUSTER", "NODES")
		_ = conn.Close()
		if err != nil {
			lastErr = fmt.Errorf("CLUSTER NODES on source seed %s: %w", seed.addr, err)
			continue
		}

		masters := mastersWithSlots(parseClusterNodesTable(topology))
		if len(masters) == 0 {
			lastErr = fmt.Errorf("source seed %s: no masters with assigned slots in CLUSTER NODES", seed.addr)
			continue
		}
		return masters, net.JoinHostPort(seed.ip, strconv.Itoa(seed.port)), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("params.source_nodes: no reachable source seed")
	}
	return nil, "", lastErr
}

// mastersWithSlots извлекает из топологии мастеров СО слотами, отсортированных по
// возрастанию первого слота (детерминированный 1:1-маппинг). Мастер без слотов
// данных не владеет → в миграционный маппинг не берётся.
func mastersWithSlots(table []clusterNodeRow) []sourceMaster {
	var out []sourceMaster
	for _, row := range table {
		if !row.isMaster || len(row.slots) == 0 {
			continue
		}
		first := row.slots[0].from
		for _, r := range row.slots[1:] {
			if r.from < first {
				first = r.from
			}
		}
		out = append(out, sourceMaster{id: row.id, ipPort: row.ipPort, firstSlot: first})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].firstSlot != out[j].firstSlot {
			return out[i].firstSlot < out[j].firstSlot
		}
		return out[i].id < out[j].id // tie-break (диапазоны не пересекаются — почти не нужен)
	})
	return out
}

// alreadyReplicaOf — несёт ли топология самого узла строку с его ip:port, уже
// привязанную как реплика к masterID (идемпотентность join). Свежий изолированный
// узел свою строку как master с пустым master-id → false.
func alreadyReplicaOf(table []clusterNodeRow, node clusterNode, masterID string) bool {
	row := findNodeRow(table, node)
	return row != nil && !row.isMaster && row.masterID == masterID
}

// parseSourceSeeds резолвит список source_nodes (host:port-строки старого
// кластера) в clusterNode для коннекта + MEET. key = "source-<i>" (для сообщений).
func parseSourceSeeds(v *structpb.Value) ([]clusterNode, error) {
	raw := stringList(v)
	if len(raw) == 0 {
		return nil, fmt.Errorf("params.source_nodes: must be a non-empty list of host:port")
	}
	out := make([]clusterNode, 0, len(raw))
	for i, s := range raw {
		ip, port, err := splitIPPort(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("params.source_nodes[%d] %q: %w", i, s, err)
		}
		out = append(out, clusterNode{
			key:  "source-" + strconv.Itoa(i),
			addr: net.JoinHostPort(ip, strconv.Itoa(port)),
			ip:   ip,
			port: port,
		})
	}
	return out, nil
}

// waitNodeKnows ждёт (ограниченный retry, переиспользует gossip-таймауты), пока
// CLUSTER NODES узла начнёт содержать строку с masterID — узел узнал целевого
// старого мастера и REPLICATE по его id пройдёт. Не сошлось за лимит → ошибка.
func waitNodeKnows(ctx context.Context, conn redisConn, masterID string) error {
	for attempt := 0; attempt < gossipPollAttempts; attempt++ {
		nodes, err := conn.Do(ctx, "CLUSTER", "NODES")
		if err != nil {
			return fmt.Errorf("CLUSTER NODES: %w", err)
		}
		if tableHasID(parseClusterNodesTable(nodes), masterID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gossipPollInterval):
		}
	}
	return fmt.Errorf("gossip did not converge: source master %s not visible after %d attempts", masterID, gossipPollAttempts)
}

// tableHasID — есть ли в топологии строка с данным node-id (узел узнал мастера).
func tableHasID(table []clusterNodeRow, id string) bool {
	for _, row := range table {
		if row.id == id {
			return true
		}
	}
	return false
}

// mappingSummary — детерминированная сводка маппинга (новый-ключ -> старый-master-id)
// для Output (без секретов: ключи nodes и node-id, не адреса/пароли).
func mappingSummary(results []joinResult) string {
	parts := make([]string, 0, len(results))
	for _, r := range results {
		parts = append(parts, r.node.key+"->"+r.masterID)
	}
	return strings.Join(parts, ",")
}
