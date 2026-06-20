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
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// HeartbeatKey — Redis-ключ Hash-а heartbeat-кэша конкретного SID.
func HeartbeatKey(sid string) string { return "soul:" + sid + ":hb" }

// heartbeatCapsField — поле Hash-а `soul:<sid>:hb`, хранящее анонсированный при
// connect-е набор Soul-capabilities (ADR-056 §S5 forward-compat) — отсортированный
// уникальный список через запятую. Лежит в том же Hash-е, что heartbeat (а не
// отдельным ключом с собственным TTL): presence-фильтр таргет-резолвера уже
// отбрасывает оффлайн-хосты по живому SID-lease ДО чтения caps, поэтому stale-caps
// мёртвого хоста до Reaper-purge никого не таргетит. На Hello caps пишется ВСЕГДА
// перезаписью (включая пустым) — иначе старый бинарь, переподключившийся после
// нового, унаследовал бы stale-флаг "passage".
const heartbeatCapsField = "caps"

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

// SetSoulCapabilities перезаписывает набор capabilities SID-а в heartbeat-Hash-е
// (поле [heartbeatCapsField]) одним HSET. Вызывается на Hello — ВСЕГДА, включая
// пустой набор (явная перезапись чужого stale-флага при reconnect старого бинаря).
//
// caps нормализуется (уникум, отсортирован, пустые отброшены) и сериализуется
// comma-joined. Пустой набор пишет пустую строку — [SoulHasCapability] трактует её
// как «нет ни одной capability» (fail-closed для фич-зависимых прогонов).
func SetSoulCapabilities(ctx context.Context, c *Client, sid string, caps []string) error {
	if c == nil {
		return errors.New("redis.SetSoulCapabilities: nil client")
	}
	if sid == "" {
		return errors.New("redis.SetSoulCapabilities: empty sid")
	}
	err := c.underlying().HSet(ctx, HeartbeatKey(sid), heartbeatCapsField, joinCaps(caps)).Err()
	if err != nil {
		return fmt.Errorf("redis.SetSoulCapabilities: HSET %q: %w", HeartbeatKey(sid), err)
	}
	return nil
}

// SoulHasCapability сообщает, анонсировал ли активный стрим SID-а данную
// capability (ADR-056 §S5). Читает поле [heartbeatCapsField] одного SID-а (HGET).
//
// Отсутствие ключа/поля или пустой набор → false (fail-closed): старый Soul без
// capability-анонса или хост, не присылавший Hello, трактуется как НЕ
// поддерживающий фичу. Сетевой/протокольный сбой HGET → ошибка (caller — staged-
// гейт run.go — обязан отвергнуть прогон, а не угадать поддержку).
func SoulHasCapability(ctx context.Context, c *Client, sid, capability string) (bool, error) {
	if c == nil {
		return false, errors.New("redis.SoulHasCapability: nil client")
	}
	if sid == "" {
		return false, errors.New("redis.SoulHasCapability: empty sid")
	}
	v, err := c.underlying().HGet(ctx, HeartbeatKey(sid), heartbeatCapsField).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("redis.SoulHasCapability: HGET %q[%s]: %w", HeartbeatKey(sid), heartbeatCapsField, err)
	}
	for _, c := range strings.Split(v, ",") {
		if c == capability {
			return true, nil
		}
	}
	return false, nil
}

// SoulsLackingCapability — batched-проверка для набора SID-ов: возвращает
// подмножество SID-ов, которые НЕ анонсировали данную capability (ADR-056 §S5
// staged-гейт). Один Redis-pipeline HGET per SID (round-trip-ов O(1)).
//
// SID без ключа/поля/с пустым набором попадает в результат (fail-closed). Пустой
// `sids` → пустой результат без обращения к Redis. Ошибка pipeline → возврат
// ошибки целиком (caller отвергает staged-прогон, не угадывает).
func SoulsLackingCapability(ctx context.Context, c *Client, sids []string, capability string) ([]string, error) {
	if c == nil {
		return nil, errors.New("redis.SoulsLackingCapability: nil client")
	}
	if len(sids) == 0 {
		return nil, nil
	}
	pipe := c.underlying().Pipeline()
	type pending struct {
		sid string
		cmd *redis.StringCmd
	}
	cmds := make([]pending, 0, len(sids))
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		cmds = append(cmds, pending{sid: sid, cmd: pipe.HGet(ctx, HeartbeatKey(sid), heartbeatCapsField)})
	}
	if len(cmds) == 0 {
		return nil, nil
	}
	// Pipeline.Exec возвращает ошибку первой неуспешной команды; redis.Nil
	// (отсутствие поля) — НЕ повод проваливать весь пакет, его разбираем per-cmd.
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("redis.SoulsLackingCapability: pipeline EXEC: %w", err)
	}
	var lacking []string
	for _, p := range cmds {
		v, err := p.cmd.Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				lacking = append(lacking, p.sid) // нет ключа/поля → fail-closed.
				continue
			}
			return nil, fmt.Errorf("redis.SoulsLackingCapability: HGET %q: %w", HeartbeatKey(p.sid), err)
		}
		if !capsContain(v, capability) {
			lacking = append(lacking, p.sid)
		}
	}
	return lacking, nil
}

// joinCaps нормализует набор capabilities в стабильную comma-joined строку
// (уникум, отсортирован, пустые отброшены).
func joinCaps(caps []string) string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if c == "" {
			continue
		}
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// capsContain сообщает, есть ли capability в comma-joined-строке.
func capsContain(joined, capability string) bool {
	for _, c := range strings.Split(joined, ",") {
		if c == capability {
			return true
		}
	}
	return false
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
