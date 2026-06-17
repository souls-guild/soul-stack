package redis

// Reliable-queue доставки уведомлений Herald (ADR-052(d), S3). at-least-once
// claim-queue поверх Redis (hot→Redis, ADR-006: статусы попыток НЕ в PG).
//
// Модель (parity reliable-queue Redis: pending-LIST → processing-LIST +
// per-claim lease-ключ):
//
//   - pending (LIST `herald:delivery:pending`) — очередь job-ов; Enqueue =
//     LPUSH JSON-payload-а (неблокирующий).
//   - processing (LIST `herald:delivery:processing`) — claimed-job-ы; Claim =
//     BRPOPLPUSH pending→processing (АТОМАРНЫЙ pop+перенос: при крэше worker-а
//     job не теряется, остаётся в processing), затем SET lease-ключа PX=ttl.
//   - lease (string `herald:delivery:lease:<id>`, PX=leaseTTL) — heartbeat
//     владения job-ом. Жив → job обрабатывается; истёк → job осиротел.
//   - Ack (успех/терминал) = LREM job из processing + DEL lease.
//   - Requeue (retry) = LREM job из processing + LPUSH нового payload-а в
//     pending (caller инкрементит attempt) + DEL lease.
//   - RequeueExpired (mini-reaper) = для каждого job в processing без живого
//     lease-ключа — перенос обратно в pending (осиротевшие после крэша).
//
// Конкурентные Claim с разных Keeper-инстансов безопасны (BRPOPLPUSH атомарен —
// один job достаётся одному worker-у). at-least-once: дубль возможен, если
// worker доставил, но не успел Ack до крэша — приемлемо (решение пользователя,
// ADR-052(d)).
//
// Backend оперирует OPAQUE payload-ом (string job_id + []byte JSON): redis-
// пакет НЕ импортирует herald (избегаем цикла), сериализация DeliveryJob —
// ответственность herald-пакета.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Convention-ключи очереди доставки (фиксированы, стиль `<подсистема>:<очередь>`
// как `apply:summons` / `tempo:<aid>`). Не per-Herald: единая очередь на все
// каналы, разбор адресата — внутри payload-а job-а (herald-пакет).
const (
	heraldPendingKey    = "herald:delivery:pending"
	heraldProcessingKey = "herald:delivery:processing"
	heraldLeasePrefix   = "herald:delivery:lease:"
)

func heraldLeaseKey(jobID string) string { return heraldLeasePrefix + jobID }

// HeraldDeliveryQueue — handle reliable-queue доставки поверх Redis-клиента.
// Stateless относительно job-а: все операции принимают payload/id явно.
type HeraldDeliveryQueue struct {
	client *Client
}

// NewHeraldDeliveryQueue оборачивает Redis-клиент в очередь доставки. nil-клиент
// — программная ошибка wire-up-а (daemon при отсутствии Redis вовсе не поднимает
// доставку, fail-open, см. setupHeraldDelivery).
func NewHeraldDeliveryQueue(c *Client) (*HeraldDeliveryQueue, error) {
	if c == nil {
		return nil, errors.New("redis.NewHeraldDeliveryQueue: nil client")
	}
	return &HeraldDeliveryQueue{client: c}, nil
}

// Enqueue кладёт сериализованный job в pending (LPUSH — неблокирующий). Вызов из
// dispatcher-а (tap-путь): должен быть быстрым, чтобы Dispatch не залипал на
// сетевом I/O и не подвешивал tap-consumer/Close (ctx caller-а несёт deadline).
func (q *HeraldDeliveryQueue) Enqueue(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("redis.HeraldDeliveryQueue.Enqueue: empty payload")
	}
	if err := q.client.underlying().LPush(ctx, heraldPendingKey, payload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Enqueue: LPUSH %q: %w", heraldPendingKey, err)
	}
	return nil
}

// ClaimedJob — результат успешного [HeraldDeliveryQueue.Claim].
type ClaimedJob struct {
	// Payload — сериализованный job (тот же байт-в-байт, что лёг в processing;
	// Ack/Requeue требуют точное значение для LREM).
	Payload []byte
	// JobID — id job-а (caller извлекает из payload-а и передаёт сюда для
	// lease-ключа); см. SetLease.
	JobID string
}

// Claim блокирующе ждёт job в pending до timeout-а и атомарно переносит его в
// processing (BRPOPLPUSH). Возвращает (nil, nil) на пустую очередь по истечении
// timeout-а — caller повторит claim-цикл. lease-ключ ставит [SetLease] (отдельно:
// job_id извлекается из payload-а уже в herald-пакете).
//
// blockTimeout=0 → бесконечная блокировка (нежелательно — не реагирует на
// ctx.Done без сетевого пинга); caller передаёт конечный (poll-interval).
func (q *HeraldDeliveryQueue) Claim(ctx context.Context, blockTimeout time.Duration) (*ClaimedJob, error) {
	res, err := q.client.underlying().BRPopLPush(ctx, heraldPendingKey, heraldProcessingKey, blockTimeout).Result()
	if errors.Is(err, redis.Nil) {
		// Таймаут — pending пуст. Не ошибка.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis.HeraldDeliveryQueue.Claim: BRPOPLPUSH: %w", err)
	}
	return &ClaimedJob{Payload: []byte(res)}, nil
}

// SetLease ставит/продлевает lease-ключ job-а (PX=ttl). Worker зовёт его сразу
// после Claim и продлевает периодически, пока доставляет. Истечение ключа —
// сигнал mini-reaper-у ([RequeueExpired]), что job осиротел.
func (q *HeraldDeliveryQueue) SetLease(ctx context.Context, jobID string, ttl time.Duration) error {
	if jobID == "" {
		return errors.New("redis.HeraldDeliveryQueue.SetLease: empty jobID")
	}
	if ttl <= 0 {
		return fmt.Errorf("redis.HeraldDeliveryQueue.SetLease: ttl must be > 0, got %v", ttl)
	}
	if err := q.client.underlying().Set(ctx, heraldLeaseKey(jobID), "1", ttl).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.SetLease: SET %q: %w", heraldLeaseKey(jobID), err)
	}
	return nil
}

// Ack снимает job из processing после терминала (delivered/failed) и удаляет
// lease-ключ. payload — точное значение из [ClaimedJob.Payload] (LREM требует
// побайтового совпадения). LREM count=1 — снять одну копию (дублей быть не
// должно: id уникален).
func (q *HeraldDeliveryQueue) Ack(ctx context.Context, jobID string, payload []byte) error {
	if err := q.client.underlying().LRem(ctx, heraldProcessingKey, 1, payload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Ack: LREM processing: %w", err)
	}
	// lease чистим best-effort: истечёт сам по TTL, но явный DEL освобождает
	// память сразу. Ошибку DEL не пробрасываем — Ack главного (LREM) уже прошёл.
	_ = q.client.underlying().Del(ctx, heraldLeaseKey(jobID)).Err()
	return nil
}

// Requeue возвращает job на повтор: снимает старый payload из processing и
// кладёт newPayload (caller инкрементил attempt) обратно в pending. lease старого
// job-а удаляется. Атомарность LREM+LPUSH здесь не критична: при крэше между
// ними старый payload остаётся в processing и его подберёт [RequeueExpired] по
// истёкшему lease — at-least-once сохраняется.
func (q *HeraldDeliveryQueue) Requeue(ctx context.Context, jobID string, oldPayload, newPayload []byte) error {
	if len(newPayload) == 0 {
		return errors.New("redis.HeraldDeliveryQueue.Requeue: empty newPayload")
	}
	if err := q.client.underlying().LPush(ctx, heraldPendingKey, newPayload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Requeue: LPUSH pending: %w", err)
	}
	if err := q.client.underlying().LRem(ctx, heraldProcessingKey, 1, oldPayload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Requeue: LREM processing: %w", err)
	}
	_ = q.client.underlying().Del(ctx, heraldLeaseKey(jobID)).Err()
	return nil
}

// expiredRequeueFn — callback извлечения jobID из payload-а для [RequeueExpired].
// redis-пакет не знает форму job-а; herald-пакет передаёт парсер. Возврат
// ok=false → payload битый, mini-reaper его дропает (LREM без перепостановки).
type expiredRequeueFn func(payload []byte) (jobID string, ok bool)

// RequeueExpired — mini-reaper осиротевших job-ов: сканирует processing и для
// каждого job без живого lease-ключа переносит его обратно в pending (worker,
// клеймивший job, умер, не успев Ack/Requeue). Возвращает число возвращённых.
//
// Сканирование через LRange (снимок processing) + точечная проверка lease по
// каждому job-у: processing обычно короткий (in-flight доставки), полный скан
// дёшев. Перепостановка — Requeue с тем же payload-ом (attempt НЕ меняем — это
// не новая попытка по решению worker-а, а возврат после краша).
func (q *HeraldDeliveryQueue) RequeueExpired(ctx context.Context, parse expiredRequeueFn) (int, error) {
	items, err := q.client.underlying().LRange(ctx, heraldProcessingKey, 0, -1).Result()
	if err != nil {
		return 0, fmt.Errorf("redis.HeraldDeliveryQueue.RequeueExpired: LRANGE processing: %w", err)
	}
	requeued := 0
	for _, raw := range items {
		payload := []byte(raw)
		jobID, ok := parse(payload)
		if !ok {
			// Битый payload в processing — не зацикливаем mini-reaper на нём:
			// снимаем без перепостановки.
			_ = q.client.underlying().LRem(ctx, heraldProcessingKey, 1, payload).Err()
			continue
		}
		exists, err := q.client.underlying().Exists(ctx, heraldLeaseKey(jobID)).Result()
		if err != nil {
			return requeued, fmt.Errorf("redis.HeraldDeliveryQueue.RequeueExpired: EXISTS lease: %w", err)
		}
		if exists == 1 {
			// lease жив — job обрабатывается, не трогаем.
			continue
		}
		// Осиротел: lease истёк, владелец не Ack-нул. Возвращаем тот же payload.
		if err := q.client.underlying().LPush(ctx, heraldPendingKey, payload).Err(); err != nil {
			return requeued, fmt.Errorf("redis.HeraldDeliveryQueue.RequeueExpired: LPUSH pending: %w", err)
		}
		if err := q.client.underlying().LRem(ctx, heraldProcessingKey, 1, payload).Err(); err != nil {
			return requeued, fmt.Errorf("redis.HeraldDeliveryQueue.RequeueExpired: LREM processing: %w", err)
		}
		requeued++
	}
	return requeued, nil
}
