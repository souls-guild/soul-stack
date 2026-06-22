package cel

import (
	"regexp"
	"strings"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// stringType — целевой тип для канонической стрингификации CEL-значений
// при склейке в интерполяции ([templating.md §5]).
var stringType ref.Type = types.StringType

// Базовые CEL-функции — из стандартной библиотеки google/cel-go, подключаемой
// через cel.StdLib() в [New]. Из неё реально используются выражениями Soul Stack
// ([templating.md §2.3]):
//
//   - size(x)            — размер строки/списка/map.
//   - contains(s, sub)   — подстрока/членство (метод receiver-формы).
//   - timestamp/duration — арифметика времени.
//
// Все они pure: без I/O, сети, sleep.
//
// Кастомные функции Soul Stack живут каждая в своём файле и регистрируются
// дополнительными EnvOption-ами:
//
//   - glob(pattern)      — [glob.go], сопоставление по шаблону.
//   - vault(path)        — [vault.go], keeper-side чтение Vault KV (macro,
//                          регистрируется только при Engine с KVReader).
//   - merge(m, m...)     — [merge.go], SHALLOW last-wins слияние map-ов
//                          ([ADR-010 Amendment 2026-06-22]).
//
// Расширение списка кастомных функций — через ADR, не молча ([templating.md §2.3]).
//
// now() из стартового минимума [templating.md §2.3] cel-go как глобальной
// функции не предоставляет (eval-time время дают через timestamp-литералы
// и переменные контекста). Введение now() как кастомной функции — за
// рамками pilot и требует решения о eval-time семантике; до тех пор
// обращение к now(...) отсекается guard-ом как unsupported.

// unsupportedPatterns — конструкции, заявленные спекой, но НЕ входящие в
// pilot-scope. Отсекаются до compile, чтобы вернуть осмысленный
// [ErrUnsupported] вместо невнятного CEL-«no such field/overload».
//
// soulprint.hosts / .where(...) больше НЕ здесь: они реализованы compile-time
// AST-rewrite-ом статического литерал-предиката в нативный filter-comprehension
// (см. hosts.go); изоляция destiny-прохода и валидация receiver/литерала
// делаются там же, не текстовым guard-ом.
//
// vault( больше НЕ здесь безусловно: при Engine с KVReader ([WithVault]) функция
// vault() зарегистрирована и работает ([vault.go]); guard на неё остаётся только
// когда KVReader не задан (см. vaultGuard в [guardUnsupported]) — чтобы дать
// осмысленный ErrUnsupported вместо «no such function» в контекстах без Vault.
//
// Каждый паттерн ловит характерный токен конструкции:
//   - now(                   — eval-time время (см. выше).
var unsupportedPatterns = []struct {
	feature string
	re      *regexp.Regexp
}{
	{"now()", regexp.MustCompile(`\bnow\s*\(`)},
}

// vaultGuard ловит обращение к vault() — отсекается только когда Engine собран
// без KVReader (vault() не зарегистрирован, см. [guardUnsupported]).
var vaultGuard = regexp.MustCompile(`\bvault\s*\(`)

// internalIdentGuard ловит идентификаторы с префиксом `__` в АВТОРСКОМ
// выражении. Префикс `__` зарезервирован за internal-механизмами CEL-слоя:
// macro vault() разворачивается в `__vault_read(path, __vault_resolver)`, где
// `__vault_read`/`__vault_resolver` — hidden-аргументы, недоступные автору.
//
// БЕЗ этого guard-а автор пишет напрямую `${ __vault_read('secret/любой',
// __vault_resolver).password }` и читает ЛЮБОЙ путь, минуя macro vault() и
// завязанные на токен `vault(` guard/lint/mask (security-blocker). Guard
// действует ВСЕГДА (и с KVReader, и без): `__`-идентификатор в авторском
// тексте — всегда ошибка, не зависящая от наличия vault-клиента.
//
// guardUnsupported вызывается на АВТОРСКОМ тексте ДО macro-expansion
// (`__vault_read`/`__vault_resolver` появляются только внутри env.Compile),
// поэтому легальный `vault('secret/x')` под guard не попадает. `\w` слева НЕ
// допускается (иначе `a__b` ложно-срабатывал бы), `.`/начало токена допустимы:
// в словаре Soul Stack нет легальных голых идентификаторов с `__`.
//
// Матч ведётся по тексту С ВЫРЕЗАННЫМИ строковыми литералами (см.
// stripStringLiterals): `__`-последовательность ВНУТРИ литерала — это данные,
// а не CEL-идентификатор (например, поле `__host` в предикате-строке
// soulprint.hosts.where("__host == 'x'"), которое cel-вызвать ничего не может),
// и под запрет не попадает.
var internalIdentGuard = regexp.MustCompile(`(^|\W)__\w`)

// guardUnsupported возвращает [ErrUnsupported], если выражение содержит
// конструкцию вне pilot-scope. vaultEnabled=true (Engine с KVReader) снимает
// guard на vault() — функция зарегистрирована и работает. essence guard-ом НЕ
// отсекается: она объявлена переменной и резолвится из Vars.Essence
// (effective-слой); пустой Essence даёт штатный no-such-key, а не панику.
func guardUnsupported(expr string, vaultEnabled bool) error {
	for _, p := range unsupportedPatterns {
		if p.re.MatchString(expr) {
			return &ErrUnsupported{Expr: expr, Feature: p.feature}
		}
	}
	if internalIdentGuard.MatchString(stripStringLiterals(expr)) {
		return &ErrUnsupported{Expr: expr, Feature: "идентификатор с префиксом '__' (зарезервирован за internal-механизмами CEL)"}
	}
	if !vaultEnabled && vaultGuard.MatchString(expr) {
		return &ErrUnsupported{Expr: expr, Feature: "vault(...)"}
	}
	return nil
}

// normalize приводит выражение к каноничной форме для ключа compile-cache:
// схлопывает внутренние пробельные последовательности и обрезает края.
// Семантику CEL не меняет (пробелы вне строковых литералов незначимы);
// строковые литералы в выражениях Soul Stack используют одинарные кавычки
// ([templating.md §2.2]), пробелы внутри них сохраняются как есть — поэтому
// нормализация затрагивает только пробелы, не трогая содержимое литералов.
func normalize(expr string) string {
	return spaceRun.ReplaceAllStringFunc(strings.TrimSpace(expr), normalizeWhitespace)
}

var spaceRun = regexp.MustCompile(`'[^']*'|"[^"]*"|\s+`)

// stringLiteralRe матчит строковый литерал CEL (одинарные/двойные кавычки).
// Используется stripStringLiterals для вырезания содержимого литералов перед
// текстовым guard-ом по идентификаторам — содержимое литерала это данные, не
// CEL-токены.
var stringLiteralRe = regexp.MustCompile(`'[^']*'|"[^"]*"`)

// stripStringLiterals заменяет содержимое строковых литералов на пустые кавычки,
// сохраняя структуру выражения вне литералов. Нужно guard-ам, которые ищут
// CEL-идентификаторы/вызовы по тексту: токен внутри литерала (`"__host"`) — не
// идентификатор, а данные. Не для семантики CEL — только для текстового анализа.
func stripStringLiterals(expr string) string {
	return stringLiteralRe.ReplaceAllStringFunc(expr, func(lit string) string {
		return lit[:1] + lit[len(lit)-1:]
	})
}

func normalizeWhitespace(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '\'', '"':
		return s // строковый литерал — не трогаем
	default:
		return " "
	}
}
