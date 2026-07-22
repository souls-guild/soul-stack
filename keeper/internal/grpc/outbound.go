package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	keeperredis "github.com/souls-guild/soul-stack/keeper/internal/redis"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// tracer for in-process spans of the EventStream subsystem. Uses the global
// TracerProvider set up by [obs.SetupOTel] in main; when OTel is disabled the
// provider is no-op — spans are free and the code doesn't need to branch (ADR-024 §1.2).
var tracer = otel.Tracer("keeper/grpc")

// Outbound — public Keeper-side API for sending messages into the EventStream
// (Keeper → Soul, M2.5 + cluster-mode pub/sub routing).
//
// The implementation delegates enqueue to [StreamManager] and writes an audit
// event for diagnostic observability (`apply.dispatched` / `apply.cancelled`;
// seed rotation goes through [eventstream_seedrotation.go] separately, since
// issuing a seed is a keeper-internal operation, not just a send).
//
// Cluster-mode routing: if the local StreamManager doesn't hold the SID,
// Outbound checks the Redis lease holder (`soul:<sid>:lock`). holder !=
// self → publish FromKeeper to the Redis pub/sub channel
// `outbound:<sid>`; the holder Keeper receives it via subscription and
// forwards it into its own stream. holder == "" → ErrSoulNotConnected.
// holder == self with no local stream → lease inconsistency (the stream
// closed but the lease hasn't been released yet) — log + ErrSoulNotConnected.
//
// With nil Redis (single-instance / dev build) routing degrades to
// per-instance: nil lookup → immediate ErrSoulNotConnected.
//
// Callers:
//   - scenario-runner (post-M2.5) — `SendApply` after Keeper-side destiny
//     rendering;
//   - Operator API / admin flow — `SendCancel` on an active apply_id;
//   - seedrotation-handler within the package — `SendSeedRotationReply` after
//     successfully issuing a new cert.
type Outbound struct {
	manager     *StreamManager
	auditWriter audit.Writer
	logger      *slog.Logger

	redis   *keeperredis.Client
	kid     string
	metrics *GRPCMetrics
}

// OutboundDeps — constructor parameters for [NewOutbound]. Manager /
// AuditWriter / Logger are required; Redis + KID are for cluster-mode
// routing (nil Redis is allowed, single-instance fallback).
type OutboundDeps struct {
	Manager     *StreamManager
	AuditWriter audit.Writer
	Logger      *slog.Logger

	// Redis — client for checking the SoulLease holder and publishing to
	// the pub/sub channel. nil → cluster-mode routing is disabled (lookup
	// on the current Keeper is the only path).
	Redis *keeperredis.Client
	// KID — Keeper instance identifier. Required if Redis != nil (without
	// it, self-filtering and determining "is this our lease" are
	// impossible). An empty string is fine when Redis is nil.
	KID string

	// Metrics — keeper_grpc_* collectors (ADR-024). nil → dispatch metrics
	// are disabled ([GRPCMetrics] methods are nil-safe no-ops). Should be
	// the same descriptor as [EventStreamDeps.Metrics] (one Registry).
	Metrics *GRPCMetrics
}

// NewOutbound assembles an Outbound over a registered [StreamManager].
//
// auditWriter is required — Keeper-side operations (apply-dispatch, cancel)
// are recorded as facts in `audit_log`. logger is required. Redis +
// KID are optional (nil → cluster routing is disabled).
func NewOutbound(deps OutboundDeps) (*Outbound, error) {
	if deps.Manager == nil {
		return nil, errors.New("grpc: Outbound manager is required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("grpc: Outbound auditWriter is required")
	}
	if deps.Logger == nil {
		return nil, errors.New("grpc: Outbound logger is required")
	}
	if deps.Redis != nil && deps.KID == "" {
		return nil, errors.New("grpc: Outbound KID required when Redis is set")
	}
	return &Outbound{
		manager:     deps.Manager,
		auditWriter: deps.AuditWriter,
		logger:      deps.Logger,
		redis:       deps.Redis,
		kid:         deps.KID,
		metrics:     deps.Metrics,
	}, nil
}

// SendApply puts an `ApplyRequest` on the target SID's outbound channel.
//
// PM-decision M2.5(2): thread-safe lookup via RWMutex, doesn't block on
// recv (per-entry chan has a buffer sized [outboundBufferSize]).
// PM-decision M2.5(1): on a full buffer → drop+log → [ErrOutboundQueueFull].
//
// The `apply.dispatched` audit event is written ONLY on successful
// enqueue/publish. On failure, the caller (scenario-runner) decides what to
// write as `run.completed`.
func (o *Outbound) SendApply(ctx context.Context, sid string, req *keeperv1.ApplyRequest) error {
	if req == nil {
		return errors.New("grpc: ApplyRequest is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ApplyRequest{ApplyRequest: req},
	}

	// In-process span per dispatch unit (not for the whole long-lived stream).
	// sid / apply_id are attributes for trace filtering (can't go in
	// metric labels — cardinality, ADR-024 §2.2); no secrets involved. With
	// OTel disabled the tracer is a no-op — Start/End are free.
	ctx, span := tracer.Start(ctx, "grpc.apply_dispatch",
		trace.WithAttributes(
			attribute.String("sid", sid),
			attribute.String("apply_id", req.GetApplyId()),
		),
	)
	defer span.End()

	// Inject the grpc.apply_dispatch span's trace context into ApplyRequest so
	// Soul raises apply.run as its child (end-to-end trace operator → Keeper →
	// Soul, ADR-024). Propagator is the global composite TraceContext from
	// obs.SetupOTel. req is mutated intentionally: it's built per-dispatch with
	// no shared owner. In cluster-mode req is serialized to Redis as-is
	// (deliver → PublishOutbound), so trace_context travels inside the
	// protobuf bytes automatically — no extra handling needed on the pub/sub path.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	req.TraceContext = carrier["traceparent"]

	err := o.deliver(ctx, sid, msg, "apply", req.GetApplyId())
	o.metrics.ObserveApplyDispatch(err)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "apply dispatch failed")
		return err
	}

	if err := o.auditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyDispatched,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: req.GetApplyId(),
		Payload: map[string]any{
			"sid":         sid,
			"apply_id":    req.GetApplyId(),
			"tasks_count": len(req.GetTasks()),
		},
	}); err != nil {
		// The message is already delivered — an audit failure doesn't undo
		// the dispatch (same pattern as the Bootstrap handler).
		o.logger.Warn("outbound: audit apply.dispatched failed (message enqueued)",
			slog.String("sid", sid),
			slog.String("apply_id", req.GetApplyId()),
			slog.Any("error", err),
		)
	}
	return nil
}

// SendErrand puts an `ErrandRequest` on the target SID's outbound channel.
//
// LOCAL-ONLY variant: the caller (`errand.Dispatcher`) has already decided
// on the lease holder (`ReadSoulLeaseHolder`) and calls SendErrand only when
// holder == self. So the cluster pub/sub fallback here is the same as
// [SendApply] (in case of a lease-change race between ReadHolder and Send):
// if there's no local stream, deliver forwards to Redis itself. The caller
// gets [ErrSoulNotConnected] on total failure.
//
// The audit event is written by the dispatcher (`errand.invoked`/`completed`/…)
// — this is purely a "pipe" function, like [SendSeedRotationReply] /
// [SendAugurReply].
func (o *Outbound) SendErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error {
	if req == nil {
		return errors.New("grpc: ErrandRequest is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ErrandRequest{ErrandRequest: req},
	}
	return o.deliver(ctx, sid, msg, "errand", req.GetErrandId())
}

// PublishErrand — REMOTE-ONLY path for errand dispatch (cross-keeper).
// Used by `errand.Dispatcher` when the lease holder is NOT our KID:
// publishes straight to `outbound:<sid>`, and the holder Keeper forwards it
// into its own stream.
//
// A separate function from SendErrand: so the Dispatcher can explicitly
// choose the remote path without the "try local → fallback to Redis"
// round-trip (that's what deliver does, but the dispatcher wants it
// shorter — a local stream shouldn't exist here per lease semantics). On a
// publish error / 0 subscribers, returns [ErrSoulNotConnected].
func (o *Outbound) PublishErrand(ctx context.Context, sid string, req *keeperv1.ErrandRequest) error {
	if req == nil {
		return errors.New("grpc: ErrandRequest is nil")
	}
	if o.redis == nil {
		return fmt.Errorf("%w: %s (redis not configured)", ErrSoulNotConnected, sid)
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_ErrandRequest{ErrandRequest: req},
	}
	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: errand pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", req.GetErrandId()),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		o.logger.Warn("outbound: errand pub/sub reached zero subscribers",
			slog.String("sid", sid),
			slog.String("errand_id", req.GetErrandId()),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	return nil
}

// SendCancelErrand puts a `CancelErrand` on the target SID's outbound
// channel (ADR-033, slice E5).
//
// LOCAL-ONLY variant: symmetric to [SendErrand], the caller
// (`errand.Dispatcher`) has already decided on the lease holder — deliver
// forwards to Redis pub/sub itself in case of a lease-change race. On total
// failure — [ErrSoulNotConnected].
//
// The audit event is written by the dispatcher (`errand.cancelled` — a
// separate event type, with the initiating AID); this is a "pipe" function
// with no audit of its own, same pattern as [SendErrand].
func (o *Outbound) SendCancelErrand(ctx context.Context, sid, errandID string) error {
	if errandID == "" {
		return errors.New("grpc: CancelErrand errandID is empty")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelErrand{CancelErrand: &keeperv1.CancelErrand{
			ErrandId: errandID,
		}},
	}
	return o.deliver(ctx, sid, msg, "errand-cancel", errandID)
}

// PublishCancelErrand — REMOTE-ONLY path for cancel (cross-keeper, ADR-033
// slice E5). Used by `errand.Dispatcher` when the lease holder is NOT our
// KID: publishes straight to `outbound:<sid>`, and the holder Keeper
// forwards it into its own stream.
//
// Same pattern as [PublishErrand]: 0 subscribers → [ErrSoulNotConnected].
func (o *Outbound) PublishCancelErrand(ctx context.Context, sid, errandID string) error {
	if errandID == "" {
		return errors.New("grpc: CancelErrand errandID is empty")
	}
	if o.redis == nil {
		return fmt.Errorf("%w: %s (redis not configured)", ErrSoulNotConnected, sid)
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelErrand{CancelErrand: &keeperv1.CancelErrand{
			ErrandId: errandID,
		}},
	}
	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: errand-cancel pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		o.logger.Warn("outbound: errand-cancel pub/sub reached zero subscribers",
			slog.String("sid", sid),
			slog.String("errand_id", errandID),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	return nil
}

// SendCancel puts a `CancelApply` on the outbound channel.
//
// PM-decision M2.5(3): best-effort signal — Keeper doesn't track the
// active apply_id on the Soul side. The Soul-side ApplyRunner checks
// ctx.Done()/the cancel channel and sends back a `RunResult` with
// `status: CANCELLED`.
func (o *Outbound) SendCancel(ctx context.Context, sid string, applyID, reason string) error {
	if applyID == "" {
		return errors.New("grpc: CancelApply applyID is empty")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_CancelApply{CancelApply: &keeperv1.CancelApply{
			ApplyId: applyID,
			Reason:  reason,
		}},
	}
	if err := o.deliver(ctx, sid, msg, "cancel", applyID); err != nil {
		return err
	}

	if err := o.auditWriter.Write(ctx, &audit.Event{
		EventType:     audit.EventApplyCancelled,
		Source:        audit.SourceSoulGRPC,
		CorrelationID: applyID,
		Payload: map[string]any{
			"sid":      sid,
			"apply_id": applyID,
			"reason":   reason,
		},
	}); err != nil {
		o.logger.Warn("outbound: audit apply.cancelled failed (message enqueued)",
			slog.String("sid", sid),
			slog.String("apply_id", applyID),
			slog.Any("error", err),
		)
	}
	return nil
}

// SendSeedRotationReply puts a `SeedRotationReply` on the outbound channel.
//
// The audit event for seed rotation is written separately in
// [handleSeedRotationRequest] (`soul.seed-rotated`) — Outbound itself
// doesn't write audit for this channel, to avoid duplicating correlation.
// A pure "pipe" function.
func (o *Outbound) SendSeedRotationReply(ctx context.Context, sid string, reply *keeperv1.SeedRotationReply) error {
	if reply == nil {
		return errors.New("grpc: SeedRotationReply is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SeedRotationReply{SeedRotationReply: reply},
	}
	return o.deliver(ctx, sid, msg, "seed-rotation-reply", "")
}

// SendAugurReply puts an `AugurReply` on the target SID's outbound channel
// (ADR-025, augur.md §5). Purely a "pipe" function — the audit
// (`augur.fetch_brokered` / `augur.access_denied`) is written by the augur
// handler itself (see [handleAugurRequest]), because the allow/deny
// decision and the fact of the read are known there, not at the send
// level. Same pattern as [SendSeedRotationReply].
func (o *Outbound) SendAugurReply(ctx context.Context, sid string, reply *keeperv1.AugurReply) error {
	if reply == nil {
		return errors.New("grpc: AugurReply is nil")
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_AugurReply{AugurReply: reply},
	}
	return o.deliver(ctx, sid, msg, "augur-reply", reply.GetRequestId())
}

// SendSigilSnapshot puts a `SigilSnapshot` (the full active set) on the
// target SID's outbound channel (ADR-026(h), Option A).
//
// The snapshot is the sole authoritative source of the active set on the
// Soul: it's applied as ReplaceAll, and a grant missing from the set is
// forgotten by the Soul (near-instant revoke, S6c). Purely a "pipe"
// function — no audit is written (grant/revoke are already recorded in
// `audit_log` under `plugin.allow`/`plugin.revoke`; distribution is just
// state replication, same pattern as [SendSeedRotationReply]).
//
// nil sigils are allowed (an empty snapshot = "no plugin is granted" — a
// valid state that needs to reach the Soul so it clears its old set).
func (o *Outbound) SendSigilSnapshot(ctx context.Context, sid string, sigils []*keeperv1.PluginSigil) error {
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilSnapshot{
			SigilSnapshot: &keeperv1.SigilSnapshot{Sigils: sigils},
		},
	}
	return o.deliver(ctx, sid, msg, "sigil-snapshot", "")
}

// RebroadcastSigils distributes the given active set of trust seals to
// every Soul whose EventStream is held LOCALLY on this Keeper instance
// (ADR-026(h), S6c), as ONE [keeperv1.SigilSnapshot] per stream (ReplaceAll).
//
// Called on a cluster-wide invalidate signal (`sigil:invalidate`): after an
// allow/revoke on any node, every node re-broadcasts the fresh set to its
// connected Souls. This is how near-instant revoke works: the Soul forgets
// the revoked grant via ReplaceAll without waiting for a reconnect.
//
// Local streams only (StreamManager.SIDs): cluster fanout is handled by
// pub/sub itself — each node distributes to ITS OWN Souls. Cluster-mode
// pub/sub forwarding via `outbound:<sid>` is NOT used here (distributing to
// other nodes' Souls is that node's own job, driven by the same invalidate
// signal).
//
// Resilient to per-stream failures: dropping one Soul (full buffer / closed
// stream) is logged and does NOT interrupt distribution to the rest — that
// Soul will pick up a fresh connect-time snapshot on its next reconnect.
// Returns the number of Souls the snapshot was delivered to successfully
// (for the caller's metric/log).
//
// sigils is the full active set; an empty set is also sent as a fact (a
// snapshot with empty sigils[] means no grant is active → the Soul clears
// its cache, S6c).
func (o *Outbound) RebroadcastSigils(ctx context.Context, sigils []*keeperv1.PluginSigil) int {
	sids := o.manager.SIDs()
	delivered := 0
	for _, sid := range sids {
		if err := o.SendSigilSnapshot(ctx, sid, sigils); err != nil {
			o.logger.Warn("outbound: sigil snapshot re-broadcast to soul failed — skipping",
				slog.String("sid", sid),
				slog.Any("error", err),
			)
			continue
		}
		delivered++
	}
	o.logger.Debug("outbound: sigil snapshot re-broadcast complete",
		slog.Int("streams", len(sids)),
		slog.Int("delivered", delivered),
		slog.Int("sigils", len(sigils)),
	)
	return delivered
}

// SendSigilTrustAnchors puts `SigilTrustAnchors` (the full set of Sigil
// signature trust anchors) on the target SID's outbound channel
// (ADR-026(h), R3-S6).
//
// ReplaceAll semantics: the set is the sole authoritative source of anchors
// on the Soul, applied as a full replacement of the holder
// ([pluginhost.AnchorSet.SetAnchors]). An anchor outside the new set is
// "forgotten" by the Soul (the retired key stops verifying). Purely a
// "pipe" function — no audit is written (key rotation is recorded in
// `audit_log` by the S7 rotation handler; distribution is just set
// replication, same pattern as [SendSigilSnapshot]).
//
// A nil/empty pubkeyPEM is allowed (an empty set = "Sigil disabled / no
// anchors" — a valid state; the Soul's no_trust_anchor fail-closed behavior
// protects it). Best-effort distribution across local streams is in
// [RebroadcastTrustAnchors].
func (o *Outbound) SendSigilTrustAnchors(ctx context.Context, sid string, pubkeyPEM []string) error {
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_SigilTrustAnchors{
			SigilTrustAnchors: &keeperv1.SigilTrustAnchors{PubkeyPem: pubkeyPEM},
		},
	}
	return o.deliver(ctx, sid, msg, "sigil-trust-anchors", "")
}

// RebroadcastTrustAnchors distributes the given set of trust anchors to
// every Soul whose EventStream is held LOCALLY on this Keeper instance
// (ADR-026(h), R3-S6), as one [keeperv1.SigilTrustAnchors] per stream
// (ReplaceAll).
//
// Called on the cluster-wide `sigil:anchors-changed` signal (after a
// signing-key rotation on any node, every node re-broadcasts the fresh set
// to its Souls) and by the "keeper Signer hot-reload" daemon watcher.
// Cluster fanout is handled by pub/sub itself — each node distributes to
// ITS OWN Souls and leaves others alone (cluster-mode pub/sub forwarding
// via `outbound:<sid>` is NOT used here — same pattern as
// [RebroadcastSigils]).
//
// Resilient to per-stream failures: dropping one Soul (full buffer / closed
// stream) is logged and does NOT interrupt distribution to the rest — that
// Soul will pick up a fresh connect-time set on its next reconnect. Returns
// the number of Souls the set was delivered to successfully (for the
// caller's metric/log).
//
// pubkeyPEM is the full set of anchors; an empty set is also sent as a fact
// (empty = "Sigil disabled / no anchors" → the Soul clears its holder,
// fail-closed verify).
func (o *Outbound) RebroadcastTrustAnchors(ctx context.Context, pubkeyPEM []string) int {
	sids := o.manager.SIDs()
	delivered := 0
	for _, sid := range sids {
		if err := o.SendSigilTrustAnchors(ctx, sid, pubkeyPEM); err != nil {
			o.logger.Warn("outbound: sigil trust-anchors re-broadcast to soul failed — skipping",
				slog.String("sid", sid),
				slog.Any("error", err),
			)
			continue
		}
		delivered++
	}
	o.logger.Debug("outbound: sigil trust-anchors re-broadcast complete",
		slog.Int("streams", len(sids)),
		slog.Int("delivered", delivered),
		slog.Int("anchors", len(pubkeyPEM)),
	)
	return delivered
}

// deliver — the common logic for delivering FromKeeper across three paths:
//
//  1. The local StreamManager holds the SID → enqueue (full buffer →
//     ErrOutboundQueueFull).
//  2. Cluster-mode: Redis lease holder != self → publish to the pub/sub
//     channel `outbound:<sid>`. Subscribers=0 → log + ErrSoulNotConnected
//     (nobody's listening — the Soul will reconnect).
//  3. Cluster-mode inconsistency: holder == self but there's no local
//     stream → log warn + ErrSoulNotConnected (the lease hasn't been
//     released yet after a disconnect).
//  4. Nobody holds it → ErrSoulNotConnected.
//
// kind / applyID are for logs and diagnostics (apply_id can be empty for
// seed-rotation-reply).
func (o *Outbound) deliver(ctx context.Context, sid string, msg *keeperv1.FromKeeper, kind, applyID string) error {
	if entry := o.manager.lookup(sid); entry != nil {
		if !entry.send(msg) {
			o.logger.Warn("outbound: deliver dropped (queue full or closed)",
				slog.String("sid", sid),
				slog.String("kind", kind),
				slog.String("apply_id", applyID),
			)
			return fmt.Errorf("%w: %s", ErrOutboundQueueFull, sid)
		}
		return nil
	}

	if o.redis == nil {
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}

	holder, err := keeperredis.ReadSoulLeaseHolder(ctx, o.redis, sid)
	if err != nil {
		o.logger.Warn("outbound: lease holder lookup failed",
			slog.String("sid", sid),
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if holder == "" {
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if holder == o.kid {
		// The lease is ours, but there's no local stream — a disconnect
		// happened between Recv and lease.Release. The caller gets
		// NotConnected; on the next reconnect the Soul will take the lease
		// either from us again or from another Keeper.
		o.logger.Warn("outbound: local lease without active stream",
			slog.String("sid", sid),
			slog.String("kid", o.kid),
			slog.String("kind", kind),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}

	n, err := keeperredis.PublishOutbound(ctx, o.redis, sid, o.kid, msg)
	if err != nil {
		o.logger.Warn("outbound: pub/sub publish failed",
			slog.String("sid", sid),
			slog.String("holder_kid", holder),
			slog.String("kind", kind),
			slog.Any("error", err),
		)
		return fmt.Errorf("%w: %s", ErrSoulNotConnected, sid)
	}
	if n == 0 {
		// A holder exists in the lease but isn't subscribed to the channel —
		// a race with Unregister on that side (the stream closed, the lease
		// is still alive for TTL/3..TTL seconds). Same semantics as
		// NotConnected.
		o.logger.Warn("outbound: pub/sub publish reached zero subscribers",
			slog.String("sid", sid),
			slog.String("holder_kid", holder),
			slog.String("kind", kind),
		)
		return fmt.Errorf("%w: %s (no subscribers on outbound channel)", ErrSoulNotConnected, sid)
	}
	o.logger.Debug("outbound: forwarded via pub/sub",
		slog.String("sid", sid),
		slog.String("holder_kid", holder),
		slog.String("kind", kind),
		slog.Int64("subscribers", n),
	)
	return nil
}
