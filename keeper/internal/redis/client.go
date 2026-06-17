// Package redis — Keeper-side обёртка над `github.com/redis/go-redis/v9`.
//
// Назначение — общий клиент Redis для Keeper-инстанса (lease-based лидерство
// Reaper-а, в будущем heartbeat-cache flush, push-coordination — ADR-006).
// Живёт в `keeper/internal/`, не в `shared/`, по той же причине, что
// `keeper/internal/pg`: Soul-бинарь не должен зависеть от Redis-клиентa
// напрямую (ADR-011 / изоляция).
//
// Lease-семантика (Lua-скрипты compare-and-set/delete, ErrLeaseLost) — в
// соседнем файле [lease.go]. Этот файл — про connect/ping/close.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Config — параметры подключения. Соответствует блоку `keeper.yml::redis`
// (см. shared/config/keeper.go::KeeperRedis).
//
// `PasswordRef` — vault-ref формы `vault:<mount>/<path>`. Реальный resolve
// через Vault-клиент пока не реализован (отдельный slice M0.5d, привязан к
// pattern-у postgres DSN resolve). Поведение в M0.6/Reaper.a:
//
//   - пустая строка → подключение без password (dev-стек, docker-compose);
//   - `vault:...`-префикс → возвращается [ErrPasswordResolveNotImplemented];
//   - всё прочее → используется как plaintext (для unit/integration-тестов
//     с password-protected Redis-ом).
//
// Plaintext-ветка осознанно: keeper.yml в проде не содержит plaintext-секретов
// (semantic-валидация `password_ref` уже сейчас принимает только `vault:`-форму
// — см. shared/config/semantic.go::checkVaultRef), а тесты иногда удобнее
// гонять с inline-password, чем держать Vault-fixture.
type Config struct {
	Addr        string
	PasswordRef string
	DB          int
}

// ErrPasswordResolveNotImplemented — sentinel, который вернёт [NewClient]
// для `password_ref` в форме `vault:...`. M0.5d закроет, переиспользовав
// тот же resolve-pattern, что и для postgres DSN.
var ErrPasswordResolveNotImplemented = errors.New("redis: password vault-resolve not implemented (pending M0.5d)")

// Client — тонкая обёртка над `*redis.Client`. Дополнительной логики нет —
// конструктор скрывает зависимость go-redis от прочих пакетов keeper-а
// (вторая причина — единая точка для будущего OTel-tracing/instrumentation).
type Client struct {
	rdb *redis.Client
}

// NewClient создаёт клиент и делает один Ping, чтобы Reaper-runner на
// старте сразу падал при недоступном Redis-е, а не на первом Acquire.
// При vault-ref в password_ref возвращает [ErrPasswordResolveNotImplemented].
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("redis: addr is empty")
	}

	password, err := resolvePassword(cfg.PasswordRef)
	if err != nil {
		return nil, err
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: password,
		DB:       cfg.DB,
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis: ping %q: %w", cfg.Addr, err)
	}

	return &Client{rdb: rdb}, nil
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
// (lease.go). Внешним пакетам не экспонируется.
func (c *Client) underlying() *redis.Client { return c.rdb }

// resolvePassword — pre-M0.5d заглушка vault-resolve-а. См. doc-comment
// [Config.PasswordRef].
func resolvePassword(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if strings.HasPrefix(ref, "vault:") {
		return "", ErrPasswordResolveNotImplemented
	}
	return ref, nil
}
