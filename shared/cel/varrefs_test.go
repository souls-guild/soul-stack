package cel

import (
	"errors"
	"reflect"
	"testing"
)

func newVarRefsEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestVarRefs_SelectForm — `${vars.a}-${vars.b}` → [a b] (кейс #10): извлекает
// имена через select-форму, в порядке появления при PostOrderVisit.
func TestVarRefs_SelectForm(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${vars.a}-${vars.b}")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("VarRefs = %v, want [a b]", got)
	}
}

// TestVarRefs_LiteralOutsideMarker — `text vars.x` ВНЕ `${ … }` ссылкой не
// считается (кейс #10): это литеральный текст, а не CEL-блок → пустой срез.
func TestVarRefs_LiteralOutsideMarker(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("text vars.x")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("VarRefs = %v, want [] (vars.x вне ${} — литерал)", got)
	}
}

// TestVarRefs_StringLiteralInsideExpr — `${ "vars.x" }` (строковый литерал CEL) —
// не ссылка: это StringConstant, не Select-узел → пустой срез.
func TestVarRefs_StringLiteralInsideExpr(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs(`${ "vars.x" }`)
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("VarRefs = %v, want [] (строковый литерал не Select)", got)
	}
}

// TestVarRefs_OtherBaseIgnored — ссылки на input/soulprint/register НЕ считаются
// (только base=="vars"); смешанная строка отдаёт лишь vars-имена.
func TestVarRefs_OtherBaseIgnored(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ input.x }/${ vars.a }/${ soulprint.self.sid }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (только vars.*)", got)
	}
}

// TestVarRefs_Dedup — повторная ссылка на тот же var дедуплицируется.
func TestVarRefs_Dedup(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ vars.a }-${ vars.a }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (дедуп)", got)
	}
}

// TestVarRefs_IndexForm — index-форма `vars['k']` → детерминированная ошибка
// ErrVarIndexForm (кейс #10, зафиксированное поведение index-формы).
func TestVarRefs_IndexForm(t *testing.T) {
	e := newVarRefsEngine(t)
	_, err := e.VarRefs(`${ vars['k'] }`)
	if err == nil || !errors.Is(err, ErrVarIndexForm) {
		t.Fatalf("VarRefs index-форма: ожидался ErrVarIndexForm, получено: %v", err)
	}
}

// TestVarRefs_NestedSelect — вложенный select `vars.a.b` извлекает корневой
// var-ключ `a` (PostOrderVisit посещает Select `vars.a` отдельным уровнем).
func TestVarRefs_NestedSelect(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ vars.cfg.path }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cfg"}) {
		t.Errorf("VarRefs = %v, want [cfg] (корневой var-ключ вложенного select)", got)
	}
}

// TestVarRefs_MacroBlockNoVars — блок с валидным макро-выражением БЕЗ vars-ссылок
// (`[1,2].exists(x, x > 0)`) рядом с vars-блоком: VarRefs обходит его AST, не
// находит base=="vars" и собирает ссылку только из второго блока, без ошибки.
// Покрывает ветку, где parseNoMacro успешен, но PostOrderVisit не даёт vars-имён.
func TestVarRefs_MacroBlockNoVars(t *testing.T) {
	e := newVarRefsEngine(t)
	got, err := e.VarRefs("${ [1,2].exists(x, x > 0) }-${ vars.a }")
	if err != nil {
		t.Fatalf("VarRefs: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("VarRefs = %v, want [a] (макро-блок без vars пропущен, vars.a собран)", got)
	}
}

// TestVarRefs_BrokenBlockRejectedEarly фиксирует фактическую границу: блок с
// синтаксически-битым CEL до per-block parseNoMacro НЕ доходит — scanInterpolation
// (parseBlock) сам гейтит блок через env.Parse и не находит закрывающую `}` для
// невалидного выражения, отдавая *ErrCompile. То есть `continue` на perr в
// varrefs.go (зеркало DetectSealed) для входа через VarRefs недостижим: VarRefs не
// «молча пропускает» битый блок, а возвращает ошибку парс-фазы. Тест защищает это
// поведение от регресса (если parseBlock ослабят, маска изменится — тест упадёт).
func TestVarRefs_BrokenBlockRejectedEarly(t *testing.T) {
	e := newVarRefsEngine(t)
	_, err := e.VarRefs("${ vars.a }-${ vars.b + }")
	if err == nil {
		t.Fatal("VarRefs: ожидалась ошибка парс-фазы на битом блоке, получено nil")
	}
	var compileErr *ErrCompile
	if !errors.As(err, &compileErr) {
		t.Errorf("VarRefs: ожидался *ErrCompile, получено %T: %v", err, err)
	}
}
