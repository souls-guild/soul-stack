// Package output — рендереры табличного и JSON-вывода soulctl.
//
// Стиль таблицы — kubectl-like: колонки, выровненные через text/tabwriter
// (минимум 3 пробела между колонками). JSON-вывод — pretty-printed
// (json.MarshalIndent с двухпробельным отступом), чтобы быть удобным для
// `jq` и для глаз.
//
// Соглашение: пустые значения в таблице печатаются как `<none>` (kubectl-стиль),
// чтобы оператор сразу видел отсутствие данных, а не пустую ячейку.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Format — режим вывода. Глобальный флаг --output на root-команде soulctl.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml" // зарезервировано: пока совпадает с JSON
)

// ParseFormat — нормализация значения флага. Пустая строка = FormatTable.
func ParseFormat(v string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "table":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("неизвестный формат %q: ожидается table|json|yaml", v)
	}
}

// JSON печатает v как pretty-JSON в w. nil-значения сериализуются как `null`.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Table печатает строки rows под заголовком header через tabwriter.
// Пустая ячейка заменяется на `<none>`.
func Table(w io.Writer, header []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, c := range row {
			if c == "" {
				cells[i] = "<none>"
			} else {
				cells[i] = c
			}
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// JoinList — компактный join для колонок-списков. Пустой список → "".
func JoinList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.Join(items, ",")
}
