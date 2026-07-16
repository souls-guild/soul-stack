// Package topology resolves "which hosts participate in a scenario run":
// roster of incarnation hosts by Coven labels (ADR-008: `incarnation.name` —
// root Coven label) + last-reported soulprint facts from
// `souls.soulprint_facts` (migration 015, ADR-018).
//
// Read-only layer: SELECT-only. Soulprint writes are done by
// keeper/internal/grpc (SoulprintReport handler), roster/coven writes are done by
// keeper/internal/soul. topology consumes the result for scenario resolver
// (M2.x scenario-runner).
//
// Cross-incarnation isolation (ADR-008): resolver reads hosts strictly of one
// incarnation — souls where `incarnation.name` is present in `coven[]`.
// Other incarnations do not appear in the result.
package topology

import (
	"log/slog"
	"time"
)

// stalenessThreshold — threshold for "stale" soulprint. If
// `received_at < now - threshold`, resolver logs warning (ADR-018:
// "warn in OTel when skew > 10 min"). Stale facts DO NOT block the run —
// scenario operates on last-reported (PM-decision: last-reported + OTel warn).
const stalenessThreshold = 10 * time.Minute

// HostFacts — logical view of a run host: registry data from `souls`
// (SID, Coven, last-reported soulprint) + declared role (source — Choir
// Voice, fallback — `incarnation.spec.hosts[].role`; ADR-044 p.2, ADR-008,
// scenario/orchestration.md §4.1).
//
// Soulprint — deserialized JSONB `souls.soulprint_facts` (map, not typed:
// scenario resolver accesses arbitrary paths `soulprint.self.<path>`
// via CEL, typing — at proto SoulprintFacts layer, not here).
//
// Role — declared, NOT actual. Source by precedence (ADR-044 p.2): role
// of Voice from `incarnation_choir_voices` (Choir absorbed declared role) >
// `spec.hosts[].role` (fallback for hosts WITHOUT Voice and for bootstrap-create,
// wire-compatibility). Can be empty ("") for hosts outside declared-spec without
// Voice (ADR-008). Actual role — only probe + `where:` on scenario side,
// not here.
//
// Choirs — names of Choirs (ADR-044) where SID is a Voice (memberships from
// `incarnation_choir_voices`, 060_create_choirs.up.sql). Stable per-host fact for
// `where:` targeting by group (`X in soulprint.self.choirs`); projected into
// `soulprint.self.choirs` and `soulprint.hosts[].choirs` (S-T4, symmetry with Role).
// nil/empty — host does not belong to any Choir of the incarnation (or push run,
// where Choirs don't apply). Sorted lexicographically (determinism).
//
// CollectedAt — Soul-side timestamp of fact collection; ReceivedAt — Keeper-side
// timestamp of SoulprintReport arrival. Both zero (time.Time{}), if Soul
// has not yet sent SoulprintReport (freshly connected host).
//
// Status — legacy lifecycle snapshot of `souls.status` (NOT presence: authority
// is online — Redis SID-lease, ADR-006(a)). Used ONLY by SQL-presence
// fallback of resolver (lease==nil / Redis failure); in lease-aware path, presence
// is decided by lease, status is not read for filtering.
type HostFacts struct {
	SID   string
	Coven []string
	// Traits — operator-set key-value метки хоста (ADR-060): key → (scalar |
	// list). Registry-данные `souls.traits` (миграция 087); проецируются в
	// `soulprint.self.traits` / `soulprint.hosts[].traits` для таргетинга
	// `where:` (registry-проекция, как Coven). nil/пустой — нет меток.
	Traits      map[string]any
	Role        string
	Choirs      []string
	Status      string
	Soulprint   map[string]any
	CollectedAt time.Time
	ReceivedAt  time.Time
}

// stale reports whether the host's soulprint is stale relative to now.
// Zero ReceivedAt (Soul has not yet sent report) — NOT stale: host is fresh,
// facts simply haven't arrived yet, separate path, not reason for warn here.
func (h *HostFacts) stale(now time.Time) bool {
	if h.ReceivedAt.IsZero() {
		return false
	}
	return h.ReceivedAt.Before(now.Add(-stalenessThreshold))
}

// logAttrs — attributes for structured warning about stale soulprint.
func (h *HostFacts) logAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("sid", h.SID),
		slog.Time("received_at", h.ReceivedAt),
	}
}
