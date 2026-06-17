package redis

// SoulLease — координация «какой Keeper-инстанс держит активный EventStream
// к данному Soul» через Redis-lease (ADR-006(b) → [storage.md → Lease на SID]).
//
// Ключ — `soul:<sid>:lock`, значение — `kid` Keeper-а, TTL продлевается
// renewal-goroutine-ой (паттерн идентичен Reaper-у).
//
// Семантика лидерства одного Soul-стрима:
//   - Один Keeper в момент времени держит lease → принимает EventStream.
//   - Конкурирующий Keeper при попытке принять стрим того же SID получает
//     [ErrLeaseTaken]; handler закрывает стрим с `code.AlreadyExists`.
//   - На graceful shutdown Keeper делает Release; следующий Keeper свободно
//     занимает на ближайшем reconnect-е Soul-а.
//   - На crash Keeper-а: TTL истекает, следующий reconnect Soul-а захватывает
//     lease у нового Keeper-а.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// SoulStreamAlive сообщает, держит ли какой-либо Keeper-инстанс живой
// EventStream к данному SID — по наличию SID-lease-ключа `soul:<sid>:lock`.
//
// Lease (а НЕ heartbeat-Hash) — авторитетный признак живого стрима: ключ
// существует только пока renewal-goroutine handler-а его продлевает (TTL
// [defaultSoulLeaseTTL]), и пропадает при штатном Release либо по истечении
// TTL после crash-а инстанса. Heartbeat-Hash (`soul:<sid>:hb`) для этой
// проверки не годится — он без TTL (см. heartbeat.go) и пережил бы давно
// закрытый стрим, давая ложно-живой ответ.
//
// Назначение — lease-aware Reaper-правило `mark_disconnected`: idle-Soul на
// живом стриме (шлёт лишь soulprint раз в refresh_interval, PG `last_seen_at`
// stale) не должен ложно метиться `disconnected`, пока его стрим держит
// lease (ADR-006(a)).
func SoulStreamAlive(ctx context.Context, c *Client, sid string) (bool, error) {
	if c == nil {
		return false, errors.New("redis.SoulStreamAlive: nil client")
	}
	if sid == "" {
		return false, errors.New("redis.SoulStreamAlive: empty sid")
	}
	n, err := c.underlying().Exists(ctx, SoulLeaseKey(sid)).Result()
	if err != nil {
		return false, fmt.Errorf("redis.SoulStreamAlive: EXISTS %q: %w", SoulLeaseKey(sid), err)
	}
	return n > 0, nil
}

// SoulsStreamAlive — batched-вариант [SoulStreamAlive] для набора SID-ов
// (presence-фильтр таргет-резолвера, ADR-006(a)). Возвращает множество SID-ов
// с живым lease-ключом `soul:<sid>:lock` (EXISTS).
//
// Реализация — один Redis-pipeline на EXISTS-команду per SID: round-trip-ов
// O(1) вместо O(N) последовательных EXISTS. Резолвер таргетит хосты ОДНОГО
// incarnation (десятки–сотни), pipeline дёшев; отдельный Redis-Set живых
// SID-ов (Variant B) не вводим — лишний источник истины рядом с lease-ключом.
//
// Пустой `sids` → пустой результат без обращения к Redis. nil-элементы /
// пустые строки в `sids` пропускаются (не формируют валидный lease-ключ).
// Ошибка pipeline-а → возврат ошибки целиком (caller — резолвер — деградирует
// fail-safe на SQL-presence, см. topology.Resolver).
func SoulsStreamAlive(ctx context.Context, c *Client, sids []string) (map[string]struct{}, error) {
	if c == nil {
		return nil, errors.New("redis.SoulsStreamAlive: nil client")
	}
	alive := make(map[string]struct{}, len(sids))
	if len(sids) == 0 {
		return alive, nil
	}

	pipe := c.underlying().Pipeline()
	// Параллельный slice (sid ↔ его *IntCmd): после Exec читаем результат
	// каждой команды по индексу. Пустые SID пропускаем — для них команда не
	// ставится, индекс не резервируется.
	type pending struct {
		sid string
		cmd *redis.IntCmd
	}
	cmds := make([]pending, 0, len(sids))
	for _, sid := range sids {
		if sid == "" {
			continue
		}
		cmds = append(cmds, pending{sid: sid, cmd: pipe.Exists(ctx, SoulLeaseKey(sid))})
	}
	if len(cmds) == 0 {
		return alive, nil
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis.SoulsStreamAlive: pipeline EXEC: %w", err)
	}
	for _, p := range cmds {
		n, err := p.cmd.Result()
		if err != nil {
			return nil, fmt.Errorf("redis.SoulsStreamAlive: EXISTS %q: %w", SoulLeaseKey(p.sid), err)
		}
		if n > 0 {
			alive[p.sid] = struct{}{}
		}
	}
	return alive, nil
}

// SoulLeaseOwner возвращает KID Keeper-инстанса, держащего активный
// EventStream к данному SID — значение lease-ключа `soul:<sid>:lock` (GET).
//
// В отличие от [SoulStreamAlive] (EXISTS — есть ли вообще владелец), здесь нужен
// именно ВЛАДЕЛЕЦ: multi-keeper-guard run-goroutine-пути (acolytes=0) сверяет
// его с собственным KID. Если SendApply пойдёт Soul-у, чей стрим держит ДРУГОЙ
// Keeper-инстанс, RunResult уйдёт туда, а владелец-прогон здесь зависнет в
// applying (footgun, поправимый только переходом на acolytes>0 / work-queue
// ADR-027).
//
// Возврат: ok=false без ошибки — lease-ключа нет (Soul не на стриме ни у кого:
// EXISTS вернул бы false, redis.Nil здесь не ошибка). ok=true + kid — владелец
// известен. Ошибка — только сетевой/протокольный сбой GET (caller — guard —
// деградирует молча: при ошибке проверки warn не печатается).
func SoulLeaseOwner(ctx context.Context, c *Client, sid string) (kid string, ok bool, err error) {
	if c == nil {
		return "", false, errors.New("redis.SoulLeaseOwner: nil client")
	}
	if sid == "" {
		return "", false, errors.New("redis.SoulLeaseOwner: empty sid")
	}
	v, gerr := c.underlying().Get(ctx, SoulLeaseKey(sid)).Result()
	if gerr != nil {
		if errors.Is(gerr, redis.Nil) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("redis.SoulLeaseOwner: GET %q: %w", SoulLeaseKey(sid), gerr)
	}
	return v, true, nil
}

// SoulLeaseKey формирует Redis-ключ lease-а для конкретного SID.
//
// Convention `soul:<sid>:lock` зафиксирована в docs/keeper/storage.md
// (Redis — горячий слой, роль (b) Lease на SID).
func SoulLeaseKey(sid string) string {
	return "soul:" + sid + ":lock"
}

// AcquireSoulLease — обёртка [Acquire] с фиксированным префиксом ключа.
// Параметры symmetric Reaper-овскому use-case-у:
//
//   - sid — SID Soul-а (FQDN), формирует ключ.
//   - kid — идентификатор Keeper-инстанса, пишется как value.
//   - ttl — продолжительность жизни ключа без Renew. Renewal-goroutine
//     обязана продлевать чаще чем ttl (типично ttl/3).
//
// На конфликт возвращает [ErrLeaseTaken]; caller (EventStream-handler)
// закрывает стрим с `code.AlreadyExists`.
func AcquireSoulLease(ctx context.Context, c *Client, sid, kid string, ttl time.Duration) (*Lease, error) {
	return Acquire(ctx, c, SoulLeaseKey(sid), kid, ttl)
}
