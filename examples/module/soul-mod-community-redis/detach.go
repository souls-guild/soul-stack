// detached-state плагина community.redis — отвязка инстанса от master-а
// (REPLICAOF NO ONE) ЦЕЛИКОМ через go-redis: INFO replication (диагностика и
// идемпотентность) → REPLICAOF NO ONE. НИКАКОГО redis-cli/shell — capability
// плагина остаётся только network_outbound.
//
// Промоушен бывшей реплики в самостоятельный master — финальный шаг миграции из
// внешнего источника (после offset-synced подтвердил, что данные догнаны): рвём
// репликацию, инстанс становится автономным master-ом.
//
// Идемпотентен: INFO replication → role==master уже → no-op (changed=false). Это
// делает шаг безопасным к повтору (rerun сценария на уже промоутнутом инстансе не
// «промоутит» его снова, просто подтверждает роль).
package main

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyDetached отвязывает инстанс от master-а (REPLICAOF NO ONE), промоутя его в
// самостоятельный master. Идемпотентен: уже master (role==master в INFO
// replication) → changed=false, no-op. Output.previous_master несёт прежний
// master_host:master_port (для аудита/диагностики) — это адрес сервера, не секрет.
func (m *RedisModule) applyDetached(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	password := stringOrEmpty(params.GetFields()["password"])

	info, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication: "+redactError(err, password))
	}
	repl := parseInfoSection(info)

	// Уже master → отвязывать нечего (no-op, идемпотентно). REPLICAOF NO ONE на
	// master-е Redis принимает молча, но честный probe-skip избегает ложного
	// changed=true и лишней команды.
	if repl["role"] == "master" {
		return sendOutcome(stream, false, "instance is already master (no-op)", map[string]any{
			"changed":         false,
			"previous_master": "",
		})
	}

	// Прежний master для отчёта (поля есть у реплики; "" если по какой-то причине
	// отсутствуют — не блокирует промоушен).
	previousMaster := ""
	if host := repl["master_host"]; host != "" {
		previousMaster = host
		if port := repl["master_port"]; port != "" {
			previousMaster = host + ":" + port
		}
	}

	if _, err := conn.Do(ctx, "REPLICAOF", "NO", "ONE"); err != nil {
		return sendFailure(stream, "REPLICAOF NO ONE: "+redactError(err, password))
	}

	return sendOutcome(stream, true, "instance detached from master (promoted to master)", map[string]any{
		"changed":         true,
		"previous_master": previousMaster,
	})
}
