// cluster-state плагина community.redis — сборка hash-slot-кластера Redis
// (16384 слота) ЦЕЛИКОМ через go-redis: CLUSTER MEET / ADDSLOTS / REPLICATE +
// CLUSTER INFO / CLUSTER NODES (диагностика и идемпотентность). НИКАКОГО
// shell/redis-cli/exec — capability плагина остаётся только network_outbound.
//
// PILOT (2026-06-22): только action=create. add-node/remove-node/reshard —
// follow-up (зеркало redis-cli), пока отвергаются Validate как нереализованные.
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
	var errs []string

	if action := firstString(f["action"]); action != "create" {
		errs = append(errs, fmt.Sprintf("params.action: %q not supported (only \"create\")", action))
	}

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

// applyCluster — диспетчер cluster-state. Сейчас только action=create.
func (m *RedisModule) applyCluster(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	action := firstString(f["action"])
	if action != "create" {
		return sendFailure(stream, fmt.Sprintf("cluster: action %q not supported (only \"create\")", action))
	}
	return m.applyClusterCreate(ctx, stream, params)
}

// applyClusterCreate строит кластер из nodes-map. Идемпотентен: если кластер уже
// сформирован (cluster_state:ok, все наши ноды на месте, 16384 слота покрыты) —
// changed=false, no-op.
func (m *RedisModule) applyClusterCreate(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], params *structpb.Struct) error {
	f := params.GetFields()
	password := firstString(f["password"])
	username := firstString(f["username"])

	nodes, err := parseClusterNodes(f["nodes"])
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	replicas := intOrDefault(f["replicas_per_shard"], 0)

	plan, err := buildClusterPlan(nodes, replicas)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	connect := func(node clusterNode) (redisConn, error) {
		return m.openConn(ctx, connConfig{addr: node.addr, username: username, password: password})
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
	ip = strings.TrimSpace(firstString(spec["ip"]))
	port = intOrDefault(spec["port"], 0)
	addr = strings.TrimSpace(firstString(spec["addr"]))

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

	// Один долгоживущий коннект на каждую ноду (нужен и для MYID, и для
	// ADDSLOTS/REPLICATE именно на этой ноде).
	conns := make([]redisConn, len(all))
	idByKey := make(map[string]string, len(all))
	for i, n := range all {
		c, err := connect(n)
		if err != nil {
			return fmt.Errorf("connect %s: %w", n.addr, err)
		}
		conns[i] = c
		defer func(c redisConn) { _ = c.Close() }(c)

		id, err := c.Do(ctx, "CLUSTER", "MYID")
		if err != nil {
			return fmt.Errorf("CLUSTER MYID %s: %w", n.addr, err)
		}
		idByKey[n.key] = strings.TrimSpace(id)
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
