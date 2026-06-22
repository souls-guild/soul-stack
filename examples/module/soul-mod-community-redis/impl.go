// soul-mod-community-redis — реальный SoulModule-плагин Soul Stack
// (community.redis): ОСНОВНОЙ интерфейс к живому Redis в redis-консолидации
// (концепция Ansible-роли). Scenario сервиса оркеструет порядок/таргетинг,
// плагин исполняет ОДНУ операцию над одним инстансом.
//
// PILOT (2026-06-22): реализованы два state — `command` (raw verb-state,
// changed=false по умолчанию) и `config` (CONFIG SET из map). acl / cluster /
// replica / sentinel / failover — следующие батчи.
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
	Close() error
}

// connConfig — параметры коннекта. password держится отдельно и НИКОГДА не
// логируется / не кладётся в события (ИБ-инвариант ADR-010).
type connConfig struct {
	addr     string // host:port или unix:/path
	username string
	password string
	db       int
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
	case "config":
		errs = append(errs, validateAddr(f)...)
		if len(stringMap(f["config"])) == 0 {
			errs = append(errs, "params.config: must be a non-empty map of directives")
		}
	case "cluster":
		// cluster коннектится к нодам из nodes-map, единый addr не требуется.
		errs = append(errs, validateCluster(f)...)
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (expected command|config|cluster)", req.GetState()))
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
		return sendFailure(stream, "connect: "+redactError(err, cfg.password))
	}
	defer func() { _ = conn.Close() }()

	switch req.GetState() {
	case "command":
		return m.applyCommand(ctx, stream, conn, req.GetParams())
	case "config":
		return m.applyConfig(ctx, stream, conn, req.GetParams())
	default:
		return sendFailure(stream, fmt.Sprintf("unknown state %q (expected command|config|cluster)", req.GetState()))
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

// applyConfig — CONFIG SET по каждой директиве (+ опц. CONFIG REWRITE). Порядок
// детерминированный (отсортированные ключи) ради воспроизводимого вывода.
// changed=true при ≥1 применённой директиве. Значения директив в события идут —
// это конфиг redis, не пароль; password в cfg отдельно и не светится.
func (m *RedisModule) applyConfig(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], conn redisConn, params *structpb.Struct) error {
	f := params.GetFields()
	directives := stringMap(f["config"])
	if len(directives) == 0 {
		return sendFailure(stream, "params.config: must be a non-empty map of directives")
	}
	rewrite := boolOrDefault(f["rewrite"], false)

	applied := make([]string, 0, len(directives))
	for _, key := range sortedKeys(directives) {
		if _, err := conn.Do(ctx, "CONFIG", "SET", key, directives[key]); err != nil {
			return sendFailure(stream, fmt.Sprintf("CONFIG SET %s: %v", key, err))
		}
		applied = append(applied, key)
	}

	if rewrite {
		if _, err := conn.Do(ctx, "CONFIG", "REWRITE"); err != nil {
			return sendFailure(stream, fmt.Sprintf("CONFIG REWRITE: %v", err))
		}
	}

	return sendOutcome(stream, true, fmt.Sprintf("CONFIG SET applied: %s", strings.Join(applied, ",")), map[string]any{
		"applied": strings.Join(applied, ","),
		"count":   int64(len(applied)),
		"rewrite": rewrite,
	})
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
