package cel

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/cel-go/common/types/ref"
)

// ErrPredicateNotBool — типизированный признак «top-level предикат вычислился,
// но вернул не-bool». Оборачивается внутрь [ErrEval] (см. [Engine.EvalPredicate]),
// чтобы caller отличал эту ситуацию от прочих runtime-ошибок eval (no-such-key,
// div-by-zero) через errors.Is — без хрупкого матчинга текста сообщения.
// Sentinel additive: текст ErrEval сохранён, потребители на старом тексте не
// ломаются.
var ErrPredicateNotBool = errors.New("предикат вернул не bool")

// ErrCompile — ошибка компиляции CEL-выражения (синтаксис, неизвестный
// идентификатор, несовместимые типы). По [templating.md §10] это ошибка
// фазы валидации до старта прогона.
type ErrCompile struct {
	Expr string
	Err  error
}

func (e *ErrCompile) Error() string {
	return fmt.Sprintf("CEL compile %q: %v", e.Expr, e.Err)
}

func (e *ErrCompile) Unwrap() error { return e.Err }

// ErrEval — ошибка вычисления CEL-выражения (div-by-zero, обращение к
// null-полю и т.п.). По [templating.md §10] — runtime-error шага.
type ErrEval struct {
	Expr string
	Err  error
}

func (e *ErrEval) Error() string {
	return fmt.Sprintf("CEL eval %q: %v", e.Expr, e.Err)
}

func (e *ErrEval) Unwrap() error { return e.Err }

// ErrUnsupported — выражение использует конструкцию вне pilot-scope (now(); либо
// vault() при Engine без KVReader — см. functions.go). Не паника, не CEL-ошибка:
// отдельный класс, чтобы caller отличал «ещё не реализовано / недоступно здесь»
// от «ошибка автора Destiny».
type ErrUnsupported struct {
	Expr    string
	Feature string
}

func (e *ErrUnsupported) Error() string {
	return fmt.Sprintf("CEL unsupported %q: конструкция %s ещё не реализована (pilot)", e.Expr, e.Feature)
}

// EvalExpression вычисляет выражение, в котором вся строка трактуется как
// CEL — форма top-level expression-ключей (where:/when:/changed_when:/
// failed_when:/until:, [templating.md §2.1]). Возвращает ref.Val «как
// есть»; интерпретацию типа (bool для where:/when:, и т.п.) делает caller.
//
// Если vars.Loop непуст (`loop:`-итерация, destiny/tasks.md §7), выражение
// компилируется против дочернего env с объявленными loop-именами — голая
// форма `<as>.*` резолвится наравне с базовым контекстом.
//
// Ошибки: [ErrCompile] / [ErrUnsupported] (до eval), [ErrEval] (на eval).
func (e *Engine) EvalExpression(expr string, vars Vars) (ref.Val, error) {
	norm := normalize(expr)

	loopNames := vars.loopNames()
	env, err := e.loopEnv(loopNames)
	if err != nil {
		return nil, err
	}
	prg, err := e.compile(env, strings.Join(loopNames, "\x00"), norm, vars.AllowHosts)
	if err != nil {
		return nil, err
	}

	act := vars.activation(e.migration)
	if e.kv != nil {
		// Per-eval resolver для vault(): immutable Engine.kv + request-scoped ctx.
		// Кладётся в активацию (а не на Engine) → vault() concurrency-safe при
		// общем Engine. ctx по умолчанию Background (офлайн soul-lint/Trial).
		ctx := vars.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		act[vaultResolverVar] = &vaultResolver{ctx: ctx, kv: e.kv}
	}

	out, _, err := prg.Eval(act)
	if err != nil {
		return nil, &ErrEval{Expr: expr, Err: err}
	}

	// DSL-coverage hook ([ADR-023]): после успешного eval, не на compile
	// (Program кешируется — хук на compile занизил бы метрику при повторных
	// eval). EvalInterpolation вызывает EvalExpression для каждого блока,
	// поэтому здесь же ловятся и интерполяционные `${ … }`. nil-sink →
	// no-op. expr передаём нормализованным — ключ кеша/coverage один.
	if e.sink != nil {
		e.sink.Record(norm, out)
	}
	return out, nil
}

// EvalPredicate вычисляет top-level bool-предикат (вся строка = CEL,
// [templating.md §2.1]) и приводит результат к bool. Удобная обёртка над
// [Engine.EvalExpression] для flow-control-ключей (when:/changed_when:/
// failed_when:/where:): пустой expr → (true, nil) (нет предиката = безусловно);
// не-bool результат → [ErrEval] (предикат обязан возвращать булево).
//
// Ошибки: [ErrCompile]/[ErrUnsupported] (до eval) и [ErrEval] (на eval либо при
// не-bool результате) — caller отличает их через errors.As для трактовки по
// таблице ошибок ([templating.md §10]).
func (e *Engine) EvalPredicate(expr string, vars Vars) (bool, error) {
	if expr == "" {
		return true, nil
	}
	out, err := e.EvalExpression(expr, vars)
	if err != nil {
		return false, err
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, &ErrEval{
			Expr: expr,
			Err:  fmt.Errorf("предикат вернул %s, ожидался bool: %w", out.Type().TypeName(), ErrPredicateNotBool),
		}
	}
	return b, nil
}

// EvalInterpolation вычисляет строку со встроенными `${ … }`-выражениями
// ([templating.md §2.2]). Правила:
//
//   - Ровно один блок `${expr}` без сопровождающего текста — возвращается
//     нативное Go-значение результата CEL ([templating.md §5(а)]).
//   - Иначе (несколько блоков и/или окружающий текст) — каждый блок
//     вычисляется и стрингифицируется, результат склеивается со строкой
//     ([templating.md §5(б)]). Стрингификация list/map при склейке —
//     ошибка ([templating.md §5]).
//   - Литерал `\${` экранирует маркер: в выводе остаётся `${`
//     ([templating.md §9.1]).
//
// Балансировка скобок внутри `${ … }` — через CEL-парсер (см.
// scanInterpolation), не текстовым подсчётом.
func (e *Engine) EvalInterpolation(raw string, vars Vars) (any, error) {
	segs, err := e.scanInterpolation(raw)
	if err != nil {
		return nil, err
	}

	// Ячейка — ровно один CEL-блок без окружающего текста: нативный тип.
	if len(segs) == 1 && segs[0].expr {
		val, err := e.EvalExpression(segs[0].text, vars)
		if err != nil {
			return nil, err
		}
		return toNative(val.Value()), nil
	}

	var b strings.Builder
	for _, s := range segs {
		if !s.expr {
			b.WriteString(s.text)
			continue
		}
		val, err := e.EvalExpression(s.text, vars)
		if err != nil {
			return nil, err
		}
		str, err := stringify(s.text, val)
		if err != nil {
			return nil, err
		}
		b.WriteString(str)
	}
	return b.String(), nil
}

// toNative нормализует нативное значение CEL в чистые Go-данные, пригодные для
// дальнейшего рендера/loop-раскрытия (map[string]any / []any / скаляры).
// Контейнеры CEL над dyn-слоем (filter-comprehension над soulprint.hosts)
// возвращают .Value() как []ref.Val и map с ref.Val/interface-ключами — их
// нужно развернуть рекурсивно, иначе renderValue/resolveLoopItems не узнают
// типы. Скаляры (string/int/bool/…) проходят насквозь.
func toNative(v any) any {
	switch t := v.(type) {
	case ref.Val:
		return toNative(t.Value())
	case []ref.Val:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = toNative(el)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = toNative(el)
		}
		return out
	case map[ref.Val]ref.Val:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[toNativeKey(k.Value())] = toNative(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = toNative(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[toNativeKey(k)] = toNative(val)
		}
		return out
	default:
		return v
	}
}

// toNativeKey приводит ключ map к строке (ключи soulprint.hosts-элементов —
// строки; cel мог обернуть их в ref.Val/interface).
func toNativeKey(k any) string {
	if rv, ok := k.(ref.Val); ok {
		k = rv.Value()
	}
	if s, ok := k.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", k)
}

// segment — кусок интерполируемой строки: либо литеральный текст
// (expr=false), либо CEL-выражение без обёртки `${ }` (expr=true).
type segment struct {
	text string
	expr bool
}

// scanInterpolation разбивает raw на сегменты литералов и `${ … }`-блоков.
// Закрывающая `}` блока определяется CEL-парсером: подстрока после `${`
// расширяется до первой `}`, при которой содержимое парсится как валидный
// CEL — это и есть `}` на верхнем уровне вложенности скобок выражения
// ([templating.md §2.2]). Если ни одна `}` не даёт валидного парса до
// конца строки — [ErrCompile] («${ без закрывающей }», [templating.md §10]).
//
// Экранирование `\${` ([templating.md §9.1]): последовательность не
// открывает блок, в литерал попадает `${`.
func (e *Engine) scanInterpolation(raw string) ([]segment, error) {
	var segs []segment
	var lit strings.Builder
	i := 0
	for i < len(raw) {
		// Экранированный маркер: \${  →  литеральный ${.
		if raw[i] == '\\' && i+2 < len(raw) && raw[i+1] == '$' && raw[i+2] == '{' {
			lit.WriteString("${")
			i += 3
			continue
		}
		if raw[i] == '$' && i+1 < len(raw) && raw[i+1] == '{' {
			if lit.Len() > 0 {
				segs = append(segs, segment{text: lit.String()})
				lit.Reset()
			}
			inner, next, err := e.parseBlock(raw, i+2)
			if err != nil {
				return nil, err
			}
			segs = append(segs, segment{text: inner, expr: true})
			i = next
			continue
		}
		lit.WriteByte(raw[i])
		i++
	}
	if lit.Len() > 0 {
		segs = append(segs, segment{text: lit.String()})
	}
	return segs, nil
}

// parseBlock находит конец `${ … }`-блока, открытого на позиции start
// (первый байт после `${`). Возвращает текст выражения (trimmed) и индекс
// сразу за закрывающей `}`.
func (e *Engine) parseBlock(raw string, start int) (string, int, error) {
	for end := start; end < len(raw); end++ {
		if raw[end] != '}' {
			continue
		}
		inner := raw[start:end]
		if _, issues := e.env.Parse(strings.TrimSpace(inner)); issues == nil || issues.Err() == nil {
			return strings.TrimSpace(inner), end + 1, nil
		}
	}
	return "", 0, &ErrCompile{
		Expr: raw[start:],
		Err:  fmt.Errorf("${ без закрывающей } или невалидное выражение"),
	}
}

// stringify приводит CEL-значение к строке для склейки ([templating.md §5]).
// Скаляры (int/float/bool/string) и timestamp — каноничная стрингификация
// через cel-go ConvertToType(StringType). list/map склеить со строкой
// нельзя — [ErrEval] ([templating.md §5]).
func stringify(expr string, val ref.Val) (string, error) {
	switch v := val.Value().(type) {
	case string:
		return v, nil
	}

	str := val.ConvertToType(stringType)
	if s, ok := str.Value().(string); ok {
		return s, nil
	}

	return "", &ErrEval{
		Expr: expr,
		Err:  fmt.Errorf("результат типа %s нельзя склеить со строкой (вынеси в отдельную ячейку)", val.Type().TypeName()),
	}
}
