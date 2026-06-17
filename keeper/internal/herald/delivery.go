package herald

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DeliveryJob — задание на доставку одного уведомления: матч события прогона
// против одного включённого Tiding-правила (ADR-052(c)). Несёт ссылку на
// Herald-канал, событие и КОПИЮ payload-а. Доставку (webhook-вызов, retry,
// claim-queue) выполняет [DeliveryWorker].
//
// Payload-гигиена: PayloadCopy — копия уже-замаскированного audit-payload-а
// (audit-writer прогнал MaskSecrets перед записью, ADR-022). Dispatcher НЕ
// обогащает payload ничем — resolved-секреты/input в него не добавляются
// (инвариант A, ADR-027/ADR-052(e)). Копия защищает от мутации общего
// payload-указателя downstream-кодом доставки.
//
// JSON-сериализуем: job кладётся в Redis-очередь как JSON (claim-queue,
// ADR-052(d)). ID/Attempt — служебные поля очереди: ID уникален (ULID, ключ
// lease-а и LREM-идентичности), Attempt — номер текущей попытки (0-based;
// инкрементится при retry, на исчерпании retryMax → терминальный fail).
type DeliveryJob struct {
	// ID — уникальный идентификатор job-а (ULID). Ключ lease-а в очереди и
	// идентичность для LREM/requeue. Заполняется dispatcher-ом при постановке.
	ID string `json:"id"`
	// Attempt — номер текущей попытки доставки (0-based). 0 на первой постановке;
	// worker инкрементит при requeue. Достиг retryMax → терминальный fail.
	Attempt int `json:"attempt"`
	// Herald — имя Herald-канала (PK heralds.name), куда слать.
	Herald string `json:"herald"`
	// Tiding — имя сработавшего Tiding-правила (для аудита/корреляции).
	Tiding string `json:"tiding"`
	// EventType — тип сматченного события прогона.
	EventType audit.EventType `json:"event_type"`
	// CorrelationID — voyage_id / apply_id события (для цепочки аудита).
	CorrelationID string `json:"correlation_id"`
	// OccurredAt — момент события (Event.CreatedAt; zero → время матча).
	OccurredAt time.Time `json:"occurred_at"`
	// PayloadCopy — копия замаскированного payload-а события.
	PayloadCopy map[string]any `json:"payload"`
	// Annotations — статические поля оператора из Tiding (ADR-052(h)). Перенесены
	// dispatcher-ом; merge в тело webhook (новый верхнеуровневый ключ `annotations`)
	// делает worker off-path при сборке webhookPayload ([buildPayload], N3).
	Annotations map[string]any `json:"annotations,omitempty"`
	// Projection — allow-list путей payload из Tiding (ADR-052(h)). Перенесён
	// dispatcher-ом; сужение payload-копии делает worker off-path (пусто = полная
	// форма) при сборке webhookPayload ([projectPayload], N3).
	Projection []string `json:"projection,omitempty"`
}

// marshalJob сериализует job для очереди. Ошибка маршалинга — программная
// (payload уже замаскирован и состоит из JSON-совместимых типов после audit-
// нормализации); caller (dispatcher/worker) логирует и дропает job.
func marshalJob(job *DeliveryJob) ([]byte, error) {
	b, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("herald: marshal delivery job: %w", err)
	}
	return b, nil
}

// unmarshalJob восстанавливает job из очереди. Битый JSON в очереди — аномалия
// (положить мог только marshalJob); worker дропает такой job (mini-reaper тоже).
func unmarshalJob(payload []byte) (*DeliveryJob, error) {
	var job DeliveryJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil, fmt.Errorf("herald: unmarshal delivery job: %w", err)
	}
	return &job, nil
}

// jobIDFromPayload извлекает только ID job-а из сериализованного payload-а —
// для mini-reaper-а ([redis.HeraldDeliveryQueue.RequeueExpired]), которому нужен
// id для проверки lease-ключа без полного разбора job-а. ok=false → битый
// payload (mini-reaper дропнет).
func jobIDFromPayload(payload []byte) (string, bool) {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.ID == "" {
		return "", false
	}
	return probe.ID, true
}

// webhookPayload — JSON-тело webhook-POST-а (ADR-052(d), формат зафиксирован в
// [buildPayload]). Отдельный typed-struct (не map) — стабильный набор/порядок
// ключей и явный контракт приёмника.
//
// Annotations — опциональный additive-ключ (ADR-052(h)/(i)): объект статических
// полей оператора из Tiding; omitempty опускает ключ при пустых annotations, не
// ломая приёмники, читающие event_type/payload/…
type webhookPayload struct {
	EventType   string         `json:"event_type"`
	OccurredAt  string         `json:"occurred_at"`
	Herald      string         `json:"herald"`
	Tiding      string         `json:"tiding"`
	Payload     map[string]any `json:"payload"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

func marshalWebhookPayload(p webhookPayload) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("herald: marshal webhook payload: %w", err)
	}
	return b, nil
}

// DeliveryQueue — точка постановки задания на доставку из dispatcher-а
// (ADR-052(c)). Реализации: [RedisDeliveryQueue] (S3, claim-queue) и
// [LogDeliveryQueue] (fallback при отсутствии Redis — доставка деградирует).
type DeliveryQueue interface {
	// Enqueue ставит задание в очередь доставки. Best-effort на стороне
	// dispatcher-а: ошибка логируется, но не влияет на обработку других
	// матчей того же события (tap уже отвязан от audit write-path).
	Enqueue(ctx context.Context, job *DeliveryJob) error
}

// QueueBackend — узкая поверхность reliable-очереди доставки, нужная herald-у
// (реализация — адаптер в daemon поверх [redis.HeraldDeliveryQueue]). Сужение
// (вместо *redis.Client) изолирует herald от go-redis и даёт fake/miniredis-
// backed в тестах. Backend оперирует opaque-payload-ом (herald (де)сериализует
// DeliveryJob сам). Экспортирован, чтобы wiring-адаптер в daemon его реализовал.
type QueueBackend interface {
	// Enqueue кладёт сериализованный job в pending (неблокирующий LPUSH).
	Enqueue(ctx context.Context, payload []byte) error
	// Claim блокирующе ждёт job до blockTimeout, атомарно перенося его в
	// processing. (nil, nil) → очередь пуста (таймаут).
	Claim(ctx context.Context, blockTimeout time.Duration) (*ClaimedJob, error)
	// SetLease ставит/продлевает lease-ключ claimed-job-а (PX=ttl).
	SetLease(ctx context.Context, jobID string, ttl time.Duration) error
	// Ack снимает job из processing после терминала + удаляет lease.
	Ack(ctx context.Context, jobID string, payload []byte) error
	// Requeue возвращает job на повтор (newPayload в pending, oldPayload из
	// processing, lease удаляется).
	Requeue(ctx context.Context, jobID string, oldPayload, newPayload []byte) error
	// RequeueExpired — mini-reaper: возвращает осиротевшие (lease истёк) job-ы
	// из processing в pending. parse извлекает jobID из payload-а.
	RequeueExpired(ctx context.Context, parse func(payload []byte) (string, bool)) (int, error)
}

// queueBackend — внутренний alias для краткости в worker/reaper-коде.
type queueBackend = QueueBackend

// ClaimedJob — результат [QueueBackend.Claim]: opaque-payload claimed-job-а.
// Зеркалит redis.ClaimedJob (узкий контракт без импорта redis-пакета в
// сигнатурах herald-API; адаптер в daemon конвертирует).
type ClaimedJob struct {
	// Payload — сериализованный job (точное значение для Ack/Requeue-LREM).
	Payload []byte
	// JobID — id job-а (опц., backend может не заполнять — worker извлекает из
	// payload-а сам).
	JobID string
}

// RedisDeliveryQueue — claim-queue-реализация [DeliveryQueue] поверх Redis
// (ADR-052(d), hot→Redis). Enqueue сериализует job и кладёт в pending-LIST через
// backend; claim/retry/ack делает [DeliveryWorker].
type RedisDeliveryQueue struct {
	backend queueBackend
	logger  *slog.Logger
}

// NewRedisDeliveryQueue собирает очередь поверх backend-а. Logger опционален.
func NewRedisDeliveryQueue(backend queueBackend, logger *slog.Logger) *RedisDeliveryQueue {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RedisDeliveryQueue{backend: backend, logger: logger}
}

// Enqueue сериализует job (присвоив ID, если пуст) и кладёт в Redis-очередь.
// Неблокирующий по сути (LPUSH), но ctx caller-а (tap-consume) несёт deadline —
// при затыке Redis Enqueue вернёт ошибку по deadline, а не подвесит Close.
func (q *RedisDeliveryQueue) Enqueue(ctx context.Context, job *DeliveryJob) error {
	if job.ID == "" {
		job.ID = audit.NewULID()
	}
	payload, err := marshalJob(job)
	if err != nil {
		return err
	}
	return q.backend.Enqueue(ctx, payload)
}

// LogDeliveryQueue — fallback-реализация [DeliveryQueue]: логирует факт матча,
// доставку не выполняет. Используется при отсутствии Redis (fail-open: keeper не
// падает, доставка деградирует — ADR-052(d) wiring) и в unit-тестах dispatcher-а.
type LogDeliveryQueue struct {
	Logger *slog.Logger
}

// Enqueue логирует задание (info-уровень: матч уведомления — наблюдаемое
// событие, но не доставка). nil-logger → молчаливый no-op.
func (q *LogDeliveryQueue) Enqueue(_ context.Context, job *DeliveryJob) error {
	if q == nil || q.Logger == nil {
		return nil
	}
	q.Logger.Info("herald: notification matched (delivery degraded — Redis queue unavailable)",
		slog.String("herald", job.Herald),
		slog.String("tiding", job.Tiding),
		slog.String("event_type", string(job.EventType)),
		slog.String("correlation_id", job.CorrelationID))
	return nil
}
