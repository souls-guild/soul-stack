package statemigrate

import (
	"context"
	"errors"
	"testing"
)

// Этот файл добивает completeness-покрытие DSL поверх ops_test.go /
// parse_test.go / statemigrate_test.go: операторы (литеральные/missing-path
// ветки), foreach (пустой map, null-in), CEL-sandbox negative (по одному кейсу
// на запрещённый идентификатор), атомарность при ошибке и edge-пути.
// Не дублирует уже покрытые кейсы (rename happy/to-exists/source-missing, set
// CEL/nested/intermediate, delete-noop, foreach map/list/nested/scalar).

// --- 1. Операторы: литералы и поведение на отсутствующем пути ----------------

// TestSet_LiteralScalar — set строкового литерала без ${ … } проходит как есть
// (interpolateValue не гоняет CEL для строк без маркера).
func TestSet_LiteralScalar(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.acl", Value: "off ~* &* +@all"}},
	}, map[string]any{})
	if out["acl"] != "off ~* &* +@all" {
		t.Fatalf("acl = %v, want литерал без интерполяции", out["acl"])
	}
}

// TestSet_OverwritesExisting — set по существующему пути перезаписывает значение
// (документированная семантика «Существующий Path перезаписывается»).
func TestSet_OverwritesExisting(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.x", Value: "new"}},
	}, map[string]any{"x": "old"})
	if out["x"] != "new" {
		t.Fatalf("x = %v, want new (перезапись)", out["x"])
	}
}

// TestDelete_ExistingRemovesKey — happy-path delete: существующий ключ удаляется.
func TestDelete_ExistingRemovesKey(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.drop"}},
	}, map[string]any{"drop": 1, "keep": 2})
	if _, ok := out["drop"]; ok {
		t.Fatalf("ключ drop не удалён: %#v", out)
	}
	// keep остаётся; JSON-нормализация числовых типов (deepCopyMap в Apply).
	assertDeepEqualJSON(t, out, map[string]any{"keep": 2})
}

// TestDelete_NestedRemovesLeafKeepsParent — delete вложенного листа удаляет
// только лист, родительский map остаётся.
func TestDelete_NestedRemovesLeafKeepsParent(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.cfg.port"}},
	}, map[string]any{"cfg": map[string]any{"port": 6379, "host": "h"}})
	cfg, _ := out["cfg"].(map[string]any)
	if cfg == nil {
		t.Fatalf("родительский cfg удалён вместе с листом: %#v", out)
	}
	if _, ok := cfg["port"]; ok {
		t.Fatalf("port не удалён: %#v", cfg)
	}
	if cfg["host"] != "h" {
		t.Fatalf("host затронут: %#v", cfg)
	}
}

// TestDelete_ThroughScalarMidPathNoOp — delete с промежуточным НЕ-map сегментом
// (state.a — скаляр, путь state.a.b) = no-op, не ошибка (путь не существует
// целиком — нечего удалять, см. deletePath).
func TestDelete_ThroughScalarMidPathNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Delete: &DeleteOp{Path: "state.a.b.c"}},
	}, map[string]any{"a": "scalar"})
	if out["a"] != "scalar" {
		t.Fatalf("a затронут delete через скаляр: %#v", out)
	}
}

// --- 2. foreach: краевые коллекции ------------------------------------------

// TestForeach_EmptyMapNoOp — foreach по пустому map не итерирует (тело Do ни
// разу не выполняется), state без изменений. (Пустой список покрыт в
// statemigrate_test.go TestApply_EmptyForeachNoMaterialize.)
func TestForeach_EmptyMapNoOp(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.src }", As: "v", Do: []Op{
			{Set: &SetOp{Path: "state.touched", Value: true}},
		}}},
	}, map[string]any{"src": map[string]any{}, "keep": 1})
	if _, ok := out["touched"]; ok {
		t.Fatalf("тело foreach выполнилось на пустом map: %#v", out)
	}
	// state без изменений (src остался пустым map, keep на месте). Сравнение
	// через JSON-нормализацию: deepCopyMap в Apply приводит int к float64.
	assertDeepEqualJSON(t, out, map[string]any{"src": map[string]any{}, "keep": 1})
}

// TestForeach_NullInNotIterable — foreach in: даёт null (отсутствующий ключ →
// CEL no-such-key) → ошибка (не список и не map). Фиксирует, что null
// трактуется как неитерируемый, а не как пустая коллекция.
func TestForeach_NullInNotIterable(t *testing.T) {
	_, err := apply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.missing }", As: "v", Do: []Op{
			{Delete: &DeleteOp{Path: "state.x"}},
		}}},
	}, map[string]any{"x": 1})
	if err == nil {
		t.Fatalf("ошибка = nil, want ошибку на null-коллекции")
	}
	// Может быть ClassForeachType (если CEL вернул nil) либо ClassCELInterp
	// (если no-such-key поднялся как ошибка резолва) — обе валидны: главное,
	// что foreach по null не молчит.
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("ошибка = %v (%T), want *EvalError", err, err)
	}
	if ee.Class != ClassForeachType && ee.Class != ClassCELInterp {
		t.Fatalf("class = %s, want ForeachType|CELInterp", ee.Class)
	}
}

// TestForeach_NestedAsAccessInValue — вложенный доступ к <as-name> внутри
// set.value (v.field), отдельно от уже покрытого доступа в path-сегменте.
func TestForeach_NestedAsAccessInValue(t *testing.T) {
	out := mustApply(t, []Op{
		{Foreach: &ForeachOp{In: "${ state.items }", As: "it", Do: []Op{
			{Set: &SetOp{Path: "state.out.${ it.key }", Value: "${ it.nested.deep }"}},
		}}},
	}, map[string]any{"items": []any{
		map[string]any{"key": "a", "nested": map[string]any{"deep": "DA"}},
		map[string]any{"key": "b", "nested": map[string]any{"deep": "DB"}},
	}})
	res, _ := out["out"].(map[string]any)
	if res["a"] != "DA" || res["b"] != "DB" {
		t.Fatalf("out = %#v, want {a:DA, b:DB}", res)
	}
}

// --- 3. CEL-sandbox negative: запрещённые идентификаторы в set.value ---------

// applySetValueErr прогоняет одиночный set с заданным выражением value над
// пустым state и возвращает ошибку Apply (или fail, если её нет). Все
// sandbox-нарушения в set.value поднимаются как *EvalError класса
// ClassCELInterp (interpolateValue оборачивает ошибку Evaluator.Interpolate).
func applySetValueErr(t *testing.T, valueExpr string) *EvalError {
	t.Helper()
	_, err := apply(t, []Op{
		{Set: &SetOp{Path: "state.x", Value: valueExpr}},
	}, map[string]any{})
	if err == nil {
		t.Fatalf("set.value %q: ошибка = nil, want sandbox-ошибку", valueExpr)
	}
	var ee *EvalError
	if !errors.As(err, &ee) {
		t.Fatalf("set.value %q: ошибка = %v (%T), want *EvalError", valueExpr, err, err)
	}
	if ee.Class != ClassCELInterp {
		t.Fatalf("set.value %q: class = %s, want %s", valueExpr, ee.Class, ClassCELInterp)
	}
	return ee
}

// TestSet_SandboxForbidsContextVars — register/soulprint/essence/input в
// set.value запрещены (migration = чистая функция от старого state, ADR-019):
// эти переменные не объявлены в migration-CEL → ошибка резолва. По одному
// negative-кейсу на каждый запрещённый идентификатор.
func TestSet_SandboxForbidsContextVars(t *testing.T) {
	for _, expr := range []string{
		"${ register.foo }",
		"${ soulprint.self.os.family }",
		"${ essence.bar }",
		"${ input.baz }",
	} {
		t.Run(expr, func(t *testing.T) {
			applySetValueErr(t, expr)
		})
	}
}

// TestSet_SandboxForbidsVault — vault(...) в set.value запрещён (миграция не
// тянет секреты): guard migration-CEL отсекает обращение.
func TestSet_SandboxForbidsVault(t *testing.T) {
	applySetValueErr(t, "${ vault('secret/x').password }")
}

// TestSet_SandboxForbidsNow — now() в set.value запрещён (воспроизводимость
// миграции): guard отсекает eval-time время.
func TestSet_SandboxForbidsNow(t *testing.T) {
	applySetValueErr(t, "${ now() }")
}

// TestForeach_SandboxForbidsContextVarInIn — sandbox-запрет действует и на
// foreach.in (не только set.value): соседний контекст недоступен и там.
func TestForeach_SandboxForbidsContextVarInIn(t *testing.T) {
	_, err := apply(t, []Op{
		{Foreach: &ForeachOp{In: "${ input.items }", As: "v", Do: []Op{
			{Delete: &DeleteOp{Path: "state.x"}},
		}}},
	}, map[string]any{"x": 1})
	if err == nil {
		t.Fatalf("foreach in: с input.* — ошибка = nil, want sandbox-ошибку")
	}
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassCELInterp {
		t.Fatalf("ошибка = %v, want *EvalError ClassCELInterp", err)
	}
}

// --- 4. Атомарность / forward-only на уровне ядра ---------------------------

// TestApply_FailedOpLeavesInputUntouched — ошибка операции в середине шага НЕ
// мутирует caller-ский входной state (deep-copy на входе Apply) и возвращает
// нулевой Result (FinalState/Steps пусты). Это и есть гарантия атомарности,
// доступная ядру: транзакционный слой поверх делает ROLLBACK по этой ошибке.
func TestApply_FailedOpLeavesInputUntouched(t *testing.T) {
	ev := mustEvaluator(t)
	in := map[string]any{"a": 1, "b": 2}

	chain := Chain{{FromVersion: 1, ToVersion: 2, Transform: []Op{
		{Set: &SetOp{Path: "state.c", Value: 3}},            // успешная мутация…
		{Rename: &RenameOp{From: "state.a", To: "state.b"}}, // …затем ошибка: to уже есть
	}}}

	res, err := Apply(context.Background(), in, chain, ev)
	if err == nil {
		t.Fatalf("ошибка = nil, want ClassRenameToExists")
	}
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassRenameToExists {
		t.Fatalf("ошибка = %v, want ClassRenameToExists", err)
	}
	// Входной state не тронут (включая частичную мутацию state.c из первой op).
	if len(in) != 2 || in["a"] != 1 || in["b"] != 2 {
		t.Fatalf("входной state мутирован при ошибке: %#v", in)
	}
	// Result — нулевой (ядро не отдаёт частичный результат).
	if res.FinalState != nil || res.Steps != nil {
		t.Fatalf("Result не нулевой при ошибке: %#v", res)
	}
}

// TestApply_LaterStepFailureDiscardsEarlierStep — ошибка во втором шаге цепочки
// не оставляет частично-применённый результат (FinalState пуст), хотя первый
// шаг сам по себе успешен. Forward-only: восстановление — через state_history
// транзакционного слоя, не через частичный возврат ядра.
func TestApply_LaterStepFailureDiscardsEarlierStep(t *testing.T) {
	ev := mustEvaluator(t)
	in := map[string]any{"v": 1}

	chain := Chain{
		{FromVersion: 1, ToVersion: 2, Transform: []Op{
			{Set: &SetOp{Path: "state.step1", Value: "ok"}},
		}},
		{FromVersion: 2, ToVersion: 3, Transform: []Op{
			{Set: &SetOp{Path: "state.bad", Value: "${ now() }"}}, // sandbox-ошибка
		}},
	}

	res, err := Apply(context.Background(), in, chain, ev)
	if err == nil {
		t.Fatalf("ошибка = nil, want sandbox-ошибку второго шага")
	}
	if res.FinalState != nil || res.Steps != nil {
		t.Fatalf("частичный Result при ошибке шага 2: %#v", res)
	}
	if len(in) != 1 || in["v"] != 1 {
		t.Fatalf("входной state мутирован: %#v", in)
	}
}

// --- 5. Edge: глубокая вложенность и конфликты ------------------------------

// TestRename_DeepNestedPaths — rename между глубоко вложенными путями: значение
// переносится со всей структурой, источник удаляется, целевые промежуточные
// map создаются.
func TestRename_DeepNestedPaths(t *testing.T) {
	out := mustApply(t, []Op{
		{Rename: &RenameOp{From: "state.a.b.c.d", To: "state.x.y.z"}},
	}, map[string]any{
		"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": "moved"}}},
	})
	x, _ := out["x"].(map[string]any)
	y, _ := x["y"].(map[string]any)
	if y["z"] != "moved" {
		t.Fatalf("state.x.y.z = %v, want moved", y["z"])
	}
	// Источник — оставшийся пустой родительский map (deletePath удаляет только
	// лист d, не схлопывает пустые родители — фиксируем фактическое поведение).
	a, _ := out["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	c, _ := b["c"].(map[string]any)
	if _, ok := c["d"]; ok {
		t.Fatalf("источник state.a.b.c.d не удалён: %#v", c)
	}
}

// TestRename_ToExistsNested — rename во вложенный уже существующий to → ошибка
// ClassRenameToExists (конфликт целевого ключа на глубине).
func TestRename_ToExistsNested(t *testing.T) {
	_, err := apply(t, []Op{
		{Rename: &RenameOp{From: "state.src", To: "state.dst.inner"}},
	}, map[string]any{
		"src": "v",
		"dst": map[string]any{"inner": "occupied"},
	})
	var ee *EvalError
	if !errors.As(err, &ee) || ee.Class != ClassRenameToExists {
		t.Fatalf("ошибка = %v, want ClassRenameToExists", err)
	}
}

// TestSet_TypeMismatchOverwritesMapWithScalar — set скаляром по пути, где сейчас
// map: целевой лист перезаписывается целиком (set по ЛИСТУ всегда перезапись,
// type-mismatch ошибкой НЕ является — в отличие от промежуточного сегмента).
func TestSet_TypeMismatchOverwritesMapWithScalar(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.obj", Value: "scalar"}},
	}, map[string]any{"obj": map[string]any{"was": "map"}})
	if out["obj"] != "scalar" {
		t.Fatalf("obj = %#v, want перезапись map скаляром", out["obj"])
	}
}

// TestSet_DeepNestedThroughExistingMaps — set глубоко вложенного листа сквозь
// уже существующие промежуточные map (навигация, а не создание с нуля).
func TestSet_DeepNestedThroughExistingMaps(t *testing.T) {
	out := mustApply(t, []Op{
		{Set: &SetOp{Path: "state.a.b.c.new", Value: "added"}},
	}, map[string]any{
		"a": map[string]any{"b": map[string]any{"c": map[string]any{"old": "kept"}}},
	})
	a, _ := out["a"].(map[string]any)
	b, _ := a["b"].(map[string]any)
	c, _ := b["c"].(map[string]any)
	if c["new"] != "added" || c["old"] != "kept" {
		t.Fatalf("state.a.b.c = %#v, want {old:kept, new:added}", c)
	}
}
