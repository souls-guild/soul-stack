//go:build integration

// L1 integration for the Voyage host-scope pushdown (NIM-842).
//
// WHY a Postgres and not the fake. [HostScopeSQL] is the most involved predicate
// on any read path here: a correlated NOT EXISTS over
// `jsonb_array_elements_text(target_resolved)`, LEFT JOINed onto `souls`, closed
// with `IS NOT TRUE`. The unit guards in api/handlers assert that it reaches the
// statement; only a database says whether it PARSES and whether it means what the
// comment claims.
//
// Three properties live entirely here:
//
//   - ALL, not ANY: a run touching one visible and one invisible host must be
//     hidden. `EXISTS` instead of `NOT EXISTS` would flip this and every test
//     above the SQL would still pass.
//   - `IS NOT TRUE`, not `NOT`: a host absent from `souls` makes the coven overlap
//     NULL. Under plain `NOT` that row leaves the subquery and the Voyage becomes
//     visible — fail-OPEN, and no fake can show it.
//   - `total` is counted under the same WHERE, so the number of hidden runs is
//     not published alongside their absence.

package voyage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func seedScopeSoul(t *testing.T, sid string, covens []string) {
	t.Helper()
	seedScopeSoulTraits(t, sid, covens, nil)
}

func seedScopeSoulTraits(t *testing.T, sid string, covens []string, traits []byte) {
	t.Helper()
	var traitsArg any
	if traits != nil {
		traitsArg = traits
	}
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO souls (sid, coven, status, traits)
		VALUES ($1, $2, 'connected', COALESCE($3::jsonb, '{}'::jsonb))
		ON CONFLICT (sid) DO UPDATE SET coven = EXCLUDED.coven, traits = EXCLUDED.traits`,
		sid, covens, traitsArg); err != nil {
		t.Fatalf("seedScopeSoulTraits(%s): %v", sid, err)
	}
}

// scopeFor renders a purview over this package's subquery aliases — the exact
// mapping keeper/internal/api/handlers builds — and wraps it in [HostScopeSQL],
// which is what the handler hands to List/SelectByIDScoped.
func scopeFor(t *testing.T, expr string) func(int) (string, []any, int) {
	t.Helper()
	cols := rbac.ScopeColumns{Coven: ScopeCovenColumn, Host: ScopeHostColumn, Traits: ScopeTraitsColumn}
	var p rbac.Purview
	switch expr {
	case "":
		// zero Purview → FALSE per target (entitled to no host)
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
		return HostScopeSQL(func(i int) (string, []any, int) {
			return rbac.PurviewSQL(p, cols, i)
		}, startIdx)
	}
}

// seedScopeFixture lays out the cases the predicate has to tell apart.
func seedScopeFixture(t *testing.T) {
	t.Helper()
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")

	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls`); err != nil {
		t.Fatalf("clear souls: %v", err)
	}
	seedScopeSoul(t, "dev-web-01", []string{"dev"})
	seedScopeSoul(t, "dev-web-02", []string{"dev"})
	seedScopeSoul(t, "prod-db-01", []string{"prod"})
	// retired-01 is deliberately absent from souls.

	insert := func(id string, kind Kind, resolved string) {
		v := &Voyage{
			VoyageID:       id,
			Kind:           kind,
			Input:          []byte(`{}`),
			TargetResolved: json.RawMessage(resolved),
			Status:         StatusPending,
			StartedByAID:   "archon-test",
		}
		if kind == KindScenario {
			v.ScenarioName = strptr("restart")
		} else {
			v.Module = strptr("core.cmd.shell")
		}
		if err := Insert(ctx, integrationPool, v); err != nil {
			t.Fatalf("Insert(%s): %v", id, err)
		}
	}

	insert("01H0000000000000000000DEV1", KindCommand, `["dev-web-01","dev-web-02"]`) // all in dev
	insert("01H0000000000000000000MIX1", KindCommand, `["dev-web-01","prod-db-01"]`) // straddles
	insert("01H000000000000000000PROD1", KindCommand, `["prod-db-01"]`)              // all in prod
	insert("01H000000000000000000GONE1", KindCommand, `["retired-01"]`)              // host deleted
	insert("01H0000000000000000000SCN1", KindScenario, `["service-redis"]`)          // names no host
}

func listScopedIDs(t *testing.T, expr string) ([]string, int) {
	t.Helper()
	items, total, err := List(context.Background(), integrationPool,
		ListFilter{HostScope: scopeFor(t, expr)}, 0, 50)
	if err != nil {
		t.Fatalf("List(%q): %v", expr, err)
	}
	ids := make([]string, 0, len(items))
	for _, v := range items {
		ids = append(ids, v.VoyageID)
	}
	return ids, total
}

func contains(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestIntegration_VoyageList_HidesRunsThatStraddleTheBoundary — the ALL rule. A
// run touching one dev host and one prod host must not be visible to a
// dev-scoped operator: showing it means showing `prod-db-01` in its targets, and
// that identity is the whole disclosure.
func TestIntegration_VoyageList_HidesRunsThatStraddleTheBoundary(t *testing.T) {
	seedScopeFixture(t)

	ids, total := listScopedIDs(t, "coven=dev")

	if !contains(ids, "01H0000000000000000000DEV1") {
		t.Errorf("the all-dev run is missing from %v", ids)
	}
	if contains(ids, "01H0000000000000000000MIX1") {
		t.Errorf("a run straddling dev and prod was shown to a dev-scoped operator: %v", ids)
	}
	if contains(ids, "01H000000000000000000PROD1") {
		t.Errorf("an all-prod run was shown to a dev-scoped operator: %v", ids)
	}
	// The all-dev run plus the scenario run, which names no host.
	if total != 2 {
		t.Errorf("total = %d, want 2 (dev command run + scenario run) — the COUNT must run "+
			"under the same WHERE as the page; ids=%v", total, ids)
	}
}

// TestIntegration_VoyageList_UnknownHostIsFailClosed — the `IS NOT TRUE` arm. A
// target absent from `souls` makes the coven overlap NULL, and plain `NOT` would
// drop it from the subquery and publish the run.
func TestIntegration_VoyageList_UnknownHostIsFailClosed(t *testing.T) {
	seedScopeFixture(t)

	ids, _ := listScopedIDs(t, "coven=dev")

	if contains(ids, "01H000000000000000000GONE1") {
		t.Errorf("a run against a host absent from souls was shown to a coven-scoped "+
			"operator — the NULL predicate read as in-scope: %v", ids)
	}
}

// TestIntegration_VoyageList_ScenarioRunsAreNeverNarrowed — a kind=scenario
// Voyage names incarnations, and `incarnation.history` governs it. Even an
// operator entitled to NO host keeps it.
func TestIntegration_VoyageList_ScenarioRunsAreNeverNarrowed(t *testing.T) {
	seedScopeFixture(t)

	ids, total := listScopedIDs(t, "")

	if len(ids) != 1 || ids[0] != "01H0000000000000000000SCN1" {
		t.Errorf("rows = %v, want only the scenario run: a host right must not take away "+
			"runs that name no host", ids)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
}

// TestIntegration_VoyageList_UnrestrictedSeesEveryRun — a cluster-admin keeps the
// whole list, including the run whose host has been deleted. Without this the
// guards above are satisfied by a predicate that hides everything.
func TestIntegration_VoyageList_UnrestrictedSeesEveryRun(t *testing.T) {
	seedScopeFixture(t)

	ids, total := listScopedIDs(t, "*")

	if len(ids) != 5 || total != 5 {
		t.Errorf("rows = %v, total = %d; an unrestricted operator must see all 5", ids, total)
	}
}

// TestIntegration_VoyageList_HostScopeReachesADecommissionedHost — the host
// dimension resolves over the SID the Voyage recorded, not the joined row, so an
// operator scoped to a host keeps its run history after the host leaves the
// registry.
func TestIntegration_VoyageList_HostScopeReachesADecommissionedHost(t *testing.T) {
	seedScopeFixture(t)

	ids, _ := listScopedIDs(t, "host=retired-01")

	if !contains(ids, "01H000000000000000000GONE1") {
		t.Errorf("rows = %v, want the run against retired-01: the host arm reads the "+
			"Voyage's own target list, which survives the host", ids)
	}
}

// TestIntegration_VoyageSelectByIDScoped_OutOfScopeIsNotFound — the single-object
// read narrows in the WHERE, so out-of-boundary and absent are ONE answer:
// ErrVoyageNotFound, which the handler turns into the same 404 either way.
func TestIntegration_VoyageSelectByIDScoped_OutOfScopeIsNotFound(t *testing.T) {
	seedScopeFixture(t)
	ctx := context.Background()

	if _, err := SelectByIDScoped(ctx, integrationPool, "01H000000000000000000PROD1",
		scopeFor(t, "coven=dev")); err != ErrVoyageNotFound {
		t.Errorf("err = %v, want ErrVoyageNotFound for an out-of-scope run", err)
	}
	if _, err := SelectByIDScoped(ctx, integrationPool, "01H0000000000000000000DEV1",
		scopeFor(t, "coven=dev")); err != nil {
		t.Errorf("err = %v, want the in-scope run to be served", err)
	}
	// Unnarrowed: the orchestration read must still see it.
	if _, err := SelectByID(ctx, integrationPool, "01H000000000000000000PROD1"); err != nil {
		t.Errorf("SelectByID must stay unnarrowed for the worker: %v", err)
	}
}

// TestIntegration_VoyageList_TraitDimensionNarrows — the trait arm is the most
// involved predicate [rbac.PurviewSQL] can render (nested `jsonb_typeof` guards
// plus a `jsonb_array_elements` scan), and this subquery is a NEW place it runs:
// against `_s.traits` reached through a LEFT JOIN, where a missing host makes it
// NULL rather than false. Nothing else exercises it through this predicate.
func TestIntegration_VoyageList_TraitDimensionNarrows(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-test")
	if _, err := integrationPool.Exec(ctx, `DELETE FROM souls`); err != nil {
		t.Fatalf("clear souls: %v", err)
	}
	seedScopeSoulTraits(t, "gold-01", []string{}, []byte(`{"tier":"gold"}`))
	seedScopeSoulTraits(t, "bronze-01", []string{}, []byte(`{"tier":"bronze"}`))

	insert := func(id, resolved string) {
		v := &Voyage{
			VoyageID: id, Kind: KindCommand, Module: strptr("core.cmd.shell"),
			Input: []byte(`{}`), TargetResolved: json.RawMessage(resolved),
			Status: StatusPending, StartedByAID: "archon-test",
		}
		if err := Insert(ctx, integrationPool, v); err != nil {
			t.Fatalf("Insert(%s): %v", id, err)
		}
	}
	insert("01H0000000000000000000GOLD", `["gold-01"]`)
	insert("01H00000000000000000BRONZE", `["bronze-01"]`)
	insert("01H0000000000000000000MISS", `["never-registered"]`)

	ids, total := listScopedIDs(t, "trait.tier=gold")

	if len(ids) != 1 || ids[0] != "01H0000000000000000000GOLD" {
		t.Errorf("rows = %v, want only the gold-tier run", ids)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}
	for _, id := range ids {
		if id == "01H0000000000000000000MISS" {
			t.Errorf("a run against a host absent from souls passed the trait arm — " +
				"a NULL traits column must read as out of scope")
		}
	}
}
