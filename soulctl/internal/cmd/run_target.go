package cmd

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

// targetFlags — общий набор `--target-*` флагов всех `soulctl run <sub>`
// подкоманд (C1). Все три sub-команды собирают тот же selector → разные
// backend-эндпоинты переводят его в свою форму body (см. build).
//
// Семантика:
//   - sids/coven — exact-match списки (CSV в флаге → []string).
//   - glob → CEL-выражение `sid.glob("X")` (shared/cel.glob, member-overload).
//   - regex → CEL-выражение `sid.matches("X")` (stdlib).
//   - where → raw CEL (оператор-asserted).
//
// AND-merge: glob/regex/where склеиваются `&&` в один итоговый `where`. sids и
// coven остаются отдельными полями — backend сам делает AND-пересечение
// (ADR-040/ADR-041 security invariant: invocation сужает scope, не расширяет).
type targetFlags struct {
	SIDs  string
	Coven string
	Glob  string
	Regex string
	Where string
}

// bind навешивает `--target-*` флаги на cobra-команду. Имена совпадают у всех
// `run` sub-команд (отсюда общий helper, без дубликата).
func (t *targetFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&t.SIDs, "target-sids", "",
		"CSV exact-match SID-ов (`host1,host2`)")
	c.Flags().StringVar(&t.Coven, "target-coven", "",
		"CSV Coven-меток (AND-семантика по `souls.coven`)")
	c.Flags().StringVar(&t.Glob, "target-glob", "",
		"shell-glob по SID; превращается в CEL `sid.glob(\"X\")`")
	c.Flags().StringVar(&t.Regex, "target-regex", "",
		"regex по SID; превращается в CEL `sid.matches(\"X\")`")
	c.Flags().StringVar(&t.Where, "target-where", "",
		"raw CEL-предикат (`soulprint.self.os.family == \"debian\"`)")
}

// resolvedTarget — итог парсинга/склейки. Любой пустой компонент остаётся
// пустым; вызывающая sub-команда сама решает, какие поля укладывать в body.
type resolvedTarget struct {
	SIDs  []string
	Coven []string
	Where string
}

// hasAny — хоть один компонент target-а задан.
func (r resolvedTarget) hasAny() bool {
	return len(r.SIDs) > 0 || len(r.Coven) > 0 || r.Where != ""
}

// resolve парсит CSV-поля, склеивает glob/regex/where в итоговый CEL. Пустые
// токены CSV отбрасываются (`a,,b` → `[a, b]`); валидация формы pattern-а —
// серверная (soul-lint валидирует CEL, синтаксис filepath.Match — runtime).
func (t targetFlags) resolve() (resolvedTarget, error) {
	out := resolvedTarget{
		SIDs:  splitCSV(t.SIDs),
		Coven: splitCSV(t.Coven),
	}
	parts := make([]string, 0, 3)
	if t.Glob != "" {
		parts = append(parts, "sid.glob("+quoteCEL(t.Glob)+")")
	}
	if t.Regex != "" {
		parts = append(parts, "sid.matches("+quoteCEL(t.Regex)+")")
	}
	if t.Where != "" {
		// Заворачиваем raw CEL в скобки — оператор мог написать `a || b`,
		// без скобок AND-склейка изменила бы приоритет.
		parts = append(parts, "("+t.Where+")")
	}
	out.Where = strings.Join(parts, " && ")
	return out, nil
}

// require — проверка «target обязан быть задан». Используется sub-командами,
// где scope без target бессмыслен (cmd: без хостов нечего запускать).
func (r resolvedTarget) require() error {
	if !r.hasAny() {
		return errors.New("требуется хотя бы один `--target-*` флаг (sids/coven/glob/regex/where)")
	}
	return nil
}

// splitCSV режет строку по запятой, тримит пробелы, отбрасывает пустые токены.
// Пустой вход → nil (а не []string{}), чтобы json-omitempty корректно работало.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// quoteCEL экранирует строковый литерал для CEL. CEL принимает double-quoted
// строки с escape-семантикой Go (cel-spec § Lexical analysis → string), поэтому
// достаточно экранировать `\` и `"`.
func quoteCEL(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
