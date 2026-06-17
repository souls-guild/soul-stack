package tmpl

import (
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// allowedSprig — закрытый allowlist sprig-функций по [templating.md §3.3].
// Подключение через whitelist, не denylist: всё, чего нет в этом наборе,
// шаблону недоступно. При upgrade sprig allowlist пересматривается явно —
// новые функции по умолчанию запрещены.
//
// Сознательно исключены (вне набора, для справки — denylist в
// [templating.md §3.3]):
//   - окружение/исполнение/сеть: env, expandenv, exec, getHostByName;
//   - криптогенерация: derivePassword, genCA, genPrivateKey,
//     genSelfSignedCert, genSignedCert, buildCustomCert;
//   - случайность (недетерминизм рендера): randAlphaNum, randAlpha,
//     randAscii, randNumeric, randBytes;
//   - метапрограммирование (SSTI): tpl, include.
//
// [templating.md §3.3]: docs/templating.md
var allowedSprig = []string{
	// Nil-handling.
	"default", "coalesce", "empty",

	// Строки.
	"upper", "lower", "trim", "trimAll", "trimPrefix", "trimSuffix",
	"quote", "squote", "replace", "repeat", "split", "splitList", "join",

	// Конверсия.
	// toYaml/fromYaml в этот список НЕ входят: их нет в upstream sprig
	// (Helm-only). Реализованы как собственные функции Soul Stack в
	// yaml_funcs.go ([customFuncs]) и подмешиваются в FuncMap отдельно
	// ([templating.md §3.3]).
	"toString", "int", "int64", "float64",
	"toJson", "fromJson",

	// Арифметика.
	"add", "sub", "mul", "div", "mod",

	// Base64 / хэш (без секретогенерации).
	"b64enc", "b64dec", "sha256sum",
}

// buildFuncMap собирает FuncMap для Engine: функции из sprig-allowlist-а
// (взятые из sprig.TxtFuncMap()) плюс собственные функции Soul Stack
// ([customFuncs] — toYaml/fromYaml, которых в sprig нет). Встроенные
// функции Go text/template (eq, index, len, printf, …) добавляются
// движком автоматически и в этот набор не входят ([templating.md §3.3]).
//
// Если имя из allowlist-а отсутствует в текущей версии sprig — это
// расхождение allowlist-а со сборкой, возвращается ошибка (баг, не
// пользовательский ввод).
func buildFuncMap() (template.FuncMap, error) {
	src := sprig.TxtFuncMap()
	custom := customFuncs()
	funcs := make(template.FuncMap, len(allowedSprig)+len(custom))
	for _, name := range allowedSprig {
		fn, ok := src[name]
		if !ok {
			return nil, fmt.Errorf("tmpl: функция %q из allowlist отсутствует в sprig", name)
		}
		funcs[name] = fn
	}
	for name, fn := range custom {
		funcs[name] = fn
	}
	return funcs, nil
}
