package soulpurview

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// TestWhereSQL_Terminals verifies the fail-open/fail-closed terminals of the SQL
// pushdown: Unrestricted → "TRUE" (no narrowing), empty purview → "FALSE" (zero
// hosts, NOT the whole registry). No args either way.
func TestWhereSQL_Terminals(t *testing.T) {
	sql, args, next := Resolve(rbac.Purview{Unrestricted: true}).WhereSQL(Columns, 1)
	if sql != "TRUE" || len(args) != 0 || next != 1 {
		t.Fatalf("Unrestricted WhereSQL = (%q, %v, %d); want (TRUE, [], 1)", sql, args, next)
	}
	sql, args, next = Resolve(rbac.Purview{}).WhereSQL(Columns, 1)
	if sql != "FALSE" || len(args) != 0 || next != 1 {
		t.Fatalf("empty WhereSQL = (%q, %v, %d); want (FALSE, [], 1)", sql, args, next)
	}
}

// TestWhereSQL_HostGlob verifies the host dimension (`host matches <glob>`)
// pushes down as a `sid LIKE ...` predicate over the souls sid column — the
// replacement for the removed regex/keyset machinery (now pure SQL).
func TestWhereSQL_HostGlob(t *testing.T) {
	sql, args, next := Resolve(rbac.Purview{
		Exprs: []*rbac.ScopeExpr{mustExpr(t, "host matches web-*")},
	}).WhereSQL(Columns, 1)

	if !strings.Contains(sql, "sid LIKE $1") {
		t.Fatalf("host-glob WhereSQL = %q; want a `sid LIKE $1` predicate", sql)
	}
	if len(args) != 1 {
		t.Fatalf("host-glob args = %v; want one (the LIKE pattern)", args)
	}
	if next != 2 {
		t.Fatalf("host-glob next idx = %d; want 2", next)
	}
	// `web-*` → SQL-LIKE `web-%` (glob star → percent).
	if got, ok := args[0].(string); !ok || !strings.Contains(got, "web-") || !strings.HasSuffix(got, "%") {
		t.Fatalf("host-glob LIKE pattern = %v; want a `web-…%%` string", args[0])
	}
}

// TestWhereSQL_Trait verifies the trait dimension pushes down over the jsonb
// `traits` column with BOTH the scalar (`->>`) and list (`?|`) arms, so a trait
// stored as a scalar OR a list matches.
func TestWhereSQL_Trait(t *testing.T) {
	sql, args, next := Resolve(rbac.Purview{
		Exprs: []*rbac.ScopeExpr{mustExpr(t, "trait.tier=gold")},
	}).WhereSQL(Columns, 1)

	if !strings.Contains(sql, "traits ->>") || !strings.Contains(sql, "traits ->") || !strings.Contains(sql, "?|") {
		t.Fatalf("trait WhereSQL = %q; want both the ->> (scalar) and ?| (list) arms over traits", sql)
	}
	if len(args) != 2 || next != 3 {
		t.Fatalf("trait args=%v next=%d; want 2 args (values,key) and next=3", args, next)
	}
	// One arg is the key "tier", the other the values []string{"gold"}.
	var sawKey, sawVals bool
	for _, a := range args {
		if s, ok := a.(string); ok && s == "tier" {
			sawKey = true
		}
		if vs, ok := a.([]string); ok && len(vs) == 1 && vs[0] == "gold" {
			sawVals = true
		}
	}
	if !sawKey || !sawVals {
		t.Fatalf("trait args = %v; want key \"tier\" and values [gold]", args)
	}
}

// TestWhereSQL_AbsentDimension_FailClosed verifies a condition on a dimension the
// souls table does NOT carry (service/incarnation) renders FALSE — never TRUE.
func TestWhereSQL_AbsentDimension_FailClosed(t *testing.T) {
	sql, _, _ := Resolve(rbac.Purview{
		Exprs: []*rbac.ScopeExpr{mustExpr(t, "service=redis")},
	}).WhereSQL(Columns, 1)
	if !strings.Contains(sql, "FALSE") || strings.Contains(sql, "TRUE") {
		t.Fatalf("service-scope WhereSQL = %q; want FALSE (souls carry no service column)", sql)
	}
}

// TestWhereSQL_PlaceholderOffset verifies startIdx threads through: placeholders
// begin at startIdx (so the scope fragment composes with preceding filter args).
func TestWhereSQL_PlaceholderOffset(t *testing.T) {
	sql, _, next := Resolve(rbac.Purview{
		Exprs: []*rbac.ScopeExpr{mustExpr(t, "coven=prod")},
	}).WhereSQL(Columns, 3)
	if !strings.Contains(sql, "coven && $3::text[]") {
		t.Fatalf("coven WhereSQL at startIdx=3 = %q; want `coven && $3::text[]`", sql)
	}
	if next != 4 {
		t.Fatalf("next idx = %d; want 4", next)
	}
}
