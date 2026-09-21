package handlers

// Host-identity guards for the run-history read family (NIM-842).
//
// What is being defended: `GET /v1/voyages`, `/{id}`, `/{id}/targets`,
// `GET /v1/push-runs` and `GET /v1/push/{apply_id}` are gated by
// `incarnation.history` / `push.read` — rights that say nothing about hosts —
// and every one of them hands back host identity. A kind=command Voyage's
// `target_origin` names the covens and SIDs it was aimed at, `/targets` returns
// the resolved SIDs as `target_id`, and a push run's `inventory_sids` is a
// literal list of machines. An Archon holding bare `incarnation.history` and NO
// soul right at all could enumerate every host any command run had ever touched,
// including hosts in covens whose existence `GET /v1/souls` would have hidden.
//
// Every assertion below is on the STATEMENT, not on the rows. Twice deliberate:
// the fakes replay fixed rows whatever the WHERE says, so an assertion on the
// response cannot fail — and the leak being closed travels on `total`, which a
// Go post-filter would leave intact while the rows themselves disappeared.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// covenPurview — a `coven=<c>` boundary, the shape of a role scoped to one slice
// of the fleet.
func covenPurview(t *testing.T, coven string) rbac.Purview {
	t.Helper()
	return rbac.Purview{Exprs: []*rbac.ScopeExpr{mustScopeExpr("coven=" + coven)}}
}

// --- voyages ---

// TestVoyageList_HostScopeReachesTheCountAsWellAsThePage is the counter guard.
// The predicate must be rendered into the COUNT query too: a page filtered
// without its count hides the runs and still reports how many exist, which is
// the size of the command history on hosts the caller may not see.
func TestVoyageList_HostScopeReachesTheCountAsWellAsThePage(t *testing.T) {
	store := &fakeVoyageStore{listCount: 7}
	scoper := &recordingScoper{pv: covenPurview(t, "dev")}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(), scoper)

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}

	for _, q := range []struct {
		name string
		sql  string
		args []any
	}{
		{"count", store.countSQL, store.countArgs},
		{"page", store.listSQL, store.listArgs},
	} {
		if !strings.Contains(q.sql, "jsonb_array_elements_text(voyages.target_resolved)") {
			t.Errorf("%s SQL does not walk the resolved targets: %q", q.name, q.sql)
		}
		if !strings.Contains(q.sql, "_s.coven && $") {
			t.Errorf("%s SQL carries no coven predicate: %q", q.name, q.sql)
		}
		if len(q.args) == 0 || !reflect.DeepEqual(q.args[len(q.args)-1], []string{"dev"}) {
			t.Errorf("%s bindings end %#v, want the purview's coven set []string{\"dev\"}", q.name, q.args)
		}
	}
}

// TestVoyageList_ScopeIsResolvedFromSoulList — the boundary comes from
// `soul.list`, the right that governs which hosts an operator may know about.
// Resolving the route's own `incarnation.history` instead would make the
// narrowing a no-op, since holding it is the precondition for being here.
func TestVoyageList_ScopeIsResolvedFromSoulList(t *testing.T) {
	store := &fakeVoyageStore{}
	scoper := &recordingScoper{pv: covenPurview(t, "dev")}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(), scoper)

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if scoper.resource != "soul" || scoper.action != "list" {
		t.Errorf("purview resolved for %q.%q, want soul.list", scoper.resource, scoper.action)
	}
}

// TestVoyageList_NoPurviewIsFailClosed — the ticket's exact failure scenario: an
// Archon with `incarnation.history` and NO soul right. The predicate must render
// FALSE for every target, hiding every command Voyage rather than showing all of
// them.
func TestVoyageList_NoPurviewIsFailClosed(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: rbac.Purview{}})

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	// The PER-TARGET predicate, spelled exactly. A bare Contains(sql, "FALSE")
	// would pass on any purview at all, because the malformed-row arm of
	// HostScopeSQL renders its own FALSE unconditionally.
	for name, sql := range map[string]string{"count": store.countSQL, "page": store.listSQL} {
		if !strings.Contains(sql, "WHERE (FALSE) IS NOT TRUE") {
			t.Errorf("%s SQL of an operator entitled to no host must render FALSE per "+
				"target, so every target fails and every command run is hidden: %q", name, sql)
		}
	}
}

// TestVoyageList_ScenarioRunsAreNotNarrowedByAHostRight — a kind=scenario Voyage
// names incarnations, not hosts, and `incarnation.history` is the right that
// governs it. Narrowing it by `soul.list` would deny an incarnation-scoped
// auditor the runs they are entitled to, which is a different bug with the same
// smell.
func TestVoyageList_ScenarioRunsAreNotNarrowedByAHostRight(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: rbac.Purview{}})

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if !strings.Contains(store.countSQL, "voyages.kind <> 'command'") {
		t.Errorf("the narrowing must exempt kind=scenario, which names no host: %q", store.countSQL)
	}
}

// TestVoyageList_UnrestrictedSeesEverything — a cluster-admin must not lose the
// list. Without this the guards above pass on a handler that hides everything.
func TestVoyageList_UnrestrictedSeesEverything(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: rbac.Purview{Unrestricted: true}})

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	// The per-target predicate, spelled exactly. Neither a bare Contains("TRUE")
	// nor a bare Contains("FALSE") discriminates here: `IS NOT TRUE` and the
	// malformed-row `FALSE` are in the statement whatever the purview. And the
	// bindings cannot discriminate either — an unrestricted purview and an empty
	// one both render a literal with no placeholders.
	if !strings.Contains(store.countSQL, "WHERE (TRUE) IS NOT TRUE") {
		t.Errorf("an unrestricted operator must render TRUE per target, so no target is "+
			"ever out of scope: %q", store.countSQL)
	}
	if got := len(store.countArgs); got != 4 {
		t.Errorf("count bindings = %d (%#v), want the 4 fixed ones and nothing more", got, store.countArgs)
	}
}

// TestVoyageGet_ScopeIsInTheWhereNotAPostCheck — the single-object read narrows
// in SQL, so "absent" and "out of the caller's boundary" are the same answer by
// construction rather than two branches that could drift apart.
func TestVoyageGet_ScopeIsInTheWhereNotAPostCheck(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: covenPurview(t, "dev")})

	// The id resolves to nothing in the fake; what matters is the statement.
	_, _ = h.GetTyped(context.Background(), claimsFor("archon-alice"), "01M2AGZV4FJ3FTZVFF5RKG9Q1P")

	if !strings.Contains(store.getSQL, "jsonb_array_elements_text(voyages.target_resolved)") {
		t.Errorf("get SQL does not narrow by the hosts the Voyage names: %q", store.getSQL)
	}
	if len(store.getArgs) < 2 {
		t.Fatalf("get bindings = %#v, want the id plus the purview's values", store.getArgs)
	}
}

// TestVoyageTargets_ProbesThroughTheScopedRead — /targets is the route that
// actually returns SIDs, so its existence-probe must be the narrowed one. An
// unnarrowed probe would 200 and then hand back the host list.
func TestVoyageTargets_ProbesThroughTheScopedRead(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: covenPurview(t, "dev")})

	_, _ = h.TargetsTyped(context.Background(), claimsFor("archon-alice"), "01M2AGZV4FJ3FTZVFF5RKG9Q1P")

	if !strings.Contains(store.getSQL, "_s.coven && $") {
		t.Errorf("the targets probe is not scope-narrowed: %q", store.getSQL)
	}
}

// TestVoyageRead_NullCovenCountsAsOutOfScope — the coven predicate is an array
// overlap, so a host missing from `souls` makes it NULL rather than false. Under
// a plain `NOT` that NULL would drop the target from the subquery and read as
// in-scope — fail-OPEN exactly where least is known. `IS NOT TRUE` is what makes
// the unknown case hide the run.
func TestVoyageRead_NullCovenCountsAsOutOfScope(t *testing.T) {
	store := &fakeVoyageStore{}
	h := newVoyageHandlerScoped(store, &fakeVoyageCommandResolver{}, allowAll(),
		&recordingScoper{pv: covenPurview(t, "dev")})

	if _, err := h.ListTyped(context.Background(), claimsFor("archon-alice"),
		VoyageListInput{Page: sharedapi.Page{Offset: 0, Limit: 50}}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if !strings.Contains(store.countSQL, "IS NOT TRUE") {
		t.Errorf("a NULL coven (host gone from the registry) must count as out of scope, "+
			"which plain NOT does not do: %q", store.countSQL)
	}
	if !strings.Contains(store.countSQL, "LEFT JOIN souls") {
		t.Errorf("the subquery must LEFT JOIN souls, so a deleted host yields NULL rather "+
			"than dropping the target: %q", store.countSQL)
	}
}

// --- push runs ---

// TestPushRead_ScopeIsResolvedFromSoulList — like the Voyage twin, the boundary
// is `soul.list`. `push.read` is what lets you read the run; `soul.list` is what
// decides whether the machines in its inventory are yours to know about.
func TestPushRead_ScopeIsResolvedFromSoulList(t *testing.T) {
	scoper := &recordingScoper{pv: covenPurview(t, "dev")}
	h := NewPushHandler(nil, scoper, nil)

	sql, args, _ := h.pushHostScope(claimsFor("archon-alice"))(1)

	if scoper.resource != "soul" || scoper.action != "list" {
		t.Errorf("purview resolved for %q.%q, want soul.list", scoper.resource, scoper.action)
	}
	if !strings.Contains(sql, "unnest(push_runs.inventory_sids)") {
		t.Errorf("predicate does not walk the inventory: %q", sql)
	}
	if len(args) == 0 || !reflect.DeepEqual(args[0], []string{"dev"}) {
		t.Errorf("bindings = %#v, want the purview's coven set []string{\"dev\"}", args)
	}
}

// TestPushRead_NoScoperIsFailClosed — an unconfigured resolver hides every run
// rather than showing all of them. This direction is the one that matters: the
// opposite default would leave any deployment that forgets the wiring unscoped,
// which is indistinguishable from never having made the fix.
func TestPushRead_NoScoperIsFailClosed(t *testing.T) {
	h := NewPushHandler(nil, nil /*scoper*/, nil)

	sql, _, _ := h.pushHostScope(claimsFor("archon-alice"))(1)

	if !strings.Contains(sql, "FALSE") {
		t.Errorf("a nil resolver must render FALSE per host, hiding every run: %q", sql)
	}
}

// fakePushReader captures what the handler asks the orchestrator for — the
// filter it hands to ListRows and the predicate it hands to GetRowScoped. Those
// two handoffs ARE the fix: [PushHandler.pushHostScope] rendering a correct
// predicate protects nothing if the handler never passes it down.
type fakePushReader struct {
	gotFilter    pushorch.ListFilter
	gotHostScope func(startIdx int) (string, []any, int)
}

func (f *fakePushReader) ListRows(_ context.Context, filter pushorch.ListFilter, _, _ int) ([]*pushorch.PushRunRow, int, error) {
	f.gotFilter = filter
	return nil, 0, nil
}

func (f *fakePushReader) GetRowScoped(_ context.Context, _ string, hostScope func(startIdx int) (string, []any, int)) (*pushorch.PushRunRow, error) {
	f.gotHostScope = hostScope
	return nil, pushorch.ErrNotFound
}

// TestPushListRuns_HandsTheScopeToTheStore — the list must reach the store
// CARRYING the boundary. Asserting only that pushHostScope renders well would
// pass on a handler that builds the predicate and then drops it.
func TestPushListRuns_HandsTheScopeToTheStore(t *testing.T) {
	reader := &fakePushReader{}
	h := NewPushHandlerWithReader(reader, &recordingScoper{pv: covenPurview(t, "dev")}, nil)

	if _, err := h.ListRunsTyped(context.Background(), claimsFor("archon-alice"), nil, "", 0, 50); err != nil {
		t.Fatalf("ListRunsTyped: %v", err)
	}
	if reader.gotFilter.HostScope == nil {
		t.Fatal("the store was asked for a page with NO host scope — every run of every host")
	}
	sql, args, _ := reader.gotFilter.HostScope(1)
	if !strings.Contains(sql, "unnest(push_runs.inventory_sids)") {
		t.Errorf("filter predicate does not walk the inventory: %q", sql)
	}
	if len(args) == 0 || !reflect.DeepEqual(args[0], []string{"dev"}) {
		t.Errorf("filter bindings = %#v, want the purview's coven set []string{\"dev\"}", args)
	}
}

// TestPushGet_HandsTheScopeToTheStore — the single-object twin of the above.
func TestPushGet_HandsTheScopeToTheStore(t *testing.T) {
	reader := &fakePushReader{}
	h := NewPushHandlerWithReader(reader, &recordingScoper{pv: covenPurview(t, "dev")}, nil)

	_, _ = h.GetTyped(context.Background(), claimsFor("archon-alice"), "01M2AGZV4FJ3FTZVFF5RKG9Q1P")

	if reader.gotHostScope == nil {
		t.Fatal("the store was asked for a run with NO host scope — the whole inventory of any run")
	}
	sql, _, _ := reader.gotHostScope(2)
	if !strings.Contains(sql, "unnest(push_runs.inventory_sids)") {
		t.Errorf("get predicate does not walk the inventory: %q", sql)
	}
}

// --- cadence child runs: the side door ---

// TestCadenceRuns_NarrowsLikeTheVoyageList — `GET /v1/cadences/{id}/runs` lists
// child Voyages through the SAME voyageDTO under the SAME `incarnation.history`
// gate as `GET /v1/voyages`. Narrowing one and not the other would leave the
// host identity reachable one route over, which is what this diff found in its
// own first cut.
//
// Both handlers build the predicate through [voyageHostScopeFor], so the guard
// is that the boundary reaches the query at all — two copies of the formula
// would be two boundaries, and the looser one would win.
func TestCadenceRuns_NarrowsLikeTheVoyageList(t *testing.T) {
	store := scenarioStore()
	scoper := &recordingScoper{pv: covenPurview(t, "dev")}
	h := NewCadenceHandler(store, nil, nil, allowAll(), scoper, nil, nil, nil, 0, nil)

	_, err := h.RunsTyped(context.Background(), claimsFor("archon-alice"),
		audit.NewULID(), nil, 0, 50)
	if err != nil {
		t.Fatalf("RunsTyped: %v", err)
	}
	if scoper.resource != "soul" || scoper.action != "list" {
		t.Errorf("purview resolved for %q.%q, want soul.list", scoper.resource, scoper.action)
	}
	for name, sql := range map[string]string{"count": store.voyageCountSQL, "page": store.voyageListSQL} {
		if !strings.Contains(sql, "jsonb_array_elements_text(voyages.target_resolved)") {
			t.Errorf("%s SQL of the cadence-runs route is not narrowed: %q", name, sql)
		}
	}
}
