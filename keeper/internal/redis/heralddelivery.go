package redis

// Reliable delivery queue for Herald notifications (ADR-052(d), S3). An
// at-least-once claim queue on top of Redis (hot→Redis, ADR-006: attempt
// status is NOT stored in PG).
//
// Model (Redis reliable-queue parity: pending LIST → processing LIST +
// per-claim lease key):
//
//   - pending (LIST `herald:delivery:{q}:pending`) — job queue; Enqueue =
//     LPUSH of the JSON payload (non-blocking).
//   - processing (LIST `herald:delivery:{q}:processing`) — claimed jobs; Claim =
//     BRPOPLPUSH pending→processing (an ATOMIC pop+move: if the worker crashes
//     the job isn't lost, it stays in processing), followed by SET of the
//     lease key with PX=ttl.
//   - lease (string `herald:delivery:{q}:lease:<id>`, PX=leaseTTL) — heartbeat
//     of job ownership. Alive → job is being processed; expired → job is
//     orphaned.
//
// The `{q}` hash tag on all three keys puts the whole queue keyspace in one
// Redis Cluster slot — required so the multi-key BRPOPLPUSH pending→processing
// isn't rejected with CROSSSLOT (details at the const block below).
//   - Ack (success/terminal) = LREM job from processing + DEL lease.
//   - Requeue (retry) = LREM job from processing + LPUSH the new payload into
//     pending (caller increments attempt) + DEL lease.
//   - RequeueExpired (mini-reaper) = for every job in processing without a
//     live lease key — move it back to pending (orphaned after a crash).
//
// Concurrent Claim calls from different Keeper instances are safe (BRPOPLPUSH
// is atomic — one job goes to exactly one worker). at-least-once: a duplicate
// is possible if a worker delivered but crashed before Ack — acceptable (user
// decision, ADR-052(d)).
//
// The backend operates on an OPAQUE payload (string job_id + []byte JSON): the
// redis package does NOT import herald (to avoid a cycle) — serializing
// DeliveryJob is the herald package's responsibility.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Convention keys for the delivery queue (fixed, `<subsystem>:<queue>` style
// like `apply:summons` / `tempo:<aid>`). Not per-Herald: a single queue for all
// channels, recipient resolution happens inside the job payload (herald
// package).
//
// Hash tag `{q}` (ADR-006 amendment, cluster mode): `Claim` does an atomic
// BRPOPLPUSH pending→processing — both KEYS must live in the same slot,
// otherwise Redis Cluster rejects the multi-key command with CROSSSLOT. The
// `{q}` hash tag is present in ALL queue keys (pending/processing/lease), so
// the entire delivery keyspace lands in one slot (i.e. CLUSTER KEYSLOT
// matches). In standalone/sentinel the hash tag is just part of the key name
// and doesn't change behavior.
const (
	heraldPendingKey    = "herald:delivery:{q}:pending"
	heraldProcessingKey = "herald:delivery:{q}:processing"
	heraldLeasePrefix   = "herald:delivery:{q}:lease:"
)

func heraldLeaseKey(jobID string) string { return heraldLeasePrefix + jobID }

// HeraldDeliveryQueue is a handle to the delivery reliable queue on top of a
// Redis client. Stateless with respect to a job: all operations take the
// payload/id explicitly.
type HeraldDeliveryQueue struct {
	client *Client
}

// NewHeraldDeliveryQueue wraps a Redis client into a delivery queue. A nil
// client is a wiring bug (the daemon doesn't stand up delivery at all when
// Redis is absent, fail-open — see setupHeraldDelivery).
func NewHeraldDeliveryQueue(c *Client) (*HeraldDeliveryQueue, error) {
	if c == nil {
		return nil, errors.New("redis.NewHeraldDeliveryQueue: nil client")
	}
	return &HeraldDeliveryQueue{client: c}, nil
}

// Enqueue pushes a serialized job into pending (LPUSH — non-blocking). Called
// from the dispatcher (tap path): must be fast so Dispatch doesn't stall on
// network I/O and block the tap-consumer/Close (the caller's ctx carries the
// deadline).
func (q *HeraldDeliveryQueue) Enqueue(ctx context.Context, payload []byte) error {
	if len(payload) == 0 {
		return errors.New("redis.HeraldDeliveryQueue.Enqueue: empty payload")
	}
	if err := q.client.underlying().LPush(ctx, heraldPendingKey, payload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Enqueue: LPUSH %q: %w", heraldPendingKey, err)
	}
	return nil
}

// ClaimedJob is the result of a successful [HeraldDeliveryQueue.Claim].
type ClaimedJob struct {
	// Payload is the serialized job (byte-for-byte identical to what was
	// stored in processing; Ack/Requeue need the exact value for LREM).
	Payload []byte
	// JobID is the job's id (the caller extracts it from the payload and
	// passes it here for the lease key); see SetLease.
	JobID string
}

// Claim blocks waiting for a job in pending until timeout, and atomically
// moves it to processing (BRPOPLPUSH). Returns (nil, nil) on an empty queue
// once the timeout elapses — the caller repeats the claim loop. [SetLease]
// sets the lease key separately (job_id is extracted from the payload in the
// herald package).
//
// blockTimeout=0 → blocks forever (undesirable — doesn't react to ctx.Done
// without a network ping); the caller passes a finite value (poll interval).
func (q *HeraldDeliveryQueue) Claim(ctx context.Context, blockTimeout time.Duration) (*ClaimedJob, error) {
	res, err := q.client.underlying().BRPopLPush(ctx, heraldPendingKey, heraldProcessingKey, blockTimeout).Result()
	if errors.Is(err, redis.Nil) {
		// Timeout — pending is empty. Not an error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis.HeraldDeliveryQueue.Claim: BRPOPLPUSH: %w", err)
	}
	return &ClaimedJob{Payload: []byte(res)}, nil
}

// SetLease sets/renews the job's lease key (PX=ttl). The worker calls it right
// after Claim and renews it periodically while delivering. Expiry of the key
// signals the mini-reaper ([RequeueExpired]) that the job is orphaned.
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

// Ack removes the job from processing after a terminal outcome
// (delivered/failed) and deletes the lease key. payload must be the exact
// value from [ClaimedJob.Payload] (LREM requires a byte-for-byte match). LREM
// count=1 — remove one copy (there shouldn't be duplicates: id is unique).
func (q *HeraldDeliveryQueue) Ack(ctx context.Context, jobID string, payload []byte) error {
	if err := q.client.underlying().LRem(ctx, heraldProcessingKey, 1, payload).Err(); err != nil {
		return fmt.Errorf("redis.HeraldDeliveryQueue.Ack: LREM processing: %w", err)
	}
	// Lease cleanup is best-effort: it will expire on its own via TTL, but an
	// explicit DEL frees memory right away. We don't propagate the DEL error —
	// the main Ack (LREM) already succeeded.
	_ = q.client.underlying().Del(ctx, heraldLeaseKey(jobID)).Err()
	return nil
}

// Requeue returns a job for retry: removes the old payload from processing and
// pushes newPayload (caller has already incremented attempt) back into
// pending. The old job's lease is deleted. Atomicity of LREM+LPUSH doesn't
// matter here: if a crash happens between them, the old payload stays in
// processing and [RequeueExpired] will pick it up once its lease expires —
// at-least-once is preserved.
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

// expiredRequeueFn is a callback that extracts jobID from a payload for
// [RequeueExpired]. The redis package doesn't know the job's shape; the herald
// package supplies the parser. Returning ok=false → the payload is corrupt,
// the mini-reaper drops it (LREM without requeuing).
type expiredRequeueFn func(payload []byte) (jobID string, ok bool)

// RequeueExpired is a mini-reaper for orphaned jobs: scans processing and, for
// every job without a live lease key, moves it back to pending (the worker
// that claimed the job died before it could Ack/Requeue). Returns the number
// requeued.
//
// Scans via LRange (a snapshot of processing) + a point lease check per job:
// processing is typically short (in-flight deliveries), so a full scan is
// cheap. Requeuing reuses the same payload (attempt is NOT changed — this
// isn't a new attempt by worker decision, it's a recovery after a crash).
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
			// Corrupt payload in processing — don't let the mini-reaper loop on
			// it: remove it without requeuing.
			_ = q.client.underlying().LRem(ctx, heraldProcessingKey, 1, payload).Err()
			continue
		}
		exists, err := q.client.underlying().Exists(ctx, heraldLeaseKey(jobID)).Result()
		if err != nil {
			return requeued, fmt.Errorf("redis.HeraldDeliveryQueue.RequeueExpired: EXISTS lease: %w", err)
		}
		if exists == 1 {
			// Lease is alive — job is being processed, leave it alone.
			continue
		}
		// Orphaned: lease expired, owner never Ack'd. Return the same payload.
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
