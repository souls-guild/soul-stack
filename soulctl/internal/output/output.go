// Package output renders soulctl's table and JSON output.
//
// Table style is kubectl-like: columns aligned via text/tabwriter (at least
// 3 spaces between columns). JSON output is pretty-printed
// (json.MarshalIndent with 2-space indent) to be friendly to both `jq` and
// the eye.
//
// Convention: empty table values print as `<none>` (kubectl style), so the
// operator immediately sees missing data instead of a blank cell.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// Format is the output mode. The global --output flag on soulctl's root command.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml" // reserved: currently identical to JSON
)

// ParseFormat normalizes the flag value. Empty string means FormatTable.
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

// JSON prints v as pretty JSON to w. nil values serialize as `null`.
func JSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Table prints rows under the header via tabwriter. An empty cell is
// replaced with `<none>`.
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

// JoinList is a compact join for list columns. An empty list becomes "".
func JoinList(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return strings.Join(items, ",")
}
