// migrate-cluster плагина community.redis — миграция «старый кластер → новый
// кластер той же топологии» в ТРИ операционных шага (каждый — отдельный action cluster):
//
//	join-external      — вступление НОВЫХ cluster-mode нод в УЖЕ существующий
//	                     (старый) кластер репликами старых мастеров 1:1 (CLUSTER
//	                     MEET + REPLICATE); новые ноды догоняют данные.
//	failover-takeover  — промоушен этих реплик в мастера через GRACEFUL CLUSTER
//	                     FAILOVER (сначала sync-gate master_link_status==up на ВСЕХ;
//	                     fail-closed без эскалации на FORCE/TAKEOVER — split-brain).
//	forget-external    — выкидывание старых узлов из кластера (CLUSTER FORGET всех
//	                     старых node-id на каждой новой ноде; слоты уже у новых).
//
// ЦЕЛИКОМ через go-redis (CLUSTER NODES / MEET / REPLICATE / FAILOVER / FORGET +
// INFO replication), как create/add-node: никакого redis-cli/shell, capability
// остаётся network_outbound. ТА ЖЕ сеть и ТОТ ЖЕ пароль кластера (оператор
// выравнивает новый пароль == старый до запуска) — единый password/tls на старые
// seed-ноды и на новые узлы.
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
// делает каждую репликой смаппленного старого мастера (migration step 1):
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

// ============================ failover-takeover ==============================

// validateClusterFailoverTakeover — статические проверки failover-takeover:
// непустой nodes-map (новые узлы — реплики старых мастеров). Тексты без пароля.
func validateClusterFailoverTakeover(f map[string]*structpb.Value) []string {
	if len(nodeSpecs(f["nodes"])) == 0 {
		return []string{"params.nodes: must be a non-empty map (key -> {addr|ip+port}) of the NEW cluster nodes (replicas to promote)"}
	}
	return nil
}

// applyClusterFailoverTakeover промоутит новые узлы (реплики старых мастеров после
// join-external) в мастера через GRACEFUL CLUSTER FAILOVER (migration step 2):
//
//  1. ★sync-gate ДО первого failover: на КАЖДОМ новом узле INFO replication
//     master_link_status == "up" (реплика догнала свой старый мастер). Хоть один
//     не догнал → ОШИБКА до любого failover (ранний failover на недогнанной
//     реплике теряет незаписанный хвост репликации);
//  2. на каждом новом узле: идемпотентность (уже master → no-op), иначе GRACEFUL
//     CLUSTER FAILOVER (без аргументов: мастер останавливает запись, ждёт догонку,
//     лосслесс) → poll CLUSTER NODES узла, пока он не стал master СО слотами.
//
// ★FAIL-CLOSED: graceful не сошёлся за лимит → ОШИБКА. НЕ эскалируем на
// FORCE/TAKEOVER — они промоутят без согласия старого мастера (split-brain + потеря
// данных, противоречит «безопасность на первом месте»). Оператор разбирается явно.
//
// Финальный Output несёт per-node-статус (promoted|already) + число
// промоутнутых. Пароль НЕ попадает в события (ИБ ADR-010).
func (m *RedisModule) applyClusterFailoverTakeover(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])

	newNodes, err := parseClusterNodes(f["nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
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

	// ★SYNC-GATE: ВСЕ новые реплики обязаны быть синхронны (master_link_status==up)
	// ДО первого failover. Если промоутить первый шард на догнавшей реплике, а
	// второй ещё догоняет — на его failover потеряется хвост. Проверяем всё разом,
	// до любого мутирующего CLUSTER FAILOVER. master-узлы (уже промоутнутые,
	// повторный apply) sync-gate пропускают: у master нет master_link_status.
	syncState := make([]bool, len(newNodes)) // true → узел уже master (failover не нужен)
	for i, node := range newNodes {
		isMaster, err := nodeSyncReady(ctx, connect, node)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		syncState[i] = isMaster
	}

	// Все синхронны (или уже master) — промоутим. Идемпотентность: узел уже master
	// → no-op (failover не шлём).
	promoted := 0
	statuses := make([]string, len(newNodes))
	for i, node := range newNodes {
		if syncState[i] {
			statuses[i] = node.key + "=already"
			continue
		}
		if err := failoverNode(ctx, connect, node); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		statuses[i] = node.key + "=promoted"
		promoted++
	}

	return sendOutcome(stream, promoted > 0, fmt.Sprintf(
		"failover-takeover: %d/%d new nodes promoted to master (graceful)", promoted, len(newNodes)),
		map[string]any{
			"promoted": int64(promoted),
			"nodes":    int64(len(newNodes)),
			"per_node": strings.Join(statuses, ","),
		})
}

// nodeSyncReady проверяет готовность одного нового узла к failover ДО его запуска.
// Возвращает (isMaster, error): isMaster=true — узел УЖЕ master (повторный apply,
// failover не нужен, sync-gate неприменим). isMaster=false + nil — узел реплика,
// её линк здоров (master_link_status=="up"), failover можно запускать. Реплика с
// нездоровым линком (или нештатный INFO) → ОШИБКА (fail до любого failover).
func nodeSyncReady(ctx context.Context, connect func(clusterNode) (redisConn, error), node clusterNode) (bool, error) {
	conn, err := connect(node)
	if err != nil {
		return false, fmt.Errorf("connect new node %s: %w", node.addr, err)
	}
	defer func() { _ = conn.Close() }()

	info, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return false, fmt.Errorf("INFO replication on %s: %w", node.addr, err)
	}
	repl := parseInfoSection(info)
	if repl["role"] == "master" {
		return true, nil // уже промоутнут (идемпотентность), sync-gate неприменим
	}
	if repl["master_link_status"] != "up" {
		// Реплика ещё не догнала свой старый мастер. Ранний failover потерял бы
		// хвост репликации — отказываем ДО первого failover (fail-closed).
		return false, fmt.Errorf(
			"new node %s not synced before failover: master_link_status=%q (want \"up\") — replica has not caught up, refusing to fail over",
			node.addr, repl["master_link_status"])
	}
	return false, nil
}

// failoverNode промоутит один новый узел (синхронную реплику) в master через
// GRACEFUL CLUSTER FAILOVER (БЕЗ аргументов) и ждёт, пока узел реально станет
// master СО слотами (CLUSTER NODES самого узла). graceful: старый мастер
// останавливает запись, отдаёт реплике хвост, та берёт слоты — лосслесс.
//
// ★FAIL-CLOSED: не сошёлся за gossipPollAttempts → ОШИБКА. НЕ шлём FORCE/TAKEOVER
// (промоушен без согласия мастера = split-brain). Узел уже master (внезапно
// промоутнулся между sync-gate и сюда) → poll сойдётся сразу (no-op de-facto).
func failoverNode(ctx context.Context, connect func(clusterNode) (redisConn, error), node clusterNode) error {
	conn, err := connect(node)
	if err != nil {
		return fmt.Errorf("connect new node %s: %w", node.addr, err)
	}
	defer func() { _ = conn.Close() }()

	// GRACEFUL: без FORCE/TAKEOVER. Redis координирует с мастером (остановка записи
	// + дослать хвост + смена эпохи). На уже-master узле FAILOVER вернёт ошибку
	// "You should send CLUSTER FAILOVER to a replica" — но сюда мы попадаем только
	// для НЕ-master узлов (sync-gate выше), так что путь штатный.
	if _, err := conn.Do(ctx, "CLUSTER", "FAILOVER"); err != nil {
		return fmt.Errorf("CLUSTER FAILOVER on %s: %w", node.addr, err)
	}

	// Дожидаемся завершения graceful failover: узел стал master И владеет слотами
	// (только тогда промоушен фактически состоялся). Слоты появляются в строке
	// самого узла его же CLUSTER NODES после смены роли.
	if err := waitNodePromoted(ctx, conn, node); err != nil {
		return fmt.Errorf("new node %s: %w", node.addr, err)
	}
	return nil
}

// waitNodePromoted ждёт (ограниченный retry, переиспользует gossip-таймауты), пока
// CLUSTER NODES узла покажет его самого как master СО слотами. Не сошлось за лимит
// → ОШИБКА (FAIL-CLOSED: graceful failover не завершился, эскалации на FORCE НЕТ).
func waitNodePromoted(ctx context.Context, conn redisConn, node clusterNode) error {
	for attempt := 0; attempt < gossipPollAttempts; attempt++ {
		nodes, err := conn.Do(ctx, "CLUSTER", "NODES")
		if err != nil {
			return fmt.Errorf("CLUSTER NODES: %w", err)
		}
		if row := findNodeRow(parseClusterNodesTable(nodes), node); row != nil && row.isMaster && len(row.slots) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gossipPollInterval):
		}
	}
	return fmt.Errorf("graceful CLUSTER FAILOVER did not complete after %d attempts (node not master with slots) — NOT escalating to FORCE/TAKEOVER (split-brain risk); resolve manually", gossipPollAttempts)
}

// ============================== forget-external ==============================

// validateClusterForgetExternal — статические проверки forget-external: непустой
// nodes-map (новые узлы, исполняющие FORGET) и непустой source_nodes (старые seed,
// откуда берутся node-id для забывания). Тексты без пароля.
func validateClusterForgetExternal(f map[string]*structpb.Value) []string {
	var errs []string
	if len(nodeSpecs(f["nodes"])) == 0 {
		errs = append(errs, "params.nodes: must be a non-empty map (key -> {addr|ip+port}) of the NEW cluster nodes")
	}
	if len(stringList(f["source_nodes"])) == 0 {
		errs = append(errs, "params.source_nodes: must be a non-empty list of seed nodes (host:port) of the OLD cluster to forget")
	}
	return errs
}

// applyClusterForgetExternal выкидывает старые узлы из кластера через CLUSTER
// FORGET на каждой новой ноде (migration step 3, после failover-takeover):
//
//  1. коннект к source_nodes (старые seed, перебор по порядку) → CLUSTER NODES →
//     node-id СТАРОГО кластера (мастера И реплики — выкидываем всех). ★Строки новых
//     нод (по ip:port из nodes-map) ОТФИЛЬТРОВАНЫ: пост-join они в той же топологии,
//     их id → self-forget;
//  2. на КАЖДОЙ новой ноде: CLUSTER FORGET <old-id> для каждого старого id.
//
// ★БЕЗ миграции слотов (в отличие от remove-node): слоты УЖЕ у новых мастеров
// после failover-takeover, старые мастера их лишились. Идемпотентно: старый id
// уже неизвестен ноде → FORGET вернёт "Unknown node", глотаем (no-op); старых в
// топологии seed уже не осталось (все забыты) → пустой oldIDs, changed=false. Все
// старые seed недоступны (кластер уже погашен, id взять неоткуда) → ОШИБКА с
// понятным текстом (мы не знаем, что забывать — не идемпотентный путь).
//
// Финальный Output несёт число забытых старых узлов и число новых нод, на которых
// FORGET исполнен. Пароль НЕ попадает в события (ИБ ADR-010).
func (m *RedisModule) applyClusterForgetExternal(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])

	newNodes, err := parseClusterNodes(f["nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	sources, err := parseSourceSeeds(f["source_nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	// node-id старого кластера с первой доступной source-seed (как join-external).
	// ★ИСКЛЮЧАЕМ новые ноды: после join-external + failover-takeover новые ноды —
	// члены ТОГО ЖЕ кластера, и CLUSTER NODES старого seed перечисляет их тоже. Без
	// фильтра их id попали бы в oldIDs → нода форгетила бы СЕБЯ (Redis: "I can't
	// forget myself" — это НЕ "unknown node", hard-fail). Фильтр — по ip:port из
	// nodes-map (надёжнее, чем CLUSTER MYID на каждой ноде: один проход топологии).
	oldIDs, seedEndpoint, err := sourceNodeIDs(ctx, connect, sources, newNodes)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	// На КАЖДОЙ новой ноде FORGET всех старых id. "Unknown node" (нода уже забыла
	// старого) → идемпотентность, глотаем. Считаем число (нода × старый) пар, где
	// FORGET реально что-то забыл.
	forgotten := 0
	for _, node := range newNodes {
		n, err := forgetIDsOnNode(ctx, connect, node, oldIDs)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		forgotten += n
	}

	return sendOutcome(stream, forgotten > 0, fmt.Sprintf(
		"forget-external: forgot %d old node(s) across %d new node(s)", len(oldIDs), len(newNodes)),
		map[string]any{
			"old_nodes":  int64(len(oldIDs)),
			"new_nodes":  int64(len(newNodes)),
			"forgotten":  int64(forgotten),
			"source_via": seedEndpoint,
		})
}

// sourceNodeIDs коннектится к source-seed-ам по порядку, берёт CLUSTER NODES с
// первой ответившей и возвращает node-id СТАРОГО кластера (мастера И реплики —
// забываем всех), детерминированно отсортированные. Возвращает также endpoint
// сработавшего seed-а (для Output). Все seed-ы недоступны → ошибка.
//
// ★ Узлы, чей ip:port совпадает с newNodes, ИСКЛЮЧАЮТСЯ: после join-external +
// failover-takeover новые ноды — члены того же кластера и попадают в CLUSTER NODES
// старого seed; их id в oldIDs привёл бы к self-forget ("can't forget myself").
//
// Отличие от sourceMasters: тот берёт только мастеров со слотами (для маппинга
// репликации), здесь нужны ВСЕ старые узлы (forget выкидывает целиком старый кластер).
func sourceNodeIDs(ctx context.Context, connect func(clusterNode) (redisConn, error), seeds, newNodes []clusterNode) ([]string, string, error) {
	// Set ip:port новых нод — их строки в топологии старого seed отбрасываем.
	newEndpoints := make(map[string]struct{}, len(newNodes))
	for _, n := range newNodes {
		newEndpoints[net.JoinHostPort(n.ip, strconv.Itoa(n.port))] = struct{}{}
	}

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

		rows := parseClusterNodesTable(topology)
		if len(rows) == 0 {
			// Битый/пустой ответ seed (ни одной строки) — пробуем следующий seed.
			lastErr = fmt.Errorf("source seed %s: no nodes in CLUSTER NODES", seed.addr)
			continue
		}
		var ids []string
		for _, row := range rows {
			if row.id == "" {
				continue
			}
			if _, isNew := newEndpoints[row.ipPort]; isNew {
				continue // новая нода (уже в кластере) — не забываем (иначе self-forget)
			}
			ids = append(ids, row.id)
		}
		// ids может быть пустым, если все строки топологии — новые ноды (старые уже
		// забыты/выключены): забывать нечего — идемпотентный no-op (caller вернёт
		// changed=false), а НЕ ошибка. Сюда мы дошли с непустой топологией (битый
		// seed отсеян выше), поэтому пустой ids — это steady-state, а не сбой seed.
		sort.Strings(ids) // детерминированный порядок FORGET (стабильный вывод/asserts)
		return ids, net.JoinHostPort(seed.ip, strconv.Itoa(seed.port)), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("params.source_nodes: no reachable source seed")
	}
	return nil, "", lastErr
}

// forgetIDsOnNode исполняет CLUSTER FORGET <old-id> на одной новой ноде для каждого
// старого id. Возвращает число реально забытых. Два класса ошибок глотаются как
// идемпотентность: "Unknown node" (нода уже забыла этот id — gossip-anti-entropy /
// повторный apply) и "can't forget myself" (id оказался самой ноды — defense-in-
// depth: oldIDs уже отфильтрован по ip:port в sourceNodeIDs, но gossip мог добавить
// новую ноду в seed-топологию ПОСЛЕ фильтра, ip-форма могла разойтись — глотаем,
// чтобы не падать на безопасном no-op).
func forgetIDsOnNode(ctx context.Context, connect func(clusterNode) (redisConn, error), node clusterNode, oldIDs []string) (int, error) {
	conn, err := connect(node)
	if err != nil {
		return 0, fmt.Errorf("connect new node %s: %w", node.addr, err)
	}
	defer func() { _ = conn.Close() }()

	done := 0
	for _, id := range oldIDs {
		_, err := conn.Do(ctx, "CLUSTER", "FORGET", id)
		if err != nil {
			// Идемпотентность: нода уже забыла старого ("Unknown node") либо id — она
			// сама ("can't forget myself", пост-join gossip-гонка). Не ошибка, дальше.
			if isUnknownNodeErr(err) || isCantForgetSelfErr(err) {
				continue
			}
			return done, fmt.Errorf("CLUSTER FORGET %s on %s: %w", id, node.addr, err)
		}
		done++
	}
	return done, nil
}

// isCantForgetSelfErr — CLUSTER FORGET по собственному node-id → Redis отвечает
// "ERR I tried hard but I can't forget myself...". Пост-join новая нода — член того
// же кластера; sourceNodeIDs её отфильтровывает, но на gossip-гонку глотаем и здесь.
func isCantForgetSelfErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "can't forget myself")
}
