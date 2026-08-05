package subject

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// The load-bearing boundary of NIM-280, asserted from the side that moved.
//
// Targeting expands; operator authorization never does. A coven or trait on an
// INCARNATION reaches its members as a SUBJECT — that is the whole feature — but
// an Archon's role scope keeps reading the row's own column and nothing else. If
// the two ever shared a resolver, tagging an incarnation `prod` would hand out
// permanent visibility of every host on its roster, which is exactly the
// escalation NIM-281 closed.
//
// The rbac package carries its own guards over the scope predicate's text
// (scope_own_labels_test.go). This one is the pair statement neither package can
// make alone: ONE host, ONE label, TWO answers.

// boundaryHost — a host whose only route to `prod` is its incarnation. It
// carries no label of its own, so any reach it has comes from the second level.
func boundaryHost() Host {
	return Host{
		SID:    "host-a.example.com",
		Covens: nil,
		Traits: nil,
		Member: []Incarnation{{
			Service: "redis", Name: "redis-prod",
			Covens: []string{"prod"},
			Traits: map[string]any{"owner": "dba"},
		}},
	}
}

func TestBoundary_IncarnationCovenReachesSubjectButNotScope(t *testing.T) {
	h := boundaryHost()

	if !selCoven("prod").Matches(h) {
		t.Fatal("subject: a coven on the incarnation must reach its members (NIM-280)")
	}

	// The same label, resolved as an operator's scope. `souls.coven` is the
	// host's OWN column, and this host's is empty — so the scope does not reach
	// it, and no join is there to make it.
	sql, args, _ := rbac.PurviewSQL(
		rbac.Purview{Exprs: []*rbac.ScopeExpr{mustScope(t, "coven=prod")}},
		rbac.ScopeColumns{Coven: "souls.coven", Host: "souls.sid", Traits: "souls.traits"},
		1,
	)
	if strings.Contains(sql, "incarnation") {
		t.Fatalf("scope predicate reaches incarnations — a label nobody attached to the host would grant access:\n%s", sql)
	}
	if len(args) != 1 {
		t.Fatalf("scope bound %d args, want only the scope's own value: %v", len(args), args)
	}
	if vals, ok := args[0].([]string); !ok || len(vals) != 1 || vals[0] != "prod" {
		t.Fatalf("scope bound %v; a host-resolved value here would mean the two resolvers were wired together", args[0])
	}
}

func TestBoundary_IncarnationTraitReachesSubjectButNotScope(t *testing.T) {
	h := boundaryHost()

	if !selTrait("owner", "dba").Matches(h) {
		t.Fatal("subject: a trait on the incarnation must reach its members (NIM-280)")
	}

	sql, _, _ := rbac.PurviewSQL(
		rbac.Purview{Exprs: []*rbac.ScopeExpr{mustScope(t, "trait.owner=dba")}},
		rbac.ScopeColumns{Coven: "souls.coven", Host: "souls.sid", Traits: "souls.traits"},
		1,
	)
	if strings.Contains(sql, "incarnation") {
		t.Fatalf("trait scope reaches incarnations — `owner=dba` on an incarnation would expose hosts nobody labelled:\n%s", sql)
	}
	if !strings.Contains(sql, "souls.traits ->>") {
		t.Fatalf("trait scope is no longer a plain column comparison on the host's own traits:\n%s", sql)
	}
}

// TestBoundary_SubjectPredicateIsNotAScopePredicate — the subject predicate folds
// both levels into its arguments by construction ([Host.Args]), so splicing it
// into a visibility query would widen that query silently. The two must stay
// structurally distinguishable: a scope narrows by the SCOPE's values, a subject
// matches by the HOST's resolved facts.
func TestBoundary_SubjectPredicateIsNotAScopePredicate(t *testing.T) {
	args := boundaryHost().Args()

	// The incarnation's label IS in the subject's arguments — this is the feature.
	if len(args.Covens) != 1 || args.Covens[0] != "prod" {
		t.Fatalf("subject args = %v, want the incarnation's label folded in", args.Covens)
	}
	if len(args.TraitPairs) != 1 || args.TraitPairs[0] != "owner=dba" {
		t.Fatalf("subject args = %v, want the incarnation's trait folded in", args.TraitPairs)
	}

	// rbac's coven predicate binds the SCOPE's values against the row's own
	// column; the subject's binds the HOST's resolved set against the rule's.
	// Same dimension, opposite directions — never one function.
	if got := rbac.CovenScopeSQL("souls.coven", "$1"); got != "souls.coven && $1::text[]" {
		t.Fatalf("CovenScopeSQL = %q, want a plain array overlap on the row's own column", got)
	}
}

func mustScope(t *testing.T, s string) *rbac.ScopeExpr {
	t.Helper()
	e, err := rbac.ParseScopeExpr(s)
	if err != nil {
		t.Fatalf("ParseScopeExpr(%q): %v", s, err)
	}
	return e
}
