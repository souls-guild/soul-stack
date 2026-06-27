// probe-states плагина community.redis — read-probe-операции над живым Redis
// ЦЕЛИКОМ через go-redis (без redis-cli/shell, capability только
// network_outbound). Все state — read-only, changed=false КОНСТРУКТИВНО
// (прецедент core.http.probe / core.exec.run): это проверка, не изменение.
// Output несёт результат probe для health-gate (retry/until/failed_when) и для
// волатильного where-таргетинга (роль хоста).
//
//	pinged — health-probe: go-redis PING → ожидает PONG. Заменяет idiom
//	         community.redis.command args:[PING] (Output.result == 'PONG'
//	         сохраняется как поле result — совместимо с register.self.result).
//	role   — role-probe: INFO replication → роль инстанса (master/slave).
//	         Заменяет shell-idiom `redis-cli role | head -1 | tr -d '\n'` —
//	         волатильную роль для where-таргетинга rolling-restart (ADR-008:
//	         фактическая роль волатильна, берётся живым probe перед таргетингом).
//	replica-synced — restart health-gate реплики: INFO replication →
//	         master_link_status == "up" (реплика РЕСИНКНУЛАСЬ с master-ом).
//	         Строже pinged (PONG = демон жив, но мог ещё не догнать master);
//	         Output.synced bool + master_link_status строкой для диагностики.
//	         Поле master_link_status есть ТОЛЬКО у реплики — у master-а его
//	         нет: synced=false с явной причиной (не тихий success), state
//	         предназначен для slave-пути (restart block.where slave).
//	offset-synced — safety-gate миграции из ВНЕШНЕГО источника: «link жив ≠
//	         данные догнаны». Сверяет slave_repl_offset СВОЕГО инстанса с
//	         master_repl_offset ВНЕШНЕГО master-а (ВТОРОЙ коннект к source_addr
//	         со source-секретами). caught_up=true только когда link up + НЕТ
//	         идущей full-sync (master_sync_in_progress==0) + отставание
//	         (master − slave) <= lag_threshold. Опц. сверка DBSIZE обоих при
//	         !skip_checksum. Read-only, changed=false КОНСТРУКТИВНО.
//
// КРИТ ИБ (ADR-010): params.password / source_password НИКОГДА не попадают в
// ApplyEvent.Message/.Output/ошибки. Коннект-ошибки санитизируются redactError
// (по обоим паролям); ответ PING (PONG), роль и offset — это ответ сервера, не
// секрет оператора.
package main

import (
	"context"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// applyPinged — health-probe через go-redis PING. changed=false КОНСТРУКТИВНО
// (probe, не изменение): интерпретация «здоров/нет» — на уровне scenario через
// retry/until/failed_when по register.self.result. Output.result несёт ответ
// сервера (PONG) — совместимо с прежним community.redis.command args:[PING],
// который тоже клал ответ в Output.result.
func (m *RedisModule) applyPinged(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	password := stringOrEmpty(params.GetFields()["password"])
	// Коннект уже выполнен openConn → defaultConnect, который сам шлёт PING при
	// открытии. Явный PING здесь нужен, чтобы (1) отделить health-probe от факта
	// коннекта и (2) положить ответ сервера в Output.result для health-gate.
	res, err := conn.Do(ctx, "PING")
	if err != nil {
		// Ошибка PING — ответ сервера (LOADING / MASTERDOWN / ...): её аргументы
		// пароль не несут. redactError по password — defense-in-depth/единообразие
		// с applyRole/applyConfig (драйвер теоретически мог эхнуть коннект-кредл).
		return sendFailure(stream, "PING: "+redactError(err, password))
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

// applyReplicaSynced — restart health-gate реплики: INFO replication →
// master_link_status == "up" (реплика РЕСИНКНУЛАСЬ с master-ом, не просто
// «демон отвечает PONG»). changed=false КОНСТРУКТИВНО (read-probe). Output.synced
// bool + Output.master_link_status строкой для диагностики health-gate
// (until:/failed_when: по register.self.synced).
//
// ★ Граница master/slave: поле master_link_status присутствует в INFO replication
// ТОЛЬКО у реплики (role:slave) — у master-а его нет. State предназначен для
// slave-пути (restart block.where slave). Если поля нет (это master или нештатный
// INFO) → synced=false с явной причиной в Message (НЕ тихий success): иначе
// health-gate реплики молча прошёл бы на инстансе, который ещё не реплика.
func (m *RedisModule) applyReplicaSynced(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	password := stringOrEmpty(params.GetFields()["password"])
	info, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication: "+redactError(err, password))
	}
	repl := parseInfoSection(info)
	status, present := repl["master_link_status"]
	if !present {
		// master_link_status отсутствует — это master (поля нет у роли master) либо
		// нештатный ответ. synced=false с причиной, а НЕ тихий success: health-gate
		// реплики не должен пройти на не-реплике.
		return sendOutcome(stream, false, "master_link_status отсутствует (инстанс не реплика — это master либо нештатный INFO)", map[string]any{
			"synced":             false,
			"master_link_status": "",
		})
	}
	synced := status == "up"
	return sendOutcome(stream, false, "master_link_status: "+status, map[string]any{
		"synced":             synced,
		"master_link_status": status,
	})
}

// validateOffsetSynced — статические проверки offset-synced: addr (свой) +
// source_addr (внешний master) обязательны; lag_threshold (если задан) >= 0.
func validateOffsetSynced(f map[string]*structpb.Value) []string {
	var errs []string
	errs = append(errs, validateAddr(f)...)
	if strings.TrimSpace(stringOrEmpty(f["source_addr"])) == "" {
		errs = append(errs, "params.source_addr: must be a non-empty string (host:port of the external master)")
	}
	if v := f["lag_threshold"]; v != nil && intOrDefault(v, 0) < 0 {
		errs = append(errs, "params.lag_threshold: must be an integer >= 0 (bytes)")
	}
	return errs
}

// applyOffsetSynced — safety-gate миграции из внешнего источника. conn —
// СВОЙ инстанс (addr); метод сам открывает ВТОРОЙ коннект к внешнему master-у
// (source_addr) со source-секретами (как cluster.go открывает per-node conn).
// Read-only, changed=false КОНСТРУКТИВНО (probe).
//
// caught_up=true ⟺ master_link_status=="up" И master_sync_in_progress==0 И
// (master_repl_offset − slave_repl_offset) <= lag_threshold. Любое из условий
// ложно → caught_up=false (success-event, НЕ failed: health-gate решает сам через
// until:/failed_when: по register.self.caught_up). master_link_status / поля
// offset на master-е/реплике отсутствуют у противоположной роли — это нештатный
// ввод (свой addr не реплика, либо source_addr не master): caught_up=false.
func (m *RedisModule) applyOffsetSynced(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])
	sourcePassword := stringOrEmpty(f["source_password"])
	skipChecksum := boolOrDefault(f["skip_checksum"], false)
	lagThreshold := intOrDefault(f["lag_threshold"], 0)

	// Свой инстанс: slave_repl_offset, состояние линка и идущей full-sync.
	selfInfo, err := conn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication (self): "+redactError(err, password, sourcePassword))
	}
	selfRepl := parseInfoSection(selfInfo)
	slaveOffset, slaveOffsetOK := parseOffset(selfRepl["slave_repl_offset"])
	linkStatus := selfRepl["master_link_status"]
	syncInProgress := selfRepl["master_sync_in_progress"] == "1"

	// Внешний master: ВТОРОЙ коннект со source-секретами (source_addr/
	// source_password/source_tls*). master_repl_offset — авторитетный «head».
	sourceCfg := connConfig{
		addr:     strings.TrimSpace(stringOrEmpty(f["source_addr"])),
		password: sourcePassword,
		tls:      parseSourceTLS(f),
	}
	sourceConn, err := m.openConn(ctx, sourceCfg)
	if err != nil {
		return sendFailure(stream, "connect source: "+redactError(err, password, sourcePassword, sourceCfg.tls.keyPEM))
	}
	defer func() { _ = sourceConn.Close() }()

	sourceInfo, err := sourceConn.Do(ctx, "INFO", "replication")
	if err != nil {
		return sendFailure(stream, "INFO replication (source): "+redactError(err, password, sourcePassword))
	}
	sourceRepl := parseInfoSection(sourceInfo)
	masterOffset, masterOffsetOK := parseOffset(sourceRepl["master_repl_offset"])

	// lag и caught_up. Без обоих offset-ов lag неопределён → caught_up=false
	// (нештатный ввод: свой addr не реплика, либо source не master).
	lagBytes := 0
	offsetsKnown := slaveOffsetOK && masterOffsetOK
	if offsetsKnown {
		lagBytes = masterOffset - slaveOffset
		if lagBytes < 0 {
			lagBytes = 0 // реплика «впереди» (read-after-write окно) — не отрицательный lag
		}
	}
	caughtUp := linkStatus == "up" && !syncInProgress && offsetsKnown && lagBytes <= lagThreshold

	output := map[string]any{
		"caught_up":               caughtUp,
		"lag_bytes":               int64(lagBytes),
		"master_sync_in_progress": syncInProgress,
	}

	// Опц. checksum-сверка размеров обоих наборов (грубый sanity-чек поверх
	// offset-а; offset — авторитет, DBSIZE — вспомогательный сигнал). Не влияет на
	// caught_up: разный DBSIZE на ходу нормален (TTL/eviction), но виден в Output.
	if !skipChecksum {
		dbSource, derr := dbSize(ctx, sourceConn)
		if derr != nil {
			return sendFailure(stream, "DBSIZE (source): "+redactError(derr, password, sourcePassword))
		}
		dbReplica, derr := dbSize(ctx, conn)
		if derr != nil {
			return sendFailure(stream, "DBSIZE (replica): "+redactError(derr, password, sourcePassword))
		}
		output["dbsize_source"] = dbSource
		output["dbsize_replica"] = dbReplica
	}

	return sendOutcome(stream, false, "caught_up: "+strconv.FormatBool(caughtUp)+", lag_bytes: "+strconv.Itoa(lagBytes), output)
}

// parseOffset разбирает offset-поле INFO replication в int. Пусто/нечисло → (0,
// false): поле отсутствует у противоположной роли (slave_repl_offset нет у
// master-а; master_repl_offset на реплике отражает её собственный поток, а не
// head источника — поэтому head берём с ОТДЕЛЬНОГО коннекта к source).
func parseOffset(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}

// dbSize читает DBSIZE через redisConn.Do (число ключей текущей БД). Ответ —
// число, не секрет.
func dbSize(ctx context.Context, conn redisConn) (int64, error) {
	res, err := conn.Do(ctx, "DBSIZE")
	if err != nil {
		return 0, err
	}
	n, convErr := strconv.ParseInt(strings.TrimSpace(res), 10, 64)
	if convErr != nil {
		return 0, nil // нечисловой ответ — best-effort 0 (не валим probe)
	}
	return n, nil
}
