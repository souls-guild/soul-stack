package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AugurDeps — wire-up dependencies for the `AugurRequest` handler over EventStream
// (ADR-025, augur.md). Broker (delegate=false, MVP-1): authorization resolve +
// Vault KV read + sending the AugurReply back on the same stream.
//
// All fields are required:
//   - DB — omens/rites + souls registry (resolves Omen / Rite / covens by SID);
//   - Vault — Keeper-side ReadKV (vault broker + reads prom/elk credentials via
//     Omen.AuthRef);
//   - Egress — SSRF-guarded HTTP client for prom/elk brokers (outbound HTTP to
//     the UNtrusted Omen endpoint, [augur.NewEgressClient]);
//   - AuditWriter — `augur.fetch_brokered` / `augur.access_denied`;
//   - Outbound — sends the `AugurReply` back on the Soul's stream.
//
// nil AugurDeps (handler not wired up) → the handler logs a warning and
// ignores the request (minimally-invasive fallback for builds without Augur).
type AugurDeps struct {
	DB          augurDB
	Vault       augur.KVReader
	Egress      augur.HTTPDoer
	AuditWriter audit.Writer
	Outbound    *Outbound

	// Metrics — keeper_augur_* descriptor (ADR-024). Optional: nil →
	// instrumentation disabled (nil-safe [augur.BrokerMetrics.ObserveFetch] —
	// no-op), same as Metrics in [EventStreamDeps]. Registered in the daemon's
	// `setupMetricsRegistry`, injected here when the broker is wired up.
	Metrics *augur.BrokerMetrics
}

// augurDB — the combined PG surface the resolve needs: omens/rites CRUD readers
// (augur.ExecQueryRower) + a souls reader (soul.ExecQueryRower) for resolving covens
// by SID from the authoritative registry. *pgxpool.Pool satisfies both.
type augurDB interface {
	augur.ExecQueryRower
	soul.ExecQueryRower
}

func (d *AugurDeps) validate() error {
	if d.DB == nil {
		return errors.New("grpc: AugurDeps.DB is required")
	}
	if d.Vault == nil {
		return errors.New("grpc: AugurDeps.Vault is required")
	}
	if d.Egress == nil {
		return errors.New("grpc: AugurDeps.Egress is required")
	}
	if d.AuditWriter == nil {
		return errors.New("grpc: AugurDeps.AuditWriter is required")
	}
	if d.Outbound == nil {
		return errors.New("grpc: AugurDeps.Outbound is required")
	}
	return nil
}

// augurOmenReader / augurRiteReader / augurCovenReader — registry adapters for
// the narrow reader interfaces of [augur.Resolve]. They isolate enforcement from
// a concrete pool and keep covens resolution on the authoritative souls.coven[]
// (NOT from the payload).
type augurOmenReader struct{ db augur.ExecQueryRower }

func (r augurOmenReader) OmenByName(ctx context.Context, name string) (*augur.Omen, error) {
	return augur.SelectOmenByName(ctx, r.db, name)
}

type augurRiteReader struct{ db augur.ExecQueryRower }

func (r augurRiteReader) RitesBySubject(ctx context.Context, sid string, covens []string) ([]*augur.Rite, error) {
	return augur.SelectRitesBySubject(ctx, r.db, sid, covens)
}

type augurCovenReader struct{ db soul.ExecQueryRower }

func (r augurCovenReader) CovensBySID(ctx context.Context, sid string) ([]string, error) {
	s, err := soul.SelectBySID(ctx, r.db, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, augur.ErrSubjectUnknown
		}
		return nil, err
	}
	return s.Coven, nil
}

// handleAugurRequest — handler for the [keeperv1.AugurRequest] payload (ADR-025).
//
// SID comes from the session's mTLS peer cert (passed by the caller), NOT from
// AugurRequest — the authority for a Soul's identity is the certificate (ADR-012(i)).
//
// Runs in its OWN goroutine spawned from [dispatch]: the fetch (vault/prom/elk)
// may take time, and the receive loop must not block on it (other FromSoul
// messages on the same stream — TaskEvent / RunResult — keep being received).
// Cancellation via the stream's ctx.Done() is inherited by the goroutine.
//
// DoS guard: spawning the goroutine goes through the global augurSem semaphore
// (a limit on concurrent Augur processing across ALL streams). Non-blocking
// acquire: on overflow → AugurReply{ERROR}, no new goroutine spawned (an
// UNtrusted Soul flooding AugurRequests can't exhaust the Keeper's
// goroutines/connections). The semaphore is released in processAugurRequest
// on every outcome.
//
// Flow (broker, delegate=false):
//  1. augur.Resolve (enforcement) — covens from the registry, a Rite on the
//     Omen, query ∈ allow via EXACT match on the source_type's shape.
//  2. denied → AugurReply{DENIED} + audit `augur.access_denied`.
//  3. allowed → broker dispatch by source_type (vault ReadKV / prom HTTP / elk HTTP) →
//     inline_data Struct.
//  4. fetch failure → AugurReply{ERROR} (no audit written — access was granted,
//     but the fetch didn't happen; this is an operational failure, not a
//     security event).
//  5. ok → AugurReply{OK, inline_data} + audit `augur.fetch_brokered`.
//
// Secrets/credentials are NEVER logged and NEVER go into the audit: only omen +
// query + request_id are written, never the value (augur.md §8). Reply goes out
// via Outbound.SendAugurReply.
func (h *eventStreamHandler) handleAugurRequest(ctx context.Context, sid, sessionID string, req *keeperv1.AugurRequest) {
	deps := h.deps.Augur
	if deps == nil {
		h.logger.Warn("eventstream: AugurRequest received but augur not wired up",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	if req == nil {
		h.logger.Warn("eventstream: AugurRequest payload is nil",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}

	// Non-blocking acquire: on overflow, don't spawn a goroutine — send
	// ERROR straight from the receive loop (cheap, no blocking). A nil
	// semaphore (limit disabled) → the old behavior: always spawn.
	if h.augurSem != nil {
		select {
		case h.augurSem <- struct{}{}:
			// Slot acquired — released in processAugurRequest.
		default:
			h.logger.Warn("eventstream: augur concurrency limit reached — rejecting request",
				slog.String("sid", sid),
				slog.String("session_id", sessionID),
				slog.String("request_id", req.GetRequestId()),
			)
			// Rejected before resolve — source is not yet known (the Omen's type
			// hasn't been read), outcome is error (the Soul gets AugurReply{ERROR}).
			// Duration ~0: processing never started.
			deps.Metrics.ObserveFetch(augur.SourceUnknown, augur.DecisionError, 0)
			h.sendAugurError(ctx, sid, req.GetRequestId(), "augur busy: concurrency limit reached")
			return
		}
		go func() {
			defer func() { <-h.augurSem }()
			h.processAugurRequest(ctx, sid, sessionID, req)
		}()
		return
	}
	go h.processAugurRequest(ctx, sid, sessionID, req)
}

// processAugurRequest — the processing body (split into its own function for
// goroutine readability). A reply is sent on every outcome (OK/DENIED/ERROR);
// the Soul treats status_UNSPECIFIED as DENIED (default-deny on that side).
func (h *eventStreamHandler) processAugurRequest(ctx context.Context, sid, sessionID string, req *keeperv1.AugurRequest) {
	deps := h.deps.Augur
	omenName := req.GetOmenName()
	query := req.GetQuery()
	requestID := req.GetRequestId()
	applyID := req.GetApplyId()

	// In-process span for AugurRequest processing (resolve + fetch). Attributes
	// carry NO secrets and no cardinality blow-up (augur.md §8, ADR-024 §2.2):
	// sid is the subject identity (already in logs/audit), source_type/decision
	// are a closed enum, filled in as we go. omen_name / query / the secret value
	// never go into the span. With OTel disabled the tracer is a no-op —
	// Start/End are free.
	ctx, span := augur.Tracer().Start(ctx, augur.SpanName,
		trace.WithAttributes(attribute.String("sid", sid)),
	)
	// The metric and span status are recorded once on any exit path.
	// source/decision are filled in as we go: before resolve, source is
	// unknown (the Omen's type hasn't been read).
	started := time.Now()
	source := augur.SourceUnknown
	decisionLabel := augur.DecisionError
	defer func() {
		deps.Metrics.ObserveFetch(source, decisionLabel, time.Since(started))
		span.SetAttributes(
			attribute.String("source_type", source),
			attribute.String("decision", decisionLabel),
		)
		if decisionLabel == augur.DecisionError {
			span.SetStatus(codes.Error, decisionLabel)
		}
		span.End()
	}()

	decision, err := augur.Resolve(ctx,
		augurOmenReader{db: deps.DB},
		augurRiteReader{db: deps.DB},
		augurCovenReader{db: deps.DB},
		sid, omenName, query,
	)
	if err != nil {
		// An infrastructure failure during resolve (PG unavailable) — ERROR, not
		// DENIED. We don't expose the reason externally (it may carry registry
		// details); logging it is fine.
		h.logger.Warn("eventstream: augur resolve failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.Any("error", err),
		)
		h.sendAugurError(ctx, sid, requestID, "augur resolve failed")
		return
	}

	if !decision.Allowed {
		decisionLabel = augur.DecisionDenied
		// On denied, the Omen may not have existed (denied before it was read) —
		// then source stays unknown; otherwise we take the found Omen's type.
		if decision.Omen != nil {
			source = string(decision.Omen.SourceType)
		}
		h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
			RequestId: requestID,
			Status:    keeperv1.AugurStatus_AUGUR_STATUS_DENIED,
			Error:     decision.Reason,
		})
		h.auditAugur(ctx, audit.EventAugurAccessDenied, sid, omenName, query, requestID, applyID, decision.Reason)
		h.logger.Info("eventstream: augur access denied",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.String("reason", decision.Reason),
		)
		return
	}

	// Access granted — the Omen's type is known. source is set here so a
	// fetch failure below is recorded with the right source (not unknown).
	source = string(decision.Omen.SourceType)

	// Broker dispatch by source_type. decision.Query is the canonical fetch
	// value: vault — a normalized logical path; prom/elk — promQL/index
	// "as-is" (already passed exact-match in Resolve). endpoint/auth_ref come
	// from decision.Omen (the UNtrusted endpoint is protected by an SSRF
	// guard inside the broker).
	inline, err := h.brokerFetch(ctx, deps, decision)
	if err != nil {
		// Access was granted, but the fetch failed (external system down /
		// SSRF guard rejected the endpoint / the path disappeared) — an
		// operational ERROR, not a security deny. No audit written (nothing to
		// record as "read"); secrets/credentials/response bodies never end up
		// in the error (see broker_*.go). The Soul gets a generic diagnostic.
		h.logger.Warn("eventstream: augur broker fetch failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.String("omen", omenName),
			slog.String("source_type", string(decision.Omen.SourceType)),
			slog.Any("error", err),
		)
		h.sendAugurError(ctx, sid, requestID, "augur fetch failed")
		return
	}

	decisionLabel = augur.DecisionOK
	h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
		RequestId: requestID,
		Status:    keeperv1.AugurStatus_AUGUR_STATUS_OK,
		Result:    &keeperv1.AugurReply_InlineData{InlineData: inline},
	})
	h.auditAugur(ctx, audit.EventAugurFetchBrokered, sid, omenName, query, requestID, applyID, "")
	h.logger.Info("eventstream: augur fetch brokered",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.String("omen", omenName),
		slog.String("request_id", requestID),
	)
}

// brokerFetch dispatches the fetch by the allowed Omen's source_type
// (delegate=false, augur.md §6 branch table). vault — ReadKV (Slice B);
// prometheus / elk — SSRF-guarded HTTP to Omen.Endpoint (Slice C). Returns an
// inline_data Struct or an operational error (caller → AugurReply{ERROR});
// neither the secret, the credential, nor the external response body ever
// end up in the error.
func (h *eventStreamHandler) brokerFetch(ctx context.Context, deps *AugurDeps, decision *augur.Decision) (*structpb.Struct, error) {
	omen := decision.Omen
	switch omen.SourceType {
	case augur.SourceVault:
		// decision.Query — normalized logical path (passed through ParseRef in Resolve).
		return augur.BrokerVault(ctx, deps.Vault, decision.Query)
	case augur.SourcePrometheus:
		return augur.BrokerPrometheus(ctx, deps.Vault, deps.Egress, omen.Endpoint, omen.AuthRef, decision.Query)
	case augur.SourceELK:
		return augur.BrokerELK(ctx, deps.Vault, deps.Egress, omen.Endpoint, omen.AuthRef, decision.Query)
	default:
		// Resolve already filters out unknown source_type (denied); reaching here
		// would mean the switches are out of sync — fail-safe.
		return nil, fmt.Errorf("grpc: augur unsupported source_type %q", omen.SourceType)
	}
}

// sendAugurError — sends an AugurReply{ERROR} with a generic diagnostic.
func (h *eventStreamHandler) sendAugurError(ctx context.Context, sid, requestID, reason string) {
	h.sendAugurReply(ctx, sid, &keeperv1.AugurReply{
		RequestId: requestID,
		Status:    keeperv1.AugurStatus_AUGUR_STATUS_ERROR,
		Error:     reason,
	})
}

// sendAugurReply — sends the reply on the same stream via Outbound. A Send
// failure is logged as a warning (the stream may have closed); the Soul will
// either retry on its own wait timeout or fail the step — default-deny on
// that side protects it.
func (h *eventStreamHandler) sendAugurReply(ctx context.Context, sid string, reply *keeperv1.AugurReply) {
	if err := h.deps.Augur.Outbound.SendAugurReply(ctx, sid, reply); err != nil {
		h.logger.Warn("eventstream: augur reply send failed",
			slog.String("sid", sid),
			slog.String("request_id", reply.GetRequestId()),
			slog.Any("error", err),
		)
	}
}

// auditAugur writes an augur event. The secret value is NEVER included
// (augur.md §8): only omen + query + request_id + sid; reason is set only for
// denied. query is the vault logical path (a record address, not the secret
// value); augur.md §8 allows logging the path. MaskSecrets inside the Writer
// only catches literals with a `vault:` prefix — it does NOT mask a bare
// path, and that's fine: a path isn't a secret. Best-effort: an audit
// failure doesn't undo the already-sent reply (same pattern as the other
// event handlers).
func (h *eventStreamHandler) auditAugur(ctx context.Context, evt audit.EventType, sid, omen, query, requestID, applyID, reason string) {
	payload := map[string]any{
		"sid":        sid,
		"omen":       omen,
		"query":      query,
		"request_id": requestID,
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := h.deps.Augur.AuditWriter.Write(ctx, &audit.Event{
		EventType:     evt,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload:       payload,
		CreatedAt:     time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("eventstream: augur audit write failed",
			slog.String("sid", sid),
			slog.String("event", string(evt)),
			slog.Any("error", err),
		)
	}
}
