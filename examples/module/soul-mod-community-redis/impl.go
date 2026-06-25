// soul-mod-community-redis — реальный SoulModule-плагин Soul Stack
// (community.redis): ОСНОВНОЙ интерфейс к живому Redis в redis-консолидации
// (концепция Ansible-роли). Scenario сервиса оркеструет порядок/таргетинг,
// плагин исполняет ОДНУ операцию над одним инстансом.
//
// States: `command` (raw verb-state, changed=false по умолчанию), `pinged` /
// `role` / `replica-synced` (read-probe, changed=false конструктивно, см.
// probe.go), `config` (CONFIG SET из map), `acl` (ACL LOAD — hot-reload aclfile,
// changed по diff ACL LIST до/после), `cluster` (см. cluster.go), `replica`
// (REPLICAOF, см. replica.go), `sentinel` (SENTINEL MONITOR/SET reconcile, см.
// sentinel.go). failover — следующий батч.
//
// СОЗНАТЕЛЬНО без dry-run preview: плагин на BaseModule НЕ реализует PlanReadSafe
// → host применяет default-deny (на dry_run задача получает честный «drift не
// поддержан», не ложное «нет дрифта»). Решение пользователя 2026-06-22.
//
// Backend — github.com/redis/go-redis/v9. Адрес + пароль приходят от Keeper:
// пароль уже отрезолвлен render-фазой из vault-ref (ADR-012), плагин свой
// Vault-клиент НЕ тянет (capability — только network_outbound).
//
// КРИТ ИБ (ADR-010): params["password"] НИКОГДА не попадает в ApplyEvent.Message,
// .Output, в текст ошибок или в stderr. Все коннект-ошибки санитизируются
// (redactError), вывод команд — нет (это ответ Redis, не секрет оператора).
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// RedisModule — реализация SoulModule community.redis.
//
// BaseModule даёт no-op Plan (без PlanReadSafe → default-deny на dry_run) и
// СОЗНАТЕЛЬНО НЕ реализует ErrandReadSafe (default-deny на Errand): оба
// поведения осознанны для этого плагина. Переопределяем Validate и Apply.
type RedisModule struct {
	module.BaseModule

	// connect — точка инъекции для L0. nil → реальный redis.NewClient.
	connect func(ctx context.Context, cfg connConfig) (redisConn, error)
}

// redisConn — узкий интерфейс над *redis.Client (для L0-фейка).
type redisConn interface {
	// Do выполняет произвольную команду и возвращает строковое представление
	// ответа Redis (или ошибку). Строка идёт в ApplyEvent.Output — это ответ
	// сервера, не секрет оператора.
	Do(ctx context.Context, args ...any) (string, error)
	// ConfigGet читает CONFIG GET <param> через ТИПИЗИРОВАННЫЙ путь драйвера
	// (go-redis отдаёт map[string]string нативно). НЕ через Do+strings.Fields:
	// многословные значения (напр. save "900 1 300 10 60 10000") при space-join +
	// Fields рассыпаются в перепутанные пары → ложный CONFIG SET, потеря
	// идемпотентности на day-2 update_config.
	ConfigGet(ctx context.Context, param string) (map[string]string, error)
	// GetKeysInSlot читает CLUSTER GETKEYSINSLOT <slot> <count> через
	// ТИПИЗИРОВАННЫЙ путь драйвера ([]string нативно). НЕ через Do+strings.Fields:
	// ключ Redis — произвольная байт-строка и может содержать пробел/\t/\n; при
	// space-join + Fields ключ "user 42" рассыпался бы в два токена → MIGRATE по
	// несуществующим ключам → ключ НЕ переносится, а SETSLOT NODE всё равно отдаёт
	// слот → ПОТЕРЯ ДАННЫХ. Native-path сохраняет разделители (симметрия с ConfigGet).
	GetKeysInSlot(ctx context.Context, slot, count int) ([]string, error)
	// AclList читает ACL LIST через ТИПИЗИРОВАННЫЙ путь драйвера ([]string — по
	// строке на пользователя). Используется для diff до/после ACL LOAD (changed-
	// детекция acl-state). НЕ через Do+strings.Fields: каждая строка ACL — целое
	// правило ("user alice on >hash ~* +@all") с пробелами; space-join + Fields
	// рассыпали бы её в токены → ложный diff. Native-path сохраняет строки целиком
	// (симметрия с ConfigGet/GetKeysInSlot).
	AclList(ctx context.Context) ([]string, error)
	Close() error
}

// connConfig — параметры коннекта. password и tls.*PEM держатся отдельно и
// НИКОГДА не логируются / не кладутся в события (ИБ-инвариант ADR-010).
type connConfig struct {
	addr     string // host:port или unix:/path
	username string
	password string
	db       int
	tls      tlsParams // TLS-параметры (enabled=false → plaintext-коннект)
}

// Validate — runtime-проверки поверх статических от soul-lint. Возвращает
// ValidateReply с errors (не error) — это контракт Validate. Тексты ошибок НЕ
// содержат пароль.
func (m *RedisModule) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	f := req.GetParams().GetFields()

	switch req.GetState() {
	case "command":
		errs = append(errs, validateAddr(f)...)
		if len(stringList(f["args"])) == 0 {
			errs = append(errs, "params.args: must be a non-empty list (e.g. [\"PING\"])")
		}
	case "pinged", "role", "replica-synced":
		// Read-probe: единственное обязательное — addr (PING / INFO replication
		// сами по себе аргументов не требуют).
		errs = append(errs, validateAddr(f)...)
	case "config":
		errs = append(errs, validateAddr(f)...)
		if len(stringMap(f["config"])) == 0 {
			errs = append(errs, "params.config: must be a non-empty map of directives")
		}
	case "acl":
		// acl приводит ЖИВОЙ инстанс к уже отрендеренному aclfile командой ACL LOAD
		// — никаких params кроме коннекта (addr + опц. auth/TLS). Единственное
		// обязательное — addr (как у read-probe).
		errs = append(errs, validateAddr(f)...)
	case "cluster":
		// cluster коннектится к нодам из nodes-map, единый addr не требуется.
		errs = append(errs, validateCluster(f)...)
	case "replica":
		errs = append(errs, validateReplica(f)...)
	case "sentinel":
		errs = append(errs, validateSentinel(f)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (expected command|pinged|role|replica-synced|config|acl|cluster|replica|sentinel)", req.GetState()))
	}

	if len(errs) > 0 {
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	return &pluginv1.ValidateReply{Ok: true}, nil
}

// Apply — диспетчеризация по state. Финальное событие переносит changed/failed +
// output (ADR-012). Ошибки коннекта санитизируются (redactError) — адрес
// сохраняем для диагностики, пароль вырезаем.
func (m *RedisModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	// cluster управляет НЕСКОЛЬКИМИ нодами (коннект к каждой из nodes-map), а не
	// одним addr — у него собственный жизненный цикл коннектов.
	if req.GetState() == "cluster" {
		return m.applyCluster(ctx, stream, req.GetParams())
	}

	cfg, err := parseConnConfig(req.GetParams())
	if err != nil {
		return sendFailure(stream, err.Error())
	}
	conn, err := m.openConn(ctx, cfg)
	if err != nil {
		// Редактируем И пароль, И PEM client-key: TLS-handshake-ошибка теоретически
		// может нести client-key (ИБ-инвариант ADR-010, как пароль).
		return sendFailure(stream, "connect: "+redactError(err, cfg.password, cfg.tls.keyPEM))
	}
	defer func() { _ = conn.Close() }()

	switch req.GetState() {
	case "command":
		return m.applyCommand(ctx, stream, conn, req.GetParams())
	case "pinged":
		return m.applyPinged(ctx, stream, conn, req.GetParams())
	case "role":
		return m.applyRole(ctx, stream, conn, req.GetParams())
	case "replica-synced":
		return m.applyReplicaSynced(ctx, stream, conn, req.GetParams())
	case "config":
		return m.applyConfig(ctx, stream, conn, req.GetParams())
	case "acl":
		return m.applyACL(ctx, stream, conn, req.GetParams())
	case "replica":
		return m.applyReplica(ctx, stream, conn, req.GetParams())
	case "sentinel":
		return m.applySentinel(ctx, stream, conn, req.GetParams())
	default:
		return sendFailure(stream, fmt.Sprintf("unknown state %q (expected command|pinged|role|replica-synced|config|acl|cluster|replica|sentinel)", req.GetState()))
	}
}

// applyCommand — raw-команда. changed берётся из params.changed (default false,
// probe-семантика). Output несёт result (ответ Redis). Пароль в события не
// попадает (args оператора + result сервера).
func (m *RedisModule) applyCommand(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	args := stringList(f["args"])
	if len(args) == 0 {
		return sendFailure(stream, "params.args: must be a non-empty list")
	}
	changed := boolOrDefault(f["changed"], false)

	cmdArgs := make([]any, len(args))
	for i, a := range args {
		cmdArgs[i] = a
	}
	res, err := conn.Do(ctx, cmdArgs...)
	if err != nil {
		// Ошибка Redis на саму команду — это её вывод, не секрет; но коннект-уровень
		// мог нести пароль — на всякий случай редактируем по cfg.password нельзя
		// (его тут нет), поэтому редактируем только wrap-текст. err от Do — ответ
		// сервера (WRONGPASS/ERR ...) безопасен.
		return sendFailure(stream, fmt.Sprintf("command %s: %v", args[0], err))
	}

	return sendOutcome(stream, changed, fmt.Sprintf("command %s ok", args[0]), map[string]any{
		"verb":   args[0],
		"result": res,
	})
}

// applyConfig — честный diff: CONFIG GET текущего значения каждой директивы,
// CONFIG SET только реально отличающихся (no-op → changed=false, идемпотентно как
// reconcileGlobals / cluster / replica / sentinel). Порядок детерминированный
// (отсортированные ключи) ради воспроизводимого вывода. Опц. CONFIG REWRITE
// выполняется только если хоть одна директива применена (нет дрифта между live и
// redis.conf, который надо персистить). Значения директив в Output идут — это
// конфиг redis, не пароль; но error-path санитизируется redactError по значению
// директивы (defense-in-depth: значение могло прийти из vault, напр. requirepass).
func (m *RedisModule) applyConfig(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	directives := stringMap(f["config"])
	if len(directives) == 0 {
		return sendFailure(stream, "params.config: must be a non-empty map of directives")
	}
	rewrite := boolOrDefault(f["rewrite"], false)

	applied := make([]string, 0, len(directives))
	for _, key := range sortedKeys(directives) {
		want := directives[key]
		current, err := configGet(ctx, conn, key)
		if err != nil {
			// redactError по значению директивы: defense-in-depth — значение могло
			// прийти из vault (requirepass/masterauth), а текст ошибки драйвера его
			// эхать (симметрия с replica/cluster/sentinel error-path).
			return sendFailure(stream, fmt.Sprintf("CONFIG GET %s: %s", key, redactError(err, want)))
		}
		if current == want {
			continue // no-op: live уже на желаемом значении
		}
		if _, err := conn.Do(ctx, "CONFIG", "SET", key, want); err != nil {
			return sendFailure(stream, fmt.Sprintf("CONFIG SET %s: %s", key, redactError(err, want)))
		}
		applied = append(applied, key)
	}

	if rewrite && len(applied) > 0 {
		if _, err := conn.Do(ctx, "CONFIG", "REWRITE"); err != nil {
			return sendFailure(stream, fmt.Sprintf("CONFIG REWRITE: %v", err))
		}
	}

	return sendOutcome(stream, len(applied) > 0, fmt.Sprintf("CONFIG SET applied: %s", strings.Join(applied, ",")), map[string]any{
		"applied": strings.Join(applied, ","),
		"count":   int64(len(applied)),
		"rewrite": rewrite && len(applied) > 0,
	})
}

// applyACL — hot-reload ACL: ACL LOAD заставляет Redis перечитать aclfile
// целиком (фундамент волны hot-reload; aclfile уже отрендерен destiny до этого
// шага). Идемпотентно ПО КОНСТРУКЦИИ: ACL LOAD приводит живой инстанс к
// декларированному файлу независимо от текущего состояния.
//
// changed-семантика: сама ACL LOAD «changed» не сообщает, поэтому делаем дешёвый
// честный diff — ACL LIST до и после LOAD (типизированный путь, []string по
// правилу на пользователя). Совпали → changed=false (живой инстанс уже совпадал
// с файлом, no-op как config/cluster/sentinel); отличаются → changed=true.
// Симметрия с config: тоже сверяем live и приводим к желаемому.
//
// ИБ: ACL-правила (вывод ACL LIST) НЕ кладём в Output — строка пользователя
// может нести password-hash (>hash / #sha256). Output несёт только число
// затронутых пользователей и факт changed. error-path санитизируется
// (redactError) по cfg.* через общий путь Apply; внутри — без секретов.
func (m *RedisModule) applyACL(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, _ *structpb.Struct) error {
	before, err := conn.AclList(ctx)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("ACL LIST (before): %v", err))
	}
	if _, err := conn.Do(ctx, "ACL", "LOAD"); err != nil {
		// ACL LOAD фейлит при битом aclfile / aclfile не сконфигурирован — это ответ
		// Redis (не секрет оператора), но коннект-уровень секретов тут уже нет.
		return sendFailure(stream, fmt.Sprintf("ACL LOAD: %v", err))
	}
	after, err := conn.AclList(ctx)
	if err != nil {
		return sendFailure(stream, fmt.Sprintf("ACL LIST (after): %v", err))
	}

	changed := !aclListEqual(before, after)
	return sendOutcome(stream, changed, "ACL LOAD ok", map[string]any{
		"users": int64(len(after)),
	})
}

// aclListEqual — посимвольное сравнение двух ACL LIST (порядок значим: ACL LIST
// возвращает пользователей детерминированно, по порядку загрузки из файла, и
// после LOAD порядок отражает файл). Любое расхождение → ACL изменился.
func aclListEqual(before, after []string) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		if before[i] != after[i] {
			return false
		}
	}
	return true
}

// configGet читает CONFIG GET <param> → текущее значение или "" (параметр пуст/нет).
// Использует ТИПИЗИРОВАННЫЙ ConfigGet драйвера (map[string]string), а НЕ
// Do+strings.Fields: многословные значения (save "900 1 300 10 60 10000") при
// space-join + Fields рассыпались бы в перепутанные пары → ложный CONFIG SET.
func configGet(ctx context.Context, conn redisConn, param string) (string, error) {
	m, err := conn.ConfigGet(ctx, param)
	if err != nil {
		return "", err
	}
	return m[param], nil
}

func (m *RedisModule) openConn(ctx context.Context, cfg connConfig) (redisConn, error) {
	if m.connect != nil {
		return m.connect(ctx, cfg)
	}
	return defaultConnect(ctx, cfg)
}

func defaultConnect(ctx context.Context, cfg connConfig) (redisConn, error) {
	opts := &redis.Options{
		Username: cfg.username,
		Password: cfg.password,
		DB:       cfg.db,
	}
	// unix:-префикс → unix-сокет; иначе TCP host:port.
	if path, ok := strings.CutPrefix(cfg.addr, "unix:"); ok {
		opts.Network = "unix"
		opts.Addr = path
	} else {
		opts.Network = "tcp"
		opts.Addr = cfg.addr
	}
	// TLS: при tls=true go-redis коннектится по TLS (RootCAs/client-cert/
	// skip_verify из cfg.tls). Это ОБЯЗАТЕЛЬНО для only-TLS (port 0): без
	// tls.Config go-redis шлёт plaintext и упирается в закрытый plain-порт.
	tlsCfg, err := buildTLSConfig(cfg.tls)
	if err != nil {
		return nil, err
	}
	opts.TLSConfig = tlsCfg
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &realConn{c: c}, nil
}

// realConn — обёртка *redis.Client под redisConn.
type realConn struct {
	c *redis.Client
}

func (r *realConn) Do(ctx context.Context, args ...any) (string, error) {
	res, err := r.c.Do(ctx, args...).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", nil
		}
		return "", err
	}
	return stringifyResult(res), nil
}

// ConfigGet — типизированный CONFIG GET через go-redis (map[string]string).
// Значения сохраняются целиком, включая многословные (save). redis.Nil →
// пустой map (параметр без значения).
func (r *realConn) ConfigGet(ctx context.Context, param string) (map[string]string, error) {
	m, err := r.c.ConfigGet(ctx, param).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return m, nil
}

// GetKeysInSlot — типизированный CLUSTER GETKEYSINSLOT через go-redis ([]string).
// Ключи возвращаются целиком, включая пробельные символы в имени. redis.Nil →
// пустой срез (слот опустошён).
func (r *realConn) GetKeysInSlot(ctx context.Context, slot, count int) ([]string, error) {
	keys, err := r.c.ClusterGetKeysInSlot(ctx, slot, count).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return keys, nil
}

// AclList — типизированный ACL LIST через go-redis ([]string, по строке на
// пользователя). Строки сохраняются целиком (правило ACL содержит пробелы).
// redis.Nil → пустой срез (ACL не сконфигурирован).
func (r *realConn) AclList(ctx context.Context) ([]string, error) {
	users, err := r.c.ACLList(ctx).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return users, nil
}

func (r *realConn) Close() error { return r.c.Close() }

// stringifyResult приводит ответ Redis к строке для Output. Скаляр → как есть,
// массив → join пробелом (best-effort; команды command/config возвращают простые
// ответы: OK / PONG / значение).
func stringifyResult(res any) string {
	switch v := res.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, e := range v {
			parts = append(parts, fmt.Sprintf("%v", e))
		}
		return strings.Join(parts, " ")
	default:
		return fmt.Sprintf("%v", v)
	}
}
