package redis

// Heartbeat-кэш Soul-агентов в Redis (ADR-006(a) →
// docs/keeper/storage.md — роль (a) Heartbeat-кэш).
//
// PG `souls.last_seen_at` / `last_seen_by_kid` — flush-снимок;
// real-time значение живёт здесь. На каждое app-сообщение в
// EventStream-е (Hello / TaskEvent / RunResult / SoulprintReport)
// EventStream-handler пишет [TouchHeartbeat]. PG-snapshot
// (`souls.last_seen_at`) тот же handler сбрасывает throttled —
// не чаще раза в `stale_after/3` на каждый SID
// (keeper/internal/grpc/heartbeat_flush.go), иначе Reaper-правило
// `mark_disconnected` ложно метило бы живой стрим disconnected.
//
// Структура — Hash `soul:<sid>:hb` с полями `at` (RFC3339Nano,
// UTC) и `kid`. Hash, а не два отдельных ключа, чтобы атомарно
// обновлять оба поля одной командой и читать одним HGETALL при
// flush-е. TTL не выставляется: запись живёт до явного DEL
// (Reaper-правила `purge_souls` / `mark_disconnected`); при
// рестарте всего Redis-а данные теряются, и flush-snapshot из PG
// служит fallback-ом до первого нового сообщения.

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// HeartbeatKey — Redis-ключ Hash-а heartbeat-кэша конкретного SID.
func HeartbeatKey(sid string) string { return "soul:" + sid + ":hb" }

// TouchHeartbeat обновляет heartbeat-кэш для SID атомарно. Пишет
// `at = now` (UTC, RFC3339Nano) и `kid = <kid>` одним HSET-вызовом.
//
// Не возвращает ошибки на конфликт с конкурирующим writer-ом — последняя
// запись побеждает (Soul-стрим обслуживается одним Keeper-ом одновременно
// по [SoulLease], так что конкуренции в норме нет).
func TouchHeartbeat(ctx context.Context, c *Client, sid, kid string, now time.Time) error {
	if c == nil {
		return errors.New("redis.TouchHeartbeat: nil client")
	}
	if sid == "" {
		return errors.New("redis.TouchHeartbeat: empty sid")
	}
	if kid == "" {
		return errors.New("redis.TouchHeartbeat: empty kid")
	}
	if now.IsZero() {
		now = time.Now()
	}
	err := c.underlying().HSet(ctx, HeartbeatKey(sid),
		"at", now.UTC().Format(time.RFC3339Nano),
		"kid", kid,
	).Err()
	if err != nil {
		return fmt.Errorf("redis.TouchHeartbeat: HSET %q: %w", HeartbeatKey(sid), err)
	}
	return nil
}

// ReadHeartbeat возвращает последнее значение из кэша. Полезно для
// Operator API / диагностики; основной потребитель — будущий batch-flush.
//
// На отсутствие ключа возвращает (time.Time{}, "", false, nil) —
// «не было heartbeat-а», не ошибка.
func ReadHeartbeat(ctx context.Context, c *Client, sid string) (at time.Time, kid string, ok bool, err error) {
	if c == nil {
		return time.Time{}, "", false, errors.New("redis.ReadHeartbeat: nil client")
	}
	res, err := c.underlying().HGetAll(ctx, HeartbeatKey(sid)).Result()
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("redis.ReadHeartbeat: HGETALL %q: %w", HeartbeatKey(sid), err)
	}
	if len(res) == 0 {
		return time.Time{}, "", false, nil
	}
	atStr := res["at"]
	kid = res["kid"]
	if atStr == "" || kid == "" {
		// Битая запись (полу-записаны половины). Считаем за отсутствие.
		return time.Time{}, "", false, nil
	}
	at, err = time.Parse(time.RFC3339Nano, atStr)
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("redis.ReadHeartbeat: parse at %q: %w", atStr, err)
	}
	return at, kid, true, nil
}
