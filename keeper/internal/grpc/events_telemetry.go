package grpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	grpclib "google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/essence"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
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
// a Service loader + an essence resolver. Wired up in the daemon: shared pool +
// d.serviceRegistry + d.serviceLoader + d.essenceResolver.
type telemetrySource struct {
	db       soul.ExecQueryRower
	resolver incarnation.ServiceResolver
	loader   serviceArtifactLoader
	essence  *essence.Resolver
	logger   *slog.Logger
}

// NewTelemetrySource assembles a [TelemetrySource]. db - shared pool (souls +
// incarnation + soulprint). resolver - service registry (git coordinates by name,
// d.serviceRegistry). loader - Service loader (d.serviceLoader). ess -
// essence resolver (d.essenceResolver). logger nil -> slog.Default().
//
// resolver is required in addition to the loader: [artifact.ServiceLoader.Load] requires
// a git URL in ServiceRef (empty Git is a hard error), while an incarnation only carries the
// service name + version - the URL is resolved by the registry (mirrors oracle_enqueuer /
// incarnation handlers).
func NewTelemetrySource(db soul.ExecQueryRower, resolver incarnation.ServiceResolver, loader serviceArtifactLoader, ess *essence.Resolver, logger *slog.Logger) TelemetrySource {
	if logger == nil {
		logger = slog.Default()
	}
	return &telemetrySource{db: db, resolver: resolver, loader: loader, essence: ess, logger: logger}
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
// Membership must NOT be answered from the label union of ADR-080: a host
// tagged with a string that happens to spell an incarnation's name would start
// receiving that incarnation's service config, which is not what the operator
// bound (ADR-030 amendment 2026-07-28).
const selectIncarnationsForSIDSQL = `
SELECT i.name, i.service, i.service_version, i.spec
FROM incarnation_membership m
JOIN incarnation i ON i.name = m.incarnation_name
WHERE m.sid = $1
ORDER BY i.name
`

// ResolveForSID resolves the host's effective telemetry config (ADR-072, NIM-87):
//
//	soul.EffectiveCovens -> incarnation by MEMBERSHIP (first by name)
//	  -> serviceRegistry.Resolve(inc.Service) (ref = inc.ServiceVersion)
//	  -> loader.Load -> art.Manifest.Telemetry + essence.Resolve(override)
//	  -> ResolveEffectiveTelemetry(merge+clamp).
//
// The two label questions are answered from different places on purpose:
// "which incarnation's service config is this host owed" is membership
// ([telemetrySource.incarnationForSID], the relation), while "which coven
// overlays of that service's essence apply to it" is the effective label set
// ([soul.EffectiveCovens], the ADR-080 union) - so a tag put on the incarnation
// reaches its members' essence, without a tag ever conjuring a membership.
//
// (nil, nil) - "no config": host not in the registry / in no incarnation.
// broadcast is skipped, Soul stays on the soul-local cadence. Any resolve failure -
// (nil, err): broadcast swallows it as a warning, the stream stays alive.
func (s *telemetrySource) ResolveForSID(ctx context.Context, sid string) (*keeperv1.TelemetryConfig, error) {
	covens, err := soul.EffectiveCovens(ctx, s.db, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil // host not yet in the registry - no config
		}
		return nil, fmt.Errorf("telemetry: effective covens %q: %w", sid, err)
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
		return nil, fmt.Errorf("telemetry: service %q of incarnation %q not registered", inc.Service, inc.Name)
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

	essenceMap, err := s.essence.Resolve(essence.ResolveInput{
		ServiceDir:      art.LocalDir,
		OSFamily:        s.osFamilyForSID(ctx, sid),
		Covens:          covens,
		IncarnationSpec: specEssence(inc),
	})
	if err != nil {
		return nil, fmt.Errorf("telemetry: essence resolve (%q): %w", inc.Name, err)
	}

	// An operator typo in essence-collectors (unknown names are silently
	// filtered out) - made visible in the logs, otherwise there is nothing to diagnose it with.
	if unknown := essence.UnknownTelemetryCollectors(essenceMap); len(unknown) > 0 {
		s.logger.Warn("telemetry: ignored unknown telemetry collectors in essence",
			slog.String("incarnation", inc.Name),
			slog.Any("unknown", unknown),
		)
	}

	// art.Manifest is guaranteed non-nil after a successful Load (otherwise Load
	// would have returned an error); Telemetry can be nil - ResolveEffectiveTelemetry
	// is nil-safe.
	return essence.ResolveEffectiveTelemetry(art.Manifest.Telemetry, essenceMap), nil
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
			inc       incarnation.Incarnation
			specBytes []byte
		)
		if err := rows.Scan(&inc.Name, &inc.Service, &inc.ServiceVersion, &specBytes); err != nil {
			return nil, fmt.Errorf("telemetry: scan incarnation: %w", err)
		}
		if len(specBytes) > 0 {
			if err := json.Unmarshal(specBytes, &inc.Spec); err != nil {
				return nil, fmt.Errorf("telemetry: unmarshal incarnation spec %q: %w", inc.Name, err)
			}
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
			names[i] = m.Name
		}
		s.logger.Warn("telemetry: host is a member of several incarnations - serving the first by name (v1)",
			slog.String("sid", sid),
			slog.String("chosen", matches[0].Name),
			slog.Any("incarnations", names))
	}
	return matches[0], nil
}

// osFamilyForSID is a best-effort extraction of soulprint.os.family for the essence os layer.
// A fresh host without soulprint (ErrSoulprintNotReceived) / any failure -> "" (the os layer
// is simply skipped, does not fail the resolve).
func (s *telemetrySource) osFamilyForSID(ctx context.Context, sid string) string {
	rec, err := soul.SelectSoulprint(ctx, s.db, sid)
	if err != nil {
		return ""
	}
	var facts map[string]any
	if err := json.Unmarshal(rec.FactsJSON, &facts); err != nil {
		return ""
	}
	return osFamilyOf(facts)
}

// osFamilyOf extracts soulprint.os.family from last-reported facts. A trivial
// duplicate of the scenario helper (signature over a map, not over *topology.HostFacts -
// exporting it just for 3 lines would be excessive).
func osFamilyOf(soulprint map[string]any) string {
	os, ok := soulprint["os"].(map[string]any)
	if !ok {
		return ""
	}
	family, _ := os["family"].(string)
	return family
}

// specEssence returns incarnation.spec.essence (the operator's override) or nil.
// A trivial duplicate of the scenario helper (exporting it just for 3 lines would be excessive).
func specEssence(inc *incarnation.Incarnation) map[string]any {
	if inc.Spec == nil {
		return nil
	}
	e, _ := inc.Spec["essence"].(map[string]any)
	return e
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
