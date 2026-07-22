package herald

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// DeliveryJob is a task to deliver one notification: match of run event
// against one enabled Tiding rule (ADR-052(c)). Carries reference to
// Herald channel, event and COPY of payload. Delivery (webhook call, retry,
// claim-queue) is executed by [DeliveryWorker].
//
// Payload hygiene: PayloadCopy is copy of already-masked audit-payload
// (audit-writer ran MaskSecrets before write, ADR-022). Dispatcher does NOT
// enrich payload with anything — resolved secrets/input are not added
// (invariant A, ADR-027/ADR-052(e)). Copy protects from mutation of shared
// payload pointer by downstream delivery code.
//
// JSON-serializable: job is placed in Redis queue as JSON (claim-queue,
// ADR-052(d)). ID/Attempt are queue service fields: ID is unique (ULID, key
// for lease and LREM identity), Attempt is current attempt number (0-based;
// increments on retry, on retryMax exhaustion → terminal fail).
type DeliveryJob struct {
	// ID is job's unique identifier (ULID). Key for lease in queue and
	// identity for LREM/requeue. Filled by dispatcher on enqueue.
	ID string `json:"id"`
	// Attempt is current delivery attempt number (0-based). 0 on first enqueue;
	// worker increments on requeue. Reached retryMax → terminal fail.
	Attempt int `json:"attempt"`
	// Herald is name of Herald channel (PK heralds.name) to send to.
	Herald string `json:"herald"`
	// Tiding is name of matched Tiding rule (for audit/correlation).
	Tiding string `json:"tiding"`
	// EventType is type of matched run event.
	EventType audit.EventType `json:"event_type"`
	// CorrelationID is voyage_id / apply_id of event (for audit chain).
	CorrelationID string `json:"correlation_id"`
	// OccurredAt is event timestamp (Event.CreatedAt; zero → match time).
	OccurredAt time.Time `json:"occurred_at"`
	// PayloadCopy is copy of masked event payload.
	PayloadCopy map[string]any `json:"payload"`
	// Annotations are static operator fields from Tiding (ADR-052(h)). Transferred
	// by dispatcher; merge to webhook body (new top-level key `annotations`)
	// done by worker off-path when building webhookPayload ([buildPayload], N3).
	Annotations map[string]any `json:"annotations,omitempty"`
	// Projection is allow-list of payload paths from Tiding (ADR-052(h)). Transferred
	// by dispatcher; narrowing payload copy done by worker off-path (empty = full
	// form) when building webhookPayload ([projectPayload], N3).
	Projection []string `json:"projection,omitempty"`
}

// marshalJob serializes job for queue. Marshal error is programmer error
// (payload already masked and consists of JSON-compatible types after audit
// normalization); caller (dispatcher/worker) logs and drops job.
func marshalJob(job *DeliveryJob) ([]byte, error) {
	b, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("herald: marshal delivery job: %w", err)
	}
	return b, nil
}

// unmarshalJob recovers job from queue. Corrupt JSON in queue is anomaly
// (only marshalJob could put it there); worker drops such job (mini-reaper too).
func unmarshalJob(payload []byte) (*DeliveryJob, error) {
	var job DeliveryJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return nil, fmt.Errorf("herald: unmarshal delivery job: %w", err)
	}
	return &job, nil
}

// jobIDFromPayload extracts only job ID from serialized payload —
// for mini-reaper ([redis.HeraldDeliveryQueue.RequeueExpired]), which needs
// id to check lease key without full job parse. ok=false → corrupt
// payload (mini-reaper will drop).
func jobIDFromPayload(payload []byte) (string, bool) {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || probe.ID == "" {
		return "", false
	}
	return probe.ID, true
}

// webhookPayload is JSON body of webhook POST (ADR-052(d), format fixed in
// [buildPayload]). Separate typed struct (not map) for stable key set/order
// and explicit receiver contract.
//
// Annotations is optional additive key (ADR-052(h)/(i)): object of static
// operator fields from Tiding; omitempty omits key with empty annotations, not
// breaking receivers reading event_type/payload/…
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

// DeliveryQueue is delivery task enqueue point from dispatcher
// (ADR-052(c)). Implementations: [RedisDeliveryQueue] (S3, claim-queue) and
// [LogDeliveryQueue] (fallback if Redis unavailable — delivery degrades).
type DeliveryQueue interface {
	// Enqueue puts task in delivery queue. Best-effort on dispatcher side: error
	// is logged but doesn't affect processing of other matches for same event
	// (tap already detached from audit write-path).
	Enqueue(ctx context.Context, job *DeliveryJob) error
}

// QueueBackend is narrow surface of reliable delivery queue, needed by herald
// (implementation is adapter in daemon over [redis.HeraldDeliveryQueue]). Narrowing
// (instead of *redis.Client) isolates herald from go-redis and provides fake/miniredis
// backed in tests. Backend operates on opaque payload (herald (de)serializes
// DeliveryJob itself). Exported so wiring adapter in daemon can implement it.
type QueueBackend interface {
	// Enqueue puts serialized job in pending (non-blocking LPUSH).
	Enqueue(ctx context.Context, payload []byte) error
	// Claim blocks waiting for job up to blockTimeout, atomically moving it to
	// processing. (nil, nil) → queue empty (timeout).
	Claim(ctx context.Context, blockTimeout time.Duration) (*ClaimedJob, error)
	// SetLease sets/extends lease key for claimed job (PX=ttl).
	SetLease(ctx context.Context, jobID string, ttl time.Duration) error
	// Ack removes job from processing after terminal + deletes lease.
	Ack(ctx context.Context, jobID string, payload []byte) error
	// Requeue returns job for retry (newPayload to pending, oldPayload from
	// processing, lease deleted).
	Requeue(ctx context.Context, jobID string, oldPayload, newPayload []byte) error
	// RequeueExpired is mini-reaper: returns orphaned (lease expired) jobs
	// from processing to pending. parse extracts jobID from payload.
	RequeueExpired(ctx context.Context, parse func(payload []byte) (string, bool)) (int, error)
}

// queueBackend is internal alias for brevity in worker/reaper code.
type queueBackend = QueueBackend

// ClaimedJob is result of [QueueBackend.Claim]: opaque payload of claimed job.
// Mirrors redis.ClaimedJob (narrow contract without importing redis package in
// herald API signatures; adapter in daemon converts).
type ClaimedJob struct {
	// Payload is serialized job (exact value for Ack/Requeue LREM).
	Payload []byte
	// JobID is job id (optional, backend may not fill — worker extracts from
	// payload itself).
	JobID string
}

// RedisDeliveryQueue is claim-queue implementation of [DeliveryQueue] over Redis
// (ADR-052(d), hot→Redis). Enqueue serializes job and puts in pending LIST via
// backend; claim/retry/ack done by [DeliveryWorker].
type RedisDeliveryQueue struct {
	backend queueBackend
	logger  *slog.Logger
}

// NewRedisDeliveryQueue constructs queue over backend. Logger is optional.
func NewRedisDeliveryQueue(backend queueBackend, logger *slog.Logger) *RedisDeliveryQueue {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &RedisDeliveryQueue{backend: backend, logger: logger}
}

// Enqueue serializes job (assigning ID if empty) and puts in Redis queue.
// Non-blocking in essence (LPUSH), but ctx of caller (tap-consume) carries deadline —
// if Redis stalls, Enqueue returns deadline error, not hanging Close.
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

// LogDeliveryQueue is fallback implementation of [DeliveryQueue]: logs match fact,
// doesn't perform delivery. Used if Redis unavailable (fail-open: keeper doesn't crash,
// delivery degrades — ADR-052(d) wiring) and in dispatcher unit-tests.
type LogDeliveryQueue struct {
	Logger *slog.Logger
}

// Enqueue logs task (info level: notification match is observable
// event, but not delivery). nil-logger → silent no-op.
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
