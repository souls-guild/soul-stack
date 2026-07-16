package topology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier — narrow subset of pgxpool.Pool needed by resolver (read-only).
// Symmetric to [soul.ExecQueryRower] / [incarnation.ExecQueryRower]: unit tests
// use fake without spinning up PG, production uses real pool/Conn/Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	_ Querier = (*pgx.Conn)(nil)
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// SoulLeaseChecker — narrow surface for batch checking "is Redis SID-lease alive"
// (live EventStream), needed by resolver's presence phase (Variant A, ADR-006(a)).
// Narrowing to one method isolates topology package from full keeperredis.Client
// and allows fake in unit tests. Real implementation — wrapper over
// [keeperredis.SoulsStreamAlive], assembled in cmd/keeper (see daemon.setupScenarioDeps).
//
// Returns set of SIDs with live lease (presence=online). nil-checker
// (unit tests / single-instance dev without Redis) → resolver degrades to
// SQL-presence (status='connected'), symmetric to reaper.
type SoulLeaseChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// Resolver resolves the roster of incarnation hosts and their last-reported soulprint.
//
// pool — read-only access to Postgres (`souls` + `incarnation`). lease —
// Redis check of live SID-lease (presence source, ADR-006(a)); nil →
// SQL-presence fallback. logger — for warning about stale soulprint (ADR-018,
// does not block run) and fail-safe degradation on Redis failure.
type Resolver struct {
	pool   Querier
	lease  SoulLeaseChecker
	logger *slog.Logger
}

// NewResolver constructs Resolver. pool is required; lease is optional (nil →
// SQL-presence fallback, see [Resolver]); logger can be nil (warnings are then
// suppressed).
func NewResolver(pool *pgxpool.Pool, lease SoulLeaseChecker, logger *slog.Logger) *Resolver {
	return &Resolver{pool: pool, lease: lease, logger: logger}
}

// rosterSQL — phase 1 (SQL): targeting candidates — souls where
// `incarnation.name` is present in `coven[]` (ADR-008: root Coven label) and
// whose status is NOT terminal/onboarding.
//
// Presence (online/offline) is NOT filtered here: authority for "Soul online" —
// live Redis SID-lease, checked by phase 2 ([Resolver.filterAlive]). Status
// in `souls` carries only lifecycle snapshot; candidates are cut only by terminal
// (`revoked`/`expired`/`destroyed`) and onboarding (`pending`) — targeting them is impossible
// regardless of lease. `connected`/`disconnected` (legacy presence snapshot for
// Operator API) are NOT in the filter — presence is decided by lease.
//
// ORDER BY sid — deterministic order (scenario/orchestration.md §:
// lexicographically by SID; otherwise destructive operations are not reproducible).
const rosterSQL = `
SELECT sid, coven, traits, status,
       soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE $1 = ANY(coven)
  AND status NOT IN ('pending', 'revoked', 'expired', 'destroyed')
ORDER BY sid ASC
`

// incarnationSpecSQL reads spec of one incarnation to extract declared roles
// (`spec.hosts[].role`). Cross-incarnation isolation: exactly one row by PK.
const incarnationSpecSQL = `
SELECT spec
FROM incarnation
WHERE name = $1
`

// choirVoicesSQL reads Choir memberships of all hosts of one incarnation in one
// query (ADR-044, S-T4/S-T6): SID → names of Choirs where it is a Voice, + role
// of Voice in each Choir. Cross-incarnation isolation — filter by
// `incarnation_name` (PK includes it, ADR-044 section 3). One round-trip per
// roster (symmetric to loadDeclaredRoles, no N+1); join by
// `incarnation_choir_voices_sid_idx` (060_create_choirs.up.sql). ORDER BY choir_name —
// deterministic order of names inside `choirs[]` of each host and
// deterministic role selection on multi-choir conflict (ADR-044 p.2:
// absorption of declared role by Choir, see loadChoirMemberships).
const choirVoicesSQL = `
SELECT sid, choir_name, role
FROM incarnation_choir_voices
WHERE incarnation_name = $1
ORDER BY sid ASC, choir_name ASC
`

// LoadIncarnationHosts resolves scenario run hosts for incarnation
// `incarnationName`: online-souls with this Coven label + last-reported
// soulprint + declared role from `incarnation.spec.hosts[].role`.
//
// Two-phase (ADR-006(a)):
//   - Phase 1 (SQL, [rosterSQL]): candidates by Coven membership + non-terminal/
//     non-onboarding status. Presence is NOT decided here.
//   - Phase 2 (Redis, [Resolver.filterAlive]): filtering candidates without live
//     SID-lease (presence = online ⇔ lease is alive). nil-lease (unit / single-
//     instance dev) → fallback to SQL-presence (status='connected').
//
// Semantics:
//   - Nonexistent incarnation / no online hosts → empty slice, NOT
//     error (PM-decision #3).
//   - Cross-incarnation isolation: only souls with Coven label
//     `incarnationName` and spec of exactly this incarnation are read (ADR-008, PM-decision #4).
//   - Stale soulprint (`received_at < now - 10m`) → warn to logger,
//     run is not blocked (ADR-018, PM-decision #2).
func (r *Resolver) LoadIncarnationHosts(ctx context.Context, incarnationName string) ([]*HostFacts, error) {
	specRoles, err := r.loadDeclaredRoles(ctx, incarnationName)
	if err != nil {
		return nil, err
	}
	choirs, choirRoles, err := r.loadChoirMemberships(ctx, incarnationName)
	if err != nil {
		return nil, err
	}

	rows, err := r.pool.Query(ctx, rosterSQL, incarnationName)
	if err != nil {
		return nil, fmt.Errorf("topology: roster query: %w", err)
	}
	defer rows.Close()

	var candidates []*HostFacts
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		// Precedence role (ADR-044 p.2): Choir absorbs declared role.
		// voice.role (from incarnation_choir_voices) > spec.hosts[].role.
		// spec.hosts[].role remains fallback for hosts WITHOUT Voice (and for
		// bootstrap-create, where Choir memberships don't exist yet, wire-compatibility).
		if voiceRole, ok := choirRoles[h.SID]; ok {
			h.Role = voiceRole
		} else {
			h.Role = specRoles[h.SID]
		}
		h.Choirs = choirs[h.SID]
		candidates = append(candidates, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: roster iter: %w", err)
	}

	hosts, err := r.filterAlive(ctx, candidates)
	if err != nil {
		return nil, err
	}

	warnStale(ctx, r.logger, hosts, time.Now())
	return hosts, nil
}

// filterAlive — phase 2: presence filter of candidates by live Redis SID-lease
// (ADR-006(a), Variant A). Online ⇔ lease key `soul:<sid>:lock` exists.
//
// lease==nil (unit tests / single-instance dev without Redis) → fallback to
// SQL-presence: keep only status='connected' candidates (legacy snapshot
// in PG in single-instance mode is coherent with stream fact by construction).
// Symmetric to reaper (`mark_disconnected`, lease==nil → pure-SQL).
//
// Redis check error → fail-safe: to prevent Redis network failure from "killing"
// the entire incarnation (no_hosts → error_locked), degrade to the same
// SQL-presence fallback (status='connected') with warning, not returning error
// to run. Run targets the last known snapshot until Redis recovers.
func (r *Resolver) filterAlive(ctx context.Context, candidates []*HostFacts) ([]*HostFacts, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	if r.lease == nil {
		return filterConnectedSnapshot(candidates), nil
	}

	sids := make([]string, len(candidates))
	for i, h := range candidates {
		sids[i] = h.SID
	}
	alive, err := r.lease.SoulsStreamAlive(ctx, sids)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("topology: lease presence check failed — fallback to SQL snapshot (fail-safe)",
				slog.Any("error", err))
		}
		return filterConnectedSnapshot(candidates), nil
	}

	out := make([]*HostFacts, 0, len(candidates))
	for _, h := range candidates {
		if _, ok := alive[h.SID]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// filterConnectedSnapshot — SQL-presence fallback: keeps candidates with
// legacy snapshot status='connected' in PG. Used when lease==nil or on
// Redis failure (fail-safe). Candidate Status is read from scan ([HostFacts.Status]).
func filterConnectedSnapshot(candidates []*HostFacts) []*HostFacts {
	out := make([]*HostFacts, 0, len(candidates))
	for _, h := range candidates {
		if h.Status == "connected" {
			out = append(out, h)
		}
	}
	return out
}

// loadDeclaredRoles reads `incarnation.spec.hosts[].role` and builds a map
// SID → declared role. Nonexistent incarnation → empty map (roles of all
// hosts will be "" — allowed, ADR-008: declared role can be null for
// hosts outside declared-spec).
func (r *Resolver) loadDeclaredRoles(ctx context.Context, incarnationName string) (map[string]string, error) {
	var specJSON []byte
	err := r.pool.QueryRow(ctx, incarnationSpecSQL, incarnationName).Scan(&specJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("topology: incarnation spec query: %w", err)
	}
	return parseDeclaredRoles(specJSON), nil
}

// loadChoirMemberships reads `incarnation_choir_voices` and builds two maps:
//   - choirs: SID → names of Choirs where this SID is a Voice (ADR-044, S-T4);
//   - roles:  SID → role of Voice (ADR-044, S-T6/p.2: Choir absorbs declared
//     role, host role now comes from Voice, not from spec.hosts[].role).
//
// One query for entire roster (symmetric to loadDeclaredRoles, no N+1); each
// SID can be present in multiple Choirs → slice of names. Hosts without
// Voices are absent from both maps (Choirs remains nil, role — fallback
// to spec in LoadIncarnationHosts).
//
// Multi-choir role conflict (fixed by ADR-044 amendment): HostFacts.Role —
// scalar, but SID can be a Voice in multiple Choirs of one incarnation with
// different non-empty roles. Deterministic rule — take role from FIRST by
// choir_name sort order Choir WITH NON-EMPTY role (SQL already ORDER BY ... choir_name
// ASC, Choirs with empty/NULL role are skipped, so first encountered
// non-empty role is the result) + WARN log about conflict. If roles are empty in all
// Choirs — SID is not added to map roles → fallback to spec.
//
// Cross-incarnation isolation — filter choirVoicesSQL by `incarnation_name`.
// Order of names inside choirs slice is deterministic (ORDER BY choir_name in SQL).
func (r *Resolver) loadChoirMemberships(ctx context.Context, incarnationName string) (choirs map[string][]string, roles map[string]string, err error) {
	rows, err := r.pool.Query(ctx, choirVoicesSQL, incarnationName)
	if err != nil {
		return nil, nil, fmt.Errorf("topology: choir voices query: %w", err)
	}
	defer rows.Close()

	choirs = map[string][]string{}
	roles = map[string]string{}
	// roleChoir[sid] — name of Choir from which role was taken (for WARN about conflict).
	roleChoir := map[string]string{}
	for rows.Next() {
		// role is nullable (060_create_choirs.up.sql — TEXT without NOT NULL): AddVoice writes SQL
		// NULL when role is omitted (ADR-044 p.2/p.4 — role is optional). Scan into
		// *string (pattern from crud.go scanVoice / scanHost for nullable), otherwise pgx
		// fails with "cannot scan NULL into *string" and breaks entire roster. nil/empty
		// role → no role → fallback to spec.hosts[].role in LoadIncarnationHosts.
		var sid, choirName string
		var role *string
		if err := rows.Scan(&sid, &choirName, &role); err != nil {
			return nil, nil, fmt.Errorf("topology: scan choir voice: %w", err)
		}
		choirs[sid] = append(choirs[sid], choirName)
		if role == nil || *role == "" {
			continue
		}
		if existing, ok := roles[sid]; !ok {
			roles[sid] = *role
			roleChoir[sid] = choirName
		} else if existing != *role && r.logger != nil {
			r.logger.Warn("topology: multi-choir role conflict — taking first Choir by sort order",
				slog.String("sid", sid),
				slog.String("resolved_choir", roleChoir[sid]),
				slog.String("resolved_role", existing),
				slog.String("conflicting_choir", choirName),
				slog.String("conflicting_role", *role))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("topology: choir voices iter: %w", err)
	}
	return choirs, roles, nil
}

// parseDeclaredRoles extracts SID → role from freeform incarnation spec.
// Expected form: `spec.hosts` — list of objects with `sid` and `role`
// (scenario/orchestration.md §4.1). spec is freeform (jsonb): any deviation
// from form — skip element, NOT error (resolver is read-only, not spec validator;
// spec form validation — at incarnation creation layer).
func parseDeclaredRoles(specJSON []byte) map[string]string {
	roles := map[string]string{}
	if len(specJSON) == 0 {
		return roles
	}

	var spec struct {
		Hosts []struct {
			SID  string `json:"sid"`
			Role string `json:"role"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return roles
	}
	for _, h := range spec.Hosts {
		if h.SID != "" && h.Role != "" {
			roles[h.SID] = h.Role
		}
	}
	return roles
}

// scanHost parses one row of roster. soulprint_facts (JSONB) → map;
// NULL column (Soul has not yet sent SoulprintReport) → nil map.
func scanHost(row pgx.Row) (*HostFacts, error) {
	var (
		h           HostFacts
		traitsJSON  []byte
		factsJSON   []byte
		collectedAt *time.Time
		receivedAt  *time.Time
	)
	if err := row.Scan(&h.SID, &h.Coven, &traitsJSON, &h.Status, &factsJSON, &collectedAt, &receivedAt); err != nil {
		return nil, fmt.Errorf("topology: scan host: %w", err)
	}

	// traits jsonb (ADR-060): '{}' (NOT NULL DEFAULT) → empty map, not nil.
	if len(traitsJSON) > 0 {
		if err := json.Unmarshal(traitsJSON, &h.Traits); err != nil {
			return nil, fmt.Errorf("topology: unmarshal traits for %q: %w", h.SID, err)
		}
	}
	if len(factsJSON) > 0 {
		if err := json.Unmarshal(factsJSON, &h.Soulprint); err != nil {
			return nil, fmt.Errorf("topology: unmarshal soulprint for %q: %w", h.SID, err)
		}
	}
	if collectedAt != nil {
		h.CollectedAt = collectedAt.UTC()
	}
	if receivedAt != nil {
		h.ReceivedAt = receivedAt.UTC()
	}
	return &h, nil
}

// inventorySQL — read-only selection of souls by SID list for push run
// (Variant C, [keeper/internal/pushorch]). Field form matches [rosterSQL]
// (one scanHost handles both paths): SID, coven, status, soulprint facts
// + timestamps.
//
// Difference from rosterSQL — filter is NOT by Coven membership, but by exact SID list;
// incarnation-spec phase is not here (push run is not tied to incarnation,
// declared roles don't apply — Role="" for all). Status filter is the same:
// exclude terminal (`revoked`/`expired`/`destroyed`) and onboarding (`pending`) —
// SshDispatcher makes no sense on "not-ready" hosts regardless of lease.
//
// ORDER BY sid — determinism for per-host dispatch.
const inventorySQL = `
SELECT sid, coven, traits, status,
       soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE sid = ANY($1)
  AND status NOT IN ('pending', 'revoked', 'expired', 'destroyed')
ORDER BY sid ASC
`

// LoadByInventory resolves push run hosts by exact SID list
// (`POST /v1/push/apply::inventory`, Variant C). Symmetric to
// [Resolver.LoadIncarnationHosts], but:
//
//   - input filter — SID list, not Coven label;
//   - declared roles are absent (Role="" for all — push hosts are not tied to
//     incarnation.spec);
//   - second phase (filterAlive) applies the same: lease-presence for
//     fail-safe filter of "live" hosts; lease==nil → SQL-snapshot fallback.
//
// Semantics:
//   - not-found SID / hard-terminal status / onboarding → silently absent
//     from result (caller gets len(out) < len(sids));
//   - empty sids → empty result, not error;
//   - stale soulprint (`received_at < now - 10m`) → warn to logger,
//     run is not blocked (parity with LoadIncarnationHosts).
//
// FK to operators / cross-incarnation isolation do NOT apply: push inventory —
// flat list, without incarnation boundary.
func (r *Resolver) LoadByInventory(ctx context.Context, sids []string) ([]*HostFacts, error) {
	if len(sids) == 0 {
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, inventorySQL, sids)
	if err != nil {
		return nil, fmt.Errorf("topology: inventory query: %w", err)
	}
	defer rows.Close()

	var candidates []*HostFacts
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		// Role="" — push не имеет declared-роли (см. doc).
		candidates = append(candidates, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("topology: inventory iter: %w", err)
	}

	hosts, err := r.filterAlive(ctx, candidates)
	if err != nil {
		return nil, err
	}

	warnStale(ctx, r.logger, hosts, time.Now())
	return hosts, nil
}

// FilterByCovens keeps hosts that have ALL of requiredCovens —
// AND intersection by labels (scenario/orchestration.md §3; [ADR-040] amendment
// 2026-05-27 "Multi-label semantics within one list"). Host appears in
// result only if each label from requiredCovens is in `h.Coven`.
// Empty requiredCovens → original slice unchanged (no filter = entire
// incarnation, ADR-009).
//
// Security invariant: AND semantics fail-closed — enumerating labels does not
// expand scope. For OR case, operator uses `target.where: CEL` with explicit
// predicate.
//
// Pure function over already-loaded roster — no round-trips to PG.
func (r *Resolver) FilterByCovens(hosts []*HostFacts, requiredCovens []string) []*HostFacts {
	if len(requiredCovens) == 0 {
		return hosts
	}

	out := make([]*HostFacts, 0, len(hosts))
	for _, h := range hosts {
		if hostHasAllCovens(h, requiredCovens) {
			out = append(out, h)
		}
	}
	return out
}

// hostHasAllCovens — AND predicate: all required labels are present in h.Coven.
// Linear scan is optimal for typical sizes (host has tens of labels,
// required — tens): map allocation is more expensive than double loop.
func hostHasAllCovens(h *HostFacts, required []string) bool {
	for _, want := range required {
		found := false
		for _, c := range h.Coven {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
