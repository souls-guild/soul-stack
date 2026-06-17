package tmpl

import (
	"errors"
	"strings"
	"testing"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestRenderBasic(t *testing.T) {
	e := newEngine(t)
	got, err := e.Render("server {{ .name }} port {{ .port }}",
		map[string]any{"name": "db", "port": 6379})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if want := "server db port 6379"; got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
}

func TestRenderAllowedFunc(t *testing.T) {
	e := newEngine(t)
	got, err := e.Render(`{{ upper .x }}`, map[string]any{"x": "abc"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "ABC" {
		t.Fatalf("upper = %q, want %q", got, "ABC")
	}
}

func TestRenderBuiltinFunc(t *testing.T) {
	// Встроенные функции text/template (printf) доступны помимо sprig.
	e := newEngine(t)
	got, err := e.Render(`{{ printf "%d-%s" .n .s }}`,
		map[string]any{"n": 3, "s": "x"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "3-x" {
		t.Fatalf("printf = %q, want %q", got, "3-x")
	}
}

func TestRenderMissingKeyError(t *testing.T) {
	e := newEngine(t)
	_, err := e.Render("{{ .missing }}", map[string]any{"present": 1})
	if err == nil {
		t.Fatal("ожидалась ошибка на отсутствующий ключ (strict-mode), получен nil")
	}
	var exec *ErrExecute
	if !errors.As(err, &exec) {
		t.Fatalf("ожидался *ErrExecute, получен %T: %v", err, err)
	}
}

func TestRenderMissingKeyOnNilVars(t *testing.T) {
	e := newEngine(t)
	_, err := e.Render("{{ .anything }}", nil)
	if err == nil {
		t.Fatal("ожидалась ошибка на обращение к ключу при nil-vars")
	}
}

func TestRenderNoVarsNoAccess(t *testing.T) {
	// Шаблон без обращений к данным рендерится и при nil-vars.
	e := newEngine(t)
	got, err := e.Render("static text", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "static text" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderParseError(t *testing.T) {
	e := newEngine(t)
	_, err := e.Render("{{ .unterminated ", nil)
	if err == nil {
		t.Fatal("ожидалась ошибка парсинга незакрытого действия")
	}
	var perr *ErrParse
	if !errors.As(err, &perr) {
		t.Fatalf("ожидался *ErrParse, получен %T: %v", err, err)
	}
}

// excludedFuncs — функции, явно запрещённые [templating.md §3.3]. Вызов
// любой из них = ошибка парсинга (функция не зарегистрирована).
var excludedFuncs = []string{
	"env", "expandenv", "exec", "getHostByName",
	"derivePassword", "genCA", "genPrivateKey",
	"genSelfSignedCert", "genSignedCert", "buildCustomCert",
	"randAlphaNum", "randAlpha", "randAscii", "randNumeric", "randBytes",
	"tpl", "include",
}

func TestRenderExcludedFuncsFail(t *testing.T) {
	e := newEngine(t)
	for _, name := range excludedFuncs {
		t.Run(name, func(t *testing.T) {
			_, err := e.Render("{{ "+name+" \"x\" }}", nil)
			if err == nil {
				t.Fatalf("функция %q должна быть недоступна, рендер прошёл без ошибки", name)
			}
			var perr *ErrParse
			if !errors.As(err, &perr) {
				t.Fatalf("ожидался *ErrParse для %q, получен %T: %v", name, err, err)
			}
		})
	}
}

func TestRenderEnvExcluded(t *testing.T) {
	// Явный кейс из verification: {{ env "PATH" }} → error.
	e := newEngine(t)
	_, err := e.Render(`{{ env "PATH" }}`, nil)
	if err == nil {
		t.Fatal(`{{ env "PATH" }} должно давать ошибку (excluded)`)
	}
	if !strings.Contains(err.Error(), "tmpl parse") {
		t.Fatalf("ожидалась parse-ошибка, получено: %v", err)
	}
}

// TestFuncMapAllowlistEnforced проверяет, что собранный FuncMap содержит
// ровно allowlist и ни одной запрещённой функции.
func TestFuncMapAllowlistEnforced(t *testing.T) {
	funcs, err := buildFuncMap()
	if err != nil {
		t.Fatalf("buildFuncMap: %v", err)
	}

	for _, name := range allowedSprig {
		if _, ok := funcs[name]; !ok {
			t.Errorf("разрешённая функция %q отсутствует в FuncMap", name)
		}
	}
	// Собственные функции Soul Stack (не из sprig) — toYaml/fromYaml.
	for _, name := range customFuncNames {
		if _, ok := funcs[name]; !ok {
			t.Errorf("собственная функция %q отсутствует в FuncMap", name)
		}
	}
	for _, name := range excludedFuncs {
		if _, ok := funcs[name]; ok {
			t.Errorf("запрещённая функция %q присутствует в FuncMap", name)
		}
	}
	if want := len(allowedSprig) + len(customFuncNames); len(funcs) != want {
		t.Errorf("FuncMap содержит %d функций, ожидалось ровно %d (allowlist + custom)",
			len(funcs), want)
	}
}

// customFuncNames — собственные функции Soul Stack поверх sprig-allowlist-а
// ([yaml_funcs.go]). Отдельны от allowedSprig: их нет в upstream sprig.
var customFuncNames = []string{"toYaml", "fromYaml"}

func TestToYaml(t *testing.T) {
	e := newEngine(t)
	cases := map[string]struct {
		tmpl string
		vars map[string]any
		want string
	}{
		"scalar": {`{{ toYaml .x }}`, map[string]any{"x": 5}, "5"},
		"string": {`{{ toYaml .x }}`, map[string]any{"x": "hello"}, "hello"},
		"list": {`{{ toYaml .x }}`,
			map[string]any{"x": []any{"a", "b"}}, "- a\n- b"},
		"map": {`{{ toYaml .x }}`,
			map[string]any{"x": map[string]any{"k": "v"}}, "k: v"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := e.Render(c.tmpl, c.vars)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got != c.want {
				t.Fatalf("toYaml = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFromYaml(t *testing.T) {
	e := newEngine(t)
	got, err := e.Render(`{{ (fromYaml .x).key }}`,
		map[string]any{"x": "key: value"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "value" {
		t.Fatalf("fromYaml.key = %q, want %q", got, "value")
	}
}

func TestYamlRoundTrip(t *testing.T) {
	// toYaml → fromYaml восстанавливает исходную структуру (доступ к полю).
	e := newEngine(t)
	got, err := e.Render(
		`{{ (fromYaml (toYaml .x)).name }}`,
		map[string]any{"x": map[string]any{"name": "redis", "port": 6379}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "redis" {
		t.Fatalf("round-trip name = %q, want %q", got, "redis")
	}
}

func TestFromYamlParseError(t *testing.T) {
	// Невалидный YAML проваливает рендер штатно (ErrExecute), а не молча.
	e := newEngine(t)
	_, err := e.Render(`{{ fromYaml .x }}`,
		map[string]any{"x": "key: : : broken"})
	if err == nil {
		t.Fatal("ожидалась ошибка на невалидном YAML, получен nil")
	}
	var exec *ErrExecute
	if !errors.As(err, &exec) {
		t.Fatalf("ожидался *ErrExecute, получен %T: %v", err, err)
	}
}

func TestRenderAllowedSprigCoverage(t *testing.T) {
	// Каждая разрешённая функция должна быть вызываема (smoke).
	e := newEngine(t)
	cases := map[string]struct {
		tmpl string
		want string
	}{
		"default":    {`{{ default "d" "" }}`, "d"},
		"trim":       {`{{ trim "  a  " }}`, "a"},
		"quote":      {`{{ quote "a" }}`, `"a"`},
		"join":       {`{{ join "," (splitList " " "a b") }}`, "a,b"},
		"toString":   {`{{ toString 5 }}`, "5"},
		"add":        {`{{ add 2 3 }}`, "5"},
		"b64enc":     {`{{ b64enc "a" }}`, "YQ=="},
		"sha256sum":  {`{{ sha256sum "" }}`, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		"upperLower": {`{{ lower (upper "Ab") }}`, "ab"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := e.Render(c.tmpl, map[string]any{})
			if err != nil {
				t.Fatalf("Render(%q): %v", c.tmpl, err)
			}
			if got != c.want {
				t.Fatalf("%s = %q, want %q", name, got, c.want)
			}
		})
	}
}
