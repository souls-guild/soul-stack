// Package redis — Keeper-side обёртка над `github.com/redis/go-redis/v9`.
//
// Назначение — общий клиент Redis для Keeper-инстанса (lease-based лидерство
// Reaper-а, heartbeat/presence-кэш, apply-events pub/sub — ADR-006). Живёт в
// `keeper/internal/`, не в `shared/`, по той же причине, что `keeper/internal/pg`:
// Soul-бинарь не должен зависеть от Redis-клиентa напрямую (ADR-011 / изоляция).
//
// Топология (ADR-006 amendment) выбирается полем [Config.Mode]:
//   - standalone (default) — один узел;
//   - sentinel — Redis Sentinel HA (master через sentinel-узлы);
//   - cluster — Redis Cluster (шардирование по слотам).
//
// Все три реализуют `redis.UniversalClient`, поэтому соседние файлы пакета
// (lease/soullease/conclave/herald/applybus/…) работают через [Client.underlying]
// единообразно. Cluster-специфика (per-master SCAN для presence, hash-tag на
// Herald-очереди) — точечно в conclave.go / heralddelivery.go.
//
// Lease-семантика (Lua-скрипты compare-and-set/delete, ErrLeaseLost) — в
// соседнем файле [lease.go]. Этот файл — про connect/ping/close и vault-resolve
// пароля.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// Mode-константы топологии Redis. Совпадают с enum `redis.mode` config-схемы
// (shared/config/schema.go::enumRedisMode). Пустой Mode == ModeStandalone.
const (
	ModeStandalone = "standalone"
	ModeSentinel   = "sentinel"
	ModeCluster    = "cluster"
)

// defaultPasswordField — поле Vault KV-secret-а, в котором лежит plain-пароль
// Redis, если в vault-ref не указан явный `#field`. Симметрия с pg.vaultDSNField
// (там фиксированное `dsn`), но здесь поле override-ится через `#field`.
const defaultPasswordField = "password"

// Config — параметры подключения. Соответствует блоку `keeper.yml::redis`
// (см. shared/config/keeper.go::KeeperRedis).
//
// `Mode` — топология (см. const-ы выше); пусто = standalone (forward-compat).
// `Addr` — адрес узла для standalone. `Sentinels`/`MasterName` — для sentinel.
// `Nodes` — для cluster.
//
// `PasswordRef` / `SentinelPasswordRef` — пароль Redis / пароль sentinel-узлов
// в форме vault-ref `vault:<mount>/<path>[#field]` (резолв через [resolvePassword]),
// либо plaintext (тесты с password-protected Redis-ом без Vault-fixture).
type Config struct {
	Mode                string
	Addr                string
	PasswordRef         string
	MasterName          string
	Sentinels           []string
	Nodes               []string
	SentinelPasswordRef string
	DB                  int
}

// passwordResolver — узкий контракт vault-резолва пароля (то, что использует
// [resolvePassword]). `*keepervault.Client` его удовлетворяет; интерфейс введён
// ради тестируемости (stub без поднятого Vault) и параллелен pg.ResolveDSN,
// который принимает конкретный `*keepervault.Client`.
type passwordResolver interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// ErrVaultClientRequired возвращается, когда `password_ref`/`sentinel_password_ref`
// начинается с `vault:`, но vault-client не передан (nil). Инвариант caller-а
// (`keeper/cmd/keeper`): vault-client поднимается до setupRedis. Parity с
// pg.ErrVaultClientRequired.
var ErrVaultClientRequired = errors.New("redis: vault client is required for vault:-ref")

// ErrPasswordFieldMissing возвращается, если поле пароля (`password` или
// `#field`-override) в Vault KV отсутствует, пустое или не string.
var ErrPasswordFieldMissing = errors.New("redis: password field missing or empty in Vault KV")

// Client — тонкая обёртка над `redis.UniversalClient` (standalone/sentinel/
// cluster — `*redis.Client` / `*redis.FailoverClient` / `*redis.ClusterClient`).
// Дополнительной логики нет: конструктор скрывает зависимость go-redis от прочих
// пакетов keeper-а и даёт единую точку для будущего OTel-tracing.
type Client struct {
	rdb redis.UniversalClient
}

// NewClient создаёт клиент по [Config.Mode] и делает один Ping, чтобы
// зависящие подсистемы на старте сразу падали при недоступном Redis-е, а не на
// первой операции.
//
// `vc` (vault-client) обязателен только если `password_ref`/`sentinel_password_ref`
// — vault-ref (`vault:...`); для plaintext/empty можно передать nil. Parity с
// pg.NewPool(ctx, cfg, vc).
func NewClient(ctx context.Context, cfg Config, vc passwordResolver) (*Client, error) {
	password, err := resolvePassword(ctx, vc, cfg.PasswordRef)
	if err != nil {
		return nil, fmt.Errorf("redis: resolve password: %w", err)
	}
	sentinelPassword, err := resolvePassword(ctx, vc, cfg.SentinelPasswordRef)
	if err != nil {
		return nil, fmt.Errorf("redis: resolve sentinel password: %w", err)
	}

	rdb, err := build(cfg, password, sentinelPassword)
	if err != nil {
		return nil, err
	}

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping (mode=%s): %w", resolvedMode(cfg.Mode), err)
	}

	return &Client{rdb: rdb}, nil
}

// build выбирает конкретную go-redis-реализацию по [Config.Mode]. Чистый свитч
// по mode: пароли уже резолвлены caller-ом ([NewClient]), vault-зависимости тут
// нет. Ping вынесен в [NewClient].
func build(cfg Config, password, sentinelPassword string) (redis.UniversalClient, error) {
	switch resolvedMode(cfg.Mode) {
	case ModeStandalone:
		if cfg.Addr == "" {
			return nil, errors.New("redis: addr is empty (mode=standalone)")
		}
		return redis.NewClient(&redis.Options{
			Addr:     cfg.Addr,
			Password: password,
			DB:       cfg.DB,
		}), nil

	case ModeSentinel:
		if cfg.MasterName == "" {
			return nil, errors.New("redis: master_name is empty (mode=sentinel)")
		}
		if len(cfg.Sentinels) == 0 {
			return nil, errors.New("redis: sentinels is empty (mode=sentinel)")
		}
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.MasterName,
			SentinelAddrs:    cfg.Sentinels,
			Password:         password,
			SentinelPassword: sentinelPassword,
			DB:               cfg.DB,
		}), nil

	case ModeCluster:
		if len(cfg.Nodes) == 0 {
			return nil, errors.New("redis: nodes is empty (mode=cluster)")
		}
		// DB в cluster-режиме не применяется (go-redis ClusterOptions без DB —
		// у Redis Cluster нет логических БД, только slot 0). Игнорируем молча:
		// config-схема не запрещает DB, но cluster всегда оперирует db0.
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    cfg.Nodes,
			Password: password,
		}), nil

	default:
		return nil, fmt.Errorf("redis: unknown mode %q", cfg.Mode)
	}
}

// resolvedMode нормализует пустой Mode в ModeStandalone (forward-compat).
func resolvedMode(mode string) string {
	if mode == "" {
		return ModeStandalone
	}
	return mode
}

// Ping — health-check для Reaper-loop-а / readiness-проб.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close закрывает underlying-клиент. Идемпотентен (повторный Close на
// go-redis/v9 возвращает [redis.ErrClosed] — игнорируем).
func (c *Client) Close() error {
	err := c.rdb.Close()
	if err == nil || errors.Is(err, redis.ErrClosed) {
		return nil
	}
	return err
}

// underlying — доступ к go-redis-клиенту для соседних файлов пакета
// (lease/soullease/conclave/herald/applybus/…). Внешним пакетам не экспонируется.
// Тип — интерфейс UniversalClient: все три топологии скрыты за ним. Cluster-
// специфику (ForEachMaster) соседние файлы достают type-switch-ем по факт-типу.
func (c *Client) underlying() redis.UniversalClient { return c.rdb }

// resolvePassword превращает password-ref из config-а в plain-пароль. Поддерживает:
//
//   - пустую строку → "" (подключение без password: dev-стек, docker-compose);
//   - не-`vault:`-строку → as-is (plaintext: unit/integration-тесты с
//     password-protected Redis-ом без Vault-fixture);
//   - vault-ref `vault:<mount>/<path>[#field]` → читает поле из Vault KV
//     (`#field` override, default `password`). `vc` обязателен; иначе
//     [ErrVaultClientRequired].
//
// Симметрия с pg.ResolveDSN, но с `#field`-override (у pg поле фиксированное `dsn`).
func resolvePassword(ctx context.Context, vc passwordResolver, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if !strings.HasPrefix(ref, "vault:") {
		return ref, nil
	}
	if vc == nil {
		return "", fmt.Errorf("%w: ref=%q", ErrVaultClientRequired, ref)
	}

	// `#field`-override отделяем ДО ParseRef: vault-ref-форма `vault:<m>/<p>#field`,
	// поле — после '#'. ParseRef валидирует и нормализует logical-path без поля.
	refPath, field := splitFieldOverride(ref)
	path, err := keepervault.ParseRef(refPath)
	if err != nil {
		return "", fmt.Errorf("redis: password_ref: %w", err)
	}
	kv, err := vc.ReadKV(ctx, path)
	if err != nil {
		return "", fmt.Errorf("redis: read vault %q: %w", path, err)
	}
	return extractPassword(kv, field)
}

// splitFieldOverride отделяет `#field`-суффикс от vault-ref-а. Без '#' —
// возвращает (ref, defaultPasswordField).
func splitFieldOverride(ref string) (refWithoutField, field string) {
	if i := strings.LastIndexByte(ref, '#'); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, defaultPasswordField
}

// extractPassword достаёт поле `field` из Vault KV payload-а (string).
func extractPassword(kv map[string]any, field string) (string, error) {
	raw, ok := kv[field]
	if !ok {
		return "", fmt.Errorf("%w: field=%q", ErrPasswordFieldMissing, field)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("redis: vault field %q has unsupported type %T (want string)", field, raw)
	}
	if s == "" {
		return "", fmt.Errorf("%w: field=%q", ErrPasswordFieldMissing, field)
	}
	return s, nil
}
