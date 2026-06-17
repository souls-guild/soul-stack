package soul

import (
	"errors"
	"reflect"
	"testing"
)

func TestApplyCovenMode_Append(t *testing.T) {
	final, removed := ApplyCovenMode([]string{"prod", "db"}, []string{"db", "edge"}, CovenAppend)
	if !reflect.DeepEqual(final, []string{"db", "edge", "prod"}) {
		t.Errorf("final = %v, want [db edge prod]", final)
	}
	if removed != nil {
		t.Errorf("removed = %v, want nil for append", removed)
	}
}

func TestApplyCovenMode_Append_Idempotent(t *testing.T) {
	final, _ := ApplyCovenMode([]string{"prod"}, []string{"prod"}, CovenAppend)
	if !reflect.DeepEqual(final, []string{"prod"}) {
		t.Errorf("final = %v, want [prod]", final)
	}
}

func TestApplyCovenMode_Replace(t *testing.T) {
	final, removed := ApplyCovenMode([]string{"a", "b"}, []string{"c", "c", "a"}, CovenReplace)
	if !reflect.DeepEqual(final, []string{"a", "c"}) {
		t.Errorf("final = %v, want [a c] (deduped, sorted)", final)
	}
	if removed != nil {
		t.Errorf("removed = %v, want nil for replace", removed)
	}
}

func TestApplyCovenMode_Remove(t *testing.T) {
	final, removed := ApplyCovenMode([]string{"prod", "db", "old"}, []string{"old", "missing"}, CovenRemove)
	if !reflect.DeepEqual(final, []string{"db", "prod"}) {
		t.Errorf("final = %v, want [db prod]", final)
	}
	if !reflect.DeepEqual(removed, []string{"old"}) {
		t.Errorf("removed = %v, want [old] (missing not removed)", removed)
	}
}

func TestApplyCovenMode_Remove_NoMatch(t *testing.T) {
	final, removed := ApplyCovenMode([]string{"prod"}, []string{"nope"}, CovenRemove)
	if !reflect.DeepEqual(final, []string{"prod"}) {
		t.Errorf("final = %v, want [prod]", final)
	}
	if removed != nil {
		t.Errorf("removed = %v, want nil", removed)
	}
}

func TestCovenSetEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a", "a", "b"}, []string{"a", "b"}, true},
		{[]string{"a"}, []string{"a", "b"}, false},
		{nil, nil, true},
		{nil, []string{}, true},
	}
	for _, c := range cases {
		if got := CovenSetEqual(c.a, c.b); got != c.want {
			t.Errorf("CovenSetEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestValidCovenMode(t *testing.T) {
	for _, m := range []CovenMode{CovenAppend, CovenReplace, CovenRemove} {
		if !ValidCovenMode(m) {
			t.Errorf("ValidCovenMode(%q) = false, want true", m)
		}
	}
	for _, m := range []CovenMode{"merge", "", "APPEND"} {
		if ValidCovenMode(m) {
			t.Errorf("ValidCovenMode(%q) = true, want false", m)
		}
	}
}

func TestNoopCovenLabelValidator(t *testing.T) {
	if err := (NoopCovenLabelValidator{}).Validate("anything"); err != nil {
		t.Errorf("noop validator returned %v, want nil", err)
	}
}

func TestBuildBulkWhere_EmptySelector(t *testing.T) {
	_, _, err := buildBulkWhere(BulkSelector{}, BulkScope{Unrestricted: true})
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Errorf("err = %v, want ErrBulkEmptySelector", err)
	}
}

func TestBuildBulkWhere_AllWithScope(t *testing.T) {
	where, args, err := buildBulkWhere(BulkSelector{All: true}, BulkScope{Covens: []string{"dev"}})
	if err != nil {
		t.Fatalf("buildBulkWhere: %v", err)
	}
	// All сам clause не добавляет; scope добавляет coven && $1.
	if where != " WHERE coven && $1" {
		t.Errorf("where = %q, want ' WHERE coven && $1'", where)
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want 1 (scope covens)", args)
	}
}

func TestBuildBulkWhere_Unrestricted_NoScopeClause(t *testing.T) {
	where, args, err := buildBulkWhere(BulkSelector{All: true}, BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("buildBulkWhere: %v", err)
	}
	if where != "" {
		t.Errorf("where = %q, want '' (all + unrestricted = no predicate)", where)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want empty", args)
	}
}

func TestBulkAssignCoven_LabelOutOfScope_NoDB(t *testing.T) {
	// Гейт (b) проверяется ДО любого обращения к БД — fakeDB с нулём вызовов.
	f := &fakeDB{}
	_, err := BulkAssignCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, "prod", CovenAppend)
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on out-of-scope label (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

func TestBulkAssignCoven_RejectsBadLabel(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, "BAD_LABEL", CovenAppend)
	if err == nil {
		t.Fatal("BulkAssignCoven with bad label returned nil err")
	}
}

func TestBulkAssignCoven_RejectsReplaceMode(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkAssignCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, "edge", CovenReplace)
	if err == nil {
		t.Fatal("BulkAssignCoven with replace mode returned nil err (replace разнесён в BulkReplaceCoven)")
	}
}

// --- BulkReplaceCoven scope-гейт (b): каждая метка набора ∈ scope ---

// Гейт (b) на replace должен отвергнуть набор, в котором ХОТЯ БЫ ОДНА метка
// вне scope (privilege-escalation вектор: `[dev, prod]` с scope=dev). Проверка
// — ДО любых обращений к БД.
func TestBulkReplaceCoven_RejectsLabelOutOfScope_NoDB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkReplaceCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, []string{"dev", "prod"})
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if f.queryCalls != 0 || f.execCalls != 0 {
		t.Errorf("DB touched on out-of-scope label set (queries=%d execs=%d)", f.queryCalls, f.execCalls)
	}
}

// Гейт (b) на replace: каждая метка набора в scope → проходит гейт и доходит
// до CountBulkMatched (который fakeDB возвращает ErrNoRows на пустой rowFunc
// → ошибка count-а, но не ErrBulkLabelOutOfScope).
func TestBulkReplaceCoven_AllLabelsInScope_PassesGateB(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkReplaceCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev", "edge"}}, []string{"dev", "edge"})
	if errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, all labels in scope must NOT trigger gate (b)", err)
	}
	// CountBulkMatched будет вызван → queryCalls > 0.
	if f.queryCalls == 0 {
		t.Errorf("queryCalls = 0, want >0 (gate b passed, count must run)")
	}
}

func TestBulkReplaceCoven_RejectsBadLabel(t *testing.T) {
	f := &fakeDB{}
	_, err := BulkReplaceCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, []string{"BAD_LABEL"})
	if err == nil {
		t.Fatal("BulkReplaceCoven with bad label returned nil err")
	}
	if errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Errorf("bad label маппится в ErrBulkLabelOutOfScope, want format-error")
	}
}

// Unrestricted-scope: любой набор меток проходит гейт (b), включая пустой.
func TestBulkReplaceCoven_Unrestricted_PassesAnyLabels(t *testing.T) {
	for _, labels := range [][]string{
		{"prod", "edge"},
		{},
		nil,
	} {
		f := &fakeDB{}
		_, err := BulkReplaceCoven(nil, bulkFakePool{f}, BulkSelector{All: true},
			BulkScope{Unrestricted: true}, labels)
		if errors.Is(err, ErrBulkLabelOutOfScope) {
			t.Errorf("labels=%v: unrestricted scope не должен срабатывать на гейт b", labels)
		}
	}
}

// --- Incarnation-селектор ---

// Incarnation-селектор должен генерировать предикат `$n = ANY(coven)` —
// идентично Coven-полю. Comb с другими критериями = AND.
func TestBulkSelector_IncarnationOnly(t *testing.T) {
	where, args, err := buildBulkWhere(BulkSelector{Incarnation: "redis"},
		BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("buildBulkWhere: %v", err)
	}
	if where != " WHERE $1 = ANY(coven)" {
		t.Errorf("where = %q, want ' WHERE $1 = ANY(coven)' (incarnation as coven-label)", where)
	}
	if len(args) != 1 || args[0] != "redis" {
		t.Errorf("args = %v, want [redis]", args)
	}
}

// Incarnation+Status: AND-комбинация двух предикатов.
func TestBulkSelector_IncarnationPlusStatus(t *testing.T) {
	where, args, err := buildBulkWhere(BulkSelector{Incarnation: "redis", Status: StatusConnected},
		BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("buildBulkWhere: %v", err)
	}
	// incarnation → $1 = ANY(coven); status → status = $2.
	if where != " WHERE $1 = ANY(coven) AND status = $2" {
		t.Errorf("where = %q, want incarnation AND status AND-комбинация", where)
	}
	if len(args) != 2 || args[0] != "redis" || args[1] != string(StatusConnected) {
		t.Errorf("args = %v, want [redis connected]", args)
	}
}

// Incarnation+Coven: оба генерируют `$n = ANY(coven)`; AND-комбинация даёт
// двойной предикат (хост обязан содержать ОБЕ метки). На SQL — `coven && ANY`
// для двух отдельных скаляров через AND, что эквивалентно пересечению.
func TestBulkSelector_IncarnationPlusCoven(t *testing.T) {
	where, args, err := buildBulkWhere(BulkSelector{Coven: "stage", Incarnation: "redis"},
		BulkScope{Unrestricted: true})
	if err != nil {
		t.Fatalf("buildBulkWhere: %v", err)
	}
	if where != " WHERE $1 = ANY(coven) AND $2 = ANY(coven)" {
		t.Errorf("where = %q, want coven AND incarnation as двойной = ANY(coven)", where)
	}
	if len(args) != 2 || args[0] != "stage" || args[1] != "redis" {
		t.Errorf("args = %v, want [stage redis]", args)
	}
}

// Пустой Incarnation не должен добавлять clause (single-criterion empty
// selector → ErrBulkEmptySelector).
func TestBulkSelector_EmptyIncarnation_NoSelector(t *testing.T) {
	_, _, err := buildBulkWhere(BulkSelector{}, BulkScope{Unrestricted: true})
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Errorf("err = %v, want ErrBulkEmptySelector (Incarnation пуст не считается)", err)
	}
}
