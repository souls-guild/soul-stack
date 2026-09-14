package topology

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier — narrow subset of pgxpool.Pool needed by resolver (read-only).
// Symmetric to [soul.ExecQueryRower] / [incarnation.ExecQueryRower]: unit tests
// use fake without spinning up PG, production uses real pool/Conn/Tx.
//
// Query only, no QueryRow: every read here is set-shaped (a roster, a set of
// Voices). The single-row read this interface used to carry was `SELECT spec
// FROM incarnation` for the declared roles of `spec.hosts[]`, and that field is
// gone (ADR-044 amendment 2026-07-30 / NIM-330). Keeping the method off the
// interface makes "the resolver does not consult incarnation.spec" a fact the
// compiler enforces rather than an assertion a test has to remember to make.
type Querier interface {
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

// rosterSQL — phase 1 (SQL): targeting candidates — members of the incarnation
// (join on `incarnation_membership`, ADR-008 amendment 2026-07-17/NIM-124: no
// longer `incarnation.name = ANY(coven)`) whose status is NOT terminal/onboarding.
// The per-host `coven` field is projected as-is (real stable tags only now).
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
// `coven` and `traits` are the host's OWN columns and nothing else (NIM-281):
// belonging to this incarnation attaches no label, so `soulprint.self.covens` /
// `.traits` show exactly what an operator put on the host — the same set the RBAC
// scope predicate resolves.
//
// ★ NO transport carve-out here, unlike [inventorySQL]. A `transport=ssh` host
// is `pending` for its whole life, so this query never returns one — and that is
// the current behaviour, kept deliberately (NIM-869): a scenario run reaches
// hosts through [scenario.ApplyDispatcher], whose only implementation is the
// gRPC stream, so an ssh host admitted to the roster would reach dispatch and
// fail `soul_not_connected` where today the run simply does not see it. Binding
// one to an incarnation is not prevented anywhere, so this is reachable, not
// hypothetical. The ticket that gives the scenario path a push branch (NIM-870)
// is the one that gets to widen this, together with the dispatch it needs.
const rosterSQL = `
SELECT s.sid, s.coven, s.traits, s.status, s.transport,
       s.soulprint_facts, s.soulprint_collected_at, s.soulprint_received_at
FROM souls s
JOIN incarnation_membership m ON m.sid = s.sid
WHERE m.incarnation_name = $1
  AND s.status NOT IN ('pending', 'revoked', 'expired', 'destroyed')
ORDER BY s.sid ASC
`

// choirVoicesSQL reads Choir memberships of all hosts of one incarnation in one
// query (ADR-044, S-T4/S-T6): SID → names of Choirs where it is a Voice, + role
// of Voice in each Choir. Cross-incarnation isolation — filter by
// `incarnation_name` (PK includes it, ADR-044 section 3). One round-trip per
// roster (no N+1); join by `incarnation_choir_voices_sid_idx`
// (060_create_choirs.up.sql). ORDER BY choir_name —
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
// `incarnationName`: online member souls + last-reported soulprint + declared
// role from the host's Choir Voice.
//
// Two-phase (ADR-006(a)):
//   - Phase 1 (SQL, [rosterSQL]): candidates by incarnation_membership +
//     non-terminal/non-onboarding status. Presence is NOT decided here.
//   - Phase 2 (Redis, [Resolver.filterAlive]): filtering candidates without live
//     SID-lease (presence = online ⇔ lease is alive). nil-lease (unit / single-
//     instance dev) → fallback to SQL-presence (status='connected').
//
// Semantics:
//   - Nonexistent incarnation / no online hosts → empty slice, NOT
//     error (PM-decision #3).
//   - Cross-incarnation isolation: only member souls of `incarnationName` and
//     Voices of exactly this incarnation are read (membership join, PM-decision #4).
//   - Stale soulprint (`received_at < now - 10m`) → warn to logger,
//     run is not blocked (ADR-018, PM-decision #2).
func (r *Resolver) LoadIncarnationHosts(ctx context.Context, incarnationName string) ([]*HostFacts, error) {
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
		// Declared role — Voice, and ONLY Voice (ADR-044 p.2 + amendment
		// 2026-07-30/NIM-330: `spec.hosts[]` is gone, there is no second tier).
		// A host with no Voice, and a Voice whose role is empty/NULL, both land
		// here as "" — that is the declared answer "no role", not a missing one,
		// so nothing downstream may substitute a default for it.
		h.Role = choirRoles[h.SID]
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
//
// A transport=ssh host is EXEMPT from both branches and is always kept
// (NIM-869). Presence for it is not a fact anyone observes: the Keeper reaches
// it by opening a connection at dispatch time, and whether that works is
// answered by the dial, not by a lease this host will never take or a
// `connected` status only the agent's Bootstrap RPC writes. Filtering it here
// was not conservative, it was unconditional — no push host ever survived the
// phase.
//
// Reachable only from [Resolver.LoadByInventory] today: [rosterSQL] returns no
// ssh host at all, on purpose (see its doc). The exemption lives here rather
// than in the push caller because presence is this function's question, and a
// second copy of "what counts as present" is how the two answers drift.
func (r *Resolver) filterAlive(ctx context.Context, candidates []*HostFacts) ([]*HostFacts, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}

	// Only the streamed hosts are asked about; an ssh host is kept whatever the
	// answer, so putting it in the query would only widen it.
	sids := make([]string, 0, len(candidates))
	for _, h := range candidates {
		if h.Transport != transportSSH {
			sids = append(sids, h.SID)
		}
	}

	if len(sids) == 0 {
		return keepAlive(candidates, nil, false), nil
	}
	if r.lease == nil {
		return keepAlive(candidates, nil, true), nil
	}
	alive, err := r.lease.SoulsStreamAlive(ctx, sids)
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("topology: lease presence check failed — fallback to SQL snapshot (fail-safe)",
				slog.Any("error", err))
		}
		return keepAlive(candidates, nil, true), nil
	}
	return keepAlive(candidates, alive, false), nil
}

// keepAlive walks candidates IN ORDER — the SQL `ORDER BY sid` is what makes a
// serial wave reproducible, so presence filtering must not reshuffle — and
// keeps a host when it is reachable by construction (transport ssh) or present
// by the chosen source. snapshot=true selects the SQL fallback
// (`status='connected'`); otherwise membership in alive decides.
func keepAlive(candidates []*HostFacts, alive map[string]struct{}, snapshot bool) []*HostFacts {
	out := make([]*HostFacts, 0, len(candidates))
	for _, h := range candidates {
		switch {
		case h.Transport == transportSSH:
			out = append(out, h)
		case snapshot:
			if h.Status == "connected" {
				out = append(out, h)
			}
		default:
			if _, ok := alive[h.SID]; ok {
				out = append(out, h)
			}
		}
	}
	return out
}

// transportSSH duplicates soul.TransportSSH as a literal: topology is a
// read-only projection over `souls` and importing the registry package for one
// enum value would invert that dependency. Guarded by a test comparing the two.
const transportSSH = "ssh"

// loadChoirMemberships reads `incarnation_choir_voices` and builds two maps:
//   - choirs: SID → names of Choirs where this SID is a Voice (ADR-044, S-T4);
//   - roles:  SID → role of Voice (ADR-044, S-T6/p.2 + amendment 2026-07-30:
//     Choir absorbed the declared role and is now its ONLY source).
//
// One query for entire roster (no N+1); each SID can be present in multiple
// Choirs → slice of names. Hosts without Voices are absent from both maps
// (Choirs remains nil; the role map lookup in LoadIncarnationHosts yields "").
//
// Multi-choir role conflict (fixed by ADR-044 amendment): HostFacts.Role —
// scalar, but SID can be a Voice in multiple Choirs of one incarnation with
// different non-empty roles. Deterministic rule — take role from FIRST by
// choir_name sort order Choir WITH NON-EMPTY role (SQL already ORDER BY ... choir_name
// ASC, Choirs with empty/NULL role are skipped, so first encountered
// non-empty role is the result) + WARN log about conflict. If roles are empty in all
// Choirs — SID is not added to map roles, and the host's role stays "".
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
		// role → no role → the host's Role stays "" in LoadIncarnationHosts.
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

// scanHost parses one row of roster. soulprint_facts (JSONB) → map;
// NULL column (Soul has not yet sent SoulprintReport) → nil map.
//
// Coven and Traits are the host's OWN columns and nothing else (NIM-281): a
// label lives only where an operator attached it, and belonging to an incarnation
// attaches none. `soulprint.self.*` therefore shows the same labels the scope
// predicate resolves in SQL.
func scanHost(row pgx.Row) (*HostFacts, error) {
	var (
		h           HostFacts
		traitsJSON  []byte
		factsJSON   []byte
		collectedAt *time.Time
		receivedAt  *time.Time
	)
	if err := row.Scan(&h.SID, &h.Coven, &traitsJSON, &h.Status, &h.Transport, &factsJSON, &collectedAt, &receivedAt); err != nil {
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
// the Choir phase is not here (push run is not tied to an incarnation, so there are
// no Voices to read — Role="" for all). Terminal statuses are excluded the same way.
//
// ★ `pending` is excluded for an AGENT host ONLY, and this is the one query
// where that holds (NIM-869). A pending agent has a bootstrap token issued and
// no identity yet — targeting it is meaningless. A `transport=ssh` host is
// pending for its whole life: `connected` is written by the agent's Bootstrap
// RPC and by nothing else, and an ssh host never makes that call. Excluding it
// by status excluded the entire push inventory, which is the only inventory
// this query serves. [rosterSQL] deliberately does NOT copy this.
//
// ORDER BY sid — determinism for per-host dispatch.
const inventorySQL = `
SELECT sid, coven, traits, status, transport,
       soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE sid = ANY($1)
  AND status NOT IN ('revoked', 'expired', 'destroyed')
  AND (status <> 'pending' OR transport = 'ssh')
ORDER BY sid ASC
`

// LoadByInventory resolves push run hosts by exact SID list
// (`POST /v1/push/apply::inventory`, Variant C). Symmetric to
// [Resolver.LoadIncarnationHosts], but:
//
//   - input filter — SID list, not Coven label;
//   - declared roles are absent (Role="" for all — push hosts belong to no
//     incarnation, so they have no Voice);
//   - second phase (filterAlive) applies the same: lease-presence for
//     fail-safe filter of "live" hosts; lease==nil → SQL-snapshot fallback;
//     transport=ssh hosts skip the phase (NIM-869).
//
// Semantics:
//   - not-found SID / hard-terminal status / an ONBOARDING AGENT → silently
//     absent from result (caller gets len(out) < len(sids)); a pending ssh host
//     is kept, since `pending` is the only status it ever holds;
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
		// Role="" - push has no declared role (see doc).
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
