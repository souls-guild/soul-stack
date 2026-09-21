package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	grpclib "google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/servicevars"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// serviceArtifactLoader is the narrow surface of the Service artifact loader
// (git snapshot materialization + manifest parse) needed by [telemetrySource].
// Implementation - *artifact.ServiceLoader; the interface keeps the source unit-fakeable.
type serviceArtifactLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
}

// telemetrySource is an implementation of [TelemetrySource] (ADR-072, NIM-87) over PG
// (souls + incarnation + soulprint) + a service registry (git coordinates) +
// a Service loader + a service-vars resolver. Wired up in the daemon: shared pool +
// d.serviceRegistry + d.serviceLoader + d.serviceVarsResolver.
type telemetrySource struct {
	db       soul.ExecQueryRower
	resolver incarnation.ServiceResolver
	loader   serviceArtifactLoader
	vars     *servicevars.Resolver
	logger   *slog.Logger
}

// NewTelemetrySource assembles a [TelemetrySource]. db - shared pool (souls +
// incarnation + soulprint). resolver - service registry (git coordinates by name,
// d.serviceRegistry). loader - Service loader (d.serviceLoader). vars -
// service-vars resolver (d.serviceVarsResolver). logger nil -> slog.Default().
//
// resolver is required in addition to the loader: [artifact.ServiceLoader.Load] requires
// a git URL in ServiceRef (empty Git is a hard error), while an incarnation only carries the
// service name + version - the URL is resolved by the registry (mirrors oracle_enqueuer /
// incarnation handlers).
func NewTelemetrySource(db soul.ExecQueryRower, resolver incarnation.ServiceResolver, loader serviceArtifactLoader, vars *servicevars.Resolver, logger *slog.Logger) TelemetrySource {
	if logger == nil {
		logger = slog.Default()
	}
	return &telemetrySource{db: db, resolver: resolver, loader: loader, vars: vars, logger: logger}
}

// selectIncarnationsForSIDSQL - the incarnations host $1 is BOUND to
// (`incarnation_membership`, migration 099). ORDER BY name - determinism of the
// v1 "first by name" policy below.
//
// ★ This used to read `FROM incarnation WHERE name = ANY(<host covens>)`, the
// derived fact NIM-124 retired: step (c) of migration 099 stripped incarnation
// names out of `souls.coven`, so the predicate stopped matching anything and
// every host fell through to the legal "no incarnation" branch. The effective
// telemetry config (ADR-072) then reached nobody, without one error in the logs
// (NIM-248). Delivery reads the relation now, the same way the reading half
// (`api/handlers/telemetry.go`) has since NIM-124.
//
// Membership must NOT be answered from `souls.coven[]`: a host tagged with a
// string that happens to spell an incarnation's name would start receiving that
// incarnation's service config, which is not what the operator bound (ADR-030
// amendment 2026-07-28). The reverse substitution is dead too — a bind attaches
// no label (NIM-281), so the relation is the only place this answer lives.
const selectIncarnationsForSIDSQL = `
SELECT i.id, i.service, i.service_version, i.covens, i.traits
FROM incarnation_membership m
JOIN incarnation i ON i.id = m.incarnation_name
WHERE m.sid = $1
ORDER BY i.id
`

// ResolveForSID resolves the host's effective telemetry config (ADR-072, NIM-87):
//
//	registry gate -> incarnation by MEMBERSHIP (first by name)
//	  -> serviceRegistry.Resolve(inc.Service) (ref = inc.ServiceVersion)
//	  -> loader.Load -> art.Manifest.Telemetry + servicevars.Resolve
//	  -> ResolveEffectiveTelemetry(merge+clamp).
//
// "Which incarnation's service config is this host owed" is MEMBERSHIP
// ([telemetrySource.incarnationForSID], the relation) and must never be answered
// from the label union, which deliberately admits a host-attached tag spelled
// like an incarnation's name. The second question this used to ask - "which
// coven overlays of that service apply to it" - no longer exists: service vars
// are host-invariant (ADR-0082). A service that needs a label-dependent value
// declares the step in `vars/_stack.yaml` (NIM-413).
//
// (nil, nil) - "no config": host not in the registry / in no incarnation.
// broadcast is skipped, Soul stays on the soul-local cadence. Any resolve failure -
// (nil, err): broadcast swallows it as a warning, the stream stays alive.
func (s *telemetrySource) ResolveForSID(ctx context.Context, sid string) (*keeperv1.TelemetryConfig, error) {
	// Registry gate: a host that is not in `souls` at all must be told nothing
	// rather than resolved against somebody's incarnation. A plain existence
	// lookup — the label query this used to call answered a question nothing on
	// this path asks any more, and its failure would have failed the telemetry
	// resolve for no reason.
	if _, err := soul.SelectBySID(ctx, s.db, sid); err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil // host not yet in the registry - no config
		}
		return nil, fmt.Errorf("telemetry: registry lookup %q: %w", sid, err)
	}

	inc, err := s.incarnationForSID(ctx, sid)
	if err != nil {
		return nil, err
	}
	if inc == nil {
		return nil, nil // host in no incarnation - Soul stays soul-local
	}

	ref, ok := s.resolver.Resolve(inc.Service)
	if !ok {
		return nil, fmt.Errorf("telemetry: service %q of incarnation %q not registered", inc.Service, inc.ID)
	}
	if inc.ServiceVersion != "" {
		// Roll out with the deployed service version, not the branch tip (mirrors
		// oracle_enqueuer.go / incarnation-handlers).
		ref.Ref = inc.ServiceVersion
	}

	art, err := s.loader.Load(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("telemetry: load service %q@%q: %w", ref.Name, ref.Ref, err)
	}

	serviceVars, err := s.vars.Resolve(servicevars.ResolveInput{
		ServiceDir: art.LocalDir,
		Incarnation: servicevars.IncarnationContext{
			ID:             inc.ID,
			Service:        inc.Service,
			ServiceVersion: inc.ServiceVersion,
			Covens:         inc.Covens,
			Traits:         inc.Traits,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: service-vars resolve (%q): %w", inc.ID, err)
	}

	// An operator typo in collectors named in the service vars (unknown names are silently
	// filtered out) - made visible in the logs, otherwise there is nothing to diagnose it with.
	if unknown := servicevars.UnknownTelemetryCollectors(serviceVars); len(unknown) > 0 {
		s.logger.Warn("telemetry: ignored unknown telemetry collectors in the service vars",
			slog.String("incarnation", inc.ID),
			slog.Any("unknown", unknown),
		)
	}

	// art.Manifest is guaranteed non-nil after a successful Load (otherwise Load
	// would have returned an error); Telemetry can be nil - ResolveEffectiveTelemetry
	// is nil-safe.
	return servicevars.ResolveEffectiveTelemetry(art.Manifest.Telemetry, serviceVars), nil
}

// incarnationForSID returns the first-by-name incarnation the host is a member
// of (v1 policy). No membership -> (nil, nil).
//
// A host may legitimately belong to several incarnations - membership is M:N
// since NIM-124 - and a single stream can carry only one telemetry cadence, so
// one of them has to win. "First by name" is kept because it is stable across
// reconnects and across Keeper instances; what changes here is that the operator
// is told. The ambiguity is logged at WARN, not DEBUG: the answer is arbitrary
// among equals, and a host quietly running a co-member's cadence is exactly the
// kind of thing nobody thinks to look for at debug level. Making the choice
// explicit (an incarnation flag, or per-incarnation cadences) is a design
// question rather than a bug fix - NIM-279.
func (s *telemetrySource) incarnationForSID(ctx context.Context, sid string) (*incarnation.Incarnation, error) {
	if sid == "" {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, selectIncarnationsForSIDSQL, sid)
	if err != nil {
		return nil, fmt.Errorf("telemetry: incarnations-for-sid query: %w", err)
	}
	defer rows.Close()

	var matches []*incarnation.Incarnation
	for rows.Next() {
		var (
			inc         incarnation.Incarnation
			traitsBytes []byte
		)
		if err := rows.Scan(&inc.ID, &inc.Service, &inc.ServiceVersion, &inc.Covens, &traitsBytes); err != nil {
			return nil, fmt.Errorf("telemetry: scan incarnation: %w", err)
		}
		if len(traitsBytes) > 0 {
			if err := json.Unmarshal(traitsBytes, &inc.Traits); err != nil {
				return nil, fmt.Errorf("telemetry: unmarshal incarnation traits %q: %w", inc.ID, err)
			}
			inc.TraitsRaw = traitsBytes // scope reads the raw jsonb, never the map (NIM-521)
		}
		incCopy := inc
		matches = append(matches, &incCopy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry: incarnations-for-sid iter: %w", err)
	}

	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.ID
		}
		s.logger.Warn("telemetry: host is a member of several incarnations - serving the first by name (v1)",
			slog.String("sid", sid),
			slog.String("chosen", matches[0].ID),
			slog.Any("incarnations", names))
	}
	return matches[0], nil
}

// broadcastTelemetryConfig hands the Soul its effective host-vitals telemetry config
// in a single [keeperv1.FromKeeper_TelemetryConfig] (ADR-072, NIM-87). Called from
// [EventStream] in the same goroutine after [broadcastVigils] and before the send-loop starts
// - the send goes directly via stream.Send (order is guaranteed, the buffer is not used).
//
// Unlike the snapshot broadcasts (Sigil/Vigil, ReplaceAll even with an empty
// set), "no config" (a host without an incarnation) is NOT an empty config but a
// non-send: Soul keeps its soul-local cadence. So (nil, nil) from ResolveForSID
// -> a silent skip.
//
// Best-effort:
//   - TelemetrySource=nil -> no-op (dev/unit/push wiring);
//   - ResolveForSID returned an error -> warn, skip, stream stays alive;
//   - (nil, nil) -> silent skip (no config);
//   - stream.Send failed -> warn (the stream is already broken, receive-loop will hit EOF).
func (h *eventStreamHandler) broadcastTelemetryConfig(
	ctx context.Context,
	stream grpclib.BidiStreamingServer[keeperv1.FromSoul, keeperv1.FromKeeper],
	sid, sessionID string,
) {
	if h.deps.TelemetrySource == nil {
		return
	}
	cfg, err := h.deps.TelemetrySource.ResolveForSID(ctx, sid)
	if err != nil {
		h.logger.Warn("eventstream: telemetry config resolve failed — skipping",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	if cfg == nil {
		h.logger.Debug("eventstream: no telemetry config for sid — skip (soul-local cadence)",
			slog.String("sid", sid), slog.String("session_id", sessionID))
		return
	}
	msg := &keeperv1.FromKeeper{
		Payload: &keeperv1.FromKeeper_TelemetryConfig{TelemetryConfig: cfg},
	}
	if err := stream.Send(msg); err != nil {
		h.logger.Warn("eventstream: telemetry config send failed",
			slog.String("sid", sid),
			slog.String("session_id", sessionID),
			slog.Any("error", err),
		)
		return
	}
	h.deps.Metrics.ObserveMessage(directionToSoul)
	h.logger.Debug("eventstream: telemetry config sent",
		slog.String("sid", sid),
		slog.String("session_id", sessionID),
		slog.Bool("enabled", cfg.GetEnabled()),
		slog.Int("interval_sec", int(cfg.GetIntervalSec())),
		slog.Any("collectors", cfg.GetCollectors()),
	)
}
