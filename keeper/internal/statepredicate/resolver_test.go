package statepredicate

import (
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// Слайс S1 (фильтр→Run + RBAC Purview S2c + Cadence-state): единый
// statepredicate.Resolver — CEL-предикат по incarnation.state. TDD-first:
// тесты фиксируют контракт ДО реализации (red), затем зеленеют.
//
// Объём S1 — Compile (валидация + кэш program) + Matches (single-incarnation
// проверка против state-map). ResolveIncarnations (list + SQL-pushdown) —
// следующий слайс (нужен incarnation-репозиторий + DB-доступ), сюда не тянем.

func newResolver(t *testing.T) Resolver {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// --- Matches: равенство по строковому state-полю ---

func TestMatches_StringEquality(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"redis_version": "8.0"}

	if ok, err := r.Matches(`state.redis_version == "8.0"`, state); err != nil || !ok {
		t.Errorf(`redis_version=="8.0" на {8.0}: ok=%v err=%v, want true,nil`, ok, err)
	}
	if ok, err := r.Matches(`state.redis_version == "8.1"`, state); err != nil || ok {
		t.Errorf(`redis_version=="8.1" на {8.0}: ok=%v err=%v, want false,nil`, ok, err)
	}
}

// --- numeric: jsonb-числа (float64 после decode) корректно сравниваются ---

func TestMatches_Numeric(t *testing.T) {
	r := newResolver(t)

	// int (как из CEL-литерала / Go-int).
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": 2000}); err != nil || !ok {
		t.Errorf("memory_mb>1000 на int(2000): ok=%v err=%v, want true,nil", ok, err)
	}
	// float64 — форма чисел после json/jsonb-decode.
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": float64(2000)}); err != nil || !ok {
		t.Errorf("memory_mb>1000 на float64(2000): ok=%v err=%v, want true,nil (jsonb coercion)", ok, err)
	}
	if ok, err := r.Matches(`state.memory_mb > 1000`, map[string]any{"memory_mb": float64(500)}); err != nil || ok {
		t.Errorf("memory_mb>1000 на float64(500): ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- in/list ---

func TestMatches_InList(t *testing.T) {
	r := newResolver(t)
	if ok, err := r.Matches(`state.redis_version in ["8.0","8.1"]`, map[string]any{"redis_version": "8.0"}); err != nil || !ok {
		t.Errorf("in-list на 8.0: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := r.Matches(`state.redis_version in ["8.0","8.1"]`, map[string]any{"redis_version": "7.4"}); err != nil || ok {
		t.Errorf("in-list на 7.4: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- вложенное поле ---

func TestMatches_Nested(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"cluster": map[string]any{"replicas": 3}}
	if ok, err := r.Matches(`state.cluster.replicas == 3`, state); err != nil || !ok {
		t.Errorf("nested replicas==3: ok=%v err=%v, want true,nil", ok, err)
	}
}

// --- no-such-key: предикат по отсутствующему state-полю → (false, nil) ---
//
// Семантика fail-closed, консистентно с rbac.EvalSoulprintExpr (S2b) и
// oracle.WhereEvaluator: недоверенный/неполный state-снимок не должен ронять
// резолвер, отсутствие нужного факта = «не сматчило».
func TestMatches_NoSuchKey(t *testing.T) {
	r := newResolver(t)
	if ok, err := r.Matches(`state.absent == "x"`, map[string]any{"redis_version": "8.0"}); err != nil || ok {
		t.Errorf("отсутствующее поле: ok=%v err=%v, want false,nil (no-such-key → no-match)", ok, err)
	}
	if ok, err := r.Matches(`state.absent > 1`, map[string]any{}); err != nil || ok {
		t.Errorf("отсутствующее numeric-поле: ok=%v err=%v, want false,nil", ok, err)
	}
	// nil-state → no-match (не паника).
	if ok, err := r.Matches(`state.redis_version == "8.0"`, nil); err != nil || ok {
		t.Errorf("nil-state: ok=%v err=%v, want false,nil", ok, err)
	}
}

// --- sandbox: предикат с vault()/now()/register/... → ошибка Compile ---
//
// state-предикат = чистая функция от state (как migration-CEL ADR-019):
// vault()/now() отсекаются guard-ом, прочие корни — необъявленностью env.
func TestCompile_SandboxRejected(t *testing.T) {
	r := newResolver(t)
	cases := []string{
		`vault("secret/x") == "y"`,
		`now() > timestamp("2020-01-01T00:00:00Z")`,
		`register.foo == 1`,
		`soulprint.self.os.family == "debian"`,
		`input.bar == 1`,
		`incarnation.name == "x"`,
		`essence.baz == 1`,
	}
	for _, expr := range cases {
		t.Run(expr, func(t *testing.T) {
			if err := r.Compile(expr); err == nil {
				t.Fatalf("Compile(%q): want sandbox/compile error, got nil", expr)
			}
		})
	}
}

// --- битый CEL → ошибка Compile ---

func TestCompile_BrokenRejected(t *testing.T) {
	r := newResolver(t)
	cases := []string{
		`state.redis_version ==`,    // незавершённое выражение
		`state.redis_version && `,   // висящий оператор
		`(`,                         // несбалансированная скобка
		`state.redis_version + "x"`, // не-bool результат отсекается на Matches, но синтаксис ок — см. отдельный тест
	}
	// Только синтаксически битые (первые три) фейлят Compile.
	for _, expr := range cases[:3] {
		t.Run(expr, func(t *testing.T) {
			if err := r.Compile(expr); err == nil {
				t.Fatalf("Compile(%q): want compile error, got nil", expr)
			}
		})
	}
}

// Не-bool результат предиката → ошибка Matches (предикат обязан быть булевым),
// а НЕ fail-closed (false, nil). Различение не-bool от runtime-no-such-key —
// через типизированный sentinel [cel.ErrPredicateNotBool], не по тексту.
func TestMatches_NonBoolRejected(t *testing.T) {
	r := newResolver(t)
	ok, err := r.Matches(`state.redis_version`, map[string]any{"redis_version": "8.0"})
	if err == nil {
		t.Fatalf("не-bool предикат: ok=%v err=nil, want error (не fail-closed)", ok)
	}
	if !errors.Is(err, cel.ErrPredicateNotBool) {
		t.Fatalf("не-bool предикат: errors.Is(err, cel.ErrPredicateNotBool)=false, err=%v", err)
	}
}

// --- пустой предикат → ошибка Compile (fail-closed, без случайного match-all) ---
//
// Пустой state-предикат двусмыслен (match-all опасен для фильтра/RBAC-селектора),
// поэтому отвергается явно: caller, которому нужен «все инкарнации», просто не
// зовёт резолвер. Симметрично rbac-отклонению пустого soulprint-селектора.
func TestCompile_EmptyRejected(t *testing.T) {
	r := newResolver(t)
	if err := r.Compile(""); err == nil {
		t.Fatal("Compile(\"\"): want error for empty predicate, got nil")
	}
	if err := r.Compile("   "); err == nil {
		t.Fatal("Compile(пробелы): want error for blank predicate, got nil")
	}
	if _, err := r.Matches("", map[string]any{}); err == nil {
		t.Fatal("Matches(\"\", …): want error for empty predicate, got nil")
	}
}

// --- кэш: повторная компиляция одного выражения переиспользует program ---
//
// Косвенная проверка: Matches многократно не падает и даёт стабильный результат
// (program кэшируется в shared/cel.Engine; прямой счётчик компиляций тут не
// инспектируем — это деталь Engine). Гарантия «не перекомпилируем на каждый
// Matches» обеспечивается переиспользованием единого Engine под sync.Once.
func TestMatches_Repeatable(t *testing.T) {
	r := newResolver(t)
	state := map[string]any{"redis_version": "8.0"}
	for i := 0; i < 100; i++ {
		ok, err := r.Matches(`state.redis_version == "8.0"`, state)
		if err != nil || !ok {
			t.Fatalf("iter %d: ok=%v err=%v, want true,nil", i, ok, err)
		}
	}
}

// Compile валидного выражения — без ошибки (нормальный путь caller-валидации
// на load: фильтр/RBAC-селектор/Cadence-target компилируют предикат заранее).
func TestCompile_ValidOK(t *testing.T) {
	r := newResolver(t)
	if err := r.Compile(`state.redis_version == "8.0" && state.memory_mb > 1000`); err != nil {
		t.Fatalf("Compile(валидный): %v", err)
	}
}

// Sandbox-ошибка Compile несёт упоминание о state-предикате/sandbox (для
// внятной диагностики оператору).
func TestCompile_SandboxErrorMessage(t *testing.T) {
	r := newResolver(t)
	err := r.Compile(`register.foo == 1`)
	if err == nil {
		t.Fatal("want error")
	}
	// Сообщение должно отличать sandbox/compile от пустого/прочего; не привязываемся
	// к точному тексту cel-go, проверяем лишь непустоту.
	if strings.TrimSpace(err.Error()) == "" {
		t.Fatal("пустое сообщение ошибки")
	}
}
