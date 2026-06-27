package handlers

// Сквозные guard-тесты trait-scoped видимости инкарнации на handler-слое (ADR-047
// amendment / ADR-060 п.7 slice 1). Составные куски покрыты порознь
// (traitScalarEquals unit / incarnation.appendScopeClause integration / Purview),
// но e2e через handler — List (ResolveListScopeFor → scope.Traits → SQL) и Get
// (GetInScopeFor → traitScalarEquals) — отсутствовал.
//
// КЛЮЧЕВОЙ инвариант (BUG#1 — консистентность List↔Get на handler-уровне):
//   - scalar-метка {env:"prod"}  + scope trait=env:prod → ВИДНА  (Get 200; List
//     отдаёт scalar-equality `traits->>$ = $` в SQL, НЕ containment `@>`);
//   - list-метка {env:[prod,stage]} + тот же scope → НЕ видна (Get 404; List —
//     тот же scalar-equality SQL, который для массива даёт его ТЕКСТ ≠ "prod").
// Рассинхрон был бы: List через `@>` показывает list-метку (array-contains-
// primitive PG §8.14.3), а Get её НЕ видит. Оба плеча обязаны быть scalar-only.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// incTraitRow — staticRow под SelectByName/SelectAll (16 колонок scanIncarnation)
// с заданным traits-map (колонка $13 / index 12). Зеркало incListRow, но с
// произвольными traits (incListRow хардкодит `{}`); covens/state опускаем (nil).
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
	}}
}

// --- Get trait-scoped (GetInScopeFor → traitScalarEquals) -------------------

// TestIncarnation_Get_TraitScalarMatch_200 — scalar-метка {env:prod} + scope
// trait=env:prod → 200 (видна). Базовое плечо trait-scope на GET-пути.
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
		t.Errorf("Code = %d, want 200 (scalar trait env=prod в scope)", rec.Code)
	}
}

// TestIncarnation_Get_TraitScalarMismatch_404 — scalar-метка {env:stage} + scope
// trait=env:prod → 404 (другое значение).
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
		t.Errorf("Code = %d, want 404 (trait env=stage не матчит scope env=prod)", rec.Code)
	}
}

// TestIncarnation_Get_TraitListLabel_404 — ★BUG#1: list-метка {env:[prod,stage]}
// + scope trait=env:prod → 404 (НЕ видна). traitScalarEquals на массиве → false
// (scalar-only): оператор со scalar-scope НЕ видит инкарнацию с list-меткой,
// содержащей это значение как элемент. Консистентно с List-плечом ниже.
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
		t.Errorf("Code = %d, want 404 (list-метка env=[prod,stage] НЕ матчит scalar-scope env=prod — BUG#1)", rec.Code)
	}
}

// TestIncarnation_Get_TraitMissingKey_404 — ключ scope отсутствует в traits → 404.
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
		t.Errorf("Code = %d, want 404 (ключ env отсутствует в traits)", rec.Code)
	}
}

// TestIncarnation_Get_TraitNumberMatch_200 — числовая scalar-метка {shard:3}
// (jsonb→float64) + scope trait=shard:3 → 200 (строковая форма float64 == "3").
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
		t.Errorf("Code = %d, want 200 (числовая scalar-метка shard=3)", rec.Code)
	}
}

// TestIncarnation_Get_TraitOR_CovenAndTrait — OR-измерений: trait не матчит, но
// coven матчит → 200 (union coven ∪ trait, как остальные измерения Purview).
func TestIncarnation_Get_TraitOR_CovenMatch_200(t *testing.T) {
	db := &fakeIncDB{
		selectByNameRow: func(name string) pgx.Row {
			// traits.env=stage (не матчит scope env=prod), но coven=prod матчит.
			r := incTraitRow(name, map[string]any{"env": "stage"})
			r.values[11] = []string{"prod"} // covens (index 11)
			return r
		},
	}
	h := NewIncarnationHandler(db, nil, nil, nil, nil, nil, nil,
		fakeIncScoper{covens: []string{"prod"}, traitExprs: []string{"env:prod"}}, nil)
	rec := doIncGet(t, h, "redis-prod")
	if rec.Code != http.StatusOK {
		t.Errorf("Code = %d, want 200 (coven-плечо OR-union матчит, хотя trait — нет)", rec.Code)
	}
}

// --- List trait-scoped (ResolveListScopeFor → scope.Traits → SQL) -----------

// listTraitSQLHandler собирает List-handler с trait-scope и перехватом COUNT-SQL +
// list-SQL. Один fakeIncDB обслуживает обе ветки.
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

// TestIncarnation_List_TraitScope_ScalarEqualitySQL — ★BUG#1: trait-scope доходит
// до SQL как scalar-equality `traits->>$N = $N`, НЕ jsonb-containment `@>`. Это
// плечо консистентности: тот же предикат на массиве даёт ТЕКСТ массива ≠ "prod"
// (list-метка НЕ матчится в List, как и в Get). Регресс на `@>` вернул бы
// рассинхрон (List показывает list-метку, Get — нет).
func TestIncarnation_List_TraitScope_ScalarEqualitySQL(t *testing.T) {
	db, sql, h := listTraitSQLHandler([]string{"env:prod"})

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(*sql, "traits->>") {
		t.Errorf("trait-scope не дошёл до SQL как traits->> scalar-equality:\n%s", *sql)
	}
	if strings.Contains(*sql, "@>") {
		t.Errorf("trait-scope использует jsonb-containment @> (BUG#1: матчит list-метку, рассинхрон с Get):\n%s", *sql)
	}
	// scope-pushdown активен (не fail-closed FALSE): SelectAll вызван.
	if !db.listCalled {
		t.Errorf("trait-scope: SelectAll не вызван (ожидался scope-pushdown, не fail-closed)")
	}
	// Ключ и значение — раздельные bind-args (env / prod), не конкатенация в текст.
	if !argsHasString(db.lastCountArgs, "env") || !argsHasString(db.lastCountArgs, "prod") {
		t.Errorf("trait key/value не пришли раздельными bind-args (env, prod): %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_TraitScope_ValueBound — значение trait-scope биндится как
// параметр (а не в SQL-текст): инъекция через значение невозможна. Подаём
// «опасное» значение, проверяем, что оно в args, а НЕ в тексте SQL.
func TestIncarnation_List_TraitScope_ValueBound(t *testing.T) {
	db, sql, h := listTraitSQLHandler([]string{"env:prod' OR '1'='1"})

	rec := doIncList(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(*sql, "OR '1'='1") {
		t.Errorf("trait-значение попало в SQL-ТЕКСТ (инъекция), а должно быть bind-арг:\n%s", *sql)
	}
	if !argsHasString(db.lastCountArgs, "prod' OR '1'='1") {
		t.Errorf("trait-значение не пришло bind-аргом: %v", db.lastCountArgs)
	}
}

// TestIncarnation_List_TraitScope_NonEmpty_NotFailClosed — trait-измерение само по
// себе делает Purview непустым (scopeEmpty=false): List НЕ fail-closed, идёт в
// SelectAll. Регресс = trait-only оператор молча получает пустой список.
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
		t.Errorf("trait-only Purview обязан НЕ быть fail-closed (SelectAll должен вызваться)")
	}
}

// TestIncarnation_List_TraitOR_CovenAndTrait_BothReachSQL — OR-union coven ∪ trait:
// оба измерения уходят в SQL (coven-плечо covens && / name = ANY; trait-плечо
// traits->>). Симметрично state∪coven-union.
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
		t.Errorf("OR-union: coven-плечо (covens &&) не в SQL:\n%s", sql)
	}
	if !strings.Contains(sql, "traits->>") {
		t.Errorf("OR-union: trait-плечо (traits->>) не в SQL:\n%s", sql)
	}
}

// argsHasString — есть ли среди bind-args строковый аргумент, равный want.
func argsHasString(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}
