package handlers

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// sidPairRows — pgx.Rows над (sid, coven)-парами (Scan(*string, *[]string)).
// Резолвер теперь читает coven для scope-eval (ADR-047 S4); cluster-wide путь
// отбрасывает coven.
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

// fakeResolverDB — voyageResolverDB, отдающий фиксированный набор (sid, coven) на
// Query (SELECT sid, coven FROM souls ...). covens опционален (nil → пустые
// covens у всех).
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

// require_alive=true с presence-чекером отсекает SID-ы без живого lease.
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

// require_alive=false → presence не применяется (все SQL-presence SID-ы остаются).
func TestVoyageCommandResolver_RequireAliveFalse_NoFilter(t *testing.T) {
	db := &fakeResolverDB{sids: []string{"a.example.com", "b.example.com"}}
	presence := &fakePresence{alive: aliveSet("a.example.com")} // b мёртв по lease.
	r := NewVoyageCommandPGResolverWithPresence(db, presence)

	out, err := r.ResolveSIDs(context.Background(), VoyageCommandFilter{RequireAlive: false})
	if err != nil {
		t.Fatalf("ResolveSIDs: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("resolved = %v, want 2 (фильтр не применён при require_alive=false)", out)
	}
}

// require_alive=true без presence-чекера (nil) → деградация на SQL-presence (без
// дополнительной фильтрации).
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

// Unrestricted scope → полный резолв без урезания (backcompat cluster-admin).
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
		t.Errorf("SIDs = %v, want оба (Unrestricted без урезания)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 0 {
		t.Errorf("DeniedExplicit = %v, want пусто (Unrestricted)", got.DeniedExplicit)
	}
}

// Coven-scope урезает широкий target до видимого подмножества (НЕ весь target).
func TestResolveSIDsInScope_CovenScope_TrimsWide(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com", "c.example.com"},
		covens: [][]string{{"prod"}, {"staging"}, {"prod"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// Широкий target (covens-фильтр, нет явных sids) + scope coven=prod →
	// видимы только prod-хосты (a, c), b урезан молча (∉ explicit).
	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod", "staging"}},
		soulpurview.Scope{Covens: []string{"prod"}})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 2 || got.SIDs[0] != "a.example.com" || got.SIDs[1] != "c.example.com" {
		t.Errorf("SIDs = %v, want [a c] (урезано до prod)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 0 {
		t.Errorf("DeniedExplicit = %v, want пусто (широкий target урезается молча)", got.DeniedExplicit)
	}
}

// Явно-указанный чужой SID (вне scope) → DeniedExplicit (handler → 403).
func TestResolveSIDsInScope_ExplicitForeignSID_Denied(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com"},
		covens: [][]string{{"prod"}, {"staging"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// Явно перечислены a (prod, виден) и b (staging, чужой). scope coven=prod.
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
		t.Errorf("DeniedExplicit = %v, want [b] (явный чужой → 403)", got.DeniedExplicit)
	}
}

// Regex-scope: видимость по regexMatch (host-измерение Purview через Visible).
func TestResolveSIDsInScope_RegexScope_TrimsToMatching(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"web-01.example.com", "db-01.example.com", "web-02.example.com"},
		covens: [][]string{nil, nil, nil},
	}
	r := NewVoyageCommandPGResolver(db)

	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod"}}, // широкий target
		soulpurview.Scope{Regexes: []string{"^web-"}})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 2 || got.SIDs[0] != "web-01.example.com" || got.SIDs[1] != "web-02.example.com" {
		t.Errorf("SIDs = %v, want [web-01 web-02] (regex ^web-)", got.SIDs)
	}
}

// Empty scope (fail-closed) → пустой SIDs; явные SID-ы → DeniedExplicit.
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
		t.Errorf("SIDs = %v, want пусто (Empty fail-closed)", got.SIDs)
	}
	if len(got.DeniedExplicit) != 1 {
		t.Errorf("DeniedExplicit = %v, want [a] (явный SID при Empty → 403)", got.DeniedExplicit)
	}
}

// Partial scope (soulprint/state не вычисляются): coven работает, soulprint —
// под-показ. Хост, доступный ТОЛЬКО по soulprint, не показывается (fail-closed).
func TestResolveSIDsInScope_PartialScope_CovenWorksSoulprintUndershown(t *testing.T) {
	db := &fakeResolverDB{
		sids:   []string{"a.example.com", "b.example.com"},
		covens: [][]string{{"prod"}, {"staging"}},
	}
	r := NewVoyageCommandPGResolver(db)

	// scope: coven=prod + Partial (soulprint-измерение не вычисляется).
	// a (prod) виден по coven; b (staging) доступен был бы только по soulprint —
	// под-показ, не зацеплен (fail-closed, никогда over-show).
	got, err := r.ResolveSIDsInScope(context.Background(),
		VoyageCommandFilter{Covens: []string{"prod", "staging"}},
		soulpurview.Scope{Covens: []string{"prod"}, Partial: true})
	if err != nil {
		t.Fatalf("ResolveSIDsInScope: %v", err)
	}
	if len(got.SIDs) != 1 || got.SIDs[0] != "a.example.com" {
		t.Errorf("SIDs = %v, want [a] (coven работает, soulprint под-показ)", got.SIDs)
	}
}
