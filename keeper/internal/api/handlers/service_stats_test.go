package handlers

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

func TestServiceStatsTyped_FailClosed_StoreNotCalled(t *testing.T) {
	cases := map[string]struct {
		scoper PurviewResolver
		nilJWT bool
	}{
		"nil claims":  {scoper: fakeIncScoper{unrestricted: true}, nilJWT: true},
		"nil scoper":  {scoper: nil},
		"empty scope": {scoper: fakeIncScoper{empty: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			db := &fakeIncDB{listRows: func() (pgx.Rows, error) { called = true; return &emptyRows{}, nil }}
			h := newRunsHandler(db, tc.scoper)
			claims := runsClaims()
			if tc.nilJWT {
				claims = nil
			}
			got, err := h.ServiceStatsTyped(context.Background(), claims)
			if err != nil {
				t.Fatalf("ServiceStatsTyped: %v", err)
			}
			if got == nil || len(got) != 0 {
				t.Errorf("got %#v, want an empty non-nil aggregate", got)
			}
			if called {
				t.Error("store was queried although the scope did not resolve")
			}
		})
	}
}

func TestServiceStatsTyped_ScopeReachesTheVisibleSet(t *testing.T) {
	var sql string
	db := &fakeIncDB{captureListSQL: func(s string) { sql = s }}
	h := newRunsHandler(db, fakeIncScoper{covens: []string{"prod"}})
	if _, err := h.ServiceStatsTyped(context.Background(), runsClaims()); err != nil {
		t.Fatalf("ServiceStatsTyped: %v", err)
	}
	if !strings.Contains(sql, "covens &&") {
		t.Fatalf("incarnation scope did not reach SQL:\n%s", sql)
	}
	found := false
	for _, a := range db.lastListArgs {
		if ss, ok := a.([]string); ok && len(ss) == 1 && ss[0] == "prod" {
			found = true
		}
	}
	if !found {
		t.Errorf("scope covens did not arrive as a bind-arg: %v", db.lastListArgs)
	}
}

func TestServiceStatsTyped_Unrestricted_NoNarrowing(t *testing.T) {
	var sql string
	db := &fakeIncDB{captureListSQL: func(s string) { sql = s }}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	if _, err := h.ServiceStatsTyped(context.Background(), runsClaims()); err != nil {
		t.Fatalf("ServiceStatsTyped: %v", err)
	}
	if strings.Contains(sql, "FROM incarnation WHERE") {
		t.Errorf("unrestricted caller got an incarnation predicate:\n%s", sql)
	}
	if !strings.Contains(sql, "WHERE TRUE") {
		t.Errorf("unrestricted caller's hosts are narrowed:\n%s", sql)
	}
}

// perResourceScoper answers per resource and records every (resource, action) asked.
type perResourceScoper struct {
	byResource map[string]rbac.Purview
	asked      []string
}

func (s *perResourceScoper) ResolvePurview(_, resource, action string) rbac.Purview {
	s.asked = append(s.asked, resource+"."+action)
	return s.byResource[resource]
}

func TestServiceStatsTyped_HostsNarrowedBySoulList(t *testing.T) {
	var sql string
	db := &fakeIncDB{captureListSQL: func(s string) { sql = s }}
	scoper := &perResourceScoper{byResource: map[string]rbac.Purview{
		"incarnation": {Exprs: []*rbac.ScopeExpr{mustScopeExpr("coven=prod")}},
		"soul":        {Exprs: []*rbac.ScopeExpr{mustScopeExpr("coven=team-a")}},
	}}
	h := newRunsHandler(db, scoper)
	if _, err := h.ServiceStatsTyped(context.Background(), runsClaims()); err != nil {
		t.Fatalf("ServiceStatsTyped: %v", err)
	}
	if want := []string{"incarnation.list", "soul.list"}; !reflect.DeepEqual(scoper.asked, want) {
		t.Errorf("purviews asked = %v, want %v", scoper.asked, want)
	}
	// $2: the soul predicate's placeholders must start after the incarnation's one.
	if !strings.Contains(sql, "souls.coven && $2::text[]") {
		t.Fatalf("soul scope did not reach the membership subquery at $2:\n%s", sql)
	}
	var bound []string
	for _, a := range db.lastListArgs {
		if ss, ok := a.([]string); ok && len(ss) == 1 {
			bound = append(bound, ss[0])
		}
	}
	// Order matters: the soul predicate's placeholders follow the incarnation's.
	if want := []string{"prod", "team-a"}; !reflect.DeepEqual(bound, want) {
		t.Errorf("scope bind-args = %v, want %v", bound, want)
	}
}

func TestServiceStatsTyped_NoSoulVisibility_CountsNoHost(t *testing.T) {
	var sql string
	db := &fakeIncDB{captureListSQL: func(s string) { sql = s }}
	scoper := &perResourceScoper{byResource: map[string]rbac.Purview{
		"incarnation": {Unrestricted: true},
	}}
	h := newRunsHandler(db, scoper)
	if _, err := h.ServiceStatsTyped(context.Background(), runsClaims()); err != nil {
		t.Fatalf("ServiceStatsTyped: %v", err)
	}
	if !strings.Contains(sql, "WHERE FALSE") {
		t.Errorf("an operator without soul.list still counts hosts:\n%s", sql)
	}
}

func TestServiceStatsTyped_Projection(t *testing.T) {
	db := &fakeIncDB{listRows: func() (pgx.Rows, error) {
		return &runsStatsRows{rows: [][3]any{
			{"postgres", int(1), int(0)},
			{"redis", int(3), int(5)},
		}}, nil
	}}
	h := newRunsHandler(db, fakeIncScoper{unrestricted: true})
	got, err := h.ServiceStatsTyped(context.Background(), runsClaims())
	if err != nil {
		t.Fatalf("ServiceStatsTyped: %v", err)
	}
	want := []ServiceStatsView{
		{Service: "postgres", Incarnations: 1, Hosts: 0},
		{Service: "redis", Incarnations: 3, Hosts: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
