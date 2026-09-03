//go:build integration

// Guard tests for the roster-source filters of `GET /v1/souls` (NIM-371) on a REAL
// Postgres: `unassigned`, a repeatable `coven`, and `sid_prefix`. These three are what
// the create form's roster picker asks the registry for.
//
// THE invariant, and the reason these run against live PG rather than a fake: every
// one of them narrows WITHIN the caller's RBAC scope and can never widen it. The
// picker turns the souls list into a name-completion surface, so a filter that
// bypassed the scope predicate would quietly make the create form an enumerator of
// the whole park — the class NIM-148 (existence oracle) and NIM-202/203 ("visible ⟺
// grantable") are about. A fake answering scope questions from one field cannot fail
// the way a mis-placed WHERE clause does.
//
// Run:
//
//	SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -count=1 -p 1 ./internal/soul/

package soul

import (
	"context"
	"testing"
)

// TestIntegration_Unassigned_CannotWidenScope — THE security guard. An operator
// scoped to one coven asks for free souls; hosts outside that scope are free too, and
// must stay invisible.
//
// Regression this catches: `unassigned` applied as a filter that replaces rather than
// intersects the scope predicate. The symptom would not be an error — it would be a
// create form offering the SIDs of somebody else's idle fleet.
func TestIntegration_Unassigned_CannotWidenScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// Two free hosts, one inside the caller's scope and one outside it.
	seedBulkSoul(t, "free-mine.example.com", []string{"prod"})
	seedBulkSoul(t, "free-theirs.example.com", []string{"other-team"})

	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Unassigned: true}, covenScope("prod"), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(unassigned, scoped): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "free-mine.example.com" {
		t.Fatalf("unassigned+scope=prod → total=%d %v, want 1 [free-mine.example.com] — a filter must never widen the scope",
			total, sidsOf(items))
	}
}

// The same invariant for the other two filters. A prefix that matches a foreign host,
// and a coven the caller is not scoped to, both come back empty rather than
// confirming that the host exists.
func TestIntegration_PrefixAndCovenFilters_CannotWidenScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "node-mine.example.com", []string{"prod"})
	seedBulkSoul(t, "node-theirs.example.com", []string{"other-team"})

	scope := covenScope("prod")

	// A prefix matching BOTH hosts still yields only the one in scope.
	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{SIDPrefix: "node-"}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(sid_prefix, scoped): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "node-mine.example.com" {
		t.Errorf("sid_prefix=node- + scope=prod → total=%d %v, want 1 [node-mine.example.com]",
			total, sidsOf(items))
	}

	// A prefix matching ONLY the foreign host is empty — not "found, forbidden".
	_, total, err = SelectAll(ctx, integrationPool,
		ListFilter{SIDPrefix: "node-theirs"}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(foreign prefix): %v", err)
	}
	if total != 0 {
		t.Errorf("sid_prefix=node-theirs + scope=prod → total=%d, want 0 (the SID must not be confirmed)", total)
	}

	// Naming a coven the caller is not scoped to does not reach it either: the filter
	// is ANDed with the scope, so asking for `other-team` from a prod-scoped role is
	// an empty intersection.
	_, total, err = SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"other-team"}}, scope, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(foreign coven): %v", err)
	}
	if total != 0 {
		t.Errorf("coven=other-team + scope=prod → total=%d, want 0", total)
	}
}

// TestIntegration_Unassigned_IsMembershipNotLabels — `unassigned` asks about
// `incarnation_membership`, deliberately not about labels.
//
// Nothing stops an operator from tagging a host with an incarnation's name, so a
// label-based implementation would call a host with a stray self-attached tag
// "occupied" while a genuine member whose tags say nothing would look free. The
// picker would then offer a host that already runs a service — two services stacked
// on one host, from a form that looked correct.
func TestIntegration_Unassigned_IsMembershipNotLabels(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "member.example.com", []string{"db"})
	// Carries the incarnation's NAME as its own tag, but belongs to nothing.
	seedBulkSoul(t, "impostor.example.com", []string{"redis-prod"})
	seedIncarnationLabeled(t, "redis-prod", []string{})
	seedMembership(t, "redis-prod", "member.example.com")

	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Unassigned: true}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(unassigned): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "impostor.example.com" {
		t.Fatalf("unassigned → total=%d %v, want 1 [impostor.example.com]: membership decides, not labels",
			total, sidsOf(items))
	}
}

// A host freed by its incarnation going away is free again: the membership FK is
// ON DELETE CASCADE, and the filter reads the relation rather than a cached flag.
func TestIntegration_Unassigned_HostFreedByDestroyedIncarnation(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "recycled.example.com", []string{"db"})
	seedIncarnationLabeled(t, "redis-old", []string{})
	seedMembership(t, "redis-old", "recycled.example.com")

	_, total, err := SelectAll(ctx, integrationPool, ListFilter{Unassigned: true}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(unassigned, bound): %v", err)
	}
	if total != 0 {
		t.Fatalf("a bound host must not be offered as free, total=%d", total)
	}

	if _, err := integrationPool.Exec(ctx, `DELETE FROM incarnation WHERE id = 'redis-old'`); err != nil {
		t.Fatalf("delete incarnation: %v", err)
	}

	items, total, err := SelectAll(ctx, integrationPool, ListFilter{Unassigned: true}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(unassigned, freed): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "recycled.example.com" {
		t.Errorf("after the incarnation is gone → total=%d %v, want 1 [recycled.example.com]", total, sidsOf(items))
	}
}

// A repeatable `coven` is an ANY-of union, not an intersection: the create form
// declares a SET of covens and wants hosts in any of them.
func TestIntegration_MultipleCovens_MatchAnyOf(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "in-eu.example.com", []string{"eu"})
	seedBulkSoul(t, "in-us.example.com", []string{"us"})
	seedBulkSoul(t, "in-apac.example.com", []string{"apac"})

	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{Covens: []string{"eu", "us"}}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven=eu,us): %v", err)
	}
	if total != 2 {
		t.Fatalf("coven=eu&coven=us → total=%d %v, want 2 (ANY-of, not all-of)", total, sidsOf(items))
	}
	for _, s := range items {
		if s.SID == "in-apac.example.com" {
			t.Errorf("apac host matched a filter that did not name its coven: %v", sidsOf(items))
		}
	}
}

// The prefix is matched LITERALLY: a `%` an operator types finds a SID containing
// one, it does not turn autocomplete into a full scan. Without escaping, one
// keystroke would list the whole visible registry — and on the roster picker that
// reads as "these are your free hosts".
func TestIntegration_SIDPrefix_LikeMetacharactersAreLiteral(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "node-1.example.com", []string{"db"})
	seedBulkSoul(t, "node-2.example.com", []string{"db"})

	for _, prefix := range []string{"%", "_", "%node"} {
		_, total, err := SelectAll(ctx, integrationPool,
			ListFilter{SIDPrefix: prefix}, unrestrictedScope(), 0, 10)
		if err != nil {
			t.Fatalf("SelectAll(sid_prefix=%q): %v", prefix, err)
		}
		if total != 0 {
			t.Errorf("sid_prefix=%q → total=%d, want 0: LIKE metacharacters must match themselves", prefix, total)
		}
	}

	// The ordinary case still works.
	items, total, err := SelectAll(ctx, integrationPool,
		ListFilter{SIDPrefix: "node-1"}, unrestrictedScope(), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(sid_prefix=node-1): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "node-1.example.com" {
		t.Errorf("sid_prefix=node-1 → total=%d %v, want 1 [node-1.example.com]", total, sidsOf(items))
	}
}

// The filters compose: free AND in one of the declared covens AND matching the
// prefix, all inside the scope. This is the exact query the roster picker issues.
func TestIntegration_RosterPickerQuery_ComposesAllFilters(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedBulkSoul(t, "node-free-prod.example.com", []string{"prod"})    // the answer
	seedBulkSoul(t, "node-busy-prod.example.com", []string{"prod"})    // free? no
	seedBulkSoul(t, "node-free-dev.example.com", []string{"dev"})      // declared coven? no
	seedBulkSoul(t, "other-free-prod.example.com", []string{"prod"})   // prefix? no
	seedBulkSoul(t, "node-free-theirs.example.com", []string{"other"}) // in scope? no
	seedIncarnationLabeled(t, "redis-existing", []string{})
	seedMembership(t, "redis-existing", "node-busy-prod.example.com")

	items, total, err := SelectAll(ctx, integrationPool, ListFilter{
		Covens:     []string{"prod", "staging"},
		Unassigned: true,
		SIDPrefix:  "node-",
		Status:     StatusPending,
	}, covenScope("prod"), 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(roster picker query): %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].SID != "node-free-prod.example.com" {
		t.Fatalf("roster picker query → total=%d %v, want 1 [node-free-prod.example.com]", total, sidsOf(items))
	}
}
