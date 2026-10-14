//go:build integration

package incarnation

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

func seedStatsIncarnation(t *testing.T, id, service string) {
	t.Helper()
	inc := &Incarnation{
		ID:                 id,
		Service:            service,
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		Status:             StatusReady,
	}
	if err := Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seed incarnation %s: %v", id, err)
	}
}

func TestIntegration_SelectServiceStats(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	seedStatsIncarnation(t, "redis-prod", "redis")
	seedStatsIncarnation(t, "redis-stage", "redis")
	seedStatsIncarnation(t, "pg-prod", "postgres")
	seedStatsIncarnation(t, "kafka-empty", "kafka")
	for _, sid := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		seedMembershipSoul(t, sid)
	}
	bind := func(inc string, sids ...string) {
		t.Helper()
		if err := AddMembers(ctx, integrationPool, inc, sids, nil); err != nil {
			t.Fatalf("AddMembers %s: %v", inc, err)
		}
	}
	// b is in both redis incarnations: redis has 3 membership rows but 2 hosts.
	bind("redis-prod", "a.example.com", "b.example.com")
	bind("redis-stage", "b.example.com")
	// c is also in postgres: hosts are distinct per service, not across services.
	bind("pg-prod", "b.example.com", "c.example.com")

	allHosts := func(start int) (string, []any, int) { return "TRUE", nil, start }
	got, err := SelectServiceStats(ctx, integrationPool, ListScope{Unrestricted: true}, allHosts)
	if err != nil {
		t.Fatalf("SelectServiceStats: %v", err)
	}
	want := []ServiceStats{
		{Service: "kafka", Incarnations: 1, Hosts: 0},
		{Service: "postgres", Incarnations: 1, Hosts: 2},
		{Service: "redis", Incarnations: 2, Hosts: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unrestricted:\n got %+v\nwant %+v", got, want)
	}

	// Scoped to one incarnation: its sibling must lend neither its count nor its
	// hosts, even though they share host b.
	oneInc := ListScope{Scope: func(start int) (string, []any, int) {
		return fmt.Sprintf("id = $%d", start), []any{"redis-stage"}, start + 1
	}}
	got, err = SelectServiceStats(ctx, integrationPool, oneInc, allHosts)
	if err != nil {
		t.Fatalf("SelectServiceStats scoped: %v", err)
	}
	want = []ServiceStats{{Service: "redis", Incarnations: 1, Hosts: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped to redis-stage:\n got %+v\nwant %+v", got, want)
	}

	// Soul visibility narrows the hosts, never the incarnations. Both scopes carry
	// a bind-arg, so this also pins the placeholder numbering across the two.
	onlyA := func(start int) (string, []any, int) {
		return fmt.Sprintf("souls.sid = ANY($%d)", start), []any{[]string{"a.example.com"}}, start + 1
	}
	redisOnly := ListScope{Scope: func(start int) (string, []any, int) {
		return fmt.Sprintf("service = $%d", start), []any{"redis"}, start + 1
	}}
	got, err = SelectServiceStats(ctx, integrationPool, redisOnly, onlyA)
	if err != nil {
		t.Fatalf("SelectServiceStats soul-scoped: %v", err)
	}
	want = []ServiceStats{{Service: "redis", Incarnations: 2, Hosts: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redis, hosts narrowed to a:\n got %+v\nwant %+v", got, want)
	}

	got, err = SelectServiceStats(ctx, integrationPool, ListScope{Unrestricted: true}, nil)
	if err != nil {
		t.Fatalf("SelectServiceStats nil hosts: %v", err)
	}
	want = []ServiceStats{
		{Service: "kafka", Incarnations: 1, Hosts: 0},
		{Service: "postgres", Incarnations: 1, Hosts: 0},
		{Service: "redis", Incarnations: 2, Hosts: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nil host scope must count no host:\n got %+v\nwant %+v", got, want)
	}

	// The predicate production renders (soulpurview over its own columns), in its
	// coven, host and trait forms, executed inside this subquery.
	if _, err := integrationPool.Exec(ctx, `UPDATE souls SET coven = ARRAY['team-a'] WHERE sid = 'a.example.com'`); err != nil {
		t.Fatalf("set coven: %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `UPDATE souls SET traits = '{"tier":"gold"}' WHERE sid = 'b.example.com'`); err != nil {
		t.Fatalf("set traits: %v", err)
	}
	soulScope := func(expr string) ScopeSQLFunc {
		t.Helper()
		e, err := rbac.ParseScopeExpr(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		s := soulpurview.Resolve(rbac.Purview{Exprs: []*rbac.ScopeExpr{e}})
		return func(start int) (string, []any, int) { return s.WhereSQL(soulpurview.Columns, start) }
	}
	for _, tc := range []struct {
		expr  string
		scope ListScope
		want  []ServiceStats
	}{
		{`coven=team-a`, redisOnly, []ServiceStats{{Service: "redis", Incarnations: 2, Hosts: 1}}},
		{`host=c.example.com`, ListScope{Unrestricted: true}, []ServiceStats{
			{Service: "kafka", Incarnations: 1, Hosts: 0},
			{Service: "postgres", Incarnations: 1, Hosts: 1},
			{Service: "redis", Incarnations: 2, Hosts: 0},
		}},
		{`trait.tier="gold"`, ListScope{Unrestricted: true}, []ServiceStats{
			{Service: "kafka", Incarnations: 1, Hosts: 0},
			{Service: "postgres", Incarnations: 1, Hosts: 1},
			{Service: "redis", Incarnations: 2, Hosts: 1},
		}},
	} {
		got, err := SelectServiceStats(ctx, integrationPool, tc.scope, soulScope(tc.expr))
		if err != nil {
			t.Fatalf("soul.list on %s: %v", tc.expr, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("soul.list on %s:\n got %+v\nwant %+v", tc.expr, got, tc.want)
		}
	}

	deny := ListScope{Scope: func(start int) (string, []any, int) { return "FALSE", nil, start }}
	got, err = SelectServiceStats(ctx, integrationPool, deny, allHosts)
	if err != nil {
		t.Fatalf("SelectServiceStats deny: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("deny scope: got %#v, want an empty non-nil slice", got)
	}
}
