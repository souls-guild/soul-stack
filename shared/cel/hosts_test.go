package cel

import (
	"errors"
	"strings"
	"testing"
)

// hostsFixture — три хоста прогона со стабильными фактами для тестов
// soulprint.hosts / .where.
func hostsFixture() []map[string]any {
	return []map[string]any{
		{
			"sid":     "web-1.example.com",
			"role":    "primary",
			"covens":  []any{"web", "prod"},
			"network": map[string]any{"primary_ip": "10.0.0.1"},
			"os":      map[string]any{"family": "debian"},
		},
		{
			"sid":     "web-2.example.com",
			"role":    "replica",
			"covens":  []any{"web", "prod"},
			"network": map[string]any{"primary_ip": "10.0.0.2"},
			"os":      map[string]any{"family": "debian"},
		},
		{
			"sid":     "db-1.example.com",
			"role":    "replica",
			"covens":  []any{"db", "prod"},
			"network": map[string]any{"primary_ip": "10.0.0.3"},
			"os":      map[string]any{"family": "rhel"},
		},
	}
}

func scenarioVars() Vars {
	return Vars{
		SoulprintHosts: hostsFixture(),
		Incarnation:    map[string]any{"name": "prod"},
		SoulprintSelf:  map[string]any{"sid": "web-1.example.com"},
		AllowHosts:     true,
	}
}

func TestHosts_ListProjection(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression("soulprint.hosts.size()", scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(3) {
		t.Fatalf("soulprint.hosts.size(): ожидали 3, получили %v", got)
	}
}

func TestHosts_WhereByRole(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression(`soulprint.hosts.where("role == 'primary'").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where role==primary size: ожидали 1, получили %v", got)
	}
}

func TestHosts_WhereByCoven(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression(`soulprint.hosts.where("'web' in covens").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(2) {
		t.Fatalf("where 'web' in covens size: ожидали 2, получили %v", got)
	}
}

func TestHosts_WhereByOs(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression(`soulprint.hosts.where("os.family == 'rhel'").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where os.family==rhel size: ожидали 1, получили %v", got)
	}
}

func TestHosts_WhereWithExternalContext(t *testing.T) {
	e := newEngine(t)
	// incarnation.name НЕ квалифицируется в __host.*; covens — квалифицируется.
	out, err := e.EvalExpression(`soulprint.hosts.where("incarnation.name in covens").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(3) {
		t.Fatalf("where incarnation.name in covens size: ожидали 3 (все в prod), получили %v", got)
	}
}

func TestHosts_WhereEqualsSoulprintWhere(t *testing.T) {
	e := newEngine(t)
	v := scenarioVars()
	a, err := e.EvalExpression(`soulprint.where("'web' in covens").size()`, v)
	if err != nil {
		t.Fatalf("soulprint.where: %v", err)
	}
	b, err := e.EvalExpression(`soulprint.hosts.where("'web' in covens").size()`, v)
	if err != nil {
		t.Fatalf("soulprint.hosts.where: %v", err)
	}
	if a.Value() != b.Value() {
		t.Fatalf("soulprint.where (%v) != soulprint.hosts.where (%v)", a.Value(), b.Value())
	}
	if a.Value() != int64(2) {
		t.Fatalf("ожидали 2, получили %v", a.Value())
	}
}

func TestHosts_FirstElementIndex0(t *testing.T) {
	e := newEngine(t)
	// [0] нативно; .first НЕ вводится.
	out, err := e.EvalExpression(`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != "10.0.0.1" {
		t.Fatalf("[0].network.primary_ip: ожидали 10.0.0.1, получили %v", got)
	}
}

func TestHosts_Interpolation(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalInterpolation(`ip=${ soulprint.hosts.where("role == 'primary'")[0].network.primary_ip }`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalInterpolation: %v", err)
	}
	if out != "ip=10.0.0.1" {
		t.Fatalf("ожидали ip=10.0.0.1, получили %v", out)
	}
}

func TestHosts_NonLiteralPredicate(t *testing.T) {
	e := newEngine(t)
	// Динамический предикат (склейка строк) → понятная compile-ошибка.
	_, err := e.EvalExpression(`soulprint.hosts.where("'" + incarnation.name + "' in covens").size()`, scenarioVars())
	if err == nil {
		t.Fatalf("ожидали ошибку для не-литерального предиката")
	}
	if !strings.Contains(err.Error(), "static string literal") {
		t.Fatalf("ожидали сообщение про static string literal, получили: %v", err)
	}
}

func TestHosts_NonStringLiteralPredicate(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression(`soulprint.hosts.where(42).size()`, scenarioVars())
	if err == nil || !strings.Contains(err.Error(), "static string literal") {
		t.Fatalf("ожидали ошибку про static string literal, получили: %v", err)
	}
}

func TestHosts_ReceiverNotSoulprint(t *testing.T) {
	e := newEngine(t)
	// generic .where на произвольном list запрещён.
	_, err := e.EvalExpression(`input.xs.where("x > 0").size()`, Vars{
		Input:      map[string]any{"xs": []any{int64(1), int64(2)}},
		AllowHosts: true,
	})
	if err == nil {
		t.Fatalf("ожидали ошибку для generic .where на input.xs")
	}
	if !strings.Contains(err.Error(), "soulprint.hosts") {
		t.Fatalf("ожидали сообщение про разрешённый только soulprint receiver, получили: %v", err)
	}
}

func TestHosts_NestedWhere(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression(`soulprint.hosts.where("soulprint.hosts.where('role == \"primary\"').size() > 0").size()`, scenarioVars())
	if err == nil {
		t.Fatalf("ожидали ошибку для nested .where в предикате")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Fatalf("ожидали сообщение про nested .where, получили: %v", err)
	}
}

func TestHosts_DestinyIsolation(t *testing.T) {
	e := newEngine(t)
	// AllowHosts=false (destiny-проход) → обращение к soulprint.hosts — ошибка.
	v := scenarioVars()
	v.AllowHosts = false
	_, err := e.EvalExpression("soulprint.hosts.size()", v)
	var ue *ErrUnsupported
	if !errors.As(err, &ue) {
		t.Fatalf("ожидали *ErrUnsupported (изоляция destiny), получили %T: %v", err, err)
	}
	// soulprint.where тоже отсекается изоляцией.
	_, err = e.EvalExpression(`soulprint.where("'web' in covens").size()`, v)
	if !errors.As(err, &ue) {
		t.Fatalf("ожидали *ErrUnsupported для soulprint.where в destiny, получили %T: %v", err, err)
	}
}

func TestHosts_DestinyIsolationDoesNotAffectSelf(t *testing.T) {
	e := newEngine(t)
	// soulprint.self.* остаётся доступен и при AllowHosts=false.
	out, err := e.EvalExpression("soulprint.self.sid", Vars{
		SoulprintSelf: map[string]any{"sid": "web-1.example.com"},
		AllowHosts:    false,
	})
	if err != nil {
		t.Fatalf("soulprint.self при AllowHosts=false: %v", err)
	}
	if out.Value() != "web-1.example.com" {
		t.Fatalf("ожидали web-1.example.com, получили %v", out.Value())
	}
}

func TestHosts_IterVarCollision(t *testing.T) {
	e := newEngine(t)
	// `__host` встречается в выражении как поле — iter-переменная получает суффикс,
	// фильтр всё равно работает корректно.
	hosts := []map[string]any{
		{"role": "primary", "__host": "x"},
		{"role": "replica", "__host": "y"},
	}
	v := Vars{SoulprintHosts: hosts, AllowHosts: true}
	out, err := e.EvalExpression(`soulprint.hosts.where("role == 'primary'").size()`, v)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("ожидали 1, получили %v", got)
	}
	// Предикат, явно ссылающийся на поле __host элемента: iter-переменная не должна
	// его затенять (она переименована в __host0).
	out, err = e.EvalExpression(`soulprint.hosts.where("__host == 'x'").size()`, v)
	if err != nil {
		t.Fatalf("EvalExpression (__host field): %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where __host=='x': ожидали 1, получили %v", got)
	}
}

func TestHosts_EmptyHostsList(t *testing.T) {
	e := newEngine(t)
	// nil SoulprintHosts при AllowHosts=true → пустой список (не паника, не ошибка).
	out, err := e.EvalExpression("soulprint.hosts.size()", Vars{AllowHosts: true})
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(0) {
		t.Fatalf("пустой soulprint.hosts: ожидали 0, получили %v", got)
	}
}

func TestHosts_SizeFunctionForm(t *testing.T) {
	e := newEngine(t)
	out, err := e.EvalExpression(`size(soulprint.hosts.where("'prod' in covens")) == 3`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if out.Value() != true {
		t.Fatalf("ожидали true, получили %v", out.Value())
	}
}

// --- MAJOR-фикс: макрос внутри/рядом с .where (no-macro round-trip) ---

func TestHosts_WhereWithMacroExistsPredicate(t *testing.T) {
	e := newEngine(t)
	// covens.exists(c, c == 'db') — идиоматичный фильтр по списку внутри предиката.
	// covens квалифицируется в __host.covens, iter-переменная c макроса — нет.
	out, err := e.EvalExpression(`soulprint.hosts.where("covens.exists(c, c == 'db')").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where covens.exists(db) size: ожидали 1, получили %v", got)
	}
}

func TestHosts_WhereWithMacroAllPredicate(t *testing.T) {
	e := newEngine(t)
	// covens.all(c, c != 'staging') — у всех хостов нет коврена staging.
	out, err := e.EvalExpression(`soulprint.hosts.where("covens.all(c, c != 'staging')").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(3) {
		t.Fatalf("where covens.all size: ожидали 3, получили %v", got)
	}
}

func TestHosts_MacroAdjacentToWhere(t *testing.T) {
	e := newEngine(t)
	// Макрос input.xs.exists(...) РЯДОМ с .where в одном выражении: оба должны
	// корректно round-trip'нуться и раскрыться нативно при финальном compile.
	v := scenarioVars()
	v.Input = map[string]any{"xs": []any{int64(1), int64(2), int64(3)}}
	out, err := e.EvalExpression(
		`size(soulprint.hosts.where("role == 'replica'")) > 0 && input.xs.exists(x, x == 2)`, v)
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if out.Value() != true {
		t.Fatalf("макрос рядом с .where: ожидали true, получили %v", out.Value())
	}
}

func TestHosts_WhereWithMacroMapPredicate(t *testing.T) {
	e := newEngine(t)
	// covens.map(c, c) — трансформ-форма макроса; iter-переменная c не
	// квалифицируется, covens → __host.covens. Хост попадает, если 'web' среди
	// маппинга ковенов (т.е. среди covens) — эквивалент 'web' in covens.
	out, err := e.EvalExpression(`soulprint.hosts.where("'web' in covens.map(c, c)").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(2) {
		t.Fatalf("where covens.map size: ожидали 2, получили %v", got)
	}
}

// --- Пробелы покрытия (qa) ---

func TestHosts_EmptyFilterIndex0(t *testing.T) {
	e := newEngine(t)
	// where никого не отобрал → [0] над пустым списком → понятная runtime-ошибка
	// (index out of bounds / no such overload), НЕ паника, НЕ opaque internal.
	_, err := e.EvalExpression(`soulprint.hosts.where("role == 'no-such-role'")[0].sid`, scenarioVars())
	if err == nil {
		t.Fatalf("ожидали runtime-ошибку для [0] над пустым фильтром")
	}
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval (runtime), получили %T: %v", err, err)
	}
}

func TestHosts_WhereOnHostMissingFact(t *testing.T) {
	e := newEngine(t)
	// Хост без секции os: where("os.family == ...") над таким хостом — это
	// no-such-key (понятная runtime-ошибка), НЕ паника.
	hosts := []map[string]any{
		{"sid": "a", "os": map[string]any{"family": "debian"}},
		{"sid": "b"}, // без os
	}
	v := Vars{SoulprintHosts: hosts, AllowHosts: true}
	_, err := e.EvalExpression(`soulprint.hosts.where("os.family == 'debian'").size()`, v)
	if err == nil {
		t.Fatalf("ожидали runtime-ошибку для where над хостом без факта os")
	}
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval (no such key), получили %T: %v", err, err)
	}
}

func TestHosts_Index0NetworkOnHostMissingFact(t *testing.T) {
	e := newEngine(t)
	// [0].network.primary_ip над хостом без секции network — no-such-key, не паника.
	hosts := []map[string]any{
		{"sid": "a", "role": "primary"}, // без network
	}
	v := Vars{SoulprintHosts: hosts, AllowHosts: true}
	_, err := e.EvalExpression(`soulprint.hosts.where("role == 'primary'")[0].network.primary_ip`, v)
	if err == nil {
		t.Fatalf("ожидали runtime-ошибку для .network над хостом без факта network")
	}
	var ee *ErrEval
	if !errors.As(err, &ee) {
		t.Fatalf("ожидали *ErrEval (no such key), получили %T: %v", err, err)
	}
}

func TestHosts_PredicateUnbalancedParen(t *testing.T) {
	e := newEngine(t)
	// Инъекция: несбалансированная ) в литерале предиката → понятная Syntax
	// error предиката (не слом окружающего выражения, не паника).
	_, err := e.EvalExpression(`soulprint.hosts.where("role == 'primary')").size()`, scenarioVars())
	if err == nil {
		t.Fatalf("ожидали syntax-ошибку для несбалансированной скобки в предикате")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("ожидали *ErrCompile (syntax), получили %T: %v", err, err)
	}
}

func TestHosts_PredicateNestedQuoteOk(t *testing.T) {
	e := newEngine(t)
	// Вложенная кавычка через экранирование внутри литерала предиката —
	// корректно парсится, инъекции в окружающее выражение нет.
	out, err := e.EvalExpression(`soulprint.hosts.where("sid == \"web-1.example.com\"").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where sid==... (вложенная кавычка): ожидали 1, получили %v", got)
	}
}

func TestHosts_MixedPredicateBothSides(t *testing.T) {
	e := newEngine(t)
	// Смешанный предикат: sid (поле элемента → __host.sid) сравнивается с
	// soulprint.self.sid (внешний контекст, не квалифицируется). Должен
	// отобрать ровно тот хост, sid которого == self.
	out, err := e.EvalExpression(`soulprint.hosts.where("sid == soulprint.self.sid").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("EvalExpression: %v", err)
	}
	if got := out.Value(); got != int64(1) {
		t.Fatalf("where sid == soulprint.self.sid size: ожидали 1, получили %v", got)
	}
}
