// cluster-state плагина community.redis — сборка hash-slot-кластера Redis
// (16384 слота) ЦЕЛИКОМ через go-redis: CLUSTER MEET / ADDSLOTS / REPLICATE +
// CLUSTER INFO / CLUSTER NODES (диагностика и идемпотентность). НИКАКОГО
// shell/redis-cli/exec — capability плагина остаётся только network_outbound.
//
// action=create собирает кластер с нуля; action=add-node присоединяет ОДНУ новую
// ноду к УЖЕ сформированному кластеру (day-2); action=remove-node выводит ОДНУ
// ноду из кластера (day-2, с миграцией её слотов на оставшиеся masters, если она
// master со слотами). action=reshard переносит N слотов с одного master-а на
// другой (day-2, зеркало redis-cli `--cluster reshard`). Миграция «старый кластер
// → новый» (см. migrate.go) — три шага: action=join-external вливает НОВЫЕ ноды в
// ЧУЖОЙ (старый) кластер репликами его мастеров 1:1; action=failover-takeover
// промоутит эти реплики в мастера через graceful CLUSTER FAILOVER (после sync-gate,
// fail-closed без FORCE/TAKEOVER); action=forget-external выкидывает старые узлы
// (CLUSTER FORGET, без миграции слотов — слоты уже у новых мастеров).
//
// ★ reshard ИМПЕРАТИВЕН и НЕ идемпотентен (осознанно, как старый
// redis-cluster-live без unless): повторный apply сдвинет ещё N слотов. Это
// exec-style day-2 операция — оператор зовёт её явно, она НЕ часть converge.
// create/add-node/remove-node/join-external/failover-takeover/forget-external,
// напротив, идемпотентны (no-op на сошедшемся входе: узел уже реплика/master/забыт).
//
// Раскладка ролей и слотов СТРОГО детерминирована (сортировка ключей nodes):
// один и тот же вход → одна и та же топология master/replica и одни и те же
// диапазоны слотов. Это критично для воспроизводимости и для idempotent-формы
// (повторный create на сформированном кластере → changed=false, no-op).
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

// totalSlots — фиксированное число hash-слотов Redis Cluster (0..16383).
const totalSlots = 16384

// gossip-сходимость: ограниченный retry (НЕ бесконечный цикл) — ждём, пока все
// ноды увидят друг друга в CLUSTER NODES после MEET.
const (
	gossipPollAttempts = 30
	gossipPollInterval = 200 * time.Millisecond
)

// clusterNode — одна нода кластера после резолва из nodes-map.
//
//	key  — стабильный ключ из nodes-map (SID/имя); задаёт детерминированный порядок.
//	addr — host:port для go-redis-коннекта.
//	ip   — IP для CLUSTER MEET (gossip оперирует ip:port, не DNS-именем).
//	port — клиентский порт для CLUSTER MEET.
type clusterNode struct {
	key  string
	addr string
	ip   string
	port int
}

// clusterPlan — детерминированная раскладка: какие ноды мастера, какие реплики,
// какие диапазоны слотов у каждого мастера, к какому мастеру привязана реплика.
type clusterPlan struct {
	masters  []clusterNode
	replicas []clusterNode
	// slots[i] — непрерывный диапазон слотов мастера masters[i].
	slots []slotRange
	// replicaOf[j] — индекс мастера в masters для реплики replicas[j].
	replicaOf []int
}

// slotRange — непрерывный полуинтервал [from, to] слотов (оба конца включительно).
type slotRange struct {
	from int
	to   int
}

// validateCluster — статические проверки cluster-params (тексты без пароля).
func validateCluster(f map[string]*structpb.Value) []string {
	switch stringOrEmpty(f["action"]) {
	case "create":
		return validateClusterCreate(f)
	case "add-node":
		return validateClusterAddNode(f)
	case "remove-node":
		return validateClusterRemoveNode(f)
	case "reshard":
		return validateClusterReshard(f)
	case "join-external":
		return validateClusterJoinExternal(f)
	case "failover-takeover":
		return validateClusterFailoverTakeover(f)
	case "forget-external":
		return validateClusterForgetExternal(f)
	default:
		return []string{fmt.Sprintf(
			"params.action: %q not supported (only \"create\", \"add-node\", \"remove-node\", \"reshard\", \"join-external\", \"failover-takeover\", \"forget-external\")", stringOrEmpty(f["action"]))}
	}
}

// validateClusterCreate — проверки create: непустой nodes-map, корректный
// replicas_per_shard и делимость состава на размер шарда.
func validateClusterCreate(f map[string]*structpb.Value) []string {
	var errs []string

	nodes := nodeSpecs(f["nodes"])
	if len(nodes) == 0 {
		errs = append(errs, "params.nodes: must be a non-empty map (key -> {addr|ip+port})")
	}

	replicas := intOrDefault(f["replicas_per_shard"], 0)
	if replicas < 0 {
		errs = append(errs, "params.replicas_per_shard: must be >= 0")
	}

	// Состав nodes обязан ровно делиться на размер шарда (1 master + N replicas).
	if len(nodes) > 0 && replicas >= 0 {
		shardSize := 1 + replicas
		if len(nodes)%shardSize != 0 {
			errs = append(errs, fmt.Sprintf(
				"params.nodes: %d nodes not divisible by shard size %d (1 master + %d replicas)",
				len(nodes), shardSize, replicas))
		}
	}

	return errs
}

// validateClusterAddNode — проверки add-node: непустые new_node и seed,
// допустимый role и (для replica) корректная связка с master.
func validateClusterAddNode(f map[string]*structpb.Value) []string {
	var errs []string

	if len(nodeSpec(f["new_node"])) == 0 {
		errs = append(errs, "params.new_node: must be a map {addr|ip+port} of the node to add")
	}
	if len(nodeSpec(f["seed"])) == 0 {
		errs = append(errs, "params.seed: must be a map {addr|ip+port} of an existing cluster node")
	}

	switch role := roleOrDefault(f["role"]); role {
	case "replica", "master":
	default:
		errs = append(errs, fmt.Sprintf("params.role: %q not supported (only \"replica\", \"master\")", role))
	}

	return errs
}

// validateClusterRemoveNode — проверки remove-node: непустые node (удаляемая) и
// seed (контакт для CLUSTER NODES + источник топологии для FORGET/миграции слотов).
func validateClusterRemoveNode(f map[string]*structpb.Value) []string {
	var errs []string

	if len(nodeSpec(f["node"])) == 0 {
		errs = append(errs, "params.node: must be a map {addr|ip+port} of the node to remove")
	}
	if len(nodeSpec(f["seed"])) == 0 {
		errs = append(errs, "params.seed: must be a map {addr|ip+port} of an existing cluster node")
	}

	return errs
}

// validateClusterReshard — статические проверки reshard: непустые from/to
// (endpoint-ы master-ов), их различие и slots >= 1. «from/to — существующие
// masters» и «slots <= числа слотов у source» проверяются в Apply по живой
// топологии (CLUSTER NODES), статически их не видно (тексты без пароля).
func validateClusterReshard(f map[string]*structpb.Value) []string {
	var errs []string

	from := nodeSpec(f["from"])
	to := nodeSpec(f["to"])
	if len(from) == 0 {
		errs = append(errs, "params.from: must be a map {addr|ip+port} of the source master")
	}
	if len(to) == 0 {
		errs = append(errs, "params.to: must be a map {addr|ip+port} of the target master")
	}
	// from != to: сравниваем по резолвленному endpoint (ip:port), чтобы {addr} и
	// {ip,port}-форма одного и того же узла тоже распознавались как совпадение.
	if len(from) > 0 && len(to) > 0 {
		if fi, fp, _, ferr := resolveNodeEndpoint(from); ferr == nil {
			if ti, tp, _, terr := resolveNodeEndpoint(to); terr == nil {
				if net.JoinHostPort(fi, strconv.Itoa(fp)) == net.JoinHostPort(ti, strconv.Itoa(tp)) {
					errs = append(errs, "params.from and params.to must be different masters")
				}
			}
		}
	}

	if intOrDefault(f["slots"], 0) < 1 {
		errs = append(errs, "params.slots: must be an integer >= 1")
	}

	return errs
}

// applyCluster — диспетчер cluster-state по action.
func (m *RedisModule) applyCluster(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	switch stringOrEmpty(params.GetFields()["action"]) {
	case "create":
		return m.applyClusterCreate(ctx, stream, params)
	case "add-node":
		return m.applyClusterAddNode(ctx, stream, params)
	case "remove-node":
		return m.applyClusterRemoveNode(ctx, stream, params)
	case "reshard":
		return m.applyClusterReshard(ctx, stream, params)
	case "join-external":
		return m.applyClusterJoinExternal(ctx, stream, params)
	case "failover-takeover":
		return m.applyClusterFailoverTakeover(ctx, stream, params)
	case "forget-external":
		return m.applyClusterForgetExternal(ctx, stream, params)
	default:
		return sendFailure(stream, fmt.Sprintf(
			"cluster: action %q not supported (only \"create\", \"add-node\", \"remove-node\", \"reshard\", \"join-external\", \"failover-takeover\", \"forget-external\")",
			stringOrEmpty(params.GetFields()["action"])))
	}
}

// roleOrDefault — нормализованная роль add-node (default replica, как у
// redis-cli `--cluster add-node ... --cluster-slave`).
func roleOrDefault(v *structpb.Value) string {
	if role := strings.TrimSpace(stringOrEmpty(v)); role != "" {
		return role
	}
	return "replica"
}

// applyClusterCreate строит кластер из nodes-map. Идемпотентен: если кластер уже
// сформирован (cluster_state:ok, все наши ноды на месте, 16384 слота покрыты) —
// changed=false, no-op.
func (m *RedisModule) applyClusterCreate(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])

	nodes, err := parseClusterNodes(f["nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	replicas := intOrDefault(f["replicas_per_shard"], 0)

	plan, err := buildClusterPlan(nodes, replicas)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			// TLS-handshake-ошибка теоретически может нести PEM client-key —
			// редактируем его ПРЯМО в connect, чтобы любой caller (probe/
			// formCluster/migrate) получил уже-санированную ошибку (ключ
			// password уже редактируется их собственными redactError по тексту).
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	// Идемпотентность: спросим первый мастер о текущем состоянии кластера.
	formed, err := clusterAlreadyFormed(ctx, connect, plan, password)
	if err != nil {
		return sendFailure(stream, "cluster probe: "+redactError(err, password))
	}
	if formed {
		return sendOutcome(stream, false, "cluster already formed (no-op)", map[string]any{
			"shards":   int64(len(plan.masters)),
			"replicas": int64(len(plan.replicas)),
			"slots":    int64(totalSlots),
		})
	}

	if err := formCluster(ctx, connect, plan, password); err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	return sendOutcome(stream, true, fmt.Sprintf("cluster created: %d masters, %d replicas", len(plan.masters), len(plan.replicas)), map[string]any{
		"shards":   int64(len(plan.masters)),
		"replicas": int64(len(plan.replicas)),
		"slots":    int64(totalSlots),
		"layout":   layoutSummary(plan),
	})
}

// applyClusterAddNode присоединяет ОДНУ новую ноду к сформированному кластеру
// (day-2): CLUSTER MEET через seed → ожидание сходимости → назначение роли.
//
//	role=replica — CLUSTER REPLICATE: новичок становится репликой указанного
//	  master-а (params.master) либо, если master не задан, мастера с наименьшим
//	  числом реплик (балансировка, как redis-cli без --cluster-master-id).
//	role=master  — пустой master (MEET без слотов). Слоты на новый master
//	  переносит ОТДЕЛЬНАЯ операция reshard (follow-up); add-node их НЕ двигает.
//
// Идемпотентен: если new_node уже в кластере (CLUSTER NODES seed-а содержит её
// ip:port) → changed=false, no-op.
func (m *RedisModule) applyClusterAddNode(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])
	role := roleOrDefault(f["role"])

	newNode, err := resolveSingleNode("new_node", f["new_node"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	seed, err := resolveSingleNode("seed", f["seed"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			// TLS-handshake-ошибка теоретически может нести PEM client-key —
			// редактируем его ПРЯМО в connect, чтобы любой caller (probe/
			// formCluster/migrate) получил уже-санированную ошибку (ключ
			// password уже редактируется их собственными redactError по тексту).
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	seedConn, err := connect(seed)
	if err != nil {
		return sendFailure(stream, "connect seed: "+redactError(err, password))
	}
	defer func() { _ = seedConn.Close() }()

	topology, err := seedConn.Do(ctx, "CLUSTER", "NODES")
	if err != nil {
		return sendFailure(stream, "CLUSTER NODES: "+redactError(err, password))
	}
	existing := parseClusterNodesTable(topology)

	// Идемпотентность: новичок уже в топологии → no-op.
	if nodeInTable(existing, newNode) {
		return sendOutcome(stream, false, "node already in cluster (no-op)", map[string]any{
			"node": newNode.ip + ":" + strconv.Itoa(newNode.port),
			"role": role,
		})
	}

	// Выбор master-а для реплики ДО MEET — на пустой топологии (новичок ещё не
	// влит) выбор детерминирован и понятен в сообщении об ошибке.
	var masterID string
	if role == "replica" {
		masterID, err = pickReplicationMaster(f["master"], existing)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
	}

	// MEET: с seed-а приглашаем новичка в gossip по его ip:port.
	if _, err := seedConn.Do(ctx, "CLUSTER", "MEET", newNode.ip, strconv.Itoa(newNode.port)); err != nil {
		return sendFailure(stream, fmt.Sprintf("CLUSTER MEET %s: %s",
			net.JoinHostPort(newNode.ip, strconv.Itoa(newNode.port)), redactError(err, password)))
	}

	// Сходимость: seed обязан увидеть новичка (всего стало known+1 нод).
	if err := waitGossipConverged(ctx, seedConn, len(existing)+1); err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	if role == "master" {
		return sendOutcome(stream, true, "node added as empty master (no slots; reshard to populate)", map[string]any{
			"node": newNode.ip + ":" + strconv.Itoa(newNode.port),
			"role": "master",
		})
	}

	// REPLICATE исполняется НА НОВИЧКЕ (он становится репликой master-id).
	newConn, err := connect(newNode)
	if err != nil {
		return sendFailure(stream, "connect new_node: "+redactError(err, password))
	}
	defer func() { _ = newConn.Close() }()
	if _, err := newConn.Do(ctx, "CLUSTER", "REPLICATE", masterID); err != nil {
		return sendFailure(stream, "CLUSTER REPLICATE: "+redactError(err, password))
	}

	return sendOutcome(stream, true, "node added as replica", map[string]any{
		"node":      newNode.ip + ":" + strconv.Itoa(newNode.port),
		"role":      "replica",
		"master_id": masterID,
	})
}

// migrateBatch — сколько ключей за один CLUSTER GETKEYSINSLOT + MIGRATE-пакет
// (redis-cli использует тот же масштаб; ограничивает размер одного MIGRATE).
const migrateBatch = 100

// applyClusterRemoveNode выводит ОДНУ ноду из кластера (day-2). Если удаляемая —
// master СО слотами, её слоты СНАЧАЛА мигрируются на оставшиеся masters
// (CLUSTER SETSLOT IMPORTING/MIGRATING + GETKEYSINSLOT + MIGRATE keys + SETSLOT
// NODE), затем CLUSTER FORGET на ВСЕХ оставшихся нодах. Если удаляемая — replica
// либо master без слотов — просто FORGET на всех.
//
// Идемпотентен: ноды уже нет в CLUSTER NODES seed-а → changed=false, no-op.
func (m *RedisModule) applyClusterRemoveNode(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])

	removeNode, err := resolveSingleNode("node", f["node"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	seed, err := resolveSingleNode("seed", f["seed"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			// TLS-handshake-ошибка теоретически может нести PEM client-key —
			// редактируем его ПРЯМО в connect, чтобы любой caller (probe/
			// formCluster/migrate) получил уже-санированную ошибку (ключ
			// password уже редактируется их собственными redactError по тексту).
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	seedConn, err := connect(seed)
	if err != nil {
		return sendFailure(stream, "connect seed: "+redactError(err, password))
	}
	defer func() { _ = seedConn.Close() }()

	topology, err := seedConn.Do(ctx, "CLUSTER", "NODES")
	if err != nil {
		return sendFailure(stream, "CLUSTER NODES: "+redactError(err, password))
	}
	table := parseClusterNodesTable(topology)

	target := findNodeRow(table, removeNode)
	if target == nil {
		// Идемпотентность: ноды уже нет в кластере → no-op.
		return sendOutcome(stream, false, "node already absent from cluster (no-op)", map[string]any{
			"node": removeNode.ip + ":" + strconv.Itoa(removeNode.port),
		})
	}

	// Слоты переносим ТОЛЬКО если удаляемая — master с назначенными слотами.
	migrated := 0
	if target.isMaster && len(target.slots) > 0 {
		moved, err := migrateSlotsAway(ctx, connect, table, *target, password)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		migrated = moved
	}

	// FORGET удаляемой ноды на ВСЕХ оставшихся (каждая нода забывает её
	// независимо — gossip-anti-entropy сам бы дозабыл, но явный FORGET на всех
	// детерминирует результат и закрывает окно ре-приглашения).
	forgotten, err := forgetOnRemaining(ctx, connect, table, *target, password)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	return sendOutcome(stream, true, fmt.Sprintf("node removed (slots migrated: %d, forgotten on %d nodes)", migrated, forgotten), map[string]any{
		"node":           removeNode.ip + ":" + strconv.Itoa(removeNode.port),
		"slots_migrated": int64(migrated),
		"forgotten_on":   int64(forgotten),
	})
}

// applyClusterReshard переносит N слотов с одного master-а (from) на другой (to)
// в УЖЕ сформированном кластере (day-2). Зеркало redis-cli `--cluster reshard`:
// выбирает первые N слотов источника (по возрастанию) и переносит каждый через
// migrateOneSlot (SETSLOT IMPORTING на цели → MIGRATING на источнике →
// GETKEYSINSLOT + MIGRATE лосслесс → SETSLOT NODE на обеих нодах).
//
// ★ НЕ ИДЕМПОТЕНТЕН (осознанно): повторный apply сдвинет ещё N слотов с from на
// to. Это императивная exec-style day-2 операция — оператор зовёт её явно, не
// часть converge. Никакого unless/probe «уже перенесено» нет.
//
// Топология (CLUSTER NODES с from) даёт node-id обоих master-ов и текущие слоты
// источника. Контакт — сам from (он master, всегда отвечает CLUSTER NODES).
func (m *RedisModule) applyClusterReshard(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	username := stringOrEmpty(f["username"])

	from, err := resolveSingleNode("from", f["from"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	to, err := resolveSingleNode("to", f["to"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	slots := intOrDefault(f["slots"], 0)
	if slots < 1 {
		return sendFailure(stream, "params.slots: must be an integer >= 1")
	}

	tlsP := parseTLS(f)
	connect := func(node clusterNode) (redisConn, error) {
		conn, err := m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password, tls: tlsP})
		if err != nil {
			// TLS-handshake-ошибка теоретически может нести PEM client-key —
			// редактируем его ПРЯМО в connect, чтобы любой caller (probe/
			// formCluster/migrate) получил уже-санированную ошибку (ключ
			// password уже редактируется их собственными redactError по тексту).
			return nil, fmt.Errorf("%s", redactError(err, tlsP.keyPEM))
		}
		return conn, nil
	}

	fromConn, err := connect(from)
	if err != nil {
		return sendFailure(stream, "connect from: "+redactError(err, password))
	}
	defer func() { _ = fromConn.Close() }()

	topology, err := fromConn.Do(ctx, "CLUSTER", "NODES")
	if err != nil {
		return sendFailure(stream, "CLUSTER NODES: "+redactError(err, password))
	}
	table := parseClusterNodesTable(topology)

	srcRow := findNodeRow(table, from)
	if srcRow == nil || !srcRow.isMaster {
		return sendFailure(stream, fmt.Sprintf("params.from %s: not a master in this cluster", from.addr))
	}
	dstRow := findNodeRow(table, to)
	if dstRow == nil || !dstRow.isMaster {
		return sendFailure(stream, fmt.Sprintf("params.to %s: not a master in this cluster", to.addr))
	}

	// Первые N слотов источника по возрастанию (детерминированно). Если у source
	// меньше N слотов — это ошибка ввода (нельзя перенести больше, чем есть).
	owned := flattenSlots(srcRow.slots)
	if slots > len(owned) {
		return sendFailure(stream, fmt.Sprintf(
			"params.slots: %d exceeds %d slots currently owned by source master %s", slots, len(owned), from.addr))
	}
	picked := owned[:slots]

	dstConn, err := connect(to)
	if err != nil {
		return sendFailure(stream, "connect to: "+redactError(err, password))
	}
	defer func() { _ = dstConn.Close() }()

	for _, slot := range picked {
		if err := migrateOneSlot(ctx, fromConn, dstConn, srcRow.id, *dstRow, slot, password); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
	}

	return sendOutcome(stream, true, fmt.Sprintf("resharded %d slots: %s -> %s", slots, from.addr, to.addr), map[string]any{
		"slots_moved": int64(slots),
		"from":        from.ip + ":" + strconv.Itoa(from.port),
		"to":          to.ip + ":" + strconv.Itoa(to.port),
	})
}

// flattenSlots разворачивает диапазоны master-а в плоский отсортированный по
// возрастанию список отдельных слотов (детерминированный выбор первых N для
// reshard). Диапазоны CLUSTER NODES уже идут по возрастанию, но сортируем для
// устойчивости к порядку токенов.
func flattenSlots(ranges []slotRange) []int {
	var out []int
	for _, r := range ranges {
		for s := r.from; s <= r.to; s++ {
			out = append(out, s)
		}
	}
	sort.Ints(out)
	return out
}

// findNodeRow возвращает строку топологии удаляемой ноды (по ip:port) или nil.
func findNodeRow(table []clusterNodeRow, node clusterNode) *clusterNodeRow {
	want := net.JoinHostPort(node.ip, strconv.Itoa(node.port))
	for i := range table {
		if table[i].ipPort == want {
			return &table[i]
		}
	}
	return nil
}

// remainingMasters — masters кластера БЕЗ удаляемой ноды, детерминированно по id.
// Цели миграции слотов; FORGET тоже идёт на все оставшиеся (masters+replicas).
func remainingMasters(table []clusterNodeRow, removeID string) []clusterNodeRow {
	var out []clusterNodeRow
	for _, row := range table {
		if row.isMaster && row.id != removeID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// migrateSlotsAway переносит ВСЕ слоты удаляемого master-а на оставшиеся masters
// (round-robin по их отсортированному порядку → детерминированно). Возвращает
// число перенесённых слотов. Зеркало redis-cli reshard для одного слота:
//
//	IMPORTING на цели → MIGRATING на источнике → перенос ключей (GETKEYSINSLOT +
//	MIGRATE) → SETSLOT NODE <to> на обеих нодах (фиксация владельца).
func migrateSlotsAway(ctx context.Context, connect func(clusterNode) (redisConn, error), table []clusterNodeRow, src clusterNodeRow, password string) (int, error) {
	dests := remainingMasters(table, src.id)
	if len(dests) == 0 {
		return 0, fmt.Errorf("cannot migrate slots: no remaining master to receive them")
	}

	srcNode, err := nodeFromRow(src)
	if err != nil {
		return 0, err
	}
	srcConn, err := connect(srcNode)
	if err != nil {
		return 0, fmt.Errorf("connect source master %s: %w", srcNode.addr, err)
	}
	defer func() { _ = srcConn.Close() }()

	// Один долгоживущий коннект на каждую целевую ноду (SETSLOT исполняется на
	// каждой). Закрываем все одним defer-ом, без defer-в-цикле.
	destConns := make([]redisConn, len(dests))
	defer func() {
		for _, c := range destConns {
			if c != nil {
				_ = c.Close()
			}
		}
	}()
	for i, d := range dests {
		dn, err := nodeFromRow(d)
		if err != nil {
			return 0, err
		}
		c, err := connect(dn)
		if err != nil {
			return 0, fmt.Errorf("connect destination master %s: %w", dn.addr, err)
		}
		destConns[i] = c
	}

	moved := 0
	for _, r := range src.slots {
		for slot := r.from; slot <= r.to; slot++ {
			di := moved % len(dests)
			if err := migrateOneSlot(ctx, srcConn, destConns[di], src.id, dests[di], slot, password); err != nil {
				return moved, err
			}
			moved++
		}
	}
	return moved, nil
}

// migrateOneSlot переносит один слот с источника на цель (redis-cli-алгоритм):
// IMPORTING(to) → MIGRATING(from) → перенос всех ключей слота пакетами через
// MIGRATE → SETSLOT NODE <to-id> на ОБЕИХ нодах (новый владелец).
func migrateOneSlot(ctx context.Context, srcConn, destConn redisConn, srcID string, dest clusterNodeRow, slot int, password string) error {
	slotArg := strconv.Itoa(slot)
	if _, err := destConn.Do(ctx, "CLUSTER", "SETSLOT", slotArg, "IMPORTING", srcID); err != nil {
		return fmt.Errorf("SETSLOT %d IMPORTING: %w", slot, err)
	}
	if _, err := srcConn.Do(ctx, "CLUSTER", "SETSLOT", slotArg, "MIGRATING", dest.id); err != nil {
		return fmt.Errorf("SETSLOT %d MIGRATING: %w", slot, err)
	}

	destIP, destPort, err := splitIPPort(dest.ipPort)
	if err != nil {
		return fmt.Errorf("slot %d destination addr: %w", slot, err)
	}
	for {
		keys, err := srcConn.GetKeysInSlot(ctx, slot, migrateBatch)
		if err != nil {
			return fmt.Errorf("GETKEYSINSLOT %d: %w", slot, err)
		}
		if len(keys) == 0 {
			break // слот опустошён
		}
		// MIGRATE <host> <port> "" <db> <timeout> [AUTH pass] KEYS k...
		args := []any{"MIGRATE", destIP, strconv.Itoa(destPort), "", "0", "5000"}
		if password != "" {
			args = append(args, "AUTH", password)
		}
		args = append(args, "KEYS")
		for _, k := range keys {
			args = append(args, k)
		}
		if _, err := srcConn.Do(ctx, args...); err != nil {
			return fmt.Errorf("MIGRATE slot %d: %w", slot, redactErr(err, password))
		}
	}

	// Зафиксировать нового владельца слота на источнике и цели. Полное
	// распространение по кластеру доделает gossip.
	if _, err := srcConn.Do(ctx, "CLUSTER", "SETSLOT", slotArg, "NODE", dest.id); err != nil {
		return fmt.Errorf("SETSLOT %d NODE (source): %w", slot, err)
	}
	if _, err := destConn.Do(ctx, "CLUSTER", "SETSLOT", slotArg, "NODE", dest.id); err != nil {
		return fmt.Errorf("SETSLOT %d NODE (destination): %w", slot, err)
	}
	return nil
}

// forgetOnRemaining исполняет CLUSTER FORGET <remove-id> на каждой оставшейся
// ноде (masters + replicas, кроме удаляемой). Возвращает число нод, на которых
// FORGET выполнен. "Unknown node" на отдельной ноде (уже забыла) — не ошибка.
func forgetOnRemaining(ctx context.Context, connect func(clusterNode) (redisConn, error), table []clusterNodeRow, remove clusterNodeRow, password string) (int, error) {
	done := 0
	for _, row := range table {
		if row.id == remove.id {
			continue
		}
		node, err := nodeFromRow(row)
		if err != nil {
			return done, err
		}
		conn, err := connect(node)
		if err != nil {
			return done, fmt.Errorf("connect %s: %w", node.addr, err)
		}
		_, err = conn.Do(ctx, "CLUSTER", "FORGET", remove.id)
		_ = conn.Close()
		if err != nil {
			// Нода могла уже забыть удаляемую (gossip-anti-entropy) → "Unknown
			// node": не ошибка, идём дальше.
			if isUnknownNodeErr(err) {
				continue
			}
			return done, fmt.Errorf("CLUSTER FORGET on %s: %w", node.addr, err)
		}
		done++
	}
	return done, nil
}

// nodeFromRow строит clusterNode из строки топологии (для коннекта по ip:port).
func nodeFromRow(row clusterNodeRow) (clusterNode, error) {
	ip, port, err := splitIPPort(row.ipPort)
	if err != nil {
		return clusterNode{}, fmt.Errorf("node %s: %w", row.id, err)
	}
	return clusterNode{key: row.id, addr: row.ipPort, ip: ip, port: port}, nil
}

// splitIPPort режет "ip:port" CLUSTER NODES в (ip, port).
func splitIPPort(ipPort string) (string, int, error) {
	h, p, err := net.SplitHostPort(ipPort)
	if err != nil {
		return "", 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 {
		return "", 0, fmt.Errorf("invalid port in %q", ipPort)
	}
	return h, n, nil
}

// isUnknownNodeErr — FORGET по уже забытой ноде → "ERR Unknown node ...".
func isUnknownNodeErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unknown node")
}

// redactErr вырезает пароль из ошибки и возвращает обёрнутую ошибку (для
// error-цепочки внутри миграции; redactError возвращает строку). Маскировка —
// единой точкой истины поверх redactError, чтобы правка маски не разъехалась.
func redactErr(err error, password string) error {
	return fmt.Errorf("%s", redactError(err, password))
}

// masterSpecGiven — задан ли явный master-endpoint. Пустой spec или spec с
// пустыми addr/ip+port (scenario при незаданном master_sid шлёт master.addr: "")
// трактуется как «не указан» → авто-выбор master-а.
func masterSpecGiven(spec map[string]*structpb.Value) bool {
	if len(spec) == 0 {
		return false
	}
	return strings.TrimSpace(stringOrEmpty(spec["addr"])) != "" ||
		strings.TrimSpace(stringOrEmpty(spec["ip"])) != ""
}

// resolveSingleNode извлекает clusterNode из единичной ноды-спецификации
// ({addr|ip+port}). key совпадает с именем поля (для сообщений об ошибке).
func resolveSingleNode(field string, v *structpb.Value) (clusterNode, error) {
	spec := nodeSpec(v)
	if len(spec) == 0 {
		return clusterNode{}, fmt.Errorf("params.%s: must be a map {addr|ip+port}", field)
	}
	ip, port, addr, err := resolveNodeEndpoint(spec)
	if err != nil {
		return clusterNode{}, fmt.Errorf("params.%s: %w", field, err)
	}
	return clusterNode{key: field, addr: addr, ip: ip, port: port}, nil
}

// pickReplicationMaster определяет node-id master-а для новой реплики. Если задан
// params.master ({addr|ip+port}) — резолвит его id из топологии по ip:port; иначе
// выбирает master с наименьшим числом уже привязанных реплик (балансировка), при
// равенстве — детерминированно по node-id.
func pickReplicationMaster(masterSpec *structpb.Value, table []clusterNodeRow) (string, error) {
	masters := mastersFromTable(table)
	if len(masters) == 0 {
		return "", fmt.Errorf("cluster has no master to replicate")
	}

	// Явный master указан, только если spec несёт непустой endpoint (пустой
	// master.addr из scenario при незаданном master_sid → авто-выбор).
	if spec := nodeSpec(masterSpec); masterSpecGiven(spec) {
		ip, port, _, err := resolveNodeEndpoint(spec)
		if err != nil {
			return "", fmt.Errorf("params.master: %w", err)
		}
		want := net.JoinHostPort(ip, strconv.Itoa(port))
		for _, mr := range masters {
			if mr.ipPort == want {
				return mr.id, nil
			}
		}
		return "", fmt.Errorf("params.master %s: not a master in this cluster", want)
	}

	// Авто-выбор: меньше всего реплик; при равенстве — меньший node-id.
	replicaCount := make(map[string]int, len(masters))
	for _, row := range table {
		if !row.isMaster && row.masterID != "" {
			replicaCount[row.masterID]++
		}
	}
	best := masters[0]
	for _, mr := range masters[1:] {
		switch {
		case replicaCount[mr.id] < replicaCount[best.id]:
			best = mr
		case replicaCount[mr.id] == replicaCount[best.id] && mr.id < best.id:
			best = mr
		}
	}
	return best.id, nil
}

// clusterNodeRow — одна разобранная строка CLUSTER NODES (нужные поля).
type clusterNodeRow struct {
	id       string
	ipPort   string // "ip:port" клиентского адреса (без @cport)
	isMaster bool
	masterID string      // id master-а для реплики ("-" → "")
	slots    []slotRange // назначенные диапазоны слотов (только у master со слотами)
}

// parseClusterNodesTable разбирает вывод CLUSTER NODES в строки. Формат строки:
//
//	<id> <ip:port@cport> <flags> <master-id> <ping> <pong> <epoch> <link> [slots...]
//
// Берём id, ip:port (до @), флаг master/slave, master-id реплики и назначенные
// диапазоны слотов (поля с 8-го). Слот-токены вида "[N-<importing-id" / "[N->-
// migrating-id" (миграция in-flight) пропускаются — это не steady-state владение.
func parseClusterNodesTable(s string) []clusterNodeRow {
	var rows []clusterNodeRow
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		ipPort := fields[1]
		if at := strings.IndexByte(ipPort, '@'); at >= 0 {
			ipPort = ipPort[:at]
		}
		masterID := fields[3]
		if masterID == "-" {
			masterID = ""
		}
		var slots []slotRange
		if len(fields) > 8 {
			slots = parseSlotTokens(fields[8:])
		}
		rows = append(rows, clusterNodeRow{
			id:       fields[0],
			ipPort:   ipPort,
			isMaster: strings.Contains(fields[2], "master"),
			masterID: masterID,
			slots:    slots,
		})
	}
	return rows
}

// parseSlotTokens разбирает slot-токены строки CLUSTER NODES в диапазоны. Токен —
// либо одиночный слот "N", либо диапазон "N-M". Importing/migrating-токены в
// квадратных скобках ("[…") — нестабильная in-flight миграция, пропускаются.
func parseSlotTokens(tokens []string) []slotRange {
	var ranges []slotRange
	for _, tok := range tokens {
		if strings.HasPrefix(tok, "[") {
			continue
		}
		from, to, ok := strings.Cut(tok, "-")
		lo, err := strconv.Atoi(from)
		if err != nil {
			continue
		}
		hi := lo
		if ok {
			if hi, err = strconv.Atoi(to); err != nil {
				continue
			}
		}
		ranges = append(ranges, slotRange{from: lo, to: hi})
	}
	return ranges
}

// nodeInTable — присутствует ли узел (по ip:port) в топологии (идемпотентность).
func nodeInTable(table []clusterNodeRow, node clusterNode) bool {
	want := net.JoinHostPort(node.ip, strconv.Itoa(node.port))
	for _, row := range table {
		if row.ipPort == want {
			return true
		}
	}
	return false
}

// masterRow — master из топологии (id + ip:port) для выбора цели REPLICATE.
type masterRow struct {
	id     string
	ipPort string
}

// mastersFromTable — детерминированно отсортированный (по id) список master-ов.
func mastersFromTable(table []clusterNodeRow) []masterRow {
	var out []masterRow
	for _, row := range table {
		if row.isMaster {
			out = append(out, masterRow{id: row.id, ipPort: row.ipPort})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// parseClusterNodes резолвит nodes-map в детерминированно отсортированный
// срез clusterNode. Каждая нода — либо {addr: "host:port"}, либо {ip, port}.
func parseClusterNodes(v *structpb.Value) ([]clusterNode, error) {
	raw := nodeSpecs(v)
	if len(raw) == 0 {
		return nil, fmt.Errorf("params.nodes: must be a non-empty map")
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // детерминированная база раскладки

	out := make([]clusterNode, 0, len(keys))
	for _, k := range keys {
		spec := raw[k]
		ip, port, addr, err := resolveNodeEndpoint(spec)
		if err != nil {
			return nil, fmt.Errorf("params.nodes[%s]: %w", k, err)
		}
		out = append(out, clusterNode{key: k, addr: addr, ip: ip, port: port})
	}
	return out, nil
}

// resolveNodeEndpoint извлекает (ip, port, addr) из спецификации одной ноды.
// Приоритет: явные ip+port; иначе split addr "host:port". addr для коннекта,
// ip+port — для CLUSTER MEET (gossip оперирует ip:port).
func resolveNodeEndpoint(spec map[string]*structpb.Value) (ip string, port int, addr string, err error) {
	ip = strings.TrimSpace(stringOrEmpty(spec["ip"]))
	port = intOrDefault(spec["port"], 0)
	addr = strings.TrimSpace(stringOrEmpty(spec["addr"]))

	switch {
	case ip != "" && port > 0:
		if addr == "" {
			addr = net.JoinHostPort(ip, strconv.Itoa(port))
		}
		return ip, port, addr, nil
	case addr != "":
		h, p, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			return "", 0, "", fmt.Errorf("addr %q: %v", addr, splitErr)
		}
		pn, convErr := strconv.Atoi(p)
		if convErr != nil || pn <= 0 {
			return "", 0, "", fmt.Errorf("addr %q: invalid port", addr)
		}
		return h, pn, addr, nil
	default:
		return "", 0, "", fmt.Errorf("must specify addr (host:port) or ip+port")
	}
}

// buildClusterPlan раскладывает ноды по ролям и слотам СТРОГО детерминированно.
//
//	shards   = len(nodes) / (1 + replicas_per_shard)
//	masters  = первые shards нод (по отсортированному порядку)
//	replicas = остальные, round-robin к мастерам
//	слоты    = 16384 поровну между мастерами; остаток — первым мастерам
func buildClusterPlan(nodes []clusterNode, replicas int) (clusterPlan, error) {
	if replicas < 0 {
		return clusterPlan{}, fmt.Errorf("params.replicas_per_shard: must be >= 0")
	}
	shardSize := 1 + replicas
	if len(nodes)%shardSize != 0 {
		return clusterPlan{}, fmt.Errorf("params.nodes: %d nodes not divisible by shard size %d", len(nodes), shardSize)
	}
	shards := len(nodes) / shardSize
	if shards < 1 {
		return clusterPlan{}, fmt.Errorf("params.nodes: need at least %d nodes for one shard", shardSize)
	}

	plan := clusterPlan{
		masters:   nodes[:shards],
		replicas:  nodes[shards:],
		slots:     allocateSlots(shards),
		replicaOf: make([]int, len(nodes)-shards),
	}
	// Round-robin реплик к мастерам: replica j → master j%shards.
	for j := range plan.replicas {
		plan.replicaOf[j] = j % shards
	}
	return plan, nil
}

// allocateSlots делит 16384 слота поровну между shards мастерами; остаток
// (16384 % shards) распределяется по одному слоту первым мастерам.
func allocateSlots(shards int) []slotRange {
	base := totalSlots / shards
	rem := totalSlots % shards
	ranges := make([]slotRange, shards)
	cursor := 0
	for i := 0; i < shards; i++ {
		size := base
		if i < rem {
			size++
		}
		ranges[i] = slotRange{from: cursor, to: cursor + size - 1}
		cursor += size
	}
	return ranges
}

// clusterAlreadyFormed проверяет идемпотентность: коннект к первому мастеру,
// CLUSTER INFO (cluster_state:ok + cluster_known_nodes совпал) и CLUSTER NODES
// (все 16384 слота покрыты). Любой коннект-фейл к первому мастеру трактуем как
// «ещё не сформирован» (ноды могут только подниматься) — НЕ ошибка.
func clusterAlreadyFormed(ctx context.Context, connect func(clusterNode) (redisConn, error), plan clusterPlan, password string) (bool, error) {
	first := plan.masters[0]
	conn, err := connect(first)
	if err != nil {
		return false, nil //nolint:nilerr // ноды поднимаются; не сформирован
	}
	defer func() { _ = conn.Close() }()

	info, err := conn.Do(ctx, "CLUSTER", "INFO")
	if err != nil {
		return false, nil //nolint:nilerr
	}
	fields := parseClusterInfo(info)
	if fields["cluster_state"] != "ok" {
		return false, nil
	}
	if known, _ := strconv.Atoi(fields["cluster_known_nodes"]); known != len(plan.masters)+len(plan.replicas) {
		return false, nil
	}
	if assigned, _ := strconv.Atoi(fields["cluster_slots_assigned"]); assigned != totalSlots {
		return false, nil
	}
	return true, nil
}

// formCluster исполняет сборку: MEET всех нод в gossip с первого мастера,
// ожидание сходимости, ADDSLOTS мастерам, REPLICATE репликам.
func formCluster(ctx context.Context, connect func(clusterNode) (redisConn, error), plan clusterPlan, password string) error {
	all := append(append([]clusterNode{}, plan.masters...), plan.replicas...)

	// Один долгоживущий коннект на каждую ноду (нужен для MEET/ADDSLOTS/REPLICATE
	// именно на этой ноде). Закрываем все одним defer-ом, без defer-в-цикле.
	conns := make([]redisConn, 0, len(all))
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for _, n := range all {
		c, err := connect(n)
		if err != nil {
			return fmt.Errorf("connect %s: %w", n.addr, err)
		}
		conns = append(conns, c)
	}

	// CLUSTER MYID нужен ТОЛЬКО мастерам: REPLICATE требует node-id мастера, у
	// реплики собственный id не используется. Спрашиваем id у мастеров (conns[:M]).
	idByKey := make(map[string]string, len(plan.masters))
	for i, master := range plan.masters {
		id, err := conns[i].Do(ctx, "CLUSTER", "MYID")
		if err != nil {
			return fmt.Errorf("CLUSTER MYID %s: %w", master.addr, err)
		}
		idByKey[master.key] = strings.TrimSpace(id)
	}

	// Gossip: с первой ноды MEET всех остальных по ip:port.
	hub := conns[0]
	for _, n := range all[1:] {
		if _, err := hub.Do(ctx, "CLUSTER", "MEET", n.ip, strconv.Itoa(n.port)); err != nil {
			return fmt.Errorf("CLUSTER MEET %s: %w", net.JoinHostPort(n.ip, strconv.Itoa(n.port)), err)
		}
	}

	if err := waitGossipConverged(ctx, hub, len(all)); err != nil {
		return err
	}

	// ADDSLOTS мастерам — их детерминированные диапазоны.
	for i, master := range plan.masters {
		r := plan.slots[i]
		args := addSlotsArgs(r)
		if _, err := conns[i].Do(ctx, args...); err != nil {
			return fmt.Errorf("CLUSTER ADDSLOTS %s [%d-%d]: %w", master.addr, r.from, r.to, err)
		}
	}

	// REPLICATE репликам — id их мастера.
	for j, replica := range plan.replicas {
		master := plan.masters[plan.replicaOf[j]]
		masterID := idByKey[master.key]
		if masterID == "" {
			return fmt.Errorf("replica %s: unknown master id for %s", replica.addr, master.key)
		}
		ci := len(plan.masters) + j
		if _, err := conns[ci].Do(ctx, "CLUSTER", "REPLICATE", masterID); err != nil {
			return fmt.Errorf("CLUSTER REPLICATE %s -> %s: %w", replica.addr, master.addr, err)
		}
	}

	return nil
}

// waitGossipConverged ждёт (ограниченный retry, НЕ бесконечно), пока hub увидит
// в CLUSTER NODES все want нод. Возвращает ошибку, если за лимит не сошлось.
func waitGossipConverged(ctx context.Context, hub redisConn, want int) error {
	for attempt := 0; attempt < gossipPollAttempts; attempt++ {
		nodes, err := hub.Do(ctx, "CLUSTER", "NODES")
		if err != nil {
			return fmt.Errorf("CLUSTER NODES: %w", err)
		}
		if countClusterNodes(nodes) >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gossipPollInterval):
		}
	}
	return fmt.Errorf("gossip did not converge: fewer than %d nodes visible after %d attempts", want, gossipPollAttempts)
}

// addSlotsArgs строит аргументы CLUSTER ADDSLOTS для непрерывного диапазона.
func addSlotsArgs(r slotRange) []any {
	args := make([]any, 0, 2+(r.to-r.from+1))
	args = append(args, "CLUSTER", "ADDSLOTS")
	for s := r.from; s <= r.to; s++ {
		args = append(args, strconv.Itoa(s))
	}
	return args
}

// parseClusterInfo разбирает вывод CLUSTER INFO ("key:value" построчно).
func parseClusterInfo(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// countClusterNodes считает непустые строки вывода CLUSTER NODES (одна строка =
// одна нода).
func countClusterNodes(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// layoutSummary — детерминированная человекочитаемая сводка раскладки для
// Output (без секретов: только key и слоты).
func layoutSummary(plan clusterPlan) string {
	parts := make([]string, 0, len(plan.masters)+len(plan.replicas))
	for i, master := range plan.masters {
		r := plan.slots[i]
		parts = append(parts, fmt.Sprintf("%s=master[%d-%d]", master.key, r.from, r.to))
	}
	for j, replica := range plan.replicas {
		parts = append(parts, fmt.Sprintf("%s=replica->%s", replica.key, plan.masters[plan.replicaOf[j]].key))
	}
	return strings.Join(parts, ",")
}
