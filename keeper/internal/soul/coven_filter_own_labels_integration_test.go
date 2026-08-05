//go:build integration

// Guard tests on a REAL Postgres for the coven axis (NIM-250 consistency,
// NIM-281 semantics): the operator-facing coven filter, the bulk selector and the
// bulk scope gate all resolve the host's OWN `souls.coven` — the same column the
// RBAC predicate that authorizes the call reads, and nothing else.
//
// A coven tag exists only where an operator attached it. Belonging to an
// incarnation attaches none: `coven=redis-prod` reaches the hosts somebody
// tagged `redis-prod`, never the members of the incarnation of that name.
// Membership is asked with `incarnation=redis-prod` and answered from
// `incarnation_membership` alone.
//
// Live PG rather than a fake: these guards are about WHICH relation each
// predicate reads, and a fake answering every coven question from one field
// cannot fail the way production did.
//
// Run:
//
//	SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -count=1 -p 1 ./internal/soul/

package soul

import (
	"context"
	"testing"
)

// seedIncarnationLabeled seeds an incarnation carrying declared coven tags
// (the plain seedIncarnationRow leaves `covens` empty).
func seedIncarnationLabeled(t *testing.T, name string, covens []string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO incarnation (name, service, service_version, status, covens)
		 VALUES ($1, 'redis', 'v1.0.0', 'ready', $2)`, name, covens); err != nil {
		t.Fatalf("seedIncarnationLabeled(%s): %v", name, err)
	}
}

func sidsOf(souls []*Soul) []string {
	out := make([]string, len(souls))
	for i, s := range souls {
		out[i] = s.SID
	}
	return out
}

// TestIntegration_ListFilter_CovenIsTheHostsOwnLabel — GUARD (NIM-281):
// `GET /v1/souls?coven=X` finds exactly the hosts an operator tagged X. Neither
// path that inheritance used to open counts: an incarnation's declared `covens[]`
// and an incarnation's NAME both reach nothing on their own.
func TestIntegration_ListFilter_CovenIsTheHostsOwnLabel(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "tagged.example.com", []string{"db", "eu-bastioned"})
	seedBulkSoul(t, "by-tag.example.com", []string{"db"})
	seedBulkSoul(t, "by-name.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-eu", []string{"eu-bastioned"})
	seedMembership(t, "redis-eu", "by-tag.example.com")
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "by-name.example.com")

	// The label an operator attached to the host — and only that host.
	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"eu-bastioned"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven=eu-bastioned): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "tagged.example.com" {
		t.Errorf("coven=eu-bastioned → total=%d %v, want 1 [tagged.example.com] — an incarnation's label must not reach its members",
			total, sidsOf(items))
	}

	// An incarnation's name is not a label at all.
	_, total, err = SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"redis-prod"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven=redis-prod): %v", err)
	}
	if total != 0 {
		t.Errorf("coven=redis-prod total = %d, want 0 — an incarnation's name is not a coven tag", total)
	}

	// A label nobody carries still matches nothing.
	_, total, err = SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"ghost"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven=ghost): %v", err)
	}
	if total != 0 {
		t.Errorf("coven=ghost total = %d, want 0", total)
	}
}

// TestIntegration_ListFilter_AgreesWithTheScopeThatAuthorizesIt — GUARD
// (NIM-250): filtering by the exact label the operator is scoped to must not
// hide anything the scope admits. The filter and the scope predicate are the same
// question asked twice; the moment they read different relations, `?coven=X`
// returns none of the hosts the same operator sees unfiltered — access granted,
// host unfindable, no error anywhere.
func TestIntegration_ListFilter_AgreesWithTheScopeThatAuthorizesIt(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "scoped.example.com", []string{"redis-prod"})
	seedBulkSoul(t, "outsider.example.com", []string{"db"})

	scope := covenScope("redis-prod")

	unfiltered, unfilteredTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope only): %v", err)
	}
	if unfilteredTotal != 1 || len(unfiltered) != 1 || unfiltered[0].SID != "scoped.example.com" {
		t.Fatalf("scope only → total=%d %v, want 1 [scoped.example.com]", unfilteredTotal, sidsOf(unfiltered))
	}

	filtered, filteredTotal, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"redis-prod"}}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope+filter): %v", err)
	}
	if filteredTotal != unfilteredTotal {
		t.Errorf("filtering by the scope's own label changed the result: %d → %d (%v)",
			unfilteredTotal, filteredTotal, sidsOf(filtered))
	}
}

// TestIntegration_CovenFilterIsLabel_IncarnationSelectorIsMembership — GUARD
// (NIM-281): the two selectors ask different questions and neither may answer the
// other.
//
//   - `coven=redis-prod` is a LABEL question: it matches the host carrying that
//     tag, and NOT the member of the incarnation of the same name.
//   - `incarnation=redis-prod` is a MEMBERSHIP question, answered from
//     `incarnation_membership` alone: it matches the member, and NOT the host
//     that merely carries a tag spelled like the incarnation.
func TestIntegration_CovenFilterIsLabel_IncarnationSelectorIsMembership(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedBulkSoul(t, "tagholder.example.com", []string{"redis-prod"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"redis-prod"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "tagholder.example.com" {
		t.Errorf("coven=redis-prod → total=%d %v, want 1 [tagholder.example.com] — the label is where an operator put it",
			total, sidsOf(items))
	}

	byMembership, err := CountBulkMatched(ctx, integrationPool,
		BulkSelector{Incarnation: "redis-prod"}, BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("CountBulkMatched(incarnation): %v", err)
	}
	if byMembership != 1 {
		t.Errorf("incarnation=redis-prod matched %d, want 1 — membership comes from the relation, never from a label",
			byMembership)
	}
}

// TestIntegration_BulkSelector_MatchesTheSameHostsAsTheListFilter — GUARD
// (NIM-250): the bulk selector reaches exactly the hosts the list filter shows. A
// selector matching a different set than the operator can see reports a `matched`
// count with nothing to explain it.
func TestIntegration_BulkSelector_MatchesTheSameHostsAsTheListFilter(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "tagged.example.com", []string{"eu-bastioned"})
	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{"eu-bastioned"})
	seedMembership(t, "redis-prod", "member.example.com")

	for _, tc := range []struct {
		label string
		want  int
	}{
		{"eu-bastioned", 1}, // the host that carries it; not the incarnation's member
		{"redis-prod", 0},   // an incarnation's name is not a label
	} {
		n, err := CountBulkMatched(ctx, integrationPool,
			BulkSelector{Coven: tc.label}, BulkScope{Unrestricted: true})
		if err != nil {
			t.Fatalf("CountBulkMatched(coven=%s): %v", tc.label, err)
		}
		if n != tc.want {
			t.Errorf("selector coven=%s matched %d, want %d", tc.label, n, tc.want)
		}

		_, total, err := SelectAll(ctx, integrationPool,
			ListFilter{Covens: []string{tc.label}}, unrestrictedScope(), 0, 10)
		if err != nil {
			t.Fatalf("SelectAll(coven=%s): %v", tc.label, err)
		}
		if total != n {
			t.Errorf("coven=%s: list shows %d, bulk selects %d — the two must be one answer", tc.label, total, n)
		}
	}
}

// TestIntegration_BulkScopeGate_ReachesOnlyOwnLabelledHosts — GUARD (NIM-281):
// scope gate (a) admits exactly the hosts the operator's read scope admits. An
// operator scoped `coven=redis-prod` reaches the hosts tagged `redis-prod` and
// NOT the members of the incarnation of that name — a write must never travel
// further than the list that showed the operator what they were writing to.
//
// Gate (b) is unchanged and still applies: the label being attached must lie
// inside the operator's own scope.
func TestIntegration_BulkScopeGate_ReachesOnlyOwnLabelledHosts(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "tagged.example.com", []string{"redis-prod"})
	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	scope := BulkScope{Covens: []string{"redis-prod", "batch"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{All: true}, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1 — gate (a) reaches the tagged host alone",
			rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "tagged.example.com"); len(got) != 2 || got[0] != "batch" || got[1] != "redis-prod" {
		t.Errorf("coven = %v, want [batch redis-prod] — the label lands on the host's OWN column", got)
	}
	if got := covenOf(t, "member.example.com"); len(got) != 1 || got[0] != "db" {
		t.Errorf("member coven = %v, want [db] untouched — belonging to redis-prod is not carrying its name", got)
	}
}

// TestIntegration_BulkScopeGate_StillRefusesUnscopedHost — GUARD: a host that
// matches the operator's scope by none of its own tags stays untouched, and an
// empty scope still reaches nobody (fail-closed).
func TestIntegration_BulkScopeGate_StillRefusesUnscopedHost(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "mine.example.com", []string{"redis-prod"})
	seedBulkSoul(t, "foreign.example.com", []string{"mysql-prod"})

	scope := BulkScope{Covens: []string{"redis-prod", "batch"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{All: true}, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1 (only the redis-prod host)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "foreign.example.com"); len(got) != 1 || got[0] != "mysql-prod" {
		t.Errorf("foreign host coven = %v, want [mysql-prod] untouched", got)
	}

	// fail-closed: a restricted operator with no covens at all reaches nobody.
	empty, err := CountBulkMatched(ctx, integrationPool,
		BulkSelector{All: true}, BulkScope{Covens: nil})
	if err != nil {
		t.Fatalf("CountBulkMatched(empty scope): %v", err)
	}
	if empty != 0 {
		t.Errorf("empty scope matched %d, want 0 (fail-closed)", empty)
	}
}
