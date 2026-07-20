package handlers

// End-to-end guard tests for trait-scoped incarnation visibility at the handler
// layer (NIM-128 unified boolean scope). The e2e through the handler — List
// (ResolveListScopeFor → rbac.PurviewSQL → SQL) and Get (GetInScopeFor →
// traitsToScopeInput → rbac.Purview.Match) — is exercised here.
//
// KEY invariant (BUG#1 fix — List↔Get consistency): a scope value matches BOTH a
// scalar label and any element of a list label, IDENTICALLY in List and Get:
//   - scalar label {env:"prod"}      + scope trait.env=prod → VISIBLE (Get 200;
//     List `traits ->> $k = ANY($v)`);
//   - list label {env:[prod,stage]}  + the same scope → VISIBLE (Get 200 via
//     traitsToScopeInput element-expansion; List `traits -> $k ?| $v`).
// The former scalar-only List↔Get divergence and the `@>` containment form are
// removed.

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

// --- Get trait-scoped (GetInScopeFor → traitsToScopeInput → Match) ----------

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

// TestIncarnation_Get_TraitListLabel_200 — NIM-128: list label {env:[prod,stage]}
// + scope trait.env=prod → 200 (visible). The unified resolver matches a scope
// value against ANY element of a list-Trait (traitsToScopeInput expands the
// list), so List and Get agree — resolving the former scalar-only List↔Get
// divergence.
func TestIncarnation_Get_TraitListLabel_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			return incTraitRow(name, map[string]any{"env": []any{"prod", "stage"}})
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (list label env=[prod,stage] matches scope env=prod on element)", rec.Code)
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

// TestIncarnation_List_TraitScope_ReachesSQL — NIM-128 unified trait resolver: the
// trait scope reaches SQL as `traits ->> $k = ANY($v) OR traits -> $k ?| $v` — the
// scalar arm (`->>`) matches a scalar label, the `?|` arm matches any element of a
// list label, so List and Get agree (BUG#1 fix — the former scalar-only List↔Get
// divergence, and the `@>` containment form, are gone). Key is a scalar bind-arg,
// values a []string bind-arg.
func TestIncarnation_List_TraitScope_ReachesSQL(t *testing.T) {
	db, sql, h := listTraitSQLHandler([]string{"env:prod"})

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*sql, "traits ->>") {
		t.Errorf("trait-scope did not reach SQL as `traits ->>` scalar arm:\n%s", *sql)
	}
	if !strings.Contains(*sql, "?|") {
		t.Errorf("trait-scope did not reach SQL as `?|` list arm:\n%s", *sql)
	}
	if strings.Contains(*sql, "@>") {
		t.Errorf("trait-scope must NOT use jsonb-containment @> (NIM-128 uses ->> / ?|):\n%s", *sql)
	}
	// scope pushdown is active (not fail-closed FALSE): SelectAll is called.
	if !db.listCalled {
		t.Errorf("trait-scope: SelectAll not called (expected scope-pushdown, not fail-closed)")
	}
	// key (scalar) and values ([]string) are separate bind-args, not concatenated
	// into SQL text.
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
// both dimensions reach SQL (coven arm `covens &&`; trait arm `traits ->>`).
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
	if !strings.Contains(sql, "traits ->>") {
		t.Errorf("OR-union: trait arm (traits ->>) not in SQL:\n%s", sql)
	}
}

// argsHasString — whether the bind-args contain a string equal to want, either as a
// scalar string arg or as an element of a []string / []any arg (NIM-128 renders a
// trait scope's values as a single []string bind-arg).
func argsHasString(args []any, want string) bool {
	match := func(a any) bool {
		s, ok := a.(string)
		return ok && s == want
	}
	for _, a := range args {
		if match(a) {
			return true
		}
		switch xs := a.(type) {
		case []string:
			for _, s := range xs {
				if s == want {
					return true
				}
			}
		case []any:
			for _, e := range xs {
				if match(e) {
					return true
				}
			}
		}
	}
	return false
}
