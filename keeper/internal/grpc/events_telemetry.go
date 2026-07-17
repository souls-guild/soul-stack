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

// selectIncarnationByCovensSQL - incarnations whose name (root Coven label,
// ADR-008) is present in the host's covens. ORDER BY name - determinism of the v1
// "first by name" policy.
const selectIncarnationByCovensSQL = `
SELECT name, service, service_version, spec
FROM incarnation
WHERE name = ANY($1)
ORDER BY name
`

// ResolveForSID resolves the host's effective telemetry config (ADR-072, NIM-87):
//
//	souls.SelectBySID -> covens/soulprint -> incarnation by covens (first by name)
//	  -> serviceRegistry.Resolve(inc.Service) (ref = inc.ServiceVersion)
//	  -> loader.Load -> art.Manifest.Telemetry + essence.Resolve(override)
//	  -> ResolveEffectiveTelemetry(merge+clamp).
//
// (nil, nil) - "no config": host not in the registry / no covens / no incarnation.
// broadcast is skipped, Soul stays on the soul-local cadence. Any resolve failure -
// (nil, err): broadcast swallows it as a warning, the stream stays alive.
func (s *telemetrySource) ResolveForSID(ctx context.Context, sid string) (*keeperv1.TelemetryConfig, error) {
	su, err := soul.SelectBySID(ctx, s.db, sid)
	if err != nil {
		if errors.Is(err, soul.ErrSoulNotFound) {
			return nil, nil // host not yet in the registry - no config
		}
		return nil, fmt.Errorf("telemetry: soul select %q: %w", sid, err)
	}

	inc, err := s.incarnationForCovens(ctx, su.Coven)
	if err != nil {
		return nil, err
	}
	if inc == nil {
		return nil, nil // no incarnation by covens - Soul stays soul-local
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
		Covens:          su.Coven,
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

// incarnationForCovens returns the first-by-name incarnation whose name is in the
// host's covens (v1 policy). Empty -> (nil, nil). >1 matches -> debug log
// (a signal to an operator: host member of several incarnations).
func (s *telemetrySource) incarnationForCovens(ctx context.Context, covens []string) (*incarnation.Incarnation, error) {
	if len(covens) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, selectIncarnationByCovensSQL, covens)
	if err != nil {
		return nil, fmt.Errorf("telemetry: incarnation-by-covens query: %w", err)
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
		return nil, fmt.Errorf("telemetry: incarnation-by-covens iter: %w", err)
	}

	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Name
		}
		s.logger.Debug("telemetry: SID matches several incarnations - taking the first by name (v1)",
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
