//go:build integration

// L1 integration for the errand scope pushdown (NIM-841).
//
// WHY a Postgres and not the fake. The unit guards in api/handlers assert that
// the predicate reaches the statement; they cannot assert that the statement
// PARSES, and this one is not a WHERE on a column — it is a LEFT JOIN onto
// `souls` whose coven arm is an array overlap and whose host arm reads the
// errand's own `sid`. A typo there is a runtime SQL error on the read path of
// every operator, and the fake would go on replaying its rows.
//
// Two semantics also live only in the database:
//
//   - `total` comes from a COUNT under the same WHERE. A page narrowed without
//     its count hides the rows and still publishes how many exist, which is the
//     leak the ticket is about, one number at a time.
//   - a host that has left `souls` makes `s.coven` NULL, so the overlap is NULL
//     rather than false. Under `WHERE <p>` that hides the row — fail-closed for
//     a coven-scoped operator — while the host arm still matches on the sid the
//     errand carries itself. Only a real NULL demonstrates which way it falls.

package errand

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// seedSoul inserts one host with its covens. A SID present in `errands` but
// absent here is the decommissioned-host case.
func seedSoul(t *testing.T, sid string, covens []string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO souls (sid, coven, status)
		VALUES ($1, $2, 'connected')
		ON CONFLICT (sid) DO UPDATE SET coven = EXCLUDED.coven`, sid, covens); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

// scopeFor renders a purview over this package's own column aliases — the exact
// mapping keeper/internal/api/handlers builds.
func scopeFor(t *testing.T, expr string) func(int) (string, []any, int) {
	t.Helper()
	cols := rbac.ScopeColumns{Coven: ScopeCovenColumn, Host: ScopeHostColumn, Traits: ScopeTraitsColumn}
	var p rbac.Purview
	switch expr {
	case "":
		// zero Purview → FALSE (entitled to no host)
	case "*":
		p = rbac.Purview{Unrestricted: true}
	default:
		e, err := rbac.ParseScopeExpr(expr)
		if err != nil {
			t.Fatalf("ParseScopeExpr(%q): %v", expr, err)
		}
		p = rbac.Purview{Exprs: []*rbac.ScopeExpr{e}}
	}
	return func(startIdx int) (string, []any, int) {
		return rbac.PurviewSQL(p, cols, startIdx)
	}
}

func seedScopeFixture(t *testing.T) *Store {
	t.Helper()
	store := resetAll(t)
	ctx := context.Background()

	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls`); err != nil {
		t.Fatalf("clear souls: %v", err)
	}
	seedSoul(t, "dev-web-01", []string{"dev"})
	seedSoul(t, "prod-db-01", []string{"prod"})
	// retired-01 is deliberately NOT in souls: the host is gone, the record of
	// what was run on it is not.

	for _, r := range []Row{
		newRow("ERR-DEV", "dev-web-01", "core.cmd.shell"),
		newRow("ERR-PROD", "prod-db-01", "core.cmd.shell"),
		newRow("ERR-GONE", "retired-01", "core.cmd.shell"),
	} {
		if err := store.Insert(ctx, r); err != nil {
			t.Fatalf("Insert(%s): %v", r.ErrandID, err)
		}
	}
	return store
}

func listIDs(t *testing.T, store *Store, f ListFilter) ([]string, int) {
	t.Helper()
	rows, total, err := store.List(context.Background(), f, 0, 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ErrandID)
	}
	return ids, total
}

// TestIntegration_ErrandList_CovenScopeNarrowsRowsAndTotal — the predicate runs,
// and `total` moves with the page. If the count were left unnarrowed it would
// report 3 while returning 1, which is the disclosure with the rows removed.
func TestIntegration_ErrandList_CovenScopeNarrowsRowsAndTotal(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{Scope: scopeFor(t, "coven=dev")})

	if len(ids) != 1 || ids[0] != "ERR-DEV" {
		t.Errorf("rows = %v, want only ERR-DEV", ids)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1 — the COUNT must run under the same WHERE as the page", total)
	}
}

// TestIntegration_ErrandList_UnknownHostIsHiddenFromACovenScope — a SID that has
// left `souls` yields a NULL coven, and the row must NOT come back. This is the
// arm a `WHERE NOT (...)` formulation would get backwards.
func TestIntegration_ErrandList_UnknownHostIsHiddenFromACovenScope(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{Scope: scopeFor(t, "coven=dev")})

	for _, id := range ids {
		if id == "ERR-GONE" {
			t.Fatalf("an errand on a host absent from souls was shown to a coven-scoped "+
				"operator: %v", ids)
		}
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

// TestIntegration_ErrandList_HostScopeReachesADecommissionedHost — the other
// direction, and the reason ScopeHostColumn is the errand's own sid rather than
// the joined one: an operator scoped to a host keeps the record of what was run
// there after the host is gone. An audit trail outliving its subject is the
// normal case, not an edge one.
func TestIntegration_ErrandList_HostScopeReachesADecommissionedHost(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{Scope: scopeFor(t, "host=retired-01")})

	if len(ids) != 1 || ids[0] != "ERR-GONE" {
		t.Errorf("rows = %v, want ERR-GONE — a host-scoped operator must keep their own "+
			"host's history after the host leaves the registry", ids)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

// TestIntegration_ErrandList_EmptyPurviewShowsNothing — fail-closed. An operator
// the resolver knows nothing about gets an empty page AND a zero total, not the
// whole table.
func TestIntegration_ErrandList_EmptyPurviewShowsNothing(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{Scope: scopeFor(t, "")})

	if len(ids) != 0 || total != 0 {
		t.Errorf("rows = %v, total = %d; an operator entitled to no host must see none", ids, total)
	}
}

// TestIntegration_ErrandList_SIDFilterCannotEscapeTheScope — the ticket's own
// call: an operator scoped to dev asking for a prod host by name. The scope is
// ANDed after the filter, so naming the host reaches nothing.
func TestIntegration_ErrandList_SIDFilterCannotEscapeTheScope(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{SID: "prod-db-01", Scope: scopeFor(t, "coven=dev")})

	if len(ids) != 0 || total != 0 {
		t.Errorf("rows = %v, total = %d; naming a host outside the purview must reach nothing", ids, total)
	}
}

// TestIntegration_ErrandGet_CarriesTheHostsCovens — the single-object gate runs
// in Go over [Row.Covens]/[Row.TraitsRaw], so Get must actually bring them back
// from the join. Empty covens would silently fail-close every coven-scoped read.
func TestIntegration_ErrandGet_CarriesTheHostsCovens(t *testing.T) {
	store := seedScopeFixture(t)

	row, err := store.Get(context.Background(), "ERR-DEV")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(row.Covens) != 1 || row.Covens[0] != "dev" {
		t.Errorf("row.Covens = %v, want [dev] — the single-object scope gate reads this", row.Covens)
	}

	gone, err := store.Get(context.Background(), "ERR-GONE")
	if err != nil {
		t.Fatalf("Get(ERR-GONE): %v", err)
	}
	if len(gone.Covens) != 0 {
		t.Errorf("a host absent from souls must yield no covens, got %v", gone.Covens)
	}
	if gone.SID != "retired-01" {
		t.Errorf("row.SID = %q — the errand's own sid must survive the LEFT JOIN", gone.SID)
	}
}

// TestIntegration_ErrandList_UnrestrictedSeesEveryErrand — the direction the
// fail-closed tests cannot check. Without it a regression rendering FALSE for
// everyone would satisfy every other test in this file, and errands are the one
// store whose single-object read also grew a LEFT JOIN.
func TestIntegration_ErrandList_UnrestrictedSeesEveryErrand(t *testing.T) {
	store := seedScopeFixture(t)

	ids, total := listIDs(t, store, ListFilter{Scope: scopeFor(t, "*")})

	if len(ids) != 3 || total != 3 {
		t.Errorf("rows = %v, total = %d; an unrestricted operator must see all 3, "+
			"including the one whose host has left the registry", ids, total)
	}
}
