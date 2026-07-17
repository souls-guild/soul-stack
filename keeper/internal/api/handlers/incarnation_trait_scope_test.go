package handlers

// End-to-end guard tests for trait-scoped incarnation visibility at the handler layer (ADR-047
// amendment / ADR-060 §7 slice 1). The constituent pieces are covered separately
// (traitScalarEquals unit / incarnation.appendScopeClause integration / Purview),
// but the e2e through the handler — List (ResolveListScopeFor → scope.Traits → SQL) and Get
// (GetInScopeFor → traitScalarEquals) — was missing.
//
// KEY invariant (BUG#1 — List↔Get consistency at the handler level):
//   - scalar label {env:"prod"}  + scope trait=env:prod → VISIBLE  (Get 200; List
//     emits scalar-equality `traits->>$ = $` in SQL, NOT containment `@>`);
//   - list label {env:[prod,stage]} + the same scope → not visible (Get 404; List —
//     the same scalar-equality SQL, which for an array yields its TEXT ≠ "prod").
// The mismatch would be: List via `@>` shows the list label (array-contains-
// primitive PG §8.14.3), while Get does NOT see it. Both arms must be scalar-only.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// incTraitRow — a staticRow for SelectByName/SelectAll (16 scanIncarnation columns)
// with a given traits map (column $13 / index 12). Mirror of incListRow, but with
// arbitrary traits (incListRow hardcodes `{}`); covens/state are omitted (nil).
func incTraitRow(name string, traits map[string]any) staticRow {
	now := time.Now()
	traitsBytes := []byte("{}")
	if traits != nil {
		b, _ := json.Marshal(traits)
		traitsBytes = b
	}
	return staticRow{values: []any{
		name, "redis", "v1", int(1),
		[]byte("{}"), []byte("{}"), "ready",
		[]byte(nil), any(nil),
		now, now, []string(nil),
		traitsBytes,
		any(nil), []byte(nil),
		"create",
		any(nil), // applying_apply_id (ADR-068 §A1)
	}}
}

// --- Get trait-scoped (GetInScopeFor → traitScalarEquals) -------------------

// TestIncarnation_Get_TraitScalarMatch_200 — scalar label {env:prod} + scope
// trait=env:prod → 200 (visible). The base trait-scope arm on the GET path.
func TestIncarnation_Get_TraitScalarMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"env": "prod"})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (scalar trait env=prod in scope)", rec.Code)
	}
}

// TestIncarnation_Get_TraitScalarMismatch_404 — scalar label {env:stage} + scope
// trait=env:prod → 404 (different value).
func TestIncarnation_Get_TraitScalarMismatch_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"env": "stage"})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (trait env=stage does not match scope env=prod)", rec.Code)
	}
}

// TestIncarnation_Get_TraitListLabel_404 — ★BUG#1: list label {env:[prod,stage]}
// + scope trait=env:prod → 404 (not visible). traitScalarEquals on an array → false
// (scalar-only): an operator with a scalar scope does NOT see an incarnation with a list
// label that contains this value as an element. Consistent with the List arm below.
func TestIncarnation_Get_TraitListLabel_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"env": []any{"prod", "stage"}})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (list label env=[prod,stage] does NOT match scalar-scope env=prod - BUG#1)", rec.Code)
	}
}

// TestIncarnation_Get_TraitMissingKey_404 — the scope key is absent from traits → 404.
func TestIncarnation_Get_TraitMissingKey_404(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"team": "dba"})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusNotFound {
		t.Errorf("Code = %d, want 404 (key env absent from traits)", rec.Code)
	}
}

// TestIncarnation_Get_TraitNumberMatch_200 — numeric scalar label {shard:3}
// (jsonb→float64) + scope trait=shard:3 → 200 (string form of float64 == "3").
func TestIncarnation_Get_TraitNumberMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"shard": float64(3)})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"shard:3"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (numeric scalar label shard=3)", rec.Code)
	}
}

// TestIncarnation_Get_TraitOR_CovenAndTrait — dimension OR: trait does not match, but
// coven matches → 200 (union coven ∪ trait, like the other Purview dimensions).
func TestIncarnation_Get_TraitOR_CovenMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			// traits.env=stage (does not match scope env=prod), but coven=prod matches.
			r := incTraitRow(name, map[string]any{"env": "stage"})
			r.values[11] = []string{"prod"} // covens (index 11)
			return r
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}, traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven arm of OR-union matches, though trait does not)", rec.Code)
	}
}

// --- List trait-scoped (ResolveListScopeFor → scope.Traits → SQL) -----------

// listTraitSQLHandler assembles a List handler with a trait scope and interception of the
// COUNT SQL + list SQL. One fakeIncDB serves both branches.
func listTraitSQLHandler(traitExprs []string) (*fakeIncDB, *string, *IncarnationHandler) {
	var sql string
	db := &fakeIncDB{
		countRow:       func(s string) pgx.Row { sql = s; return staticRow{values: []any{int(0)}} },
		listRows:       func() (pgx.Rows, error) { return &emptyRows{}, nil },
		captureListSQL: func(s string) { sql = s },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: traitExprs}, nil)
	return db, &sql, h
}

// TestIncarnation_List_TraitScope_ScalarEqualitySQL — ★BUG#1: the trait scope reaches
// SQL as scalar-equality `traits->>$N = $N`, NOT jsonb-containment `@>`. This is the
// consistency arm: the same predicate on an array yields the array's TEXT ≠ "prod"
// (the list label does NOT match in List, just like in Get). A regression to `@>` would bring
// back the mismatch (List shows the list label, Get does not).
func TestIncarnation_List_TraitScope_ScalarEqualitySQL(t *testing.T) {
	db, sql, h := listTraitSQLHandler([]string{"env:prod"})

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*sql, "traits->>") {
		t.Errorf("trait-scope did not reach SQL as traits->> scalar-equality:\n%s", *sql)
	}
	if strings.Contains(*sql, "@>") {
		t.Errorf("trait-scope uses jsonb-containment @> (BUG#1: matches list label, out of sync with Get):\n%s", *sql)
	}
	// scope pushdown is active (not fail-closed FALSE): SelectAll is called.
	if !db.listCalled {
		t.Errorf("trait-scope: SelectAll not called (expected scope-pushdown, not fail-closed)")
	}
	// key and value are separate bind-args (env / prod), not concatenated into text.
	if !argsHasString(db.lastCountArgs, "env") || !argsHasString(db.lastCountArgs, "prod") {
		t.Errorf("trait key/value did not arrive as separate bind-args (env, prod): %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_TraitScope_ValueBound — the trait-scope value is bound as a
// parameter (not into SQL text): injection via the value is impossible. We feed a
// "dangerous" value and check it is in args, NOT in the SQL text.
func TestIncarnation_List_TraitScope_ValueBound(t *testing.T) {
	db, sql, h := listTraitSQLHandler([]string{"env:prod' OR '1'='1"})

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(*sql, "OR '1'='1") {
		t.Errorf("trait value ended up in SQL TEXT (injection), should be a bind-arg:\n%s", *sql)
	}
	if !argsHasString(db.lastCountArgs, "prod' OR '1'='1") {
		t.Errorf("trait value did not arrive as a bind-arg: %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_TraitScope_NonEmpty_NotFailClosed — a trait dimension by
// itself makes the Purview non-empty (scopeEmpty=false): List is NOT fail-closed, it goes to
// SelectAll. Regression = a trait-only operator silently gets an empty list.
func TestIncarnation_List_TraitScope_NonEmpty_NotFailClosed(t *testing.T) {
	db := &fakeIncDB{
		countRow: func(_ string) pgx.Row { return staticRow{values: []any{int(1)}} },
		listRows: func() (pgx.Rows, error) {
			return &incRows{rows: []staticRow{incTraitRow("redis-prod", map[string]any{"env": "prod"})}}, nil
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !db.listCalled {
		t.Errorf("trait-only Purview must NOT be fail-closed (SelectAll should be called)")
	}
}

// TestIncarnation_List_TraitOR_CovenAndTrait_BothReachSQL — OR-union coven ∪ trait:
// both dimensions reach SQL (coven arm covens && / name = ANY; trait arm
// traits->>). Symmetric to the state∪coven union.
func TestIncarnation_List_TraitOR_CovenAndTrait_BothReachSQL(t *testing.T) {
	var sql string
	db := &fakeIncDB{
		countRow:       func(s string) pgx.Row { sql = s; return staticRow{values: []any{int(0)}} },
		listRows:       func() (pgx.Rows, error) { return &emptyRows{}, nil },
		captureListSQL: func(s string) { sql = s },
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}, traitExprs: []string{"team:dba"}}, nil)

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sql, "covens &&") {
		t.Errorf("OR-union: coven arm (covens &&) not in SQL:\n%s", sql)
	}
	if !strings.Contains(sql, "traits->>") {
		t.Errorf("OR-union: trait arm (traits->>) not in SQL:\n%s", sql)
	}
}

// argsHasString — whether the bind-args contain a string argument equal to want.
func argsHasString(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}
