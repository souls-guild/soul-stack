package operator

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// JWTUseInterval is how often one Keeper instance writes `last_used_*` for one
// token. The column answers "is this token still in use, and from where" for a
// human reading the list; a write per request would turn every authenticated
// call into a database write.
const JWTUseInterval = 5 * time.Minute

const (
	jwtUseQueue = 256
	// jwtUseMemory bounds the per-token throttle map. Past it, entries older
	// than the interval are dropped; a dropped entry only costs one early write.
	jwtUseMemory = 10_000
)

type jwtUse struct {
	id     string
	at     time.Time
	client ClientMeta
}

// JWTUseRecorder writes `operator_jwts.last_used_*` off the request path.
// RecordUse never blocks and never fails the request: under a full queue the
// sample is dropped, and the next one within the interval takes its place.
type JWTUseRecorder struct {
	db     ExecQueryRower
	logger *slog.Logger
	now    func() time.Time

	mu   sync.Mutex
	last map[string]time.Time

	queue chan jwtUse
}

// NewJWTUseRecorder builds a recorder; [JWTUseRecorder.Run] drains it.
func NewJWTUseRecorder(db ExecQueryRower, logger *slog.Logger) *JWTUseRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &JWTUseRecorder{
		db:     db,
		logger: logger,
		now:    time.Now,
		last:   make(map[string]time.Time),
		queue:  make(chan jwtUse, jwtUseQueue),
	}
}

// RecordUse notes that the token jwtID was presented. An empty or malformed id
// (a token minted before per-token accounting) is ignored.
func (r *JWTUseRecorder) RecordUse(jwtID, ip, userAgent string) {
	if r == nil || !ValidJWTID(jwtID) {
		return
	}
	client := ClientMeta{IP: ip, UserAgent: userAgent}
	now := r.now()
	r.mu.Lock()
	if prev, ok := r.last[jwtID]; ok && now.Sub(prev) < JWTUseInterval {
		r.mu.Unlock()
		return
	}
	if len(r.last) >= jwtUseMemory {
		for id, at := range r.last {
			if now.Sub(at) >= JWTUseInterval {
				delete(r.last, id)
			}
		}
	}
	r.last[jwtID] = now
	r.mu.Unlock()

	select {
	case r.queue <- jwtUse{id: jwtID, at: now, client: client}:
	default:
		r.mu.Lock()
		delete(r.last, jwtID)
		r.mu.Unlock()
	}
}

// Run writes queued samples until ctx is done. A failed write is logged and
// dropped: the timestamp is advisory and the next sample retries it.
func (r *JWTUseRecorder) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case u := <-r.queue:
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := TouchJWT(writeCtx, r.db, u.id, u.at, u.client); err != nil && ctx.Err() == nil {
				r.logger.Warn("operator: recording jwt use failed", slog.String("jwt_id", u.id), slog.Any("error", err))
			}
			cancel()
		}
	}
}
