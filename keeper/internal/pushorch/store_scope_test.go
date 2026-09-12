package pushorch

// Host-boundary guards for the push-run read path (NIM-842).
//
// `GET /v1/push/{apply_id}` returns `inventory_sids` — a literal list of the
// machines an SSH delivery touched — under `push.read`, and `GET /v1/push-runs`
// lists the runs under `incarnation.history`. Neither right says anything about
// which hosts the caller may know exist, so both are narrowed by the operator's
// `soul.list` purview, pushed into the query.
//
// These assert the statement, not the rows: the fake replays what it is given
// regardless of the WHERE, and the leak being closed rides on `total`.

import (
	"context"
	"strings"
	"testing"
)

// covenScope stands in for a `coven=dev` purview rendered over the subquery's
// aliases — the shape [HostScopeSQL] is handed in production.
func covenScope(startIdx int) (string, []any, int) {
	return ScopeCovenColumn + " && $" + itoaScope(startIdx) + "::text[]",
		[]any{[]string{"dev"}}, startIdx + 1
}

func itoaScope(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoaScope(n/10) + string(rune('0'+n%10))
}

func hostScope(startIdx int) (string, []any, int) { return HostScopeSQL(covenScope, startIdx) }

// TestSelectAll_HostScopeReachesTheCountAsWellAsThePage is the counter guard. A
// page narrowed without its count hides the runs and still publishes how many
// exist, so the predicate must be rendered into both statements.
func TestSelectAll_HostScopeReachesTheCountAsWellAsThePage(t *testing.T) {
	db := &fakeStoreDB{countV: 3}
	s := NewStore(db)

	if _, _, err := s.SelectAll(context.Background(), ListFilter{HostScope: hostScope}, 0, 50); err != nil {
		t.Fatalf("SelectAll: %v", err)
	}

	for _, q := range []struct {
		name string
		sql  string
		args []any
	}{
		{"count", db.queryRowSQL, db.queryRowArgs},
		{"page", db.querySQL, db.queryArgs},
	} {
		if !strings.Contains(q.sql, "unnest(push_runs.inventory_sids)") {
			t.Errorf("%s SQL does not walk the inventory: %q", q.name, q.sql)
		}
		if !strings.Contains(q.sql, ScopeCovenColumn+" && $") {
			t.Errorf("%s SQL carries no coven predicate: %q", q.name, q.sql)
		}
		if len(q.args) == 0 {
			t.Fatalf("%s args are empty — the predicate was rendered without its bindings", q.name)
		}
	}
}

// TestSelectAll_ScopePlaceholdersFollowTheFixedOnes — the two statements carry a
// different number of fixed parameters ($1..$2 for the count, $1..$4 for the
// page), so the predicate is rendered against each one's own starting index. A
// shared index would bind the purview's values to `limit`/`offset` and either
// error or, worse, filter on the wrong column.
func TestSelectAll_ScopePlaceholdersFollowTheFixedOnes(t *testing.T) {
	db := &fakeStoreDB{countV: 1}
	s := NewStore(db)

	if _, _, err := s.SelectAll(context.Background(), ListFilter{HostScope: hostScope}, 7, 13); err != nil {
		t.Fatalf("SelectAll: %v", err)
	}

	if !strings.Contains(db.queryRowSQL, ScopeCovenColumn+" && $3") {
		t.Errorf("count predicate must start at $3 (after statuses, ssh_provider): %q", db.queryRowSQL)
	}
	if !strings.Contains(db.querySQL, ScopeCovenColumn+" && $5") {
		t.Errorf("page predicate must start at $5 (after statuses, ssh_provider, limit, offset): %q", db.querySQL)
	}
	if n := len(db.queryArgs); n != 5 {
		t.Errorf("page bindings = %d, want 4 fixed + 1 scope value", n)
	}
}

// TestSelectAll_ScopeIsAndedAfterEveryFilter — a caller narrowing by status or
// ssh_provider must not thereby reach a run outside their boundary.
func TestSelectAll_ScopeIsAndedAfterEveryFilter(t *testing.T) {
	db := &fakeStoreDB{countV: 1}
	s := NewStore(db)

	_, _, err := s.SelectAll(context.Background(), ListFilter{
		Statuses:    []PushRunStatus{StatusSuccess},
		SSHProvider: "openssh",
		HostScope:   hostScope,
	}, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}

	provider := strings.Index(db.queryRowSQL, "ssh_provider = $2")
	scope := strings.Index(db.queryRowSQL, "unnest(push_runs.inventory_sids)")
	if provider < 0 || scope < 0 {
		t.Fatalf("expected both the provider filter and the scope predicate: %q", db.queryRowSQL)
	}
	if scope < provider {
		t.Errorf("the scope predicate must be ANDed AFTER the filters: %q", db.queryRowSQL)
	}
}

// TestGetScoped_NarrowsInTheWhere — the single-object read narrows in SQL, so a
// run reaching outside the boundary comes back as ErrNotFound: the same answer an
// absent apply_id gets, by construction rather than by two branches agreeing.
func TestGetScoped_NarrowsInTheWhere(t *testing.T) {
	db := &fakeStoreDB{}
	s := NewStore(db)

	_, _ = s.GetScoped(context.Background(), "01M2AGZV4FJ3FTZVFF5RKG9Q1P", hostScope)

	if !strings.Contains(db.queryRowSQL, "unnest(push_runs.inventory_sids)") {
		t.Errorf("get SQL does not narrow by the inventory: %q", db.queryRowSQL)
	}
	if !strings.Contains(db.queryRowSQL, ScopeCovenColumn+" && $2") {
		t.Errorf("the predicate must start at $2, after the apply_id: %q", db.queryRowSQL)
	}
	if len(db.queryRowArgs) != 2 {
		t.Errorf("get bindings = %#v, want the apply_id plus the purview's values", db.queryRowArgs)
	}
}

// TestGet_StaysUnnarrowed — the orchestration read must still see every run.
// Narrowing it would make the worker's own bookkeeping depend on who last called
// the API, which is a failure mode with no operator in it at all.
func TestGet_StaysUnnarrowed(t *testing.T) {
	db := &fakeStoreDB{}
	s := NewStore(db)

	_, _ = s.Get(context.Background(), "01M2AGZV4FJ3FTZVFF5RKG9Q1P")

	if strings.Contains(db.queryRowSQL, "unnest(push_runs.inventory_sids)") {
		t.Errorf("Store.Get must not narrow — it serves the orchestrator: %q", db.queryRowSQL)
	}
	if len(db.queryRowArgs) != 1 {
		t.Errorf("get bindings = %#v, want the apply_id alone", db.queryRowArgs)
	}
}

// TestHostScopeSQL_UnknownHostCountsAsOutOfScope — the coven predicate is an
// array overlap, so a host missing from `souls` makes it NULL, not false. Under a
// plain `NOT` that NULL would drop the host from the subquery and read as
// in-scope: fail-OPEN precisely where least is known. `IS NOT TRUE` is the whole
// of the defence, and the LEFT JOIN is what produces the NULL rather than
// dropping the row outright.
func TestHostScopeSQL_UnknownHostCountsAsOutOfScope(t *testing.T) {
	sql, _, _ := HostScopeSQL(covenScope, 1)

	if !strings.Contains(sql, "IS NOT TRUE") {
		t.Errorf("an unresolvable host must count as out of scope: %q", sql)
	}
	if strings.Contains(sql, "WHERE NOT (") {
		t.Errorf("plain NOT reads a NULL predicate as in-scope: %q", sql)
	}
	if !strings.Contains(sql, "LEFT JOIN souls") {
		t.Errorf("the join must be LEFT, or a deleted host silently leaves the inventory: %q", sql)
	}
	if !strings.Contains(sql, "NOT EXISTS") {
		t.Errorf("visibility is ALL hosts in scope, not ANY: %q", sql)
	}
}
