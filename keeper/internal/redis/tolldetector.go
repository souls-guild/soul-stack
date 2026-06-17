package redis

// Toll cluster-detector Redis-primitives (ADR-038). Тонкие helper-ы — единое
// место всех Redis-ops Toll-инфраструктуры (паттерн heartbeat.go / soullease.go
// / conclave.go): пакет `toll` потребляет их через узкие интерфейсы
// (toll.Publisher / toll.DegradedReader / ...), не тянет *redis.Client
// напрямую.
//
// Ключи: см. doc-комментарии toll-пакета — SortedSetKey ("toll:disconnects"),
// LeaseKey ("cluster:toll:leader"), DegradedKey ("cluster:degraded"). Они
// дублируются строками здесь сознательно: keeperredis — низкоуровневый слой,
// не должен import-ить toll (toll → keeperredis-направление). При расхождении
// тесты пакета `toll` (integration) поймают.

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	tollSortedSetKey = "toll:disconnects"
	tollDegradedKey  = "cluster:degraded"
)

// PublishTollDisconnect — ZADD одного disconnect-event-а в sorted-set
// `toll:disconnects` (score=unix-сек, value=member). Caller (toll.Watcher через
// adapter) формирует member через toll.EncodeDisconnect. Идемпотентность не
// гарантируется (для уникальности member-а EncodeDisconnect добавляет UnixNano-
// суффикс).
func PublishTollDisconnect(ctx context.Context, c *Client, member string, atUnix int64) error {
	if c == nil {
		return fmt.Errorf("redis.PublishTollDisconnect: nil client")
	}
	if member == "" {
		return fmt.Errorf("redis.PublishTollDisconnect: empty member")
	}
	if err := c.underlying().ZAdd(ctx, tollSortedSetKey,
		redis.Z{Score: float64(atUnix), Member: member},
	).Err(); err != nil {
		return fmt.Errorf("redis.PublishTollDisconnect: ZADD %q: %w", tollSortedSetKey, err)
	}
	return nil
}

// TollCountInWindow — ZCOUNT sorted-set по range [fromUnix, toUnix]. Caller
// (toll.Leader через adapter) считает rate.
func TollCountInWindow(ctx context.Context, c *Client, fromUnix, toUnix int64) (int64, error) {
	if c == nil {
		return 0, fmt.Errorf("redis.TollCountInWindow: nil client")
	}
	n, err := c.underlying().ZCount(ctx, tollSortedSetKey,
		fmt.Sprintf("%d", fromUnix),
		fmt.Sprintf("%d", toUnix),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.TollCountInWindow: ZCOUNT %q: %w", tollSortedSetKey, err)
	}
	return n, nil
}

// TollCountByCovenInWindow — ZRANGEBYSCORE по диапазону + group-by coven
// (ADR-038 amendment 2026-05-27, per-coven thresholds).
//
// Member-value `<sid>|<kid>|<coven>|<nano>` (см. toll.EncodeDisconnect):
// извлекаем coven из 3-го `|`-сегмента. Невалидные/слишком короткие member-ы
// пропускаются (defensive: разреш Redis-данные старых форматов после
// rolling-upgrade без падения).
//
// Возвращает map[coven]count; пустой coven попадает в ключ "" (Watcher
// допускает пустую coven-метку). На пустом окне — пустой map без ошибки.
func TollCountByCovenInWindow(ctx context.Context, c *Client, fromUnix, toUnix int64) (map[string]int64, error) {
	if c == nil {
		return nil, fmt.Errorf("redis.TollCountByCovenInWindow: nil client")
	}
	members, err := c.underlying().ZRangeByScore(ctx, tollSortedSetKey, &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", fromUnix),
		Max: fmt.Sprintf("%d", toUnix),
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis.TollCountByCovenInWindow: ZRANGEBYSCORE %q: %w", tollSortedSetKey, err)
	}
	counts := make(map[string]int64, len(members))
	for _, m := range members {
		coven, ok := extractCovenFromMember(m)
		if !ok {
			continue
		}
		counts[coven]++
	}
	return counts, nil
}

// extractCovenFromMember парсит member-value `<sid>|<kid>|<coven>|<nano>` и
// возвращает 3-й сегмент. ok=false при < 3 сегментах (невалидный/обрезанный
// member).
func extractCovenFromMember(m string) (string, bool) {
	// Поиск 1-го `|` → start of kid.
	i1 := indexByte(m, '|')
	if i1 < 0 {
		return "", false
	}
	rest := m[i1+1:]
	// 2-й `|` → start of coven.
	i2 := indexByte(rest, '|')
	if i2 < 0 {
		return "", false
	}
	rest = rest[i2+1:]
	// 3-й `|` → end of coven (есть всегда: EncodeDisconnect добавляет nano-суффикс).
	i3 := indexByte(rest, '|')
	if i3 < 0 {
		return "", false
	}
	return rest[:i3], true
}

// indexByte — локальный аналог strings.IndexByte без импорта strings ради
// единственной функции. Inline-friendly.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TollTrimBelow — ZREMRANGEBYSCORE удаляет всё со score < beforeUnix.
// Idempotent (на пустой выборке возвращает 0 без ошибки). Caller (Leader)
// чистит хвост окна на каждом тике.
func TollTrimBelow(ctx context.Context, c *Client, beforeUnix int64) error {
	if c == nil {
		return fmt.Errorf("redis.TollTrimBelow: nil client")
	}
	if err := c.underlying().ZRemRangeByScore(ctx, tollSortedSetKey,
		"-inf",
		fmt.Sprintf("(%d", beforeUnix),
	).Err(); err != nil {
		return fmt.Errorf("redis.TollTrimBelow: ZREMRANGEBYSCORE %q: %w", tollSortedSetKey, err)
	}
	return nil
}

// TollSetDegraded — `SET cluster:degraded <holder> EX <ttl>`. Не NX — Leader
// освежает TTL на каждом своём тике (re-arm). Holder для диагностики «какой
// инстанс взвёл флаг».
func TollSetDegraded(ctx context.Context, c *Client, holder string, ttl time.Duration) error {
	if c == nil {
		return fmt.Errorf("redis.TollSetDegraded: nil client")
	}
	if holder == "" {
		return fmt.Errorf("redis.TollSetDegraded: empty holder")
	}
	if ttl <= 0 {
		return fmt.Errorf("redis.TollSetDegraded: ttl must be > 0, got %v", ttl)
	}
	if err := c.underlying().Set(ctx, tollDegradedKey, holder, ttl).Err(); err != nil {
		return fmt.Errorf("redis.TollSetDegraded: SET %q: %w", tollDegradedKey, err)
	}
	return nil
}

// TollClearDegraded — `DEL cluster:degraded`. Idempotent.
func TollClearDegraded(ctx context.Context, c *Client) error {
	if c == nil {
		return fmt.Errorf("redis.TollClearDegraded: nil client")
	}
	if err := c.underlying().Del(ctx, tollDegradedKey).Err(); err != nil {
		return fmt.Errorf("redis.TollClearDegraded: DEL %q: %w", tollDegradedKey, err)
	}
	return nil
}

// TollIsDegraded — EXISTS cluster:degraded. true = флаг стоит, false = нет.
// EXISTS дешевле GET — value не нужен (флаг бинарный).
func TollIsDegraded(ctx context.Context, c *Client) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("redis.TollIsDegraded: nil client")
	}
	n, err := c.underlying().Exists(ctx, tollDegradedKey).Result()
	if err != nil {
		return false, fmt.Errorf("redis.TollIsDegraded: EXISTS %q: %w", tollDegradedKey, err)
	}
	return n == 1, nil
}
