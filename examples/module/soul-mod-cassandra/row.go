// Readers for a driver row. The driver hands back `map[string]any` carrying the CQL
// types it decoded — a `uuid` is not a string, a `map<text,text>` is a real Go map —
// so every read of a system table goes through one of these rather than a type
// assertion at the call site.
package main

import (
	"fmt"
	"strconv"
)

// rowString renders a column as text. A `uuid` or an `inet` arrives as its own type,
// and an operator reading `Output.host_id` wants the value, not the Go type — so
// anything that is not already a string is formatted. A missing or null column is
// the empty string, which is what an absent optional column means.
func rowString(row map[string]any, col string) string {
	v, ok := row[col]
	if !ok || v == nil {
		return ""
	}
	if s, isString := v.(string); isString {
		return s
	}
	return fmt.Sprint(v)
}

// rowBool reads a `boolean` column. A missing or null column is false.
func rowBool(row map[string]any, col string) bool {
	b, _ := row[col].(bool)
	return b
}

// rowTextMap reads a `map<text, text>` column — `system_schema.keyspaces.replication`
// is one. A missing or null column is an empty map, not an error: the caller
// distinguishes "no such keyspace" by the row count, not by this.
func rowTextMap(row map[string]any, col string) map[string]string {
	m, _ := row[col].(map[string]string)
	return m
}

// rowInt reads an `int` column. Cassandra's `int` is 32-bit and its `bigint` is
// 64-bit, and the driver decodes them to the matching Go width, so both are admitted
// — `system_schema.columns.position` is an int, and widening it later must not turn
// into a silent zero.
func rowInt(row map[string]any, col string) int {
	switch n := row[col].(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	default:
		return 0
	}
}

// atoiStrict parses a replication factor out of the text Cassandra stores it as.
// `system_schema.keyspaces.replication` is a `map<text, text>`, so a factor of 3
// comes back as "3"; a value that is not a number there means the strategy carries
// an option this artifact does not manage, and guessing at it would be worse than
// saying so.
func atoiStrict(where, s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer replication factor", where, s)
	}
	return n, nil
}
