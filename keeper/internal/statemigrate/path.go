package statemigrate

import (
	"fmt"
	"strings"
)

// pathSegment — один сегмент адреса state-пути: либо литерал (буквы/цифры/
// `_`/`-`), либо `${ <CEL> }`-интерполяция, резолвимая в строку до навигации.
type pathSegment struct {
	literal string // непустой, если сегмент литеральный
	expr    string // непустой, если сегмент = ${ <expr> }
}

// parsePath разбивает адрес вида `state.foo.bar.${ name }` на сегменты ПОСЛЕ
// корневого `state.` ([docs/migrations.md §«Адресация — path:»]). Префикс
// `state.` обязателен. Сегментация по `.` на верхнем уровне; точки внутри
// `${ … }` принадлежат выражению, а не разделяют сегменты.
//
// Возвращает ParseError (некорректная форма пути — ошибка автора миграции,
// ловится на разборе/применении, не на каждом state-объекте).
func parsePath(raw string) ([]pathSegment, error) {
	const prefix = "state"
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == prefix:
		// Голый `state` — корень целиком; сегментов нет. set/rename/delete
		// на корень не поддерживаются (нет смысла) — пустой путь отсекается
		// вызывающим (apply.go).
		return nil, nil
	case strings.HasPrefix(trimmed, prefix+"."):
		// ок, режем тело после `state.`
	default:
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q должен начинаться с 'state.'", raw)}
	}

	body := trimmed[len(prefix)+1:]
	if body == "" {
		return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: пустой адрес после 'state.'", raw)}
	}

	var segs []pathSegment
	i := 0
	var cur strings.Builder
	flushLiteral := func() error {
		s := cur.String()
		cur.Reset()
		if s == "" {
			return &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: пустой сегмент", raw)}
		}
		segs = append(segs, pathSegment{literal: s})
		return nil
	}

	for i < len(body) {
		// Начало `${ … }`-блока: целый сегмент-интерполяция.
		if body[i] == '$' && i+1 < len(body) && body[i+1] == '{' {
			if cur.Len() != 0 {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: ${…} должен быть отдельным сегментом (между точками)", raw)}
			}
			end := strings.IndexByte(body[i:], '}')
			if end < 0 {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: ${ без закрывающей }", raw)}
			}
			end += i
			expr := strings.TrimSpace(body[i+2 : end])
			if expr == "" {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: пустое ${ }", raw)}
			}
			segs = append(segs, pathSegment{expr: expr})
			i = end + 1
			// После блока ждём либо конец, либо разделитель `.`.
			if i < len(body) {
				if body[i] != '.' {
					return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: после ${…} ожидается '.' или конец", raw)}
				}
				i++ // съели разделитель; следующий сегмент обязателен
				if i >= len(body) {
					return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: путь оканчивается на '.'", raw)}
				}
			}
			continue
		}
		if body[i] == '.' {
			if err := flushLiteral(); err != nil {
				return nil, err
			}
			i++
			if i >= len(body) {
				return nil, &ParseError{Code: CodeOpFieldMissing, Msg: fmt.Sprintf("path %q: путь оканчивается на '.'", raw)}
			}
			continue
		}
		cur.WriteByte(body[i])
		i++
	}
	if cur.Len() > 0 {
		if err := flushLiteral(); err != nil {
			return nil, err
		}
	}
	return segs, nil
}

// resolveSegments резолвит ${ … }-сегменты в строковые ключи через Evaluator,
// возвращая плоский список итоговых ключей навигации. Литералы проходят как
// есть; expr-сегменты вычисляются и стрингифицируются. scope несёт state +
// активные foreach-переменные.
func resolveSegments(segs []pathSegment, ev Evaluator, scope Scope) ([]string, error) {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s.expr == "" {
			out = append(out, s.literal)
			continue
		}
		val, err := ev.Eval(s.expr, scope)
		if err != nil {
			return nil, &EvalError{Class: ClassCELInterp, Msg: fmt.Sprintf("сегмент пути ${ %s }", s.expr), Err: err}
		}
		key, ok := stringKey(val)
		if !ok {
			return nil, &EvalError{Class: ClassPathSegment, Msg: fmt.Sprintf("сегмент пути ${ %s } дал %T, ожидалась строка/число-ключ", s.expr, val)}
		}
		out = append(out, key)
	}
	return out, nil
}

// stringKey приводит результат CEL-сегмента к строковому ключу map. Строки —
// как есть; целые/uint — десятичная форма (ключи map в state JSON-safe строковые).
func stringKey(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case uint64:
		return fmt.Sprintf("%d", t), true
	default:
		return "", false
	}
}
