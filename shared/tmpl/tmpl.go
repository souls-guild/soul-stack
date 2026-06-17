// Package tmpl — обёртка над Go text/template для рендера файлов `.tmpl`
// модулем `core.file.rendered` ([ADR-010] §3, [templating.md §3]).
//
// Движок изолирован: в шаблоне доступны только переменные из переданного
// `vars` плюс встроенные функции Go text/template и закрытый allowlist
// sprig (см. [allowedSprig] в sprig.go). Прямого доступа к
// essence/input/register/soulprint у шаблона нет — автор обязан явно
// поднять нужные значения в `vars` на CEL-фазе ([templating.md §6]).
//
// Sandbox строится тремя барьерами ([templating.md §7.2]):
//   - strict-mode `missingkey=error` — обращение к отсутствующему ключу
//     даёт ошибку рендера, а не тихую пустую строку;
//   - sprig через whitelist — функции вне allowlist недоступны (env,
//     exec, crypto-gen, random, метапрограммирование исключены);
//   - значения `vars` подставляются как литералы, рекурсивного
//     template-eval (`tpl`) нет.
//
// Проверка расширения `.tmpl` — обязанность caller-а
// (Soul-side `core.file.rendered`), не этого пакета ([templating.md §3.4]).
//
// [ADR-010]: docs/adr/0010-templating.md#adr-010-шаблонизатор-cel-для-yaml-выражений-go-texttemplate-для-файлов
// [templating.md]: docs/templating.md
package tmpl

import (
	"fmt"
	"strings"
	"text/template"
)

// Engine рендерит `.tmpl`-шаблоны с фиксированным набором функций.
// Потокобезопасен и stateless: FuncMap собирается один раз в [New],
// дальше только читается. Один Engine переиспользуется всеми прогонами.
type Engine struct {
	funcs template.FuncMap
}

// New создаёт Engine с встроенными функциями text/template и allowlist-ом
// sprig. Ошибка возможна только при программном расхождении allowlist-а с
// текущей версией sprig (имя из allowlist отсутствует в FuncMap) — это
// баг сборки, не пользовательский ввод.
func New() (*Engine, error) {
	funcs, err := buildFuncMap()
	if err != nil {
		return nil, err
	}
	return &Engine{funcs: funcs}, nil
}

// Render компилирует и выполняет templateContent с контекстом vars.
//
// vars подставляются как `.<key>` (например, `{{ .name }}`). Обращение к
// отсутствующему ключу — ошибка ([missingkey=error]). Вызов функции вне
// allowlist-а text/template+sprig — ошибка компиляции.
//
// При nil-vars шаблон без обращений к данным отрендерится; любое `.<key>`
// в нём упадёт по strict-mode, что и требуется.
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

// ErrParse — ошибка компиляции шаблона: синтаксис или вызов функции вне
// allowlist-а. По [templating.md §10] — ошибка фазы валидации/рендера,
// шаг падает штатно.
type ErrParse struct{ Err error }

func (e *ErrParse) Error() string { return fmt.Sprintf("tmpl parse: %v", e.Err) }
func (e *ErrParse) Unwrap() error { return e.Err }

// ErrExecute — ошибка выполнения шаблона: обращение к отсутствующему
// ключу (strict-mode) или runtime-ошибка функции. По [templating.md §10]
// — runtime-error шага.
type ErrExecute struct{ Err error }

func (e *ErrExecute) Error() string { return fmt.Sprintf("tmpl execute: %v", e.Err) }
func (e *ErrExecute) Unwrap() error { return e.Err }
