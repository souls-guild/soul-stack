package api

// GET /v1/incarnations/{id}/runs/{apply_id}/events — live progress of an incarnation
// run (SSE, ADR-068 §A3). Symmetric to the existing RunDetail path + the SSE precedent
// `/mcp/events`, but on the Operator plane /v1.
//
// AUTH (ADR-068 §A0 = fetch-streaming): the frontend opens the stream via `fetch()`+
// `getReader()` and sends `Authorization: Bearer` (unlike EventSource, fetch can set
// headers) → the token is NOT in the URL. Route is under the /v1 RequireJWT chain (the
// `*/events` path). There is NO separate minting endpoint / short query-token (dropped
// from ADR-068).
//
// WHY NOT /mcp/events: that is the MCP plane (JSON-RPC tool-call streaming), its own auth.
// Dragging the web UI into the MCP channel = mixing planes. ★ /mcp/events is NOT touched
// by this slice — a narrow duplicate of the stream/masking lives here (ADR-068 §A3
// "duplicate narrowly", instead of sharing code from mcp/sse.go).
//
// ★ That duplication is also how NIM-844's re-authorization reached this stream and not the
// one it was copied from. The re-check is therefore NOT duplicated: both call
// apimiddleware.ReauthTicker, and middleware/longlived_guard_test.go derives the list of
// long-lived channels from the source so a fourth cannot appear unnoticed (NIM-858).
//
// Registered via huma.StreamResponse → the operation lands in the OpenAPI spec (drift-guard
// TestFullSpec_CoversAllRoutes) with full control of the stream body
// (heartbeat/max-lifetime/frame/masking/limits), which the huma/sse helper does not give.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// SSE parameters (parity with mcp/sse.go). Heartbeat keeps a proxy/LB from closing an idle
// connection; max-lifetime is the ceiling against FD/goroutine leaks from "stuck" clients;
// conn limits protect the instance's FD/goroutine budget and guard against a single Archon
// with hundreds of streams.
const (
	sseHeartbeatInterval = 30 * time.Second
	sseMaxLifetime       = 30 * time.Minute
	sseMaxConnsGlobal    = 256
	sseMaxConnsPerAID    = 16
)

// runEventsAccess — a narrow surface for resolving apply_id → owner+incarnation for the
// RBAC SSE subscription. Prod uses [applyrun.SelectAccessByApplyID] over the pool; the test
// substitutes a fake.
type runEventsAccess interface {
	Access(ctx context.Context, applyID string) (*applyrun.Access, error)
}

// runEventsPGAccess — the prod implementation of [runEventsAccess] over the Operator pool.
type runEventsPGAccess struct {
	db applyrun.ExecQueryRower
}

func (a runEventsPGAccess) Access(ctx context.Context, applyID string) (*applyrun.Access, error) {
	return applyrun.SelectAccessByApplyID(ctx, a.db, applyID)
}

// runEventsRBAC — the RBAC surface this stream needs: the scope-aware check that
// decides the subscription, and the revocation projection that keeps deciding it
// while the stream runs (NIM-844, see middleware/longlived.go).
//
// Declared as one interface rather than reached by a type assertion on the
// checker: a wiring that cannot answer "is this Archon revoked" must fail to
// compile, not fail to notice.
type runEventsRBAC interface {
	apimiddleware.PermissionChecker
	apimiddleware.RevocationChecker
}

// runEventsDeps — dependencies of the run SSE handler. Bus/Access/RBAC provide the stream +
// the RBAC subscription; Limiter/Logger are the resource-guard and observability. Any nil
// (except Limiter/Logger) → the subscription is rejected fail-closed (see [authorizeRunEventsSSE]).
type runEventsDeps struct {
	Bus     *applybus.EventBus
	Access  runEventsAccess
	RBAC    runEventsRBAC
	Limiter *sseConnLimiter
	Logger  *slog.Logger
	// ReauthInterval overrides how often an open stream re-asks whether the
	// subscriber may still have it (NIM-844); zero takes
	// [apimiddleware.LongLivedReauthInterval]. Only this file's tests set it, to
	// reach the second check without waiting out the production interval.
	ReauthInterval time.Duration
}

// newRunEventsDeps assembles the prod deps over applybus + Operator-pool + enforcer.
// db/rbac/bus nil → the SSE route is not mounted in router.go (opt-in wire-up).
func newRunEventsDeps(bus *applybus.EventBus, db applyrun.ExecQueryRower, rbac runEventsRBAC, logger *slog.Logger) *runEventsDeps {
	if bus == nil || db == nil || rbac == nil {
		return nil
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &runEventsDeps{
		Bus:     bus,
		Access:  runEventsPGAccess{db: db},
		RBAC:    rbac,
		Limiter: newSSEConnLimiter(sseMaxConnsGlobal, sseMaxConnsPerAID),
		Logger:  logger,
	}
}

// incRunEventsInput — huma input for GET .../runs/{apply_id}/events. Name/ApplyID are path params.
type incRunEventsInput struct {
	ID      string `path:"id" doc:"incarnation id"`
	ApplyID string `path:"apply_id" doc:"run ULID; someone else's/nonexistent -> 403 (anti-enum)"`
}

func incRunEventsOperation() huma.Operation {
	op := huma.Operation{
		OperationID:   "streamIncarnationRunEvents",
		Method:        http.MethodGet,
		Path:          "/{id}/runs/{apply_id}/events",
		Summary:       "Live run progress of an incarnation (SSE)",
		Description:   "text/event-stream: task.executed/apply.completed/failed/cancelled by apply_id. Auth: Authorization: Bearer (fetch-streaming, ADR-068 §A0). Access: the initiator OR incarnation.get/history; someone else's/nonexistent apply_id -> 403 (anti-enum, parity /mcp/events). Secrets in the payload are masked.",
		Tags:          []string{"incarnation"},
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError},
	}
	// Explicitly declare text/event-stream (parity with the huma/sse helper) — the body is
	// streaming; we do not create a named schema (inline string), to avoid a tech name in the spec.
	op.Responses = map[string]*huma.Response{
		"200": {
			Description: "SSE stream of apply events for the run",
			Content: map[string]*huma.MediaType{
				"text/event-stream": {Schema: &huma.Schema{Type: huma.TypeString}},
			},
		},
	}
	return op
}

// registerHumaIncarnationRunEvents mounts GET .../runs/{apply_id}/events as a streaming
// route (huma.StreamResponse). deps nil → no-op (opt-in wire-up). Route is under the /v1
// RequireJWT chain (canonical query-token */events) WITHOUT a chi RequireAction: the RBAC
// "initiator OR incarnation.get/history" is not expressible as an existence gate (the
// initiator may lack the right) — all authorization is in-handler (parity /mcp/events authorizeSSE).
func registerHumaIncarnationRunEvents(humaAPI huma.API, deps *runEventsDeps) {
	if deps == nil {
		return
	}
	huma.Register(humaAPI, incRunEventsOperation(), func(ctx context.Context, in *incRunEventsInput) (*huma.StreamResponse, error) {
		claims, ok := apimiddleware.ClaimsFromContext(ctx)
		if !ok || claims == nil {
			return nil, incMissingClaims()
		}
		// anti-enum: ANY denial (not found / foreign incarnation / no rights) → the same
		// 403, indistinguishable from "no access" (ULIDs are guessable, parity /mcp/events).
		grant, ok := authorizeRunEventsSSE(ctx, deps, claims.Subject, in.ID, in.ApplyID)
		if !ok {
			return nil, sseForbidden()
		}
		// conn-limit (M4): take a slot ONLY for an authorized subscription, release it in the
		// stream body's defer (huma is guaranteed to call Body on a StreamResponse).
		if deps.Limiter != nil && !deps.Limiter.Acquire(claims.Subject) {
			return nil, sseTooManyStreams()
		}
		aid := claims.Subject
		applyID := in.ApplyID
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			if deps.Limiter != nil {
				defer deps.Limiter.Release(aid)
			}
			streamRunEvents(hctx, deps, applyID, aid, grant)
		}}, nil
	})
}

// runEventsGrant — what the opening decision learned from the run's row, kept so
// the re-check (NIM-844) does not have to read it again. Both fields are
// immutable for a given apply_id: a run does not change which incarnation it
// belongs to, nor who started it. Everything that CAN change — the Archon's
// roles, their scope, whether they are revoked at all — is re-read per check.
type runEventsGrant struct {
	incarnation string
	// initiator is true when this subscriber started the run, which is the half
	// of the rule no permission can express.
	initiator bool
}

// authorizeRunEventsSSE — RBAC check of the subscription to apply_id (ADR-068 §A3, parity
// with /mcp/events authorizeSSE). fail-closed on any failure / nil deps. Order:
//   - Access/RBAC nil or lookup error / not found → deny (anti-enum, indistinguishable from a denial);
//   - apply_id belongs to a DIFFERENT incarnation (not path-{name}) → deny (foreign run);
//   - the run initiator (started_by_aid == sub) → allow;
//   - otherwise allow on incarnation.get OR incarnation.history on the incarnation.
//
// On allow it returns the [runEventsGrant] the stream re-checks against.
func authorizeRunEventsSSE(ctx context.Context, deps *runEventsDeps, sub, name, applyID string) (runEventsGrant, bool) {
	var zero runEventsGrant
	if deps.Access == nil {
		return zero, false
	}
	acc, err := deps.Access.Access(ctx, applyID)
	if err != nil {
		return zero, false
	}
	// apply_id must be a run of THIS exact incarnation (path-{name}); otherwise it is a foreign
	// run, deny without revealing that apply_id lives in another incarnation.
	if acc.IncarnationName != name {
		return zero, false
	}
	grant := runEventsGrant{
		incarnation: name,
		initiator:   acc.StartedByAID != nil && *acc.StartedByAID == sub,
	}
	if grant.initiator {
		return grant, true
	}
	if deps.RBAC == nil {
		return zero, false
	}
	if deps.RBAC.Check(sub, "incarnation", "get", map[string]string{"incarnation": name}) == nil {
		return grant, true
	}
	if deps.RBAC.Check(sub, "incarnation", "history", map[string]string{"incarnation": name}) == nil {
		return grant, true
	}
	return zero, false
}

// runEventsStillAuthorized re-decides the RBAC half of [authorizeRunEventsSSE]
// for a stream already running (NIM-844). A stream lives up to [sseMaxLifetime]
// and `apimiddleware.RejectRevoked` cannot reach a request already inside its
// handler, so the only place left to ask is here.
//
// It is the opening rule with revocation added in front of it, and the same rule
// in the same order otherwise — including the initiator short-circuit that
// precedes the nil-RBAC refusal. Making the continuing check STRICTER than the
// opening one would produce a stream that opens and then dies for a reason that
// was already true when it opened, which tells the operator nothing and costs
// them the run they were watching.
//
// Revocation comes first because it is not a permission question: the token is no
// longer trusted, so being the run's initiator stops earning anything.
func runEventsStillAuthorized(deps *runEventsDeps, sub string, g runEventsGrant) bool {
	if deps.RBAC != nil && deps.RBAC.IsRevoked(sub) {
		return false
	}
	if g.initiator {
		return true
	}
	if deps.RBAC == nil {
		return false
	}
	if deps.RBAC.Check(sub, "incarnation", "get", map[string]string{"incarnation": g.incarnation}) == nil {
		return true
	}
	return deps.RBAC.Check(sub, "incarnation", "history", map[string]string{"incarnation": g.incarnation}) == nil
}

// streamRunEvents pushes applyID's apply events into the SSE stream until the client
// disconnects, max-lifetime, or the bus closes. Frame `event/id/data`, heartbeat 30s (parity
// with mcp/sse.go). The payload is masked by [audit.MaskSecrets] on the write path (a second
// barrier over the publishers' secret hygiene).
func streamRunEvents(hctx huma.Context, deps *runEventsDeps, applyID, aid string, grant runEventsGrant) {
	hctx.SetHeader("Content-Type", "text/event-stream")
	hctx.SetHeader("Cache-Control", "no-cache")
	hctx.SetHeader("Connection", "keep-alive")
	hctx.SetHeader("X-Accel-Buffering", "no")

	bw := hctx.BodyWriter()
	flusher := unwrapFlusher(bw)
	if d := unwrapWriteDeadliner(bw); d != nil {
		// SSE is long-lived: clear the http.Server WriteTimeout for this request.
		_ = d.SetWriteDeadline(time.Time{})
	}

	// Immediate flush of the headers (200) + an SSE comment: EventSource onopen fires BEFORE
	// the first event/heartbeat (client-open immediacy, parity with mcp/sse.go
	// WriteHeader+Flush). Otherwise huma commits 200 only on the first write — the client would
	// hang until the 30s heartbeat.
	_, _ = bw.Write([]byte(":ok\n\n"))
	flush(flusher)

	ctx, cancel := context.WithTimeout(hctx.Context(), sseMaxLifetime)
	defer cancel()

	ch := deps.Bus.Subscribe(ctx, applyID)

	deps.Logger.Info("v1/sse: run-events subscriber opened",
		slog.String("apply_id", applyID), slog.String("aid", aid))
	defer deps.Logger.Info("v1/sse: run-events subscriber closed",
		slog.String("apply_id", applyID), slog.String("aid", aid))

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	// Its own ticker rather than a branch on the heartbeat: the heartbeat is a
	// client-liveness interval and this is an authorization one, and tying them
	// together would mean changing either to change the other.
	reauth := apimiddleware.ReauthTicker(deps.ReauthInterval)
	defer reauth.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reauth.C:
			if !runEventsStillAuthorized(deps, aid, grant) {
				// No closing frame: the event kinds on this stream are the bus's
				// (ADR-068 §A3), and the client learns the same way it would after
				// any disconnect — by re-opening and being answered 403.
				deps.Logger.Warn("v1/sse: run-events subscriber closed, access withdrawn under it",
					slog.String("apply_id", applyID), slog.String("aid", aid))
				return
			}
		case <-heartbeat.C:
			if _, err := bw.Write([]byte(":keepalive\n\n")); err != nil {
				return
			}
			flush(flusher)
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if err := writeRunEventFrame(bw, ev); err != nil {
				return
			}
			flush(flusher)
		}
	}
}

// writeRunEventFrame serializes an apply event into the SSE frame `event/id/data` with payload
// masking (H1, the second barrier). A narrow duplicate of mcp writeSSEEvent — /mcp/events is not touched.
func writeRunEventFrame(w io.Writer, ev applybus.Event) error {
	masked := maskRunEventPayload(ev.Payload)
	payloadJSON, err := json.Marshal(masked)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	_, err = fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", ev.Kind, ev.ApplyID, payloadJSON)
	return err
}

// maskRunEventPayload brings the payload to a masked form (H1): a map — directly MaskSecrets;
// raw JSON (cross-Keeper bridge) — decode→mask→map; otherwise — as-is. A narrow duplicate of mcp
// maskSSEPayload (ADR-068 §A3).
func maskRunEventPayload(payload any) any {
	switch p := payload.(type) {
	case nil:
		return nil
	case map[string]any:
		return audit.MaskSecrets(p)
	case json.RawMessage:
		return maskRunEventRawJSON(p)
	case []byte:
		return maskRunEventRawJSON(p)
	default:
		return payload
	}
}

func maskRunEventRawJSON(raw []byte) any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return json.RawMessage(raw)
	}
	return audit.MaskSecrets(m)
}

// --- streaming helpers (unwrap flusher/deadliner from the huma adapter's BodyWriter) ---

func flush(f http.Flusher) {
	if f != nil {
		f.Flush()
	}
}

func unwrapFlusher(w io.Writer) http.Flusher {
	for {
		if f, ok := w.(http.Flusher); ok {
			return f
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
}

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

func unwrapWriteDeadliner(w io.Writer) writeDeadliner {
	for {
		if d, ok := w.(writeDeadliner); ok {
			return d
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
}

// --- problem responses for the SSE route ---

func sseForbidden() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeForbidden, http.StatusForbidden, "forbidden: no access to this run")}
}

func sseTooManyStreams() huma.StatusError {
	return humaProblemError{Details: problemWithStatus(problem.TypeTempoExceeded, http.StatusTooManyRequests, "too many concurrent event streams; retry later")}
}

// --- conn-limiter (global + per-AID, parity with mcp sseConnLimiter, narrow duplicate) ---

type sseConnLimiter struct {
	mu        sync.Mutex
	maxGlobal int
	maxPerAID int
	global    int
	perAID    map[string]int
}

func newSSEConnLimiter(maxGlobal, maxPerAID int) *sseConnLimiter {
	return &sseConnLimiter{maxGlobal: maxGlobal, maxPerAID: maxPerAID, perAID: make(map[string]int)}
}

// Acquire reserves a slot for aid; false when the global/per-AID limit is exceeded.
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

// Release frees aid's slot (exactly once per successful Acquire).
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
