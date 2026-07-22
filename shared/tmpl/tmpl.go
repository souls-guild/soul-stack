// Package tmpl wraps Go text/template for rendering `.tmpl` files via the
// `core.file.rendered` module ([ADR-010] §3, [templating.md §3]).
//
// The engine is isolated: a template sees only variables from the passed `vars`
// plus the built-in Go text/template functions and a closed sprig allowlist (see
// [allowedSprig] in sprig.go). The template has no direct access to
// essence/input/register/soulprint — the author must explicitly lift the needed
// values into `vars` on the CEL phase ([templating.md §6]).
//
// The sandbox is built from three barriers ([templating.md §7.2]):
//   - strict-mode `missingkey=error` — accessing a missing key gives a render
//     error, not a silent empty string;
//   - sprig via whitelist — functions outside the allowlist are unavailable
//     (env, exec, crypto-gen, random, metaprogramming are excluded);
//   - `vars` values are substituted as literals, there is no recursive
//     template-eval (`tpl`).
//
// Checking the `.tmpl` extension is the caller's job (Soul-side
// `core.file.rendered`), not this package's ([templating.md §3.4]).
//
// [ADR-010]: docs/adr/0010-templating.md
// [templating.md]: docs/templating.md
package tmpl

import (
	"fmt"
	"strings"
	"text/template"
)

// Engine renders `.tmpl` templates with a fixed set of functions.
// Thread-safe and stateless: the FuncMap is built once in [New] and only read
// afterward. One Engine is reused by all runs.
type Engine struct {
	funcs template.FuncMap
}

// New creates an Engine with the built-in text/template functions and the sprig
// allowlist. An error is possible only on a programmatic mismatch between the
// allowlist and the current sprig version (an allowlist name missing from the
// FuncMap) — a build bug, not user input.
func New() (*Engine, error) {
	funcs, err := buildFuncMap()
	if err != nil {
		return nil, err
	}
	return &Engine{funcs: funcs}, nil
}

// Render compiles and executes templateContent with the vars context.
//
// vars are referenced as `.<key>` (e.g. `{{ .name }}`). Accessing a missing key
// is an error ([missingkey=error]). Calling a function outside the
// text/template+sprig allowlist is a compile error.
//
// With nil vars a template with no data references still renders; any `.<key>`
// in it fails under strict-mode, which is intended.
func (e *Engine) Render(templateContent string, vars map[string]any) (string, error) {
	tmpl := template.New("rendered").
		Funcs(e.funcs).
		Option("missingkey=error")

	tmpl, err := tmpl.Parse(templateContent)
	if err != nil {
		return "", &ErrParse{Err: err}
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", &ErrExecute{Err: err}
	}

	return buf.String(), nil
}

// ErrParse is a template compile error: syntax or a function call outside the
// allowlist. Per [templating.md §10] it is a validation/render-phase error, the
// step fails normally.
type ErrParse struct{ Err error }

func (e *ErrParse) Error() string { return fmt.Sprintf("tmpl parse: %v", e.Err) }
func (e *ErrParse) Unwrap() error { return e.Err }

// ErrExecute is a template execution error: accessing a missing key
// (strict-mode) or a function runtime error. Per [templating.md §10] it is a
// step runtime-error.
type ErrExecute struct{ Err error }

func (e *ErrExecute) Error() string { return fmt.Sprintf("tmpl execute: %v", e.Err) }
func (e *ErrExecute) Unwrap() error { return e.Err }
