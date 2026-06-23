// probe-states плагина community.redis — read-probe-операции над живым Redis
// ЦЕЛИКОМ через go-redis (без redis-cli/shell, capability только
// network_outbound). Оба state — read-only, changed=false КОНСТРУКТИВНО
// (прецедент core.http.probe / core.exec.run): это проверка, не изменение.
// Output несёт результат probe для health-gate (retry/until/failed_when) и для
// волатильного where-таргетинга (роль хоста).
//
//   pinged — health-probe: go-redis PING → ожидает PONG. Заменяет idiom
//            community.redis.command args:[PING] (Output.result == 'PONG'
//            сохраняется как поле result — совместимо с register.self.result).
//   role   — role-probe: INFO replication → роль инстанса (master/slave).
//            Заменяет shell-idiom `redis-cli role | head -1 | tr -d '\n'` —
//            волатильную роль для where-таргетинга rolling-restart (ADR-008:
//            фактическая роль волатильна, берётся живым probe перед таргетингом).
//
// КРИТ ИБ (ADR-010): params.password НИКОГДА не попадает в ApplyEvent.Message/
// .Output/ошибки. Коннект-ошибки санитизируются redactError; ответ PING (PONG)
// и роль — это ответ сервера, не секрет оператора.
package main

import (
	"context"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyPinged — health-probe через go-redis PING. changed=false КОНСТРУКТИВНО
// (probe, не изменение): интерпретация «здоров/нет» — на уровне scenario через
// retry/until/failed_when по register.self.result. Output.result несёт ответ
// сервера (PONG) — совместимо с прежним community.redis.command args:[PING],
// который тоже клал ответ в Output.result.
func (m *RedisModule) applyPinged(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, _ *structpb.Struct) error {
	// Коннект уже выполнен openConn → defaultConnect, который сам шлёт PING при
	// открытии. Явный PING здесь нужен, чтобы (1) отделить health-probe от факта
	// коннекта и (2) положить ответ сервера в Output.result для health-gate.
	res, err := conn.Do(ctx, "PING")
	if err != nil {
		// Ошибка PING — ответ сервера (LOADING / MASTERDOWN / ...), не секрет:
		// коннект-уровневый пароль здесь недоступен (вырезается в openConn-пути).
		return sendFailure(stream, "PING: "+err.Error())
	}
	return sendOutcome(stream, false, "PING ok", map[string]any{
		"result": res,
	})
}

// applyRole — role-probe через INFO replication. Возвращает фактическую
// (волатильную) роль инстанса в Output.role: "master" / "slave" (значения Redis
// в INFO replication; redis-cli role-shell отдавал те же master/slave). changed=
// false КОНСТРУКТИВНО (probe). where-таргетинг сравнивает register.self.role ==
// 'master'/'slave' (ADR-008: роль волатильна, замеряется живым probe).
//
// INFO replication выбран вместо команды ROLE: переиспользует parseInfoSection
// (replica.go) — типизированный разбор "key:value", без хрупкого парсинга
// ROLE-массива (первый элемент). master_link_status тоже доступен (бонус для
// диагностики), но для where достаточно role.
func (m *RedisModule) applyRole(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	password := stringOrEmpty(params.GetFields()["password"])
	info, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication: "+redactError(err, password))
	}
	repl := parseInfoSection(info)
	role := repl["role"]
	if role == "" {
		// INFO replication без поля role — нештатный ответ (обрезанный INFO/
		// сломанный инстанс). Явный fail, а не пустая роль в where (тихо никого
		// не таргетит → молчаливый пропуск рестарта).
		return sendFailure(stream, "INFO replication: поле role отсутствует в ответе")
	}
	return sendOutcome(stream, false, "role: "+role, map[string]any{
		"role": role,
	})
}
