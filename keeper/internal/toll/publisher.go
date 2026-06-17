// Package toll — cluster-wide detector массового оттока Soul-ов (ADR-038).
//
// Архитектура (ADR-038(2)):
//
//   - [Watcher] — per-Keeper-инстанс goroutine-источник: gRPC EventStream-handler
//     зовёт [Watcher.NotifyDisconnect] при выходе из receive-loop-а. Watcher
//     фильтрует graceful-shutdown / warmup-immunity и публикует выживший
//     disconnect-event в общий Redis sorted-set (через [Publisher]).
//   - [Publisher] — тонкий ZADD-adapter поверх *redis.Client (sorted-set
//     `toll:disconnects`, score = unix-timestamp, value = `<sid>|<kid>|<coven>`).
//     Disconnect-события публикуются СО ВСЕХ инстансов в общий ключ.
//   - [Leader] — фоновая goroutine, выигравшая Redis-lease `cluster:toll:leader`
//     (по аналогии с Reaper). Периодически читает sorted-set за окно, сравнивает
//     с baseline `souls.status='connected'` (cached), выставляет/снимает
//     Redis-ключ `cluster:degraded` с asymmetric hysteresis.
//   - [DegradedReader] — read-only поверхность для HTTP-middleware: проверяет
//     наличие ключа `cluster:degraded` на каждом write-API запросе.
//   - [DegradedMiddleware] — chi-совместимый middleware: блокирует POST
//     scenarios/push-apply при degraded (503 + Retry-After), всё прочее
//     пропускает.
//
// Инвариант ADR-038(c): Toll — passive observer. Не закрывает стримы (это
// работа Watchman), не делает recovery actions, только наблюдает + блокирует
// write-API.
package toll

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// SortedSetKey — Redis sorted-set, в который все per-instance Watcher-ы
// публикуют disconnect-события. score = unix-секунды, value = `<sid>|<kid>|<coven>`.
// Coven опционален (Watcher может не знать его на cleanup-handler-е — пустой
// сегмент допустим). Записи старше окна Leader чистит через ZREMRANGEBYSCORE.
const SortedSetKey = "toll:disconnects"

// LeaseKey — Redis-lease `cluster:toll:leader` для leader-election Leader-loop-а.
// Holder = `kid` keeper-инстанса (read-friendly для логов).
const LeaseKey = "cluster:toll:leader"

// DegradedKey — Redis-ключ-флаг cluster:degraded (set leader-ом, TTL =
// DegradedTTL). Read на каждом write-API запросе через [DegradedReader].
const DegradedKey = "cluster:degraded"

// Publisher — узкая поверхность для Watcher-а: ZADD одного disconnect-event-а
// в общий sorted-set. Daemon оборачивает [keeperredis.PublishTollDisconnect]
// в реализацию; интерфейс позволяет fake в unit-тестах ([Watcher]-tests
// проверяют фильтрацию warmup/graceful без живого Redis-а).
type Publisher interface {
	PublishDisconnect(ctx context.Context, sid, kid, coven string, at time.Time) error
}

// EncodeDisconnect формирует value-строку sorted-set-записи. Включает
// `at.UnixNano` суффиксом для уникальности member-а: ZADD по правилам
// sorted-set обновляет score существующего member-а, и два disconnect-а
// «sid=foo|kid=A|coven=` за одну секунду от одного и того же набора полей
// слились бы в одну запись. UnixNano-суффикс гарантирует уникальность
// без побочных последствий для агрегации (Leader парсит prefix, суффикс
// игнорирует).
func EncodeDisconnect(sid, kid, coven string, at time.Time) string {
	var sb strings.Builder
	sb.Grow(len(sid) + len(kid) + len(coven) + 32)
	sb.WriteString(sid)
	sb.WriteByte('|')
	sb.WriteString(kid)
	sb.WriteByte('|')
	sb.WriteString(coven)
	sb.WriteByte('|')
	sb.WriteString(strconv.FormatInt(at.UnixNano(), 10))
	return sb.String()
}

// DegradedReader — read-only поверхность cluster:degraded флага. Middleware
// зовёт IsDegraded на каждом запросе блокируемого endpoint-а; cost = один
// Redis EXISTS, без round-trip за чтением value. Daemon оборачивает
// [keeperredis.TollIsDegraded] в реализацию; для single-instance/dev без
// Redis — [NoopDegradedReader] (всегда false).
type DegradedReader interface {
	IsDegraded(ctx context.Context) (bool, error)
}

// NoopDegradedReader — fallback для single-instance/dev без Redis. Всегда
// возвращает false: блокировки нет, middleware пропускает все запросы.
type NoopDegradedReader struct{}

// IsDegraded — всегда false.
func (NoopDegradedReader) IsDegraded(context.Context) (bool, error) { return false, nil }
