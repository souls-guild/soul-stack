package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// SSE params. The heartbeat comment keeps proxy/load-balancers from closing
// the connection on idle-timeout (typically 30-60s). The `:keepalive\n\n`
// comment-line is a standard SSE trick — it never materializes as event-data
// on the client.
const (
	sseHeartbeatInterval = 30 * time.Second
	sseQueryApplyID      = "apply_id"

	// sseMaxLifetime — hard ceiling on a single SSE stream's lifetime (M4:
	// guards against FD/goroutine leaks from clients that hold the
	// connection open without reading). Stream closes on expiry; client
	// reconnects (SSE auto-reconnect). 30 min comfortably covers a typical
	// apply run.
	sseMaxLifetime = 30 * time.Minute

	// sseMaxConnsGlobal / sseMaxConnsPerAID — concurrent-stream limits (M4).
	// Global protects the instance's FD/goroutine budget; per-AID guards
	// against one Archon opening hundreds of streams. Over limit → HTTP 429.
	sseMaxConnsGlobal = 256
	sseMaxConnsPerAID = 16
)

// applyAccessStore — narrow surface over applyrun-CRUD needed by the SSE
// handler for subscription RBAC checks (M1). Production impl is
// [applyAccessPG] over pgxpool.Pool; unit tests supply a fake.
type applyAccessStore interface {
	// Access resolves apply_id → run owner + incarnation. Returns
	// applyrun.ErrApplyRunNotFound if the run doesn't exist.
	Access(ctx context.Context, applyID string) (*applyrun.Access, error)
}

// sseDeps — narrow SSE handler dependencies. Bus is kept separate from
// HandlerDeps (used by tools/call) so EventBus doesn't leak into JSON-RPC-side
// unit tests.
//
// Access and RBAC implement subscription RBAC checks (M1): only the run's
// initiator (started_by_aid) or an Archon with `incarnation.get` on the
// corresponding incarnation may subscribe to an apply_id. If Access == nil,
// the subscription is denied (fail-closed): without an access-store the
// run's owner can't be resolved, and fail-open would expose another user's
// stream. Production always wires up Access.
type sseDeps struct {
	JWTVerifier *jwt.Verifier
	Bus         *applybus.EventBus
	Access      applyAccessStore
	RBAC        PermissionChecker
	Limiter     *sseConnLimiter
	Logger      *slog.Logger
}

// sseConnLimiter — active SSE stream counter: global + per-AID (M4).
// Thread-safe. Acquire reserves a slot (or returns false when over limit),
// Release frees it.
type sseConnLimiter struct {
	mu        sync.Mutex
	maxGlobal int
	maxPerAID int
	global    int
	perAID    map[string]int
}

// newSSEConnLimiter builds a limiter. maxGlobal/maxPerAID <= 0 means the
// limit is disabled on that axis.
func newSSEConnLimiter(maxGlobal, maxPerAID int) *sseConnLimiter {
	return &sseConnLimiter{
		maxGlobal: maxGlobal,
		maxPerAID: maxPerAID,
		perAID:    make(map[string]int),
	}
}

// Acquire reserves a slot for aid. Returns false if the global or per-AID
// limit is exceeded (caller returns 429, slot is NOT taken).
func (l *sseConnLimiter) Acquire(aid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxGlobal > 0 && l.global >= l.maxGlobal {
		return false
	}
	if l.maxPerAID > 0 && l.perAID[aid] >= l.maxPerAID {
		return false
	}
	l.global++
	l.perAID[aid]++
	return true
}

// Release frees aid's slot. Not idempotent — caller must call Release
// exactly once per successful Acquire (via defer).
func (l *sseConnLimiter) Release(aid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.global > 0 {
		l.global--
	}
	if n := l.perAID[aid]; n > 1 {
		l.perAID[aid] = n - 1
	} else {
		delete(l.perAID, aid)
	}
}

// buildSSEHandler — `GET /mcp/events?apply_id=<ULID>`. JWT-auth identical to
// `POST /mcp`: scheme-insensitive Bearer.
//
// Response contract:
//
//   - Content-Type: text/event-stream
//   - Cache-Control: no-cache, X-Accel-Buffering: no — disables reverse-proxy
//     (nginx) buffering and browser caching.
//   - Each bus event → `event: <kind>\ndata: <json>\nid: <apply_id>\n\n`.
//   - Heartbeat every 30s — `:keepalive\n\n`.
//
// Auth errors return HTTP 401 / 400 / 403 / 429 with a JSON error body (not
// SSE format) — the client hasn't subscribed yet, no point opening a stream
// just to close it immediately.
//
// RBAC (M1): subscribing to an apply_id is allowed only for the run's
// initiator (apply_runs.started_by_aid == JWT.sub) or an Archon with
// `incarnation.get` on the corresponding incarnation. Nonexistent apply_id →
// 403 (anti-enum: indistinguishable from access denial — ULIDs are
// guessable).
//
// Resource limits (M4): global + per-AID concurrent-stream limit (429 over
// limit) and a hard max-lifetime ceiling (stream closes, client reconnects).
func buildSSEHandler(deps sseDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed,
				"method not allowed (use GET)")
			return
		}

		token, ok := jwt.ParseBearerToken(r.Header.Get("Authorization"))
		if !ok {
			writeJSONError(w, http.StatusUnauthorized,
				"missing or malformed Authorization header (expect: Bearer <jwt>)")
			return
		}
		claims, err := deps.JWTVerifier.Verify(token)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized,
				"unauthenticated: "+jwt.ClassifyVerifyErr(err))
			return
		}

		applyID := strings.TrimSpace(r.URL.Query().Get(sseQueryApplyID))
		if applyID == "" {
			writeJSONError(w, http.StatusBadRequest,
				"missing required query param 'apply_id'")
			return
		}

		// Subscription RBAC check (M1). Runs BEFORE Acquire/headers — a
		// denial returns as a plain JSON error, stream never opens.
		if !authorizeSSE(r.Context(), deps, claims.Subject, applyID) {
			writeJSONError(w, http.StatusForbidden,
				"forbidden: no access to this apply_id")
			return
		}

		// Connection limit (M4). Acquire runs after RBAC — only authorized
		// subscriptions take a slot. Release strictly via defer.
		if deps.Limiter != nil {
			if !deps.Limiter.Acquire(claims.Subject) {
				writeJSONError(w, http.StatusTooManyRequests,
					"too many concurrent event streams; retry later")
				return
			}
			defer deps.Limiter.Release(claims.Subject)
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			// Server configured with an http.Server lacking flush capability —
			// this is a config bug (Go stdlib HTTP/1.1 provides Flusher over
			// the response writer by default). Return 500 so the client
			// doesn't hang waiting.
			deps.Logger.Error("mcp/sse: ResponseWriter does not implement http.Flusher")
			writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Nginx/Apache reverse proxies honor this header and skip buffering.
		w.Header().Set("X-Accel-Buffering", "no")

		// SSE is a long-lived stream; the global http.Server WriteTimeout
		// (60s) would kill the connection. Clear the deadline for this one
		// request. http.NewResponseController was added in Go 1.20.
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			// stdlib http.Server supports SetWriteDeadline — an error from
			// the controller means a non-standard middleware is in the
			// stack. Log and continue: events still flow, but the client
			// gets cut off by WriteTimeout.
			deps.Logger.Warn("mcp/sse: SetWriteDeadline failed", slog.Any("error", err))
		}

		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Max-lifetime ceiling (M4): derived ctx cancelled on sseMaxLifetime
		// expiry OR client disconnect (r.Context()).
		ctx, cancel := context.WithTimeout(r.Context(), sseMaxLifetime)
		defer cancel()

		ch := deps.Bus.Subscribe(ctx, applyID)

		deps.Logger.Info("mcp/sse: subscriber opened",
			slog.String("apply_id", applyID),
			slog.String("aid", claims.Subject),
		)
		defer deps.Logger.Info("mcp/sse: subscriber closed",
			slog.String("apply_id", applyID),
			slog.String("aid", claims.Subject),
		)

		heartbeat := time.NewTicker(sseHeartbeatInterval)
		defer heartbeat.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-heartbeat.C:
				if _, err := w.Write([]byte(":keepalive\n\n")); err != nil {
					return
				}
				flusher.Flush()
			case ev, ok := <-ch:
				if !ok {
					// Bus closed the channel — usually concurrent with
					// ctx.Done() (our own goroutine). Extra return in case
					// of a race.
					return
				}
				if err := writeSSEEvent(w, ev); err != nil {
					deps.Logger.Warn("mcp/sse: write event failed",
						slog.String("apply_id", ev.ApplyID),
						slog.String("kind", string(ev.Kind)),
						slog.Any("error", err),
					)
					return
				}
				flusher.Flush()
			}
		}
	}
}

// authorizeSSE — subscription RBAC check for apply_id (M1).
//
// Rules:
//   - Access == nil → deny (fail-closed): without an access-store the run's
//     owner/incarnation can't be resolved, so the check can't run and the
//     subscription is denied. Production always wires up Access.
//   - apply_id not found → deny (anti-enum: indistinguishable from access
//     denial).
//   - started_by_aid == sub → allow (the initiator always sees their own
//     run).
//   - otherwise → allow only with `incarnation.get` permission on the run's
//     incarnation (RBAC == nil → deny, except for the owner case above).
//
// Any infrastructure error (PG unavailable) → deny (fail-closed): safer to
// refuse the subscription than expose another user's stream.
func authorizeSSE(ctx context.Context, deps sseDeps, sub, applyID string) bool {
	if deps.Access == nil {
		deps.Logger.Warn("mcp/sse: apply access-store not configured (deny)",
			slog.String("apply_id", applyID),
		)
		return false
	}
	acc, err := deps.Access.Access(ctx, applyID)
	if err != nil {
		if !errors.Is(err, applyrun.ErrApplyRunNotFound) {
			deps.Logger.Warn("mcp/sse: apply access lookup failed (deny)",
				slog.String("apply_id", applyID),
				slog.Any("error", err),
			)
		}
		return false
	}
	if acc.StartedByAID != nil && *acc.StartedByAID == sub {
		return true
	}
	if deps.RBAC == nil {
		return false
	}
	err = deps.RBAC.Check(sub, "incarnation", "get", map[string]string{
		"incarnation": acc.IncarnationName,
	})
	return err == nil
}

// writeSSEEvent serializes one apply event into an SSE frame:
//
//	event: <kind>
//	id:    <apply_id>
//	data:  <json payload>
//	<blank line>
//
// Payload runs through [audit.MaskSecrets] before serialization (H1):
// register-output / state_changes can carry secrets (`bootstrap_token` etc.)
// that would otherwise leak into the SSE frame in the clear. Payload is
// written as a single JSON string (json.Marshal never leaves \n inside the
// result, so SSE's "no multiline data" requirement holds automatically).
func writeSSEEvent(w http.ResponseWriter, ev applybus.Event) error {
	masked := maskSSEPayload(ev.Payload)
	payloadJSON, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n",
		ev.Kind, ev.ApplyID, payloadJSON,
	); err != nil {
		return err
	}
	return nil
}

// maskSSEPayload converts payload to its masked form (H1). Payload arrives
// in two shapes:
//
//   - map[string]any — local publish (events_taskevent.go / events_runresult.go);
//     masked directly via [audit.MaskSecrets].
//   - json.RawMessage / []byte — cross-Keeper cluster-bridge (applybus
//     forward); decoded into map[string]any first, masked, then returned as
//     a map (writeSSEEvent's json.Marshal re-serializes it).
//     If payload is non-object JSON (array/scalar) or decode fails, return
//     it unchanged (nothing to mask by key; a vault-ref in a scalar is an
//     extremely unlikely edge case for the SSE payload contract).
//
// Any other type is returned as-is (the payload contract is an object).
func maskSSEPayload(payload any) any {
	switch p := payload.(type) {
	case nil:
		return nil
	case map[string]any:
		return audit.MaskSecrets(p)
	case json.RawMessage:
		return maskRawJSON(p)
	case []byte:
		return maskRawJSON(p)
	default:
		return payload
	}
}

// maskRawJSON decodes a raw JSON object, masks it, and returns a map. On
// non-object JSON or a decode error, returns the original raw bytes (caller
// serializes it as-is).
func maskRawJSON(raw []byte) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return json.RawMessage(raw)
	}
	return audit.MaskSecrets(m)
}

// SSE-channel payload contract — snake_case keys, one set shared across all
// EventKind values:
//
//   - apply.started     → {apply_id, kind, at}
//   - task.executed     → + {sid, task_idx, task_status, error?: {code,message,module}}
//   - apply.completed   → + {sid, run_status, state_changes?}
//   - apply.failed      → + {sid, run_status}
//   - apply.cancelled   → + {sid, run_status}
//
// Structure is `map[string]any` (not a typed struct) because publishers live
// in the `keeper/internal/grpc` package, and pulling in a typed struct from
// there would require either a reverse import from mcp into grpc, or a third
// package. The contract is pinned in docs/keeper/mcp-tools.md (a separate
// documentation slice), not by a Go type.
