//go:build integration

// Integration tests for the global run read-view (GET /v1/runs + /v1/runs/stats):
// aggregation of apply_runs by apply_id ACROSS ALL incarnations, SQL form of
// AggregateRunStatus, status/incarnation filters, Purview-scope subquery, and
// summary counters. Uses harness from integration_test.go (testcontainers PG).

package applyrun

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// unrestrictedScope is an unrestricted scope (Unrestricted operator).
var unrestrictedScope = incarnation.ListScope{Unrestricted: true}

// seedRun is a helper: one run with applyID in incarnation inc with a given SET of
// host statuses (host-0..host-N). Terminal statuses are set via UpdateStatus
// (Insert accepts only planned/running).
func seedRun(t *testing.T, ctx context.Context, applyID, inc, scenario string, statuses ...Status) {
	t.Helper()
	aid := "archon-alice"
	for i, st := range statuses {
		sid := "host-" + string(rune('a'+i)) + "-" + applyID
		mustInsertRun(t, ctx, applyID, sid, inc, scenario, StatusRunning, &aid)
		if st == StatusRunning {
			continue
		}
		if err := UpdateStatus(ctx, integrationPool, applyID, sid, 0, st, nil); err != nil {
			t.Fatalf("seedRun UpdateStatus(%s/%s→%s): %v", applyID, sid, st, err)
		}
	}
}

// TestIntegration_ListRuns_AcrossIncarnations tests global aggregation across incarnations:
// SQL-CASE for aggregate status matches [AggregateRunStatus] on all four values,
// Incarnation is populated, order is newest first.
func TestIntegration_ListRuns_AcrossIncarnations(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HGSUC", "redis-prod", "create", StatusSuccess, StatusNoMatch)
	seedRun(t, ctx, "01HGAPP", "redis-staging", "restart", StatusSuccess, StatusRunning)
	seedRun(t, ctx, "01HGFAI", "redis-staging", "create", StatusFailed, StatusCancelled)
	seedRun(t, ctx, "01HGCAN", "redis-prod", "scale", StatusSuccess, StatusCancelled)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if total != 4 || len(runs) != 4 {
		t.Fatalf("total=%d len=%d, want 4/4", total, len(runs))
	}

	want := map[string]struct {
		inc    string
		status RunStatus
	}{
		"01HGSUC": {"redis-prod", RunStatusSuccess},
		"01HGAPP": {"redis-staging", RunStatusApplying},
		"01HGFAI": {"redis-staging", RunStatusFailed},
		"01HGCAN": {"redis-prod", RunStatusCancelled},
	}
	for _, r := range runs {
		w, ok := want[r.ApplyID]
		if !ok {
			t.Errorf("unexpected apply_id %q", r.ApplyID)
			continue
		}
		if r.Incarnation != w.inc {
			t.Errorf("%s: Incarnation = %q, want %q", r.ApplyID, r.Incarnation, w.inc)
		}
		if r.Status != w.status {
			t.Errorf("%s: Status = %q, want %q (SQL-CASE ≠ AggregateRunStatus)", r.ApplyID, r.Status, w.status)
		}
		if r.StartedAt.IsZero() {
			t.Errorf("%s: StartedAt zero", r.ApplyID)
		}
	}
	// Newest first: started_at is non-increasing from top to bottom.
	for i := 1; i < len(runs); i++ {
		if runs[i].StartedAt.After(runs[i-1].StartedAt) {
			t.Errorf("order violated: runs[%d].StartedAt (%v) is after runs[%d] (%v)",
				i, runs[i].StartedAt, i-1, runs[i-1].StartedAt)
		}
	}
}

// TestIntegration_ListRuns_StatusFilter - filter by aggregate run status:
// total/page are coherent with the filter (SQL filter, not Go post-filter).
func TestIntegration_ListRuns_StatusFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HFS01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HFS02", "redis-prod", "create", StatusFailed)
	seedRun(t, ctx, "01HFS03", "redis-prod", "create", StatusSuccess, StatusOrphaned)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Status: RunStatusFailed}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(status=failed): %v", err)
	}
	if total != 2 || len(runs) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2 (failed + orphaned run)", total, len(runs))
	}
	for _, r := range runs {
		if r.Status != RunStatusFailed {
			t.Errorf("%s: Status = %q, want failed", r.ApplyID, r.Status)
		}
	}
}

// TestIntegration_ListRuns_IncarnationFilter - filter by incarnation name.
func TestIntegration_ListRuns_IncarnationFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HIF01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HIF02", "redis-staging", "create", StatusSuccess)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Incarnation: "redis-staging"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation=redis-staging): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HIF02" {
		t.Fatalf("got total=%d runs=%+v, want only 01HIF02", total, runs)
	}
}

// TestIntegration_ListRuns_Paging - total counts ALL runs, page uses limit/offset.
func TestIntegration_ListRuns_Paging(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, id := range []string{"01HPG01", "01HPG02", "01HPG03"} {
		seedRun(t, ctx, id, "redis-prod", "create", StatusSuccess)
	}
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{}, unrestrictedScope, 0, 2)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if total != 3 || len(runs) != 2 {
		t.Errorf("total=%d len=%d, want 3/2", total, len(runs))
	}
}

// TestIntegration_ListRuns_Scope - Purview-scope subquery: the coven union {name}
// dimension narrows the result to visible incarnations; empty scope (not
// Unrestricted) fails closed to empty; covens[] labels also match.
func TestIntegration_ListRuns_Scope(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	// redis-staging receives env tag team-x (covens[] side of scope).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET covens = ARRAY['team-x'] WHERE name = 'redis-staging'`); err != nil {
		t.Fatalf("UPDATE covens: %v", err)
	}

	seedRun(t, ctx, "01HSC01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSC02", "redis-staging", "create", StatusSuccess)

	// scope by incarnation NAME (coven union {name}): only redis-prod is visible.
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{}, incarnation.ListScope{Covens: []string{"redis-prod"}}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(scope name): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].Incarnation != "redis-prod" {
		t.Fatalf("scope name: total=%d runs=%+v, want only redis-prod", total, runs)
	}

	// scope by covens[] label: only redis-staging is visible.
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{}, incarnation.ListScope{Covens: []string{"team-x"}}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(scope coven): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].Incarnation != "redis-staging" {
		t.Fatalf("scope coven: total=%d runs=%+v, want only redis-staging", total, runs)
	}

	// fail-closed: empty scope (not Unrestricted) -> no runs.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{}, incarnation.ListScope{}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(empty scope): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("empty scope: total=%d len=%d, want 0/0 (fail-closed)", total, len(runs))
	}
}

// TestIntegration_SelectRunsStats - summary counters: total + each aggregate
// status, across all time and the last 24 hours (old run falls out of the 24h bucket).
func TestIntegration_SelectRunsStats(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HST01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HST02", "redis-staging", "create", StatusSuccess)
	seedRun(t, ctx, "01HST03", "redis-prod", "create", StatusFailed)
	seedRun(t, ctx, "01HST04", "redis-staging", "restart", StatusRunning)
	seedRun(t, ctx, "01HST05", "redis-prod", "scale", StatusCancelled)

	// 01HST02 - older than 24 hours: present in All, absent from Last24h.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE apply_runs SET started_at = now() - interval '2 days' WHERE apply_id = '01HST02'`); err != nil {
		t.Fatalf("UPDATE started_at: %v", err)
	}

	stats, err := SelectRunsStats(ctx, integrationPool, unrestrictedScope)
	if err != nil {
		t.Fatalf("SelectRunsStats: %v", err)
	}
	wantAll := RunsStatusCounts{Total: 5, Applying: 1, Success: 2, Failed: 1, Cancelled: 1}
	if stats.All != wantAll {
		t.Errorf("All = %+v, want %+v", stats.All, wantAll)
	}
	want24h := RunsStatusCounts{Total: 4, Applying: 1, Success: 1, Failed: 1, Cancelled: 1}
	if stats.Last24h != want24h {
		t.Errorf("Last24h = %+v, want %+v", stats.Last24h, want24h)
	}
}

// TestIntegration_ListRuns_Sort - sorting by a whitelisted column on real PG
// (ADR-068 §B1): column order in both directions, stable tie-break by apply_id DESC
// for equal values, NULLS LAST for finished_at (applying run at the end), total is
// independent of sort.
func TestIntegration_ListRuns_Sort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	// apply_id values increase lexicographically A<B<C<D -> tie-break apply_id DESC gives
	// D,C,B,A. Two runs in redis-prod (A,B) and redis-staging (C,D); D is applying
	// (running host, finished_at IS NULL).
	seedRun(t, ctx, "01HSORTA", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSORTB", "redis-prod", "restart", StatusSuccess)
	seedRun(t, ctx, "01HSORTC", "redis-staging", "create", StatusSuccess)
	seedRun(t, ctx, "01HSORTD", "redis-staging", "scale", StatusRunning)

	ids := func(runs []RunSummary) []string {
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ApplyID
		}
		return out
	}
	eq := func(got, want []string) bool {
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

	// incarnation ASC: redis-prod < redis-staging; inside the group tie-break is
	// apply_id DESC -> [B,A, D,C].
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "incarnation", SortDir: "asc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation asc): %v", err)
	}
	if total != 4 {
		t.Errorf("incarnation asc: total=%d, want 4", total)
	}
	if want := []string{"01HSORTB", "01HSORTA", "01HSORTD", "01HSORTC"}; !eq(ids(runs), want) {
		t.Errorf("incarnation asc: order %v, want %v", ids(runs), want)
	}

	// incarnation DESC: redis-staging first -> [D,C, B,A].
	runs, totalDesc, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "incarnation", SortDir: "desc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation desc): %v", err)
	}
	if want := []string{"01HSORTD", "01HSORTC", "01HSORTB", "01HSORTA"}; !eq(ids(runs), want) {
		t.Errorf("incarnation desc: order %v, want %v", ids(runs), want)
	}
	// total does not depend on sort direction (guard 4).
	if totalDesc != total {
		t.Errorf("total diverged when changing sort_dir: asc=%d desc=%d", total, totalDesc)
	}

	// finished_at: applying run (01HSORTD, finished_at IS NULL) is last in
	// BOTH directions (NULLS LAST).
	for _, dir := range []string{"asc", "desc"} {
		runs, _, err = ListRuns(ctx, integrationPool,
			RunsFilter{Sort: "finished_at", SortDir: dir}, unrestrictedScope, 0, 50)
		if err != nil {
			t.Fatalf("ListRuns(finished_at %s): %v", dir, err)
		}
		if got := ids(runs); len(got) != 4 || got[len(got)-1] != "01HSORTD" {
			t.Errorf("finished_at %s: applying run is not last (NULLS LAST): %v", dir, got)
		}
	}
}

// TestIntegration_SelectRunsStats_Scoped - counters within Purview-scope:
// only runs from visible incarnations.
func TestIntegration_SelectRunsStats_Scoped(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HSS01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSS02", "redis-staging", "create", StatusFailed)

	stats, err := SelectRunsStats(ctx, integrationPool,
		incarnation.ListScope{Covens: []string{"redis-prod"}})
	if err != nil {
		t.Fatalf("SelectRunsStats(scoped): %v", err)
	}
	want := RunsStatusCounts{Total: 1, Success: 1}
	if stats.All != want {
		t.Errorf("All = %+v, want %+v (other incarnation outside scope)", stats.All, want)
	}
}

// runIDs returns page apply_ids in output order (order comparison helper).
func runIDs(runs []RunSummary) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ApplyID
	}
	return out
}

// eqIDs compares apply_id order element by element.
func eqIDs(got, want []string) bool {
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

// TestIntegration_ListRuns_ServiceFilter - filter by owner incarnation service
// (JOIN incarnation): list and total are narrowed IDENTICALLY (shared subquery for
// count and list); projection carries service; nonexistent service -> empty.
func TestIntegration_ListRuns_ServiceFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice") // service redis (seed default)
	seedIncarnation(t, "pg-main", "archon-alice")
	ctx := context.Background()

	// pg-main -> service postgres (seedIncarnation sets redis by default).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET service = 'postgres' WHERE name = 'pg-main'`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	seedRun(t, ctx, "01HSVC01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSVC02", "pg-main", "create", StatusSuccess)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Service: "redis"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=redis): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HSVC01" {
		t.Fatalf("service=redis: total=%d runs=%+v, want only 01HSVC01", total, runs)
	}
	if runs[0].Service != "redis" {
		t.Errorf("Service = %q, want redis (service projection)", runs[0].Service)
	}

	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{Service: "postgres"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=postgres): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HSVC02" || runs[0].Service != "postgres" {
		t.Fatalf("service=postgres: total=%d runs=%+v, want only 01HSVC02/postgres", total, runs)
	}

	// Nonexistent service - list and total are both 0.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{Service: "mongo"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=mongo): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("service=mongo: total=%d len=%d, want 0/0", total, len(runs))
	}
}

// TestIntegration_ListRuns_QFilter - free-text search q matches 4 fields
// (incarnation/scenario/service/started_by), case-insensitively (ILIKE); list and
// total are consistent; LIKE metacharacter '%' is escaped (literal, not wildcard).
func TestIntegration_ListRuns_QFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedOperator(t, "archon-zephyr")
	seedIncarnation(t, "alpha-inc", "archon-alice")
	seedIncarnation(t, "beta-inc", "archon-alice")
	seedIncarnation(t, "gamma-inc", "archon-alice")
	seedIncarnation(t, "delta-inc", "archon-alice")
	ctx := context.Background()

	// gamma-inc -> unique service mysvc (others have service='redis').
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET service = 'mysvc' WHERE name = 'gamma-inc'`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	seedRun(t, ctx, "01HQAAA", "alpha-inc", "create", StatusSuccess)    // token in incarnation
	seedRun(t, ctx, "01HQBBB", "beta-inc", "restartxyz", StatusSuccess) // token in scenario
	seedRun(t, ctx, "01HQCCC", "gamma-inc", "create", StatusSuccess)    // token in service
	aidZ := "archon-zephyr"
	mustInsertRun(t, ctx, "01HQDDD", "host-z", "delta-inc", "create", StatusRunning, &aidZ) // token in started_by

	cases := []struct{ q, want string }{
		{"alpha", "01HQAAA"},      // incarnation
		{"restartxyz", "01HQBBB"}, // scenario
		{"mysvc", "01HQCCC"},      // service
		{"zephyr", "01HQDDD"},     // started_by
		{"ALPHA", "01HQAAA"},      // case-insensitive (ILIKE)
	}
	for _, c := range cases {
		runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: c.q}, unrestrictedScope, 0, 50)
		if err != nil {
			t.Fatalf("ListRuns(q=%q): %v", c.q, err)
		}
		got := ""
		if len(runs) == 1 {
			got = runs[0].ApplyID
		}
		if total != 1 || len(runs) != 1 || got != c.want {
			t.Errorf("q=%q: total=%d len=%d got=%q, want exactly %s", c.q, total, len(runs), got, c.want)
		}
	}

	// '%' in q is literal (escaped), not a wildcard that matches everything.
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: "%"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(q=%%): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("q=%%: total=%d len=%d, want 0/0 (metacharacter is escaped)", total, len(runs))
	}
}

// TestIntegration_ListRuns_QScopeBinding - REGRESSION (security): free-text search q
// is AND-bound to Purview-scope (parenthesized OR group). Two incarnations have runs
// whose scenario matches ONE q term; scope is limited to one. q=<term> under scope ->
// ONLY the in-scope run is visible, out-of-scope does NOT leak, total counts only
// in-scope. Catches lost parentheses: `scope AND a OR b OR c OR d` = leak around scope.
func TestIntegration_ListRuns_QScopeBinding(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "in-scope-inc", "archon-alice")
	seedIncarnation(t, "out-scope-inc", "archon-alice")
	ctx := context.Background()

	// Both incarnations: run with scenario matching the same q term.
	seedRun(t, ctx, "01HQBIN", "in-scope-inc", "sharedterm", StatusSuccess)
	seedRun(t, ctx, "01HQBOUT", "out-scope-inc", "sharedterm", StatusSuccess)

	// Limited scope (coven union {name}) - only in-scope-inc is visible.
	scope := incarnation.ListScope{Covens: []string{"in-scope-inc"}}
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: "sharedterm"}, scope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(q under limited scope): %v", err)
	}
	// q match of an out-of-scope run must NOT bypass scope: exactly one in-scope run.
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HQBIN" {
		t.Fatalf("q under scope: total=%d runs=%v, want exactly 01HQBIN (out-of-scope leaked past Purview?)",
			total, runIDs(runs))
	}
	if runs[0].Incarnation != "in-scope-inc" {
		t.Errorf("run leaked outside Purview-scope: %+v", runs[0])
	}
}

// TestIntegration_ListRuns_TimeRange - filter by run START time range
// (aggregate MIN(started_at)) in outerWhere: boundaries are inclusive, list and total
// narrow IDENTICALLY (shared sub+outerWhere), stable tie-break by apply_id DESC for
// equal started_at.
func TestIntegration_ListRuns_TimeRange(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return tm
	}
	// 4 runs; set started_at explicitly. B and C share time t2 (tie-break).
	seedRun(t, ctx, "01HTMA", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMB", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMC", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMD", "redis-prod", "create", StatusSuccess)
	tt1, tt2, tt3 := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-06-01T00:00:00Z"), mustTime("2026-12-01T00:00:00Z")
	setStart := func(applyID string, ts time.Time) {
		if _, err := integrationPool.Exec(ctx,
			`UPDATE apply_runs SET started_at = $1 WHERE apply_id = $2`, ts, applyID); err != nil {
			t.Fatalf("UPDATE started_at %s: %v", applyID, err)
		}
	}
	setStart("01HTMA", tt1)
	setStart("01HTMB", tt2)
	setStart("01HTMC", tt2)
	setStart("01HTMD", tt3)

	// started_after=t2 (inclusive): {t2,t2,t3} = B,C,D. Default sort started_at DESC,
	// tie-break apply_id DESC inside t2 -> [D, C, B].
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{StartedAfter: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(after=t2): %v", err)
	}
	if total != 3 || len(runs) != 3 {
		t.Fatalf("after=t2: total=%d len=%d, want 3/3 (inclusive boundary)", total, len(runs))
	}
	if want := []string{"01HTMD", "01HTMC", "01HTMB"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("after=t2: order %v, want %v (tie-break apply_id DESC for equal started_at)", runIDs(runs), want)
	}

	// started_before=t2 (inclusive): {t1,t2,t2} = A,B,C; total=3.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{StartedBefore: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(before=t2): %v", err)
	}
	if total != 3 || len(runs) != 3 {
		t.Fatalf("before=t2: total=%d len=%d, want 3/3 (inclusive boundary)", total, len(runs))
	}

	// Range [t2,t2] - exactly runs with started_at == t2 (both boundaries inclusive):
	// C,B; list and total are identical.
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tt2, StartedBefore: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns([t2,t2]): %v", err)
	}
	if total != 2 || len(runs) != 2 {
		t.Fatalf("[t2,t2]: total=%d len=%d, want 2/2 (both boundaries inclusive)", total, len(runs))
	}
	if want := []string{"01HTMC", "01HTMB"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("[t2,t2]: order %v, want %v", runIDs(runs), want)
	}

	// Empty range (after later than before) - harmlessly empty (after>before is not
	// strictly validated at handler level).
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tt3, StartedBefore: &tt1}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(after>before): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("after>before: total=%d len=%d, want 0/0 (empty output is harmless)", total, len(runs))
	}
}

// TestIntegration_ListRuns_TimeScopeBinding - REGRESSION (security): time filter
// is AND-bound to Purview-scope and does not bypass it. Symmetric to
// [TestIntegration_ListRuns_QScopeBinding], but the user filter is a time range,
// not q. The key bind-parameter layout difference: scope lives in subquery `sub`
// (BEFORE GROUP BY, $1), while time is in `outerWhere` (AFTER GROUP BY, $2).
// Pins that positions do not drift and time does not carry an out-of-scope run
// past scope. Three runs under limited scope at different times:
//   - in-scope + in window     -> only visible one;
//   - out-of-scope + in window -> cut off by scope ($1), NOT a time-filter leak;
//   - in-scope + before window -> cut off by lower time bound ($2), proving
//     the time predicate is actually active, not a no-op under scope.
func TestIntegration_ListRuns_TimeScopeBinding(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "in-scope-inc", "archon-alice")
	seedIncarnation(t, "out-scope-inc", "archon-alice")
	ctx := context.Background()

	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return tm
	}
	setStart := func(applyID string, ts time.Time) {
		if _, err := integrationPool.Exec(ctx,
			`UPDATE apply_runs SET started_at = $1 WHERE apply_id = $2`, ts, applyID); err != nil {
			t.Fatalf("UPDATE started_at %s: %v", applyID, err)
		}
	}
	// Window is StartedAfter=tWindow (inclusive); tBefore is safely before the window.
	tBefore, tWindow := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-06-01T00:00:00Z")

	seedRun(t, ctx, "01HTSIN", "in-scope-inc", "create", StatusSuccess)   // in-scope, in window
	seedRun(t, ctx, "01HTSOUT", "out-scope-inc", "create", StatusSuccess) // out-of-scope, in window
	seedRun(t, ctx, "01HTSOLD", "in-scope-inc", "create", StatusSuccess)  // in-scope, before window
	setStart("01HTSIN", tWindow)
	setStart("01HTSOUT", tWindow)
	setStart("01HTSOLD", tBefore)

	// Limited scope (coven union {name}) - only in-scope-inc is visible; the window
	// cuts off everything before tWindow.
	scope := incarnation.ListScope{Covens: []string{"in-scope-inc"}}
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tWindow}, scope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(time under limited scope): %v", err)
	}
	// (1) out-of-scope run in the window does NOT bypass scope through time;
	// (2) total counts ONLY in-scope runs in the window (out-of-scope and
	// in-scope-before-window are not counted).
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HTSIN" {
		t.Fatalf("time under scope: total=%d runs=%v, want exactly 01HTSIN (out-of-scope leaked past Purview through time?)",
			total, runIDs(runs))
	}
	if runs[0].Incarnation != "in-scope-inc" {
		t.Errorf("run leaked outside Purview-scope through time filter: %+v", runs[0])
	}
}

// TestIntegration_ListRuns_SortService - sorting by service (ADR-068 whitelist):
// service order in both directions, stable tie-break by apply_id DESC for equal
// service; total does not depend on direction.
func TestIntegration_ListRuns_SortService(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "inc-a", "archon-alice")
	seedIncarnation(t, "inc-b", "archon-alice")
	seedIncarnation(t, "inc-c", "archon-alice")
	ctx := context.Background()

	// inc-a,inc-b -> service 'aaa' (equal -> tie-break apply_id DESC); inc-c -> 'bbb'.
	if _, err := integrationPool.Exec(ctx, `
		UPDATE incarnation SET service = CASE name
			WHEN 'inc-c' THEN 'bbb' ELSE 'aaa' END
		WHERE name IN ('inc-a','inc-b','inc-c')`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	// apply_id AA<AB<AC -> tie-break apply_id DESC inside service='aaa' gives AB,AA.
	seedRun(t, ctx, "01HSVAA", "inc-a", "create", StatusSuccess) // service aaa
	seedRun(t, ctx, "01HSVAB", "inc-b", "create", StatusSuccess) // service aaa
	seedRun(t, ctx, "01HSVAC", "inc-c", "create", StatusSuccess) // service bbb

	// service ASC: aaa (tie-break AB,AA), then bbb (AC).
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "service", SortDir: "asc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service asc): %v", err)
	}
	if total != 3 {
		t.Errorf("service asc: total=%d, want 3", total)
	}
	if want := []string{"01HSVAB", "01HSVAA", "01HSVAC"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("service asc: order %v, want %v", runIDs(runs), want)
	}

	// service DESC: bbb (AC) first, then aaa (tie-break AB,AA).
	runs, totalDesc, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "service", SortDir: "desc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service desc): %v", err)
	}
	if want := []string{"01HSVAC", "01HSVAB", "01HSVAA"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("service desc: order %v, want %v", runIDs(runs), want)
	}
	if totalDesc != total {
		t.Errorf("total diverged when changing sort_dir: asc=%d desc=%d", total, totalDesc)
	}
}
