package applyrun

// Guard tests for sorting the global runs read-view (ADR-068 B1) on pure function
// buildRunsOrderBy: field whitelist + direction + stable tie-break apply_id DESC +
// NULLS LAST for finished_at + byte-exact default. Real PG row order is in
// runsglobal_integration_test.go.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// unrestricted is a scope with no restrictions (scope subquery not added, args clean).
var unrestricted = incarnation.ListScope{Unrestricted: true}

// defaultRunsOrderBy is the previous (pre-ADR-068) hardcoded ORDER BY for listSQL.
// The default sort must be byte-exact to it (guard 6: no sort parameters = no change).
const defaultRunsOrderBy = "started_at DESC, apply_id DESC"

func TestBuildRunsOrderBy_DefaultByteExact(t *testing.T) {
	got, err := buildRunsOrderBy("", "")
	if err != nil {
		t.Fatalf("buildRunsOrderBy(\"\",\"\"): %v", err)
	}
	if got != defaultRunsOrderBy {
		t.Errorf("default = %q, want byte-exact %q", got, defaultRunsOrderBy)
	}
}

// TestBuildRunsOrderBy_Columns verifies that each of 6 whitelisted columns in
// both directions produces the correct expression with stable tie-break (guards
// 1, 2, 5).
func TestBuildRunsOrderBy_Columns(t *testing.T) {
	cases := []struct {
		sort, dir string
		want      string
	}{
		{"started_at", "asc", "started_at ASC, apply_id DESC"},
		{"started_at", "desc", "started_at DESC, apply_id DESC"},
		{"finished_at", "asc", "finished_at ASC NULLS LAST, apply_id DESC"},
		{"finished_at", "desc", "finished_at DESC NULLS LAST, apply_id DESC"},
		{"status", "asc", "status ASC, apply_id DESC"},
		{"status", "desc", "status DESC, apply_id DESC"},
		{"incarnation", "asc", "incarnation ASC, apply_id DESC"},
		{"incarnation", "desc", "incarnation DESC, apply_id DESC"},
		{"service", "asc", "service ASC, apply_id DESC"},
		{"service", "desc", "service DESC, apply_id DESC"},
		{"scenario", "asc", "scenario ASC, apply_id DESC"},
		{"scenario", "desc", "scenario DESC, apply_id DESC"},
		// Defaults apply independently: empty field → started_at, empty
		// direction → desc.
		{"", "asc", "started_at ASC, apply_id DESC"},
		{"status", "", "status DESC, apply_id DESC"},
	}
	for _, c := range cases {
		got, err := buildRunsOrderBy(c.sort, c.dir)
		if err != nil {
			t.Errorf("buildRunsOrderBy(%q,%q): %v", c.sort, c.dir, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q, want %q", c.sort, c.dir, got, c.want)
		}
		// Tie-break is mandatory in all cases (stable pagination).
		if !strings.HasSuffix(got, ", apply_id DESC") {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q: no tie-break apply_id DESC", c.sort, c.dir, got)
		}
	}
}

// TestBuildRunsOrderBy_FinishedAtNullsLast verifies applying runs (finished_at IS NULL)
// go last regardless of direction (guard 5).
func TestBuildRunsOrderBy_FinishedAtNullsLast(t *testing.T) {
	for _, dir := range []string{"asc", "desc"} {
		got, err := buildRunsOrderBy("finished_at", dir)
		if err != nil {
			t.Fatalf("buildRunsOrderBy(finished_at,%q): %v", dir, err)
		}
		if !strings.Contains(got, "NULLS LAST") {
			t.Errorf("finished_at %s: %q without NULLS LAST", dir, got)
		}
	}
	// NULLS LAST only for finished_at (meaningless for not-null columns).
	got, _ := buildRunsOrderBy("started_at", "asc")
	if strings.Contains(got, "NULLS LAST") {
		t.Errorf("started_at should not have NULLS LAST: %q", got)
	}
}

// TestBuildRunsOrderBy_InvalidField tests non-whitelist field → sentinel (→ 422).
// Catches both injection attempts and valid-in-another-context columns (created_at
// from incarnation-whitelist is invalid here) (guard 3).
func TestBuildRunsOrderBy_InvalidField(t *testing.T) {
	for _, bad := range []string{"created_at", "name", "apply_id", "started_at; DROP TABLE apply_runs", "STARTED_AT"} {
		_, err := buildRunsOrderBy(bad, "asc")
		if !errors.Is(err, ErrInvalidRunsSortField) {
			t.Errorf("buildRunsOrderBy(%q): err=%v, want ErrInvalidRunsSortField", bad, err)
		}
	}
}

// TestBuildRunsOrderBy_InvalidDir verifies that non-asc/desc direction returns a
// sentinel (-> 422). Uppercase is rejected because whitelist is strict (guard 3).
func TestBuildRunsOrderBy_InvalidDir(t *testing.T) {
	for _, bad := range []string{"sideways", "ASC", "DESC", "ascending", "1"} {
		_, err := buildRunsOrderBy("started_at", bad)
		if !errors.Is(err, ErrInvalidRunsSortDir) {
			t.Errorf("buildRunsOrderBy(started_at,%q): err=%v, want ErrInvalidRunsSortDir", bad, err)
		}
	}
}

// TestBuildRunsQuery_ServiceFilterInSub verifies that service filter goes into
// subquery sub, not outerWhere: both countSQL and listSQL are built from it, so
// total and page are narrowed identically. Exact match by i.service, bind arg.
func TestBuildRunsQuery_ServiceFilterInSub(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Service: "redis"}, unrestricted)
	if !strings.Contains(sub, "i.service = $1") {
		t.Errorf("service filter is not in sub (will not affect count):\n%s", sub)
	}
	if strings.Contains(outer, "service") {
		t.Errorf("service filter leaked into outerWhere (will desync total/list): %q", outer)
	}
	if len(args) != 1 || args[0] != "redis" {
		t.Errorf("args = %v, want [redis]", args)
	}
}

// TestBuildRunsQuery_QFilterInSub verifies that free search q covers all 4 columns
// (incarnation/scenario/service/started_by) with one `%q%` bind arg in subquery
// sub, keeping count and list consistent.
func TestBuildRunsQuery_QFilterInSub(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Q: "ab"}, unrestricted)
	for _, want := range []string{
		"ar.incarnation_name ILIKE $1", "ar.scenario ILIKE $1",
		"i.service ILIKE $1", "ar.started_by_aid ILIKE $1",
	} {
		if !strings.Contains(sub, want) {
			t.Errorf("q filter does not cover %q:\n%s", want, sub)
		}
	}
	// q is only in sub, symmetric with service. It must not be in outerWhere,
	// otherwise count from sub and list diverge.
	if strings.Contains(outer, "ILIKE") {
		t.Errorf("q leaked into outerWhere (will desync total/list): %q", outer)
	}
	if len(args) != 1 || args[0] != "%ab%" {
		t.Errorf("args = %v, want [%%ab%%]", args)
	}
}

// TestBuildRunsQuery_QScopeAndBinding is a security regression: q OR group is
// parenthesized and AND-bound to the scope condition. Without parentheses,
// precedence AND>OR gives `a OR b OR c OR (d AND scope)`, so runs matching q bypass
// Purview scope (leak). Behavioral confirmation is TestIntegration_ListRuns_QScopeBinding.
func TestBuildRunsQuery_QScopeAndBinding(t *testing.T) {
	restricted := incarnation.ListScope{Covens: []string{"team-x"}}
	sub, _, _ := buildRunsQuery(RunsFilter{Q: "term"}, restricted)
	// Opening parenthesis of the q group is in place.
	if !strings.Contains(sub, "(ar.incarnation_name ILIKE $1 OR ar.scenario ILIKE $1") {
		t.Errorf("missing q-group opening parenthesis (OR is not grouped):\n%s", sub)
	}
	// Closing group parenthesis sits right before ` AND <scope-subquery>`: the
	// whole group is AND-bound to scope, not "...OR d AND scope".
	const wantBinding = "ar.started_by_aid ILIKE $1) AND ar.incarnation_name IN (SELECT name FROM incarnation WHERE"
	if !strings.Contains(sub, wantBinding) {
		t.Errorf("q group is not parenthesized / not AND-bound to scope (Purview leak risk):\n%s", sub)
	}
}

// TestBuildRunsQuery_QStatusCompose verifies q and status composition: q narrows
// in sub before aggregation, status in outerWhere by aggregate status; both bind
// args stay ordered. countSQL/listSQL are built from sub+outerWhere, so total and
// page are consistent under both filters.
func TestBuildRunsQuery_QStatusCompose(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Q: "x", Status: RunStatusFailed}, unrestricted)
	if !strings.Contains(sub, "ILIKE $1") {
		t.Errorf("q is not in sub:\n%s", sub)
	}
	if !strings.Contains(outer, "status = $2") {
		t.Errorf("status is not in outerWhere: %q", outer)
	}
	if len(args) != 2 || args[0] != "%x%" || args[1] != string(RunStatusFailed) {
		t.Errorf("args = %v, want [%%x%% failed]", args)
	}
}

// TestBuildRunsQuery_TimeFilterInOuter verifies that start-time filter is over
// aggregate started_at, so it belongs in outerWhere after GROUP BY, not in sub.
// Otherwise filtering host rows would split the apply_id group. Boundaries are
// inclusive (>=/<=); count and list are built from sub+outerWhere, so total/page
// are consistent.
func TestBuildRunsQuery_TimeFilterInOuter(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	sub, outer, args := buildRunsQuery(
		RunsFilter{StartedAfter: &after, StartedBefore: &before}, unrestricted)
	if !strings.Contains(outer, "started_at >= $1") || !strings.Contains(outer, "started_at <= $2") {
		t.Errorf("time is not in outerWhere with inclusive boundaries: %q", outer)
	}
	// Time must not be in sub (aggregate filter after GROUP BY).
	if strings.Contains(sub, "started_at >=") || strings.Contains(sub, "started_at <=") {
		t.Errorf("time leaked into sub (will split apply_id group):\n%s", sub)
	}
	if len(args) != 2 {
		t.Fatalf("args len = %d, want 2", len(args))
	}
	if got, ok := args[0].(time.Time); !ok || !got.Equal(after) {
		t.Errorf("args[0] = %v, want %v (after)", args[0], after)
	}
	if got, ok := args[1].(time.Time); !ok || !got.Equal(before) {
		t.Errorf("args[1] = %v, want %v (before)", args[1], before)
	}
}

// TestBuildRunsQuery_TimeComposeAllFilters covers service+q+status+time
// composition: service/q narrow in sub before aggregation, while status/
// started_after/started_before stay in outerWhere by aggregate; all clauses are
// AND-bound and bind arg positions are continuous. countSQL/listSQL from
// sub+outerWhere make total consistent under all filters at once.
func TestBuildRunsQuery_TimeComposeAllFilters(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	sub, outer, args := buildRunsQuery(RunsFilter{
		Service: "redis", Q: "x", Status: RunStatusFailed,
		StartedAfter: &after, StartedBefore: &before,
	}, unrestricted)
	// sub filters: service ($1) and q ($2) before GROUP BY, AND-bound.
	if !strings.Contains(sub, "i.service = $1") || !strings.Contains(sub, "ILIKE $2") {
		t.Errorf("service/q are not in sub with correct positions:\n%s", sub)
	}
	if !strings.Contains(sub, " AND ") {
		t.Errorf("sub clauses are not AND-bound:\n%s", sub)
	}
	// outer filters: status ($3), started_at >= ($4), started_at <= ($5), all AND-bound.
	for _, want := range []string{"status = $3", "started_at >= $4", "started_at <= $5"} {
		if !strings.Contains(outer, want) {
			t.Errorf("outerWhere lacks %q: %q", want, outer)
		}
	}
	if strings.Count(outer, " AND ") != 2 {
		t.Errorf("outer clauses are not fully AND-bound (want 2 AND): %q", outer)
	}
	if len(args) != 5 {
		t.Errorf("args len = %d, want 5 (service,q,status,after,before)", len(args))
	}
}

// TestBuildRunsQuery_QEscapesLikeMeta verifies that LIKE metacharacters %/_/\ in q
// are escaped (literal, not wildcard): pattern `%...%` carries `\%`, `\_`, `\\`.
func TestBuildRunsQuery_QEscapesLikeMeta(t *testing.T) {
	_, _, args := buildRunsQuery(RunsFilter{Q: `a%b_c\d`}, unrestricted)
	if len(args) != 1 || args[0] != `%a\%b\_c\\d%` {
		t.Errorf("q arg = %q, want %q", args[0], `%a\%b\_c\\d%`)
	}
}

// TestBuildRunsQuery_ServiceAndQArgOrder verifies that service and q are bind args
// in order (service=$1, q=$2), with positions not mixed when used together.
func TestBuildRunsQuery_ServiceAndQArgOrder(t *testing.T) {
	sub, _, args := buildRunsQuery(RunsFilter{Service: "redis", Q: "x"}, unrestricted)
	if !strings.Contains(sub, "i.service = $1") || !strings.Contains(sub, "ILIKE $2") {
		t.Errorf("bind arg order is broken (service=$1, q=$2):\n%s", sub)
	}
	if len(args) != 2 || args[0] != "redis" || args[1] != "%x%" {
		t.Errorf("args = %v, want [redis %%x%%]", args)
	}
}

// TestEscapeLike verifies escaping LIKE metacharacters in one pass. Backslash goes
// first, so inserted escapes are not double-escaped.
func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"plain": "plain",
		"50%":   `50\%`,
		"a_b":   `a\_b`,
		`c\d`:   `c\\d`,
		`%_\`:   `\%\_\\`,
		"":      "",
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}
