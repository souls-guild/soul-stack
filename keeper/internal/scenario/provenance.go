package scenario

import (
	"context"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Engine provenance (ADR-0076(l)): recording what a run was executed BY, next to
// what it executed. Two engines version independently — keeper renders, soul
// applies — and until now state answered "which definition produced this" but
// never "which engines did". Half a year later, for an incarnation sitting in a
// strange state, that second question is the one being asked.
//
// Facts only. The gates are already built and stay untouched: the declared
// keeper window on the render path (NIM-159) and the per-host capability set
// before dispatch (NIM-161). Nothing here rejects a run, and a provenance read
// that fails is dropped rather than escalated — an audit field must never cost
// an apply.

// runProvenance accumulates a single run's engine facts across its render
// passes. A staged run renders once per Passage, each pass resolving its own
// destinies and requiring its own capabilities, so the stamp is the union rather
// than the last pass's view.
//
// Not safe for concurrent use: it is owned by the run goroutine, which is also
// the only place that renders and the only place that commits state.
type runProvenance struct {
	keeperVersion string
	service       config.CompatEntity
	destiny       *destinyResolver
	caps          map[string]struct{}
}

// newRunProvenance starts accumulating for a run rendered by keeperVersion
// against the given service entity. destiny may be nil (a run with no destiny
// source resolves none).
func newRunProvenance(keeperVersion string, service config.CompatEntity, destiny *destinyResolver) *runProvenance {
	return &runProvenance{
		keeperVersion: keeperVersion,
		service:       service,
		destiny:       destiny,
		caps:          make(map[string]struct{}),
	}
}

// observeRequired folds one render pass's per-host requirement map into the
// run-level union. Called where the capability gate is evaluated, so the stamp
// records exactly the set the run was judged against.
func (p *runProvenance) observeRequired(required map[string][]string) {
	if p == nil {
		return
	}
	for _, caps := range required {
		for _, c := range caps {
			if c != "" {
				p.caps[c] = struct{}{}
			}
		}
	}
}

// stamp assembles the record written to incarnation/state_history on the
// successful state commit. Returns nil when there is nothing to record, so an
// engine-less path never overwrites a real earlier stamp with an empty object.
func (p *runProvenance) stamp() *incarnation.EngineCompat {
	if p == nil {
		return nil
	}
	entities := append([]config.CompatEntity{p.service}, p.destiny.compatEntities()...)
	window := config.IntersectKeeperWindows(entities)

	// Enforced iff there was both a window to compare and a version to compare
	// with — a version-less build renders unenforced by design (ADR-0076(e)) and
	// must not read afterwards as though it had been checked.
	_, comparable := config.NormalizeEngineVersion(p.keeperVersion)

	caps := make([]string, 0, len(p.caps))
	for c := range p.caps {
		caps = append(caps, c)
	}
	sort.Strings(caps)

	out := &incarnation.EngineCompat{
		KeeperVersion:    p.keeperVersion,
		KeeperWindow:     window,
		WindowEnforced:   window.Declared() && comparable,
		SoulCapabilities: caps,
	}
	if out.IsZero() {
		return nil
	}
	return out
}

// announcedSoulVersion reads the version a host announced on its current
// connection, for the apply_runs stamp (ADR-0076(l)). Best-effort by contract:
// no reader, a host that never announced, or a Redis failure all yield "" —
// the row simply records no soul version. Deliberately NOT fail-closed, unlike
// the capability gate that reads the same Hash: that one decides whether to
// apply, this one only describes what did.
func (r *Runner) announcedSoulVersion(ctx context.Context, sid string, log *slog.Logger) string {
	return readAnnouncedSoulVersion(ctx, r.soulVersion, sid, log)
}

func readAnnouncedSoulVersion(ctx context.Context, reader SoulVersionReader, sid string, log *slog.Logger) string {
	if reader == nil || sid == "" {
		return ""
	}
	v, err := reader.ReadSoulVersion(ctx, sid)
	if err != nil {
		if log != nil {
			log.Warn("scenario: reading the announced soul version for the provenance stamp failed - row recorded without it (ADR-0076(l))",
				slog.String("sid", sid), slog.Any("error", err))
		}
		return ""
	}
	return v
}
