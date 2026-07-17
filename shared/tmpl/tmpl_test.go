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
	// Built-in text/template functions (printf) are available besides sprig.
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
		t.Fatal("expected an error for a missing key (strict-mode), got nil")
	}
	var exec *ErrExecute
	if !errors.As(err, &exec) {
		t.Fatalf("expected *ErrExecute, got %T: %v", err, err)
	}
}

func TestRenderMissingKeyOnNilVars(t *testing.T) {
	e := newEngine(t)
	_, err := e.Render("{{ .anything }}", nil)
	if err == nil {
		t.Fatal("expected an error for key access with nil vars")
	}
}

func TestRenderNoVarsNoAccess(t *testing.T) {
	// A template with no data access renders even with nil vars.
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
		t.Fatal("expected a parse error for an unterminated action")
	}
	var perr *ErrParse
	if !errors.As(err, &perr) {
		t.Fatalf("expected *ErrParse, got %T: %v", err, err)
	}
}

// excludedFuncs are functions explicitly forbidden by [templating.md §3.3].
// Calling any of them = a parse error (the function is not registered).
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
				t.Fatalf("function %q should be unavailable, render succeeded without error", name)
			}
			var perr *ErrParse
			if !errors.As(err, &perr) {
				t.Fatalf("expected *ErrParse for %q, got %T: %v", name, err, err)
			}
		})
	}
}

func TestRenderEnvExcluded(t *testing.T) {
	// Explicit case from verification: {{ env "PATH" }} → error.
	e := newEngine(t)
	_, err := e.Render(`{{ env "PATH" }}`, nil)
	if err == nil {
		t.Fatal(`{{ env "PATH" }} should give an error (excluded)`)
	}
	if !strings.Contains(err.Error(), "tmpl parse") {
		t.Fatalf("expected a parse error, got: %v", err)
	}
}

// TestFuncMapAllowlistEnforced verifies the assembled FuncMap contains
// exactly the allowlist and no forbidden function.
func TestFuncMapAllowlistEnforced(t *testing.T) {
	funcs, err := buildFuncMap()
	if err != nil {
		t.Fatalf("buildFuncMap: %v", err)
	}

	for _, name := range allowedSprig {
		if _, ok := funcs[name]; !ok {
			t.Errorf("allowed function %q is missing from FuncMap", name)
		}
	}
	// Soul Stack's own functions (not from sprig) — toYaml/fromYaml.
	for _, name := range customFuncNames {
		if _, ok := funcs[name]; !ok {
			t.Errorf("custom function %q is missing from FuncMap", name)
		}
	}
	for _, name := range excludedFuncs {
		if _, ok := funcs[name]; ok {
			t.Errorf("forbidden function %q is present in FuncMap", name)
		}
	}
	if want := len(allowedSprig) + len(customFuncNames); len(funcs) != want {
		t.Errorf("FuncMap contains %d functions, expected exactly %d (allowlist + custom)",
			len(funcs), want)
	}
}

// customFuncNames are Soul Stack's own functions on top of the sprig allowlist
// ([yaml_funcs.go]). Separate from allowedSprig: they are not in upstream sprig.
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
	// toYaml → fromYaml restores the original structure (field access).
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
	// Invalid YAML fails the render cleanly (ErrExecute), not silently.
	e := newEngine(t)
	_, err := e.Render(`{{ fromYaml .x }}`,
		map[string]any{"x": "key: : : broken"})
	if err == nil {
		t.Fatal("expected an error for invalid YAML, got nil")
	}
	var exec *ErrExecute
	if !errors.As(err, &exec) {
		t.Fatalf("expected *ErrExecute, got %T: %v", err, err)
	}
}

func TestRenderAllowedSprigCoverage(t *testing.T) {
	// Every allowed function must be callable (smoke).
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
