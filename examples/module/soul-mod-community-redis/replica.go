// replica-state плагина community.redis — привязка Redis-инстанса к master-у
// (REPLICAOF) ЦЕЛИКОМ через go-redis: INFO replication (диагностика и
// идемпотентность) → REPLICAOF host port + CONFIG SET masterauth. НИКАКОГО
// redis-cli/shell — capability плагина остаётся только network_outbound.
//
// Идемпотентен: если инстанс УЖЕ реплицирует нужный master (role:slave,
// master_host/master_port совпали, master_link_status:up) → changed=false, no-op.
//
// addr == master_addr → no-op (guard В ПЛАГИНЕ) — defense-in-depth. РЕАЛЬНАЯ
// защита от «master реплицирует сам себя» — scenario `where:` (sentinel.yml шаг
// 4): задача рендерится ТОЛЬКО на репликах (master исключён по SID). В prod
// addr=127.0.0.1:6379, master_addr=primary_ip (напр. 10.0.0.1) — addr НИКОГДА не
// равен master_addr ни на одном хосте, поэтому этот guard в prod не срабатывает;
// он ловит только вырожденную addr==master_addr-комбинацию (тест
// TestApplyReplica_SelfIsMasterNoOp), которая в сценарии не возникает.
package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// validateReplica — статические проверки replica-params (тексты без пароля).
func validateReplica(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	if strings.TrimSpace(stringOrEmpty(f["master_addr"])) == "" {
		errs = append(errs, "params.master_addr: must be a non-empty string (host:port of the master)")
	}
	return errs
}

// applyReplica приводит инстанс к роли реплики указанного master-а. addr — этот
// инстанс (на redis-хосте локальный, 127.0.0.1:6379); master_addr — host-
// инвариантный адрес master-а кластера (один на кластер). master НЕ реплицирует
// себя: addr == master_addr → no-op.
func (m *RedisModule) applyReplica(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	masterAddr := strings.TrimSpace(stringOrEmpty(f["master_addr"]))

	masterHost, masterPort, err := net.SplitHostPort(masterAddr)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("params.master_addr %q: %v", masterAddr, err))
	}
	if _, convErr := strconv.Atoi(masterPort); convErr != nil {
		return sendFailure(stream, fmt.Sprintf("params.master_addr %q: invalid port", masterAddr))
	}

	// master НЕ реплицирует сам себя: scenario зовёт replica на ВСЕХ хостах
	// (включая выбранный master), guard здесь делает master-вызов no-op.
	selfAddr := strings.TrimSpace(stringOrEmpty(f["addr"]))
	if sameRedisEndpoint(selfAddr, masterAddr) {
		return sendOutcome(stream, false, "this instance is the master (no-op)", map[string]any{
			"role":   "master",
			"master": masterAddr,
		})
	}

	// Идемпотентность: уже реплицируем нужный master со здоровым линком → no-op.
	info, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication: "+redactError(err, password))
	}
	repl := parseInfoSection(info)
	if repl["role"] == "slave" &&
		repl["master_host"] == masterHost &&
		repl["master_port"] == masterPort &&
		repl["master_link_status"] == "up" {
		return sendOutcome(stream, false, "already replica of master (no-op)", map[string]any{
			"role":   "slave",
			"master": masterAddr,
		})
	}

	// masterauth ДО REPLICAOF: реплика обязана знать пароль master-а, иначе
	// синхронизация упадёт на AUTH. CONFIG SET идемпотентен на стороне Redis.
	// Пустой password — нет требования AUTH у master-а: masterauth не ставим.
	if password != "" {
		if _, err := conn.Do(ctx, "CONFIG", "SET", "masterauth", password); err != nil {
			return sendFailure(stream, "CONFIG SET masterauth: "+redactError(err, password))
		}
	}
	if username := stringOrEmpty(f["username"]); username != "" {
		if _, err := conn.Do(ctx, "CONFIG", "SET", "masteruser", username); err != nil {
			return sendFailure(stream, "CONFIG SET masteruser: "+redactError(err, password))
		}
	}

	if _, err := conn.Do(ctx, "REPLICAOF", masterHost, masterPort); err != nil {
		return sendFailure(stream, fmt.Sprintf("REPLICAOF %s: %s", masterAddr, redactError(err, password)))
	}

	return sendOutcome(stream, true, "instance set as replica of master", map[string]any{
		"role":   "slave",
		"master": masterAddr,
	})
}

// sameRedisEndpoint — указывают ли два host:port на один и тот же Redis-инстанс.
// Сравнение по нормализованной паре (host, port); невалидный addr → false (на
// no-op-guard консервативно: не считаем master-ом то, что не распарсилось).
func sameRedisEndpoint(a, b string) bool {
	ah, ap, aerr := net.SplitHostPort(a)
	bh, bp, berr := net.SplitHostPort(b)
	if aerr != nil || berr != nil {
		return false
	}
	return ah == bh && ap == bp
}

// parseInfoSection разбирает вывод INFO (или одной секции, напр. INFO
// replication) в map "key:value" построчно. Заголовки секций (# Replication) и
// пустые строки игнорируются. CRLF-окончания Redis обрезаются.
func parseInfoSection(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
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
