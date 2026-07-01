package redis

// Lease — Redis-based лидерство для фоновых задач Keeper-а
// (ADR-006(d), Reaper-loop в M0.6+). Алгоритм:
//
//   - Acquire — `SET key holder NX EX ttl`. Если ключ уже занят
//     другим holder-ом — [ErrLeaseTaken]; вызывающий ретраит сам с
//     backoff-ом.
//   - Renew — Lua-скрипт CAS: если `GET key == holder`, обновить TTL
//     через `PEXPIRE`; иначе [ErrLeaseLost] (нас вытеснил кто-то).
//   - Release — Lua-скрипт CAS: `DEL key`, только если holder совпал.
//     На `not-mine` молча выходит (idempotent stop).
//
// Holder-строка — `kid` Keeper-инстанса (см. shared/config/keeper.go::KID),
// caller передаёт её сам. Это даёт человекочитаемые логи «lease acquired
// by keeper-eu-west-01», и позволяет реактору в будущем различать смены
// лидерства между инстансами одного кластера.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLeaseTaken — [Acquire] не получил lease, ключ удерживается другим
// holder-ом. Caller (Reaper-runner) ретраит с backoff-ом.
var ErrLeaseTaken = errors.New("redis: lease already taken")

// ErrLeaseLost — [Lease.Renew] обнаружил, что ключ принадлежит другому
// holder-у (или истёк и был перезахвачен). Caller обязан остановить
// loop — split-brain недопустим.
var ErrLeaseLost = errors.New("redis: lease lost (no longer leader)")

// Lease — handle на удерживаемый ключ.
//
// Lease НЕ потокобезопасен относительно Release vs Renew: caller обязан
// упорядочить вызовы (типично — один renewal-goroutine + один Release из
// main-defer-а). Конкурентные Renew между собой безопасны (Redis сам
// сериализует команды), но смысла в этом нет.
type Lease struct {
	client *Client
	key    string
	holder string
	ttl    time.Duration
}

// Key — Redis-ключ, который удерживает lease. Полезно для логов.
func (l *Lease) Key() string { return l.key }

// Holder — значение, записанное в ключ. Полезно для логов.
func (l *Lease) Holder() string { return l.holder }

// TTL — текущий target-TTL ключа. Renew продлевает до этого значения.
func (l *Lease) TTL() time.Duration { return l.ttl }

// Acquire пробует захватить lease. На success возвращает handle; на
// конфликт — [ErrLeaseTaken]; на сетевую/протокольную ошибку — обёрнутый err.
//
// `ttl` должно быть > 0 — отрицательное / нулевое значение означало бы
// либо моментально-истекающий lease (race-окно), либо бесконечный (Redis
// `SET ... EX 0` это ошибка). В обоих случаях — программная ошибка caller-а.
func Acquire(ctx context.Context, c *Client, key, holder string, ttl time.Duration) (*Lease, error) {
	if c == nil {
		return nil, errors.New("redis.Acquire: nil client")
	}
	if key == "" {
		return nil, errors.New("redis.Acquire: empty key")
	}
	if holder == "" {
		return nil, errors.New("redis.Acquire: empty holder")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redis.Acquire: ttl must be > 0, got %v", ttl)
	}

	ok, err := c.underlying().SetNX(ctx, key, holder, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("redis.Acquire: SETNX %q: %w", key, err)
	}
	if !ok {
		return nil, ErrLeaseTaken
	}
	return &Lease{client: c, key: key, holder: holder, ttl: ttl}, nil
}

// PeekLeaseHolder читает текущего holder-а lease-ключа (value = KID держателя)
// БЕЗ захвата — чистый `GET`. Возвращает (holder, true) если ключ жив;
// (_, false) если lease свободен / истёк (`redis.Nil`).
//
// Read-only-инспекция для наблюдаемости (`GET /v1/cluster` → кто сейчас
// Reaper-лидер по ключу reaper.LeaderLeaseKey). НЕ участвует в CAS-протоколе
// Acquire/Renew/Release: только показывает, кому ключ принадлежит прямо сейчас.
// Значение эфемерно (lease под TTL) — caller трактует ответ как снимок момента.
func PeekLeaseHolder(ctx context.Context, c *Client, key string) (string, bool, error) {
	if c == nil {
		return "", false, errors.New("redis.PeekLeaseHolder: nil client")
	}
	if key == "" {
		return "", false, errors.New("redis.PeekLeaseHolder: empty key")
	}
	v, err := c.underlying().Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("redis.PeekLeaseHolder: GET %q: %w", key, err)
	}
	return v, true, nil
}

// renewScript — CAS-renew: вернёт 1 если ключ всё ещё наш и TTL обновлён,
// иначе 0. PEXPIRE применяется к существующему ключу — атомарность
// GET+PEXPIRE гарантируется тем, что Lua-скрипт исполняется атомарно
// внутри Redis-а.
//
// PEXPIRE (миллисекунды) выбран вместо EXPIRE (секунды), чтобы lock_ttl
// уровня sub-second в тестах (typical миnireredis-тесты — 50–200 ms)
// работал точно; в проде sub-second не нужен, но overhead нулевой.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
	return 0
end
`)

// Renew продлевает TTL ключа до [Lease.TTL] (через PEXPIRE), но только
// если значение по-прежнему равно [Lease.Holder]. На несовпадение
// holder-а — [ErrLeaseLost]; ключа уже нет (истёк и не пересоздан) —
// тоже [ErrLeaseLost] (CAS вернёт 0, потому что GET вернул nil).
func (l *Lease) Renew(ctx context.Context) error {
	if l == nil || l.client == nil {
		return errors.New("redis.Lease.Renew: nil lease/client")
	}
	res, err := renewScript.Run(ctx, l.client.underlying(),
		[]string{l.key},
		l.holder,
		l.ttl.Milliseconds(),
	).Int64()
	if err != nil {
		return fmt.Errorf("redis.Lease.Renew %q: %w", l.key, err)
	}
	if res != 1 {
		return ErrLeaseLost
	}
	return nil
}

// releaseScript — CAS-delete: удалить ключ только если значение наше.
// Возвращает количество удалённых ключей (0 или 1).
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
else
	return 0
end
`)

// Release удаляет ключ, если он всё ещё принадлежит нам. На чужой holder
// или истёкший ключ — no-op (идемпотентно). Сетевая ошибка прокидывается,
// но caller обычно её игнорирует — Release вызывается из defer-shutdown-а,
// где Redis может быть уже недоступен.
//
// После Release повторный Renew/Release безопасен — вернёт [ErrLeaseLost]
// или no-op соответственно (Redis-state-driven, без флага в Go-struct).
func (l *Lease) Release(ctx context.Context) error {
	if l == nil || l.client == nil {
		return errors.New("redis.Lease.Release: nil lease/client")
	}
	_, err := releaseScript.Run(ctx, l.client.underlying(),
		[]string{l.key},
		l.holder,
	).Int64()
	if err != nil {
		return fmt.Errorf("redis.Lease.Release %q: %w", l.key, err)
	}
	return nil
}
