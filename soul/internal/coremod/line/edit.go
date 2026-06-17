package line

import "fmt"

// presentResult — итог редактирования для state present.
type presentResult struct {
	lines    []string
	changed  bool
	matched  int    // сколько строк совпало с regexp (для warning/диагностики)
	replaced int    // сколько фактически заменено/добавлено (0 или 1)
	warning  string // непусто только при множественном regexp-совпадении
}

// presentEdit реализует семантику present над уже разбитыми строками.
// Чистая функция: вход — текущие строки + параметры, выход — новые строки.
//
// С regexp: первая матчащая строка заменяется на line. Если она уже равна line
// — no-op. При >1 совпадении меняется только первая, остальные не трогаются,
// в warning сообщается, сколько ещё совпало.
//
// Без regexp: если line уже присутствует точно — no-op; иначе строка
// добавляется по insertafter/insertbefore (литерал/EOF/BOF), default — EOF.
func presentEdit(lines []string, p lineParams) presentResult {
	if p.regexp != nil {
		return presentRegexp(lines, p)
	}
	return presentLiteral(lines, p)
}

func presentRegexp(lines []string, p lineParams) presentResult {
	res := presentResult{lines: lines}

	firstIdx := -1
	for i, l := range lines {
		if p.regexp.MatchString(l) {
			res.matched++
			if firstIdx == -1 {
				firstIdx = i
			}
		}
	}
	if firstIdx == -1 {
		// Ни одной матчащей строки. В урезанном MVP regexp описывает строку,
		// которую заменяем; нет цели для замены → добавляем line (по правилам
		// вставки). Это предсказуемо: «present» гарантирует наличие line.
		return appendByPosition(lines, p)
	}
	if res.matched > 1 {
		res.warning = fmt.Sprintf("regexp matched %d lines, replaced only the first (others left untouched)", res.matched)
	}
	if lines[firstIdx] == p.line {
		// Первое совпадение уже равно целевой строке — замены нет. Но warning
		// о множественном совпадении сохраняем как информативный сигнал.
		res.changed = false
		return res
	}
	updated := make([]string, len(lines))
	copy(updated, lines)
	updated[firstIdx] = p.line
	res.lines = updated
	res.changed = true
	res.replaced = 1
	return res
}

func presentLiteral(lines []string, p lineParams) presentResult {
	res := presentResult{lines: lines}
	for _, l := range lines {
		if l == p.line {
			// Точная строка уже присутствует — no-op.
			res.changed = false
			res.matched = 1
			return res
		}
	}
	return appendByPosition(lines, p)
}

// appendByPosition вставляет p.line согласно insertafter/insertbefore.
// Допустимые значения: insertafter ∈ {"", "EOF", <литерал>}, insertbefore ∈
// {"", "BOF", <литерал>}. Литерал ищется как точное совпадение строки: вставка
// после/перед ПЕРВЫМ таким якорем. Якорь не найден → EOF (предсказуемый
// fallback, как у ansible insertafter с несуществующим anchor). Default
// (оба пусты) → EOF.
func appendByPosition(lines []string, p lineParams) presentResult {
	switch {
	case p.insertBefore == "BOF":
		return inserted(prepend(lines, p.line))
	case p.insertBefore != "":
		if idx := indexOf(lines, p.insertBefore); idx >= 0 {
			return inserted(insertAt(lines, idx, p.line))
		}
		return inserted(append(cloneLines(lines), p.line))
	case p.insertAfter == "" || p.insertAfter == "EOF":
		return inserted(append(cloneLines(lines), p.line))
	default: // insertafter — литерал
		if idx := indexOf(lines, p.insertAfter); idx >= 0 {
			return inserted(insertAt(lines, idx+1, p.line))
		}
		return inserted(append(cloneLines(lines), p.line))
	}
}

func inserted(lines []string) presentResult {
	return presentResult{lines: lines, changed: true, replaced: 1}
}

// absentResult — итог редактирования для state absent.
type absentResult struct {
	lines   []string
	changed bool
	removed int
}

// absentEdit удаляет строки. С regexp — все матчащие; без regexp — все точные
// совпадения p.line. Чистая функция.
func absentEdit(lines []string, p lineParams) absentResult {
	kept := make([]string, 0, len(lines))
	removed := 0
	for _, l := range lines {
		drop := false
		if p.regexp != nil {
			drop = p.regexp.MatchString(l)
		} else {
			drop = l == p.line
		}
		if drop {
			removed++
			continue
		}
		kept = append(kept, l)
	}
	if removed == 0 {
		return absentResult{lines: lines, changed: false}
	}
	return absentResult{lines: kept, changed: true, removed: removed}
}

func indexOf(lines []string, target string) int {
	for i, l := range lines {
		if l == target {
			return i
		}
	}
	return -1
}

func cloneLines(lines []string) []string {
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

func prepend(lines []string, line string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, line)
	out = append(out, lines...)
	return out
}

func insertAt(lines []string, idx int, line string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	out = append(out, line)
	out = append(out, lines[idx:]...)
	return out
}
