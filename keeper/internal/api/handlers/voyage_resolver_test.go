package handlers

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// sidPairRows — pgx.Rows over (sid, coven) pairs (Scan(*string, *[]string)).
// The resolver now reads coven for scope eval (ADR-047 S4); the cluster-wide path
// drops coven.
type sidPairRows struct {
	pgx.Rows
	sids   []string
	covens [][]string
	pos    int
}

func (r *sidPairRows) Next() bool { r.pos++; return r.pos <= len(r.sids) }
func (r *sidPairRows) Close()     {}
func (r *sidPairRows) Err() error { return nil }
func (r *sidPairRows) Scan(dest ...any) error {
	*dest[0].(*string) = r.sids[r.pos-1]
	if len(dest) > 1 {
		var cv []string
		if r.covens != nil {
			cv = r.covens[r.pos-1]
		}
		*dest[1].(*[]string) = cv
	}
	return nil
}

// fakeResolverDB — a voyageResolverDB returning a fixed set of (sid, coven) on
// Query (SELECT sid, coven FROM souls ...). covens is optional (nil → empty
// covens for all).
type fakeResolverDB struct {
	sids   []string
	covens [][]string
}

func (f *fakeResolverDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &sidPairRows{sids: f.sids, covens: f.covens}, nil
}
func (f *fakeResolverDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return voyageErrRow{err: pgx.ErrNoRows}
}
func (f *fakeResolverDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// require_alive=true with a presence checker drops SIDs without a live lease.
func TestVoyageCommandResolver_RequireAlive_FiltersDead(t *testing.T) {
	db := &fakeResolverDB{sids: []string{"a.example.com", "b.example.com", "c.example.com"}}
	presence := &fakePresence{alive: aliveSet("a.example.com", "c.example.com")}
	r := NewVoyageCommandPGResolverWithPresence(db, presence)

	out, err := r.ResolveSIDs(context.Background(), VoyageCommandFilter{RequireAlive: true})
	if err != nil {
		t.Fatalf("ResolveSIDs: %v", err)
	}
	want := []string{"a.example.com", "c.example.com"}
	if len(out) != len(want) {
		t.Fatalf("resolved = %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("resolved[%d] = %q, want %q", i, out[i], want[i])
		}
	}
}

// require_alive=false → presence is not applied (all SQL-presence SIDs remain).
func TestVoyageCommandResolver_RequireAliveFalse_NoFilter(t *testing.T) {
	db := &fakeResolverDB{sids: []string{"a.example.com", "b.example.com"}}
	presence := &fakePresence{alive: aliveSet("a.example.com")} // b is dead by lease.
	r := NewVoyageCommandPGResolverWithPresence(db, presence)

	out, err := r.ResolveSIDs(context.Background(), VoyageCommandFilter{RequireAlive: false})
	if err != nil {
		t.Fatalf("ResolveSIDs: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("resolved = %v, want 2 (filter not applied when require_alive=false)", out)
	}
}

// require_alive=true without a presence checker (nil) → degrades to SQL-presence (no
// extra filtering).
func TestVoyageCommandResolver_RequireAlive_NilPresence_Degrades(t *testing.T) {
	db := &fakeResolverDB{sids: []string{"a.example.com", "b.example.com"}}
	r := NewVoyageCommandPGResolver(db) // presence=nil

	out, err := r.ResolveSIDs(context.Background(), VoyageCommandFilter{RequireAlive: true})
	if err != nil {
		t.Fatalf("ResolveSIDs: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("resolved = %v, want 2 (nil presence → SQL-presence fallback)", out)
	}
}

// --- ADR-047 S4: ResolveSIDsInScope (target ∩ Purview) ---

// Unrestricted scope → full resolve without trimming (backcompat cluster-admin).
func TestResolveSIDsInScope_Unrestricted_FullResolve(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com"},
		covens: [][]string{{"prod"}, {"staging"}},
	}
	r := NewVoyageCommandPGResolver(db)

	got, err := r.ResolveSIDsInScope(context.Background(), VoyageCommandFilter{},
		soulpurview.Scope{Unrestricted: true})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 2 {
		t.Errorf("SIDs = %v, want both (Unrestricted, no trimming)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 0 {
		t.Errorf("DeniedExplicit = %v, want empty (Unrestricted)", got.DeniedExplicit)
	}
}

// Coven-scope trims a wide target to the visible subset (NOT the whole target).
func TestResolveSIDsInScope_CovenScope_TrimsWide(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com", "c.example.com"},
		covens: [][]string{{"prod"}, {"staging"}, {"prod"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// Wide target (covens filter, no explicit sids) + scope coven=prod →
	// only prod hosts are visible (a, c), b is trimmed silently (∉ explicit).
	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod", "staging"}},
		soulpurview.Scope{Covens: []string{"prod"}})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 2 || got.SIDs[0] != "a.example.com" || got.SIDs[1] != "c.example.com" {
		t.Errorf("SIDs = %v, want [a c] (trimmed to prod)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 0 {
		t.Errorf("DeniedExplicit = %v, want empty (wide target trimmed silently)", got.DeniedExplicit)
	}
}

// An explicitly listed foreign SID (out of scope) → DeniedExplicit (handler → 403).
func TestResolveSIDsInScope_ExplicitForeignSID_Denied(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com"},
		covens: [][]string{{"prod"}, {"staging"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// a (prod, visible) and b (staging, foreign) are listed explicitly. scope coven=prod.
	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{SIDs: []string{"a.example.com", "b.example.com"}},
		soulpurview.Scope{Covens: []string{"prod"}})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 1 || got.SIDs[0] != "a.example.com" {
		t.Errorf("SIDs = %v, want [a]", got.SIDs)
	}
	if len(got.DeniedExplicit) != 1 || got.DeniedExplicit[0] != "b.example.com" {
		t.Errorf("DeniedExplicit = %v, want [b] (explicit foreign -> 403)", got.DeniedExplicit)
	}
}

// Regex-scope: visibility by regexMatch (the host dimension of Purview via Visible).
func TestResolveSIDsInScope_RegexScope_TrimsToMatching(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"web-01.example.com", "db-01.example.com", "web-02.example.com"},
		covens: [][]string{nil, nil, nil},
	}
	r := NewVoyageCommandPGResolver(db)

	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod"}}, // wide target
		soulpurview.Scope{Regexes: []string{"^web-"}})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 2 || got.SIDs[0] != "web-01.example.com" || got.SIDs[1] != "web-02.example.com" {
		t.Errorf("SIDs = %v, want [web-01 web-02] (regex ^web-)", got.SIDs)
	}
}

// Empty scope (fail-closed) → empty SIDs; explicit SIDs → DeniedExplicit.
func TestResolveSIDsInScope_EmptyScope_FailClosed(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com"},
		covens: [][]string{{"prod"}},
	}
	r := NewVoyageCommandPGResolver(db)

	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{SIDs: []string{"a.example.com"}},
		soulpurview.Scope{Empty: true})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 0 {
		t.Errorf("SIDs = %v, want empty (Empty fail-closed)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 1 {
		t.Errorf("DeniedExplicit = %v, want [a] (explicit SID with Empty -> 403)", got.DeniedExplicit)
	}
}

// Partial scope (soulprint/state not evaluated): coven works, soulprint is
// under-shown. A host reachable ONLY via soulprint is not shown (fail-closed).
func TestResolveSIDsInScope_PartialScope_CovenWorksSoulprintUndershown(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com"},
		covens: [][]string{{"prod"}, {"staging"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// scope: coven=prod + Partial (the soulprint dimension is not evaluated).
	// a (prod) is visible by coven; b (staging) would be reachable only via soulprint —
	// under-shown, not picked up (fail-closed, never over-show).
	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod", "staging"}},
		soulpurview.Scope{Covens: []string{"prod"}, Partial: true})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 1 || got.SIDs[0] != "a.example.com" {
		t.Errorf("SIDs = %v, want [a] (coven works, soulprint under-shown)", got.SIDs)
	}
}
