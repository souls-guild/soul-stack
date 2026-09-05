// The `command` object's implementation — one CQL statement, run as given.
//
// A DML statement is PREPARED, with `params.args` bound to its markers: `WHERE id =
// ?` with an arg is safe in a way `WHERE id = ${ ... }` never is.
//
// ★ ARGS ON A NON-DML STATEMENT ARE REFUSED, not passed through. The driver binds
// values only on select / insert / update / delete / batch and DISCARDS them
// silently on everything else (cql.go), so `ALTER ROLE x WITH PASSWORD = ?` with an
// arg would reach the server as a literal `?` with nothing bound — a syntax error
// whose cause is nowhere in the message. Refusing says which statement and why,
// before the run.
package main

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

// maxCommandRows bounds what a SELECT can put into one event. An unbounded read of a
// real table would exhaust the plugin's memory and flood the event stream, and this
// action is an escape hatch, not a data reader — `truncated` in the Output says when
// the bound was hit rather than quietly returning a prefix.
const maxCommandRows = 100

// applyCommand runs a raw CQL statement. changed comes from params (default false,
// probe semantics): whether the statement mutated anything is the author's to
// declare, because the driver cannot tell.
func (m *CassandraModule) applyCommand(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	if errs := checkCommand(f); len(errs) > 0 {
		return sendFailure(stream, strings.Join(errs, "; "))
	}
	cql := strings.TrimSpace(stringOrEmpty(f["cql"]))
	args := make([]any, 0, len(listField(f["args"])))
	for _, v := range listField(f["args"]) {
		args = append(args, valueToNative(v))
	}
	changed := boolOrDefault(f["changed"], false)

	rows, err := s.Query(ctx, cql, args...)
	if err != nil {
		// The statement itself is NOT put into the message: an operator who
		// interpolated a value into params.cql would otherwise have it echoed into
		// the run log by the failure path.
		return sendFailure(stream, "cql: "+redactError(err, password))
	}

	// A statement the driver would not prepare may be DDL — a CREATE INDEX, a
	// materialized view, an ALTER TABLE — and DDL propagates through gossip like any
	// other schema change. Without this wait, `command.run` would be the one action
	// of this artifact that reports success before the cluster agrees, and the next
	// step would fail on a schema only some nodes have (schema.go). On a statement
	// that changed no schema the wait is already satisfied and returns at once.
	if !isDML(cql) {
		if err := awaitSchema(ctx, s, "cql"); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
	}

	truncated := len(rows) > maxCommandRows
	if truncated {
		rows = rows[:maxCommandRows]
	}
	return sendOutcome(stream, changed, fmt.Sprintf("cql ok (%d rows)", len(rows)), map[string]any{
		"row_count": int64(len(rows)),
		"truncated": truncated,
		"rows":      renderRows(rows),
	})
}

// renderRows turns driver rows into something an event can carry. Every value is
// rendered as TEXT: a CQL row holds types an event struct has no equivalent for — a
// uuid, a timestamp, a blob, a nested collection — and formatting them is what makes
// this action an escape hatch rather than a typed reader. The description says so, so
// nobody builds a comparison on the shape of these values.
func renderRows(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		rendered := make(map[string]any, len(row))
		for _, col := range sortedKeys(row) {
			rendered[col] = rowString(row, col)
		}
		out = append(out, rendered)
	}
	return out
}

// checkCommand is everything `command.run` refuses about its own params, in one
// place so Validate and Apply cannot answer differently (NIM-786).
func checkCommand(f map[string]*structpb.Value) []string {
	errs := requireString(f, "cql")
	if keyspace := stringOrEmpty(f["keyspace"]); keyspace != "" {
		if err := checkIdentifier("params.keyspace", keyspace); err != nil {
			errs = append(errs, err.Error())
		}
	}
	cql := strings.TrimSpace(stringOrEmpty(f["cql"]))
	if cql != "" && len(listField(f["args"])) > 0 && !isDML(cql) {
		errs = append(errs, "params.args: the driver binds values only on SELECT/INSERT/UPDATE/DELETE/BATCH "+
			"and would DISCARD them on this statement, leaving its ? markers unbound and the statement a syntax error. "+
			"Remove params.args and write the values into params.cql — but never a secret: a value in the statement "+
			"text is not covered by the ADR-010 masks")
	}
	return errs
}

// validateCommand — connect params plus a statement to run.
func validateCommand(f map[string]*structpb.Value) []string {
	return append(validateConnect(f), checkCommand(f)...)
}
