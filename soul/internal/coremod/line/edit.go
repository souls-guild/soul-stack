package line

import "fmt"

// presentResult — the outcome of editing for state present.
type presentResult struct {
	lines    []string
	changed  bool
	matched  int    // how many lines matched the regexp (for warning/diagnostics)
	replaced int    // how many were actually replaced/added (0 or 1)
	warning  string // non-empty only on multiple regexp matches
}

// presentEdit implements present semantics over already-split lines.
// Pure function: input is the current lines + params, output is new lines.
//
// With regexp: the first matching line is replaced with line. If it already
// equals line — no-op. On >1 match, only the first is changed, the rest are
// left alone; warning reports how many more matched.
//
// Without regexp: if line is already present verbatim — no-op; otherwise the
// line is added per insertafter/insertbefore (literal/EOF/BOF), default EOF.
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
		// No matching line. In the pared-down MVP, regexp describes the line
		// we're replacing; no target to replace → add line instead (per the
		// insertion rules). This is predictable: "present" guarantees line exists.
		return appendByPosition(lines, p)
	}
	if res.matched > 1 {
		res.warning = fmt.Sprintf("regexp matched %d lines, replaced only the first (others left untouched)", res.matched)
	}
	if lines[firstIdx] == p.line {
		// The first match already equals the target line — nothing to replace.
		// But we keep the multiple-match warning as an informational signal.
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
			// Exact line is already present — no-op.
			res.changed = false
			res.matched = 1
			return res
		}
	}
	return appendByPosition(lines, p)
}

// appendByPosition inserts p.line per insertafter/insertbefore.
// Valid values: insertafter ∈ {"", "EOF", <literal>}, insertbefore ∈
// {"", "BOF", <literal>}. A literal is matched as an exact line: inserts
// after/before the FIRST such anchor. Anchor not found → EOF (predictable
// fallback, same as ansible's insertafter with a nonexistent anchor). Default
// (both empty) → EOF.
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
	default: // insertafter is a literal
		if idx := indexOf(lines, p.insertAfter); idx >= 0 {
			return inserted(insertAt(lines, idx+1, p.line))
		}
		return inserted(append(cloneLines(lines), p.line))
	}
}

func inserted(lines []string) presentResult {
	return presentResult{lines: lines, changed: true, replaced: 1}
}

// absentResult — the outcome of editing for state absent.
type absentResult struct {
	lines   []string
	changed bool
	removed int
}

// absentEdit removes lines. With regexp — all matches; without regexp — all
// exact matches of p.line. Pure function.
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
