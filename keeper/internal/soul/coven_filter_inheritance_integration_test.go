//go:build integration

// Guard tests for NIM-250 on a REAL Postgres: the operator-facing coven filter,
// the bulk selector and the bulk scope gate all resolve a host's EFFECTIVE coven
// labels (ADR-080), the same way the RBAC predicate that authorizes the call
// does.
//
// Live PG rather than a fake: the defect was in WHICH relation each predicate
// read, and a fake answering every coven question from one field cannot fail the
// way production did.
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

// TestIntegration_ListFilter_CovenResolvesInheritedLabels — GUARD (NIM-250):
// `GET /v1/souls?coven=X` finds hosts that carry X only by inheritance. Both
// paths ADR-080 defines are covered: the incarnation's declared `covens[]` and
// its NAME.
//
// Before NIM-250 the filter matched `souls.coven` alone, so an operator whose
// scope resolved the union could see a host and then fail to find it by the very
// label that made it visible.
func TestIntegration_ListFilter_CovenResolvesInheritedLabels(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "by-tag.example.com", []string{"db"})
	seedBulkSoul(t, "by-name.example.com", []string{"db"})
	seedBulkSoul(t, "unrelated.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-eu", []string{"eu-bastioned"})
	seedMembership(t, "redis-eu", "by-tag.example.com")
	// No declared covens — this host inherits purely through the NAME.
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "by-name.example.com")

	for _, tc := range []struct{ label, wantSID string }{
		{"eu-bastioned", "by-tag.example.com"}, // incarnation.covens[]
		{"redis-prod", "by-name.example.com"},  // incarnation.name
	} {
		items, total, err := SelectAll(ctx, integrationPool,
			ListFilter{Covens: []string{tc.label}}, unrestrictedScope(), 0, 10)
		if err != nil {
			t.Fatalf("SelectAll(coven=%s): %v", tc.label, err)
		}
		if total != 1 || len(items) != 1 || items[0].SID != tc.wantSID {
			t.Errorf("coven=%s → total=%d %v, want 1 [%s] (inherited label must be findable)",
				tc.label, total, sidsOf(items), tc.wantSID)
		}
	}

	// A label nobody carries, own or inherited, still matches nothing.
	_, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"ghost"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven=ghost): %v", err)
	}
	if total != 0 {
		t.Errorf("coven=ghost total = %d, want 0", total)
	}
}

// TestIntegration_ListFilter_AgreesWithTheScopeThatAuthorizesIt — GUARD
// (NIM-250): the defect stated as the operator sees it. Filtering by the exact
// label the operator is scoped to must not hide anything the scope admits.
//
// Before the fix the scope resolved inherited labels and the filter did not, so
// `?coven=redis-prod` returned ZERO of the hosts the same operator could see
// without a filter — access granted, host unfindable.
func TestIntegration_ListFilter_AgreesWithTheScopeThatAuthorizesIt(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedBulkSoul(t, "outsider.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	scope := covenScope("redis-prod")

	unfiltered, unfilteredTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope only): %v", err)
	}
	if unfilteredTotal != 1 || len(unfiltered) != 1 || unfiltered[0].SID != "member.example.com" {
		t.Fatalf("scope only → total=%d %v, want 1 [member.example.com]", unfilteredTotal, sidsOf(unfiltered))
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
// (NIM-250 / ADR-080): the two selectors ask different questions, and widening
// the coven one must not blur them.
//
//   - `coven=redis-prod` is a LABEL question: an impostor that carries that tag
//     on itself matches, because it genuinely carries the label — and so does a
//     real member, which inherits it.
//   - `incarnation=redis-prod` is a MEMBERSHIP question, answered from
//     `incarnation_membership` alone: the impostor does NOT match. Answering it
//     from the label union would let a host-attached tag pass for belonging.
func TestIntegration_CovenFilterIsLabel_IncarnationSelectorIsMembership(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedBulkSoul(t, "impostor.example.com", []string{"redis-prod"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"redis-prod"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven): %v", err)
	}
	if total != 2 {
		t.Errorf("coven=redis-prod → total=%d %v, want 2 (the member inherits it, the impostor carries it)",
			total, sidsOf(items))
	}

	byMembership, err := CountBulkMatched(ctx, integrationPool,
		BulkSelector{Incarnation: "redis-prod"}, BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("CountBulkMatched(incarnation): %v", err)
	}
	if byMembership != 1 {
		t.Errorf("incarnation=redis-prod matched %d, want 1 — membership must come from the relation, never from the label union",
			byMembership)
	}
}

// TestIntegration_BulkSelector_CovenResolvesInheritedLabels — GUARD (NIM-250):
// the bulk selector reaches the same hosts the list filter does. A selector that
// silently matched fewer hosts than the operator could see reported a short
// `matched` with nothing to explain it.
func TestIntegration_BulkSelector_CovenResolvesInheritedLabels(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedBulkSoul(t, "outsider.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{"eu-bastioned"})
	seedMembership(t, "redis-prod", "member.example.com")

	for _, label := range []string{"redis-prod", "eu-bastioned"} {
		n, err := CountBulkMatched(ctx, integrationPool,
			BulkSelector{Coven: label}, BulkScope{Unrestricted: true})
		if err != nil {
			t.Fatalf("CountBulkMatched(coven=%s): %v", label, err)
		}
		if n != 1 {
			t.Errorf("selector coven=%s matched %d, want 1 (the inherited label)", label, n)
		}
	}
}

// TestIntegration_BulkScopeGate_ReachesInheritedHosts — GUARD (NIM-250): scope
// gate (a) admits the hosts the operator's read scope admits. An operator scoped
// `coven=redis-prod` can label the hosts OF incarnation `redis-prod` — before,
// the write silently touched nothing while the list showed them.
//
// Gate (b) is unchanged and still applies: the label being attached must lie
// inside the operator's own scope.
func TestIntegration_BulkScopeGate_ReachesInheritedHosts(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	scope := BulkScope{Covens: []string{"redis-prod", "batch"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{All: true}, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1 — gate (a) must reach a host scoped by an inherited label",
			rep.Matched, rep.Changed)
	}
	got := covenOf(t, "member.example.com")
	if len(got) != 2 || got[0] != "batch" || got[1] != "db" {
		t.Errorf("coven = %v, want [batch db] — the label lands on the host's OWN column", got)
	}
}

// TestIntegration_BulkScopeGate_StillRefusesUnscopedHost — GUARD (NIM-250): the
// widening is bounded. A host that matches the operator's scope neither by its
// own tags nor through any incarnation stays untouched, and an empty scope still
// reaches nobody (fail-closed).
func TestIntegration_BulkScopeGate_StillRefusesUnscopedHost(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	seedBulkSoul(t, "foreign.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedIncarnationLabeled(t, "mysql-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")
	seedMembership(t, "mysql-prod", "foreign.example.com")

	scope := BulkScope{Covens: []string{"redis-prod", "batch"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{All: true}, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1 (only the redis-prod member)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "foreign.example.com"); len(got) != 1 || got[0] != "db" {
		t.Errorf("foreign host coven = %v, want [db] untouched — another incarnation is not the operator's scope", got)
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
