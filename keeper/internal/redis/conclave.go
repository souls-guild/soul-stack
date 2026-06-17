package redis

// Conclave — реестр живых Keeper-инстансов кластера в Redis (ADR-006 amend,
// soul-shedding S1). Каждый инстанс на старте регистрирует свою presence-запись
// `keeper:instance:<kid>` с TTL и продлевает её renewal-goroutine-ой; на
// graceful shutdown — удаляет, на crash — запись истекает по TTL. Перечисление
// живых ([LiveKIDs] / [CountLive]) идёт SCAN-ом по префиксу.
//
// Отличие от [Lease]/[SoulLease]: это НЕ эксклюзивный lock. Каждый инстанс
// держит СВОЙ ключ (по своему KID), конкуренции за один ключ нет —
// регистрация без NX. Опц. NX-проверка ([RegisterInstance]) ловит коллизию KID
// (два keeper-процесса с одинаковым `kid` в конфиге = ошибка оператора) и
// логируется как warn, не как блокирующая ошибка: presence своего KID — это
// инвариант, а не борьба за лидерство.
//
// Авторитет presence — Redis (инвариант presence→Redis): PG-вариант отвергнут
// (presence волатильна, TTL+renew — естественная модель). Единый источник
// истины — сами TTL-ключи, без параллельного Redis-Set (как [SoulsStreamAlive]
// отверг отдельный Set живых SID-ов): десятки инстансов на кластер — SCAN
// дёшев.
//
// Питает (S2/S3, отдельные слайсы): refuse-guard «я не один» (CountLive > 1) и
// soul-shedding (есть куда уходить — LiveKIDs минус собственный KID).

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	// conclaveKeyPrefix — префикс presence-ключей Conclave. Имя ключа
	// техническое (как `soul:<sid>:lock`), сущность в словаре — Conclave.
	conclaveKeyPrefix = "keeper:instance:"

	// DefaultConclaveTTL / DefaultConclaveRenewInterval — TTL presence-ключа и
	// период его продления. TTL ≈ 3×renew, чтобы пережить кратковременные
	// GC-паузы / latency-spike-и Renew-а (тот же запас, что у SoulLease).
	DefaultConclaveTTL           = 30 * time.Second
	DefaultConclaveRenewInterval = 10 * time.Second
)

// ErrConclaveKIDTaken — [RegisterInstance] с requireUnique=true обнаружил, что
// ключ `keeper:instance:<kid>` уже существует. Означает коллизию KID (два
// keeper-процесса с одинаковым `kid` в конфиге) — ошибка конфигурации
// оператора. Caller логирует warn и продолжает регистрацию (presence своего
// KID — инвариант).
var ErrConclaveKIDTaken = errors.New("redis: conclave instance key already exists (KID collision)")

// ConclaveKey формирует Redis presence-ключ для конкретного KID.
func ConclaveKey(kid string) string {
	return conclaveKeyPrefix + kid
}

// RegisterInstance записывает presence-запись keeper-инстанса
// `keeper:instance:<kid>` с TTL `ttl` и value `meta` (лёгкие метаданные для
// диагностики — JSON / KID, caller формирует сам).
//
// requireUnique=true сначала проверяет отсутствие ключа (NX-семантика через
// `SET NX`): на коллизию KID возвращает [ErrConclaveKIDTaken] БЕЗ перезаписи —
// чтобы caller залогировал warn. requireUnique=false (штатный путь рестарта:
// тот же KID после crash-а, чужой TTL-ключ ещё не истёк) — безусловный SET,
// перетирая возможный собственный остаток.
//
// `ttl` должно быть > 0 (как [Acquire]): нулевой / отрицательный TTL — ошибка
// caller-а.
func RegisterInstance(ctx context.Context, c *Client, kid, meta string, ttl time.Duration, requireUnique bool) error {
	if c == nil {
		return errors.New("redis.RegisterInstance: nil client")
	}
	if kid == "" {
		return errors.New("redis.RegisterInstance: empty kid")
	}
	if ttl <= 0 {
		return fmt.Errorf("redis.RegisterInstance: ttl must be > 0, got %v", ttl)
	}
	key := ConclaveKey(kid)

	if requireUnique {
		ok, err := c.underlying().SetNX(ctx, key, meta, ttl).Result()
		if err != nil {
			return fmt.Errorf("redis.RegisterInstance: SETNX %q: %w", key, err)
		}
		if !ok {
			return ErrConclaveKIDTaken
		}
		return nil
	}

	if err := c.underlying().Set(ctx, key, meta, ttl).Err(); err != nil {
		return fmt.Errorf("redis.RegisterInstance: SET %q: %w", key, err)
	}
	return nil
}

// RenewInstance продлевает TTL presence-ключа `keeper:instance:<kid>` до `ttl`
// (PEXPIRE на существующий ключ). В отличие от [Lease.Renew] здесь нет CAS-а по
// holder-у: ключ принадлежит этому инстансу по построению (один KID — один
// процесс), конкуренции за него нет.
//
// Если ключ исчез (истёк из-за пропущенных renew / был удалён) — PEXPIRE
// вернёт 0; renewal-goroutine пере-создаёт presence заново ([RegisterInstance]),
// чтобы исцелиться, а не молча перестать быть видимым кластеру (restart-safe
// семантика). На «ключ есть, TTL продлён» — ok=true.
func RenewInstance(ctx context.Context, c *Client, kid string, ttl time.Duration) (ok bool, err error) {
	if c == nil {
		return false, errors.New("redis.RenewInstance: nil client")
	}
	if kid == "" {
		return false, errors.New("redis.RenewInstance: empty kid")
	}
	if ttl <= 0 {
		return false, fmt.Errorf("redis.RenewInstance: ttl must be > 0, got %v", ttl)
	}
	res, err := c.underlying().PExpire(ctx, ConclaveKey(kid), ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis.RenewInstance: PEXPIRE %q: %w", ConclaveKey(kid), err)
	}
	return res, nil
}

// DeregisterInstance удаляет presence-ключ `keeper:instance:<kid>` (graceful
// shutdown). Идемпотентно: отсутствующий ключ — no-op (DEL вернёт 0).
// Сетевая ошибка прокидывается, но caller обычно её игнорирует — вызов идёт
// из shutdown-cleanup-а, где Redis может быть уже недоступен (crash-fallback на
// TTL-expiry).
func DeregisterInstance(ctx context.Context, c *Client, kid string) error {
	if c == nil {
		return errors.New("redis.DeregisterInstance: nil client")
	}
	if kid == "" {
		return errors.New("redis.DeregisterInstance: empty kid")
	}
	if err := c.underlying().Del(ctx, ConclaveKey(kid)).Err(); err != nil {
		return fmt.Errorf("redis.DeregisterInstance: DEL %q: %w", ConclaveKey(kid), err)
	}
	return nil
}

// LiveKIDs перечисляет KID-ы живых keeper-инстансов — SCAN по префиксу
// `keeper:instance:*` с обрезкой префикса. Мёртвый инстанс (crash без
// Deregister) выпадает из выборки по TTL-expiry его ключа.
//
// SCAN (не KEYS) — неблокирующий курсор: KEYS на проде блокирует Redis на всё
// время обхода keyspace. Десятки инстансов → один-два прохода курсора, дёшево.
// count=100 — hint размера батча per-итерация (Redis волен вернуть больше/
// меньше). Дубли KID-ов между батчами SCAN-а (возможны при rehash) свёрнуты
// через set.
func LiveKIDs(ctx context.Context, c *Client) ([]string, error) {
	if c == nil {
		return nil, errors.New("redis.LiveKIDs: nil client")
	}
	seen := make(map[string]struct{})
	var kids []string
	var cursor uint64
	for {
		keys, next, err := c.underlying().Scan(ctx, cursor, conclaveKeyPrefix+"*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("redis.LiveKIDs: SCAN: %w", err)
		}
		for _, k := range keys {
			kid := k[len(conclaveKeyPrefix):]
			if kid == "" {
				continue
			}
			if _, dup := seen[kid]; dup {
				continue
			}
			seen[kid] = struct{}{}
			kids = append(kids, kid)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return kids, nil
}

// CountLive возвращает число живых keeper-инстансов (= len([LiveKIDs])).
// Питает refuse-guard «я не один» (CountLive > 1, S3) — отдельный helper, чтобы
// caller-у, которому нужно только число, не аллоцировать слайс KID-ов.
func CountLive(ctx context.Context, c *Client) (int, error) {
	kids, err := LiveKIDs(ctx, c)
	if err != nil {
		return 0, err
	}
	return len(kids), nil
}
