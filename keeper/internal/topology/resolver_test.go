package topology

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakePool - Querier stub. Routes Query by SQL content: `souls` -> roster,
// `incarnation_choir_voices` -> choir-membership (ADR-044, S-T4). Unknown SQL -
// panic (test bug).
//
// There is deliberately no `incarnation` branch and no spec field: since NIM-330
// the resolver has no second source for a host's role, and [Querier] no longer
// even carries QueryRow. A regression that reintroduced a spec read would not
// compile.
type fakePool struct {
	rosterRows []rosterRow
	rosterErr  error // Query or iteration error

	choirRows []choirVoiceRow
	choirErr  error // choir-voices Query error
}

// choirVoiceRow is one row of `incarnation_choir_voices` in order
// of choirVoicesSQL (sid, choir_name, role). role is *string: nil emulates SQL NULL
// (role is nullable, migration 060), distinguishing "no role" from Go string "". This catches
// bug of scanning NULL into plain string (pgx "cannot scan NULL into *string").
type choirVoiceRow struct {
	sid       string
	choirName string
	role      *string
}

// rosterRow — one row of `souls` roster: exactly fields of rosterSQL in order.
type rosterRow struct {
	sid         string
	coven       []string
	traitsJSON  []byte     // nil = '{}' (jsonb NOT NULL DEFAULT) → empty map; ADR-060
	status      string     // "" → default "connected" in Scan (SQL-presence fallback)
	factsJSON   []byte     // nil = NULL soulprint
	collectedAt *time.Time // nil = NULL
	receivedAt  *time.Time // nil = NULL
}

func (p *fakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	switch {
	case strings.Contains(sql, "FROM incarnation_choir_voices"):
		if p.choirErr != nil {
			return nil, p.choirErr
		}
		return &choirVoiceRows{rows: p.choirRows}, nil
	case strings.Contains(sql, "FROM souls"):
		if p.rosterErr != nil {
			return nil, p.rosterErr
		}
		return &rosterRows{rows: p.rosterRows}, nil
	default:
		panic("fakePool.Query: unexpected SQL: " + sql)
	}
}

// rosterRows iterates rosterRow by rosterRow, scan in order of rosterSQL.
type rosterRows struct {
	rows []rosterRow
	idx  int
	err  error
}

func (r *rosterRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *rosterRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	status := row.status
	if status == "" {
		// Most test cases don't care about presence snapshot — default
		// "connected" so nil-lease SQL fallback of resolver passes them.
		status = "connected"
	}
	*(dest[0].(*string)) = row.sid
	*(dest[1].(*[]string)) = row.coven
	*(dest[2].(*[]byte)) = row.traitsJSON
	*(dest[3].(*string)) = status
	*(dest[4].(*[]byte)) = row.factsJSON
	*(dest[5].(**time.Time)) = row.collectedAt
	*(dest[6].(**time.Time)) = row.receivedAt
	return nil
}

func (r *rosterRows) Err() error                                   { return r.err }
func (r *rosterRows) Close()                                       {}
func (r *rosterRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *rosterRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *rosterRows) Values() ([]any, error)                       { return nil, nil }
func (r *rosterRows) RawValues() [][]byte                          { return nil }
func (r *rosterRows) Conn() *pgx.Conn                              { return nil }

// choirVoiceRows iterates choirVoiceRow by choirVoiceRow, scan in order
// of choirVoicesSQL (sid, choir_name).
type choirVoiceRows struct {
	rows []choirVoiceRow
	idx  int
	err  error
}

func (r *choirVoiceRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *choirVoiceRows) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	*(dest[0].(*string)) = row.sid
	*(dest[1].(*string)) = row.choirName
	// role scans into *string (nullable): nil row.role → dest remains nil,
	// emulating SQL NULL without pgx panic (parity with resolver's real scan).
	*(dest[2].(**string)) = row.role
	return nil
}

func (r *choirVoiceRows) Err() error                                   { return r.err }
func (r *choirVoiceRows) Close()                                       {}
func (r *choirVoiceRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *choirVoiceRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *choirVoiceRows) Values() ([]any, error)                       { return nil, nil }
func (r *choirVoiceRows) RawValues() [][]byte                          { return nil }
func (r *choirVoiceRows) Conn() *pgx.Conn                              { return nil }

func newResolver(p *fakePool, logger *slog.Logger) *Resolver {
	return &Resolver{pool: p, logger: logger}
}

// newResolverWithLease — Resolver with fake lease-checker for presence phase
// (Variant A). lease-aware path of resolver (phase 2) is tested without Redis.
func newResolverWithLease(p *fakePool, lease SoulLeaseChecker, logger *slog.Logger) *Resolver {
	return &Resolver{pool: p, lease: lease, logger: logger}
}

// fakeLease — SoulLeaseChecker stub: alive — set of online SIDs, err —
// forced Redis error (test fail-safe degradation to SQL-presence).
type fakeLease struct {
	alive    map[string]struct{}
	err      error
	gotSIDs  []string
	gotCalls int
}

func (l *fakeLease) SoulsStreamAlive(_ context.Context, sids []string) (map[string]struct{}, error) {
	l.gotCalls++
	l.gotSIDs = append([]string{}, sids...)
	if l.err != nil {
		return nil, l.err
	}
	out := make(map[string]struct{}, len(sids))
	for _, sid := range sids {
		if _, ok := l.alive[sid]; ok {
			out[sid] = struct{}{}
		}
	}
	return out, nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func strPtr(s string) *string { return &s }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// --- LoadIncarnationHosts ---------------------------------------------

func TestLoadIncarnationHosts_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	p := &fakePool{
		choirRows: []choirVoiceRow{
			{sid: "a.example.com", choirName: "primaries", role: strPtr("master")},
			{sid: "b.example.com", choirName: "replicas", role: strPtr("replica")},
		},
		rosterRows: []rosterRow{
			{
				sid:         "a.example.com",
				coven:       []string{"redis-prod", "db"},
				factsJSON:   mustJSON(t, map[string]any{"os": map[string]any{"family": "debian"}}),
				collectedAt: ptrTime(now.Add(-time.Minute)),
				receivedAt:  ptrTime(now.Add(-time.Minute)),
			},
			{
				sid:        "b.example.com",
				coven:      []string{"redis-prod"},
				factsJSON:  nil, // Soul has not sent soulprint yet
				receivedAt: nil,
			},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(hosts))
	}

	a := hosts[0]
	if a.SID != "a.example.com" || a.Role != "master" {
		t.Errorf("host[0] = %+v, want master a.example.com", a)
	}
	if a.Soulprint == nil {
		t.Fatal("host[0].Soulprint = nil, want parsed map")
	}
	osMap, _ := a.Soulprint["os"].(map[string]any)
	if osMap["family"] != "debian" {
		t.Errorf("host[0] soulprint os.family = %v, want debian", osMap["family"])
	}
	if a.CollectedAt.IsZero() || a.ReceivedAt.IsZero() {
		t.Errorf("host[0] timestamps not populated: %+v", a)
	}

	b := hosts[1]
	if b.SID != "b.example.com" || b.Role != "replica" {
		t.Errorf("host[1] = %+v, want replica b.example.com", b)
	}
	if b.Soulprint != nil {
		t.Errorf("host[1].Soulprint = %v, want nil (NULL facts)", b.Soulprint)
	}
	if !b.ReceivedAt.IsZero() {
		t.Errorf("host[1].ReceivedAt = %v, want zero", b.ReceivedAt)
	}
}

func TestLoadIncarnationHosts_MissingIncarnation_EmptySlice(t *testing.T) {
	// PM-decision #3: nonexistent incarnation → empty slice, NOT error. Both
	// reads are set-shaped, so a missing incarnation is indistinguishable from
	// an empty roster here — deliberately (the caller that needs to tell them
	// apart asks the incarnation layer, not the resolver).
	p := &fakePool{rosterRows: nil}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(hosts))
	}
}

func TestLoadIncarnationHosts_NoCandidates_EmptySlice(t *testing.T) {
	// Incarnation exists, but there are no non-terminal/non-onboarding candidates
	// (phase-1 SQL returned empty). → empty slice, not error.
	p := &fakePool{rosterRows: nil}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 0 {
		t.Errorf("len(hosts) = %d, want 0", len(hosts))
	}
}

// --- presence phase (lease-aware, Variant A) ---------------------------

func TestLoadIncarnationHosts_LeaseAware_FiltersByLiveLease(t *testing.T) {
	// Phase 2: candidate with live lease is targeted; without lease — filtered.
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "online.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "offline.example.com", coven: []string{"redis-prod"}, status: "connected"},
		},
	}
	lease := &fakeLease{alive: map[string]struct{}{"online.example.com": {}}}
	r := newResolverWithLease(p, lease, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "online.example.com" {
		t.Fatalf("got %v, want [online.example.com]", sids(hosts))
	}
	if lease.gotCalls != 1 {
		t.Errorf("lease checked %d times, want 1 (batch)", lease.gotCalls)
	}
}

func TestLoadIncarnationHosts_LeaseAware_PresenceNotFromStatus(t *testing.T) {
	// Key invariant: presence is decided by lease, NOT snapshot souls.status.
	// disconnected snapshot with live lease (idle Soul, reconnect not reflected in PG)
	// → is targeted; connected snapshot without lease (stale) → NOT targeted.
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "idle.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
			{sid: "stale.example.com", coven: []string{"redis-prod"}, status: "connected"},
		},
	}
	lease := &fakeLease{alive: map[string]struct{}{"idle.example.com": {}}}
	r := newResolverWithLease(p, lease, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "idle.example.com" {
		t.Fatalf("got %v, want [idle.example.com] (presence = lease, not status)", sids(hosts))
	}
}

func TestLoadIncarnationHosts_LeaseAware_RedisError_FallsBackToSQLSnapshot(t *testing.T) {
	// Fail-safe: Redis check error → degradation to SQL-presence snapshot
	// (status='connected'), not run failure (no_hosts → error_locked).
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "conn.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "disc.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
		},
	}
	lease := &fakeLease{err: errors.New("redis down")}
	r := newResolverWithLease(p, lease, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "conn.example.com" {
		t.Fatalf("got %v, want [conn.example.com] (SQL snapshot fallback)", sids(hosts))
	}
	if !strings.Contains(buf.String(), "fail-safe") {
		t.Errorf("warn-log missing fail-safe message: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_NilLease_FallsBackToSQLSnapshot(t *testing.T) {
	// lease==nil (single-instance dev / unit) → SQL-presence snapshot.
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "conn.example.com", coven: []string{"redis-prod"}, status: "connected"},
			{sid: "disc.example.com", coven: []string{"redis-prod"}, status: "disconnected"},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].SID != "conn.example.com" {
		t.Fatalf("got %v, want [conn.example.com] (nil-lease SQL fallback)", sids(hosts))
	}
}

func TestLoadIncarnationHosts_RoleEmptyForHostWithoutVoice(t *testing.T) {
	// ADR-008 + ADR-044 amendment 2026-07-30: a member host that no Voice
	// places into a part has declared role "". The resolver does not invent a
	// role from the fact of membership, and there is no longer any second
	// source it could invent one from.
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "voiced.example.com", coven: []string{"redis-prod"}},
			{sid: "extra.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			{sid: "voiced.example.com", choirName: "primaries", role: strPtr("master")},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "master" {
		t.Errorf("voiced host role = %q, want master", hosts[0].Role)
	}
	if hosts[1].Role != "" {
		t.Errorf("host without a Voice role = %q, want empty", hosts[1].Role)
	}
}

func TestLoadIncarnationHosts_ChoirMemberships(t *testing.T) {
	// ADR-044, S-T4: host choir memberships — stable per-host fact. Host in
	// multiple Choirs → multiple names (deterministic order from SQL);
	// host without Voices → nil Choirs (symmetry with empty declared role).
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
			{sid: "b.example.com", coven: []string{"redis-prod"}},
			{sid: "c.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			{sid: "a.example.com", choirName: "primaries"},
			{sid: "a.example.com", choirName: "voters"},
			{sid: "b.example.com", choirName: "replicas"},
			// c.example.com — without Voices.
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if got := hosts[0].Choirs; len(got) != 2 || got[0] != "primaries" || got[1] != "voters" {
		t.Errorf("host[0].Choirs = %v, want [primaries voters]", got)
	}
	if got := hosts[1].Choirs; len(got) != 1 || got[0] != "replicas" {
		t.Errorf("host[1].Choirs = %v, want [replicas]", got)
	}
	if hosts[2].Choirs != nil {
		t.Errorf("host[2].Choirs = %v, want nil (no Voices)", hosts[2].Choirs)
	}
}

func TestLoadIncarnationHosts_RoleComesFromVoice(t *testing.T) {
	// ADR-044 p.2: the Choir absorbed the declared role; voice.role (from
	// incarnation_choir_voices) IS the role, with nothing behind it.
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			{sid: "a.example.com", choirName: "primaries", role: strPtr("voice-master")},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "voice-master" {
		t.Errorf("role = %q, want voice-master", hosts[0].Role)
	}
}

// TestLoadIncarnationHosts_NoRoleSourceLeavesRoleEmpty pins the invariant NIM-330
// exists to create: a declared role comes from a Voice or it does not come at
// all. Three ways a host can end up without one, all of which used to fall
// through to `spec.hosts[].role`:
//
//   - novoice   — no row in incarnation_choir_voices at all;
//   - nullrole  — a Voice with SQL NULL role (AddVoice writes NULL when role is
//     omitted, migration 060). This is also the path where the resolver once
//     failed with "cannot scan NULL into *string"; without the scan fix the test
//     dies here rather than in the assertion;
//   - emptyrole — a Voice with role="" (Go string).
//
// All three must resolve to "" — "no declared role", NOT a default group
// (ADR-044 amendment 2026-06-30(b), now the only rule). This guard used to have
// a complementary half — a spec that still CARRIED hosts[] in the database being
// ignored — which could not be written here because [Querier] does not expose
// the single-row read that fallback needed. That half no longer exists anywhere:
// migration 112 (NIM-408) dropped `incarnation.spec`, so the ignored-spec case
// can no longer be constructed. The real-PG side of the rule is
// TestIntegration_LoadIncarnationHosts_MemberWithoutVoiceHasNoRole.
func TestLoadIncarnationHosts_NoRoleSourceLeavesRoleEmpty(t *testing.T) {
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "emptyrole.example.com", coven: []string{"redis-prod"}},
			{sid: "novoice.example.com", coven: []string{"redis-prod"}},
			{sid: "nullrole.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			// novoice.example.com — no Voice at all.
			{sid: "emptyrole.example.com", choirName: "voters", role: strPtr("")},
			{sid: "nullrole.example.com", choirName: "voters", role: nil},
		},
	}
	r := newResolver(p, nil)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 3 {
		t.Fatalf("len(hosts) = %d, want 3", len(hosts))
	}
	for _, h := range hosts {
		if h.Role != "" {
			t.Errorf("%s role = %q, want empty — a role may only come from a Voice", h.SID, h.Role)
		}
	}
}

func TestLoadIncarnationHosts_MultiChoirRoleConflict_FirstBySortNameWins(t *testing.T) {
	// ADR-044 p.2: SID — Voice in multiple Choirs with DIFFERENT non-empty roles.
	// HostFacts.Role — scalar → deterministically take role from FIRST Choir
	// by choir_name sort + WARN about conflict. SQL returns ORDER BY choir_name
	// ASC, so fake provides rows in this order.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "a.example.com", coven: []string{"redis-prod"}},
		},
		choirRows: []choirVoiceRow{
			// "alpha" < "beta" lexicographically → alpha-role wins.
			{sid: "a.example.com", choirName: "alpha", role: strPtr("alpha-role")},
			{sid: "a.example.com", choirName: "beta", role: strPtr("beta-role")},
		},
	}
	r := newResolver(p, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if hosts[0].Role != "alpha-role" {
		t.Errorf("role = %q, want alpha-role (first Choir by sort order)", hosts[0].Role)
	}
	out := buf.String()
	if !strings.Contains(out, "conflict") {
		t.Errorf("warn-log missing conflict message: %q", out)
	}
	if !strings.Contains(out, "a.example.com") {
		t.Errorf("warn-log missing SID of conflicting host: %q", out)
	}
	if !strings.Contains(out, "beta-role") {
		t.Errorf("warn-log missing conflicting role: %q", out)
	}
}

func TestLoadIncarnationHosts_StaleSoulprintWarns(t *testing.T) {
	// PM-decision #2: received_at < now-10m → warn, run NOT blocked.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := time.Now().UTC().Add(-30 * time.Minute)
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "stale.example.com", coven: []string{"redis-prod"}, receivedAt: ptrTime(old)},
		},
	}
	r := newResolver(p, logger)

	hosts, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if len(hosts) != 1 {
		t.Fatalf("len(hosts) = %d, want 1 (stale NOT blocking)", len(hosts))
	}
	if !strings.Contains(buf.String(), "stale.example.com") {
		t.Errorf("warn-log missing SID of stale host: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "stale") {
		t.Errorf("warn-log missing expected message: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_FreshSoulprintNoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	fresh := time.Now().UTC().Add(-time.Minute)
	p := &fakePool{
		rosterRows: []rosterRow{
			{sid: "fresh.example.com", coven: []string{"redis-prod"}, receivedAt: ptrTime(fresh)},
			{sid: "neverreported.example.com", coven: []string{"redis-prod"}}, // zero received_at
		},
	}
	r := newResolver(p, logger)

	if _, err := r.LoadIncarnationHosts(context.Background(), "redis-prod"); err != nil {
		t.Fatalf("LoadIncarnationHosts: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected warn for fresh/unreported hosts: %q", buf.String())
	}
}

func TestLoadIncarnationHosts_BadSoulprintJSONErrors(t *testing.T) {
	// Broken soulprint_facts JSONB — data corruption, not normal case:
	// resolver must return error, not silently return empty soulprint.
	p := &fakePool{
		rosterRows: []rosterRow{{sid: "a.example.com", coven: []string{"redis-prod"}, factsJSON: []byte(`{bad`)}},
	}
	r := newResolver(p, nil)

	_, err := r.LoadIncarnationHosts(context.Background(), "redis-prod")
	if err == nil {
		t.Fatal("LoadIncarnationHosts returned nil err on broken soulprint JSON")
	}
}

// --- FilterByCovens ---------------------------------------------------

// TestFilterByCovens checks AND semantics of filter (ADR-040 amendment
// 2026-05-27 "Multi-label semantics within one list"; orchestration.md §3).
// Host appears in result if and only if it has ALL
// of the filter labels listed.
func TestFilterByCovens(t *testing.T) {
	hosts := []*HostFacts{
		{SID: "a", Coven: []string{"redis-prod", "db", "eu"}},          // db + eu
		{SID: "b", Coven: []string{"redis-prod", "cache"}},             // cache only
		{SID: "c", Coven: []string{"redis-prod", "eu"}},                // eu only
		{SID: "d", Coven: []string{"redis-prod", "db", "cache"}},       // db + cache
		{SID: "e", Coven: []string{"redis-prod", "db", "cache", "eu"}}, // all three
	}
	r := &Resolver{}

	t.Run("empty-required-returns-all", func(t *testing.T) {
		got := r.FilterByCovens(hosts, nil)
		if len(got) != 5 {
			t.Errorf("len = %d, want 5", len(got))
		}
	})

	t.Run("single-coven", func(t *testing.T) {
		// Single-label AND trivially matches any filter form.
		got := r.FilterByCovens(hosts, []string{"db"})
		want := []string{"a", "d", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("multi-coven-AND", func(t *testing.T) {
		// AND intersection: db + cache → only hosts with BOTH labels.
		// Previously (OR) would return {a, b, d, e}; now — only {d, e}.
		got := r.FilterByCovens(hosts, []string{"db", "cache"})
		want := []string{"d", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v (AND intersection)", sids(got), want)
		}
	})

	t.Run("multi-coven-AND-three-labels", func(t *testing.T) {
		// Triple AND: db + cache + eu → only host with all three labels.
		got := r.FilterByCovens(hosts, []string{"db", "cache", "eu"})
		want := []string{"e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("multi-coven-AND-no-host-has-all", func(t *testing.T) {
		// Host {db} + host {cache} + filter [db, cache]: neither individually
		// carries both labels → empty result (previously OR would return {a, b}).
		isolated := []*HostFacts{
			{SID: "only-db", Coven: []string{"db"}},
			{SID: "only-cache", Coven: []string{"cache"}},
		}
		got := r.FilterByCovens(isolated, []string{"db", "cache"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty (AND fail-closed)", sids(got))
		}
	})

	t.Run("coven-matching-all", func(t *testing.T) {
		got := r.FilterByCovens(hosts, []string{"eu"})
		want := []string{"a", "c", "e"}
		if !equalSIDs(sids(got), want) {
			t.Errorf("got = %v, want %v", sids(got), want)
		}
	})

	t.Run("no-match", func(t *testing.T) {
		got := r.FilterByCovens(hosts, []string{"nonexistent"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty", sids(got))
		}
	})

	t.Run("multi-coven-one-missing", func(t *testing.T) {
		// All hosts carry redis-prod, but nonexistent — no one; AND gives empty.
		got := r.FilterByCovens(hosts, []string{"redis-prod", "nonexistent"})
		if len(got) != 0 {
			t.Errorf("got = %v, want empty (one label missing from all)", sids(got))
		}
	})
}

// equalSIDs — ordered comparison of SID lists (resolver returns in order
// of original roster, so order is deterministic).
func equalSIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sids(hosts []*HostFacts) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.SID
	}
	return out
}
