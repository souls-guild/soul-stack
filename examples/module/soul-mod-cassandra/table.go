// The `table` object's implementation — CREATE / DROP TABLE.
//
// Same three-way read as `keyspace` (schema.go), with a deliberately different
// answer on the third branch: a table that exists but does not match is a FAILURE,
// not an ALTER.
//
// ★ WHY IT REFUSES INSTEAD OF RECONCILING. Cassandra cannot change a primary key at
// all, and cannot change a column's type. The only reconciliation the server would
// accept is adding a column — and a state that silently added the columns it could
// while ignoring the primary key it could not would report success on a table that
// is not the declared one. The alternative, dropping and re-creating, is data loss
// performed by a converge step. So the state says exactly what differs and stops,
// which leaves the operator holding a decision that is theirs.
//
// Table OPTIONS (compaction, compression, gc_grace_seconds, default_time_to_live,
// caching) are NOT managed: they are not compared, not set, and not reported. A
// table that needs them is created with `command.run`. This is stated in the
// description rather than half-implemented, because an `options` param that applied
// only at CREATE would silently do nothing on an existing table — the shape of
// promise this object exists to avoid.
package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"
)

const columnsQuery = `SELECT column_name, type, kind, position FROM system_schema.columns WHERE keyspace_name = ? AND table_name = ?`

// Column kinds as system_schema.columns spells them.
const (
	kindPartitionKey = "partition_key"
	kindClustering   = "clustering"
)

// tableSpec is a table's structure, in the one shape the declared and the live side
// are both normalized to.
type tableSpec struct {
	keyspace string
	name     string
	// columns maps a column name to its NORMALIZED type (identifier.go), so
	// `map<text,int>` and `map<text, int>` are one value.
	columns       map[string]string
	partitionKey  []string
	clusteringKey []string
}

// parseTableSpec reads and checks the declared table. Validate and Apply both call
// it, which is what makes their answers identical by construction rather than by two
// authors keeping two lists in step (NIM-786).
func parseTableSpec(f map[string]*structpb.Value) (tableSpec, error) {
	spec := tableSpec{
		keyspace: stringOrEmpty(f["keyspace"]),
		name:     stringOrEmpty(f["name"]),
		columns:  map[string]string{},
	}
	if err := checkIdentifier("params.keyspace", spec.keyspace); err != nil {
		return tableSpec{}, err
	}
	if err := checkIdentifier("params.name", spec.name); err != nil {
		return tableSpec{}, err
	}

	declared := mapField(f["columns"])
	if len(declared) == 0 {
		return tableSpec{}, fmt.Errorf(`params.columns: must be a non-empty map of column name to CQL type, e.g. {id: uuid, created_at: timestamp}`)
	}
	for _, col := range sortedKeys(declared) {
		if err := checkIdentifier("params.columns."+col, col); err != nil {
			return tableSpec{}, err
		}
		typeExpr, ok := stringValue(declared[col])
		if !ok {
			return tableSpec{}, fmt.Errorf("params.columns.%s: must be a string CQL type, got %s", col, valueTypeName(declared[col]))
		}
		if err := checkTypeExpr("params.columns."+col, typeExpr); err != nil {
			return tableSpec{}, err
		}
		spec.columns[col] = normalizeType(typeExpr)
	}

	var err error
	if spec.partitionKey, err = keyColumns(f["partition_key"], "params.partition_key", spec.columns, nil); err != nil {
		return tableSpec{}, err
	}
	if len(spec.partitionKey) == 0 {
		return tableSpec{}, fmt.Errorf("params.partition_key: must name at least one column — a Cassandra table has no default primary key")
	}
	if spec.clusteringKey, err = keyColumns(f["clustering_key"], "params.clustering_key", spec.columns, spec.partitionKey); err != nil {
		return tableSpec{}, err
	}
	return spec, nil
}

// keyColumns reads one half of the primary key: every entry must name a declared
// column, appear once, and not already be taken by the other half. A key naming a
// column that does not exist is refused here rather than by the server mid-run.
func keyColumns(v *structpb.Value, addr string, columns map[string]string, taken []string) ([]string, error) {
	names, err := stringList(v, addr)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range taken {
		seen[t] = true
	}
	for i, name := range names {
		if _, declared := columns[name]; !declared {
			return nil, fmt.Errorf("%s[%d]: %q is not one of params.columns (%s)", addr, i, name, strings.Join(sortedKeys(columns), ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("%s[%d]: %q appears twice in the primary key", addr, i, name)
		}
		seen[name] = true
	}
	return names, nil
}

// readTable reads the live table's structure out of system_schema.columns. A table
// with no columns does not exist — which is also the answer when the KEYSPACE does
// not exist, and the CREATE that follows says so in the server's own words.
func readTable(ctx context.Context, s cassSession, keyspace, name string) (tableSpec, bool, error) {
	rows, err := s.Query(ctx, columnsQuery, keyspace, name)
	if err != nil {
		return tableSpec{}, false, fmt.Errorf("read system_schema.columns: %w", err)
	}
	if len(rows) == 0 {
		return tableSpec{}, false, nil
	}

	live := tableSpec{keyspace: keyspace, name: name, columns: map[string]string{}}
	type positioned struct {
		name     string
		position int
	}
	var partition, clustering []positioned
	for _, row := range rows {
		col := rowString(row, "column_name")
		live.columns[col] = normalizeType(rowString(row, "type"))
		switch rowString(row, "kind") {
		case kindPartitionKey:
			partition = append(partition, positioned{col, rowInt(row, "position")})
		case kindClustering:
			clustering = append(clustering, positioned{col, rowInt(row, "position")})
		}
	}
	byPosition := func(in []positioned) []string {
		sort.Slice(in, func(i, j int) bool { return in[i].position < in[j].position })
		out := make([]string, 0, len(in))
		for _, p := range in {
			out = append(out, p.name)
		}
		return out
	}
	live.partitionKey = byPosition(partition)
	live.clusteringKey = byPosition(clustering)
	return live, true, nil
}

// diff reports every way the live table is not the declared one, in a stable order.
// Empty means they match.
//
// A column the live table has and the declaration does not is NOT drift: this object
// declares the columns it needs, not the complete set the table may carry, and
// reporting someone else's column as a difference would make the state unusable
// beside anything that adds one.
func (want tableSpec) diff(live tableSpec) []string {
	var out []string
	if strings.Join(want.partitionKey, ",") != strings.Join(live.partitionKey, ",") {
		out = append(out, fmt.Sprintf("partition key is (%s), declared (%s)",
			strings.Join(live.partitionKey, ", "), strings.Join(want.partitionKey, ", ")))
	}
	if strings.Join(want.clusteringKey, ",") != strings.Join(live.clusteringKey, ",") {
		out = append(out, fmt.Sprintf("clustering key is (%s), declared (%s)",
			strings.Join(live.clusteringKey, ", "), strings.Join(want.clusteringKey, ", ")))
	}
	for _, col := range sortedKeys(want.columns) {
		liveType, present := live.columns[col]
		switch {
		case !present:
			out = append(out, fmt.Sprintf("column %s (%s) is missing", col, want.columns[col]))
		case liveType != want.columns[col]:
			out = append(out, fmt.Sprintf("column %s is %s, declared %s", col, liveType, want.columns[col]))
		}
	}
	return out
}

// createStatement renders the CREATE TABLE. Column order is deterministic —
// partition key, then clustering key, then the rest sorted — so the same declaration
// always produces the same statement.
func (want tableSpec) createStatement() string {
	inKey := map[string]bool{}
	ordered := make([]string, 0, len(want.columns))
	for _, col := range append(append([]string{}, want.partitionKey...), want.clusteringKey...) {
		inKey[col] = true
		ordered = append(ordered, col)
	}
	for _, col := range sortedKeys(want.columns) {
		if !inKey[col] {
			ordered = append(ordered, col)
		}
	}

	defs := make([]string, 0, len(ordered)+1)
	for _, col := range ordered {
		defs = append(defs, col+" "+want.columns[col])
	}
	primary := "(" + strings.Join(want.partitionKey, ", ") + ")"
	if len(want.clusteringKey) > 0 {
		primary += ", " + strings.Join(want.clusteringKey, ", ")
	}
	defs = append(defs, "PRIMARY KEY ("+primary+")")

	// IF NOT EXISTS for the same reason keyspace.present carries it: the converge
	// read went to one coordinator, and one lagging behind a table another node
	// already has would answer "absent" and turn a converge into a hard
	// AlreadyExists.
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", qualify(want.keyspace, want.name), strings.Join(defs, ", "))
}

// applyTablePresent creates the table, or proves the existing one is the declared
// one.
func (m *CassandraModule) applyTablePresent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	want, err := parseTableSpec(f)
	if err != nil {
		return sendFailure(stream, err.Error())
	}

	live, found, err := readTable(ctx, s, want.keyspace, want.name)
	if err != nil {
		return sendFailure(stream, redactError(err, password))
	}

	switch tableConverge(found, live, want) {
	case convergeAbsent:
		if err := s.Exec(ctx, want.createStatement()); err != nil {
			return sendFailure(stream, "CREATE TABLE: "+redactError(err, password))
		}
		if err := awaitSchema(ctx, s, "table "+qualify(want.keyspace, want.name)); err != nil {
			return sendFailure(stream, redactError(err, password))
		}

		// ★ RE-READ, for the reason keyspace.go spells out: IF NOT EXISTS makes the
		// CREATE a server-side no-op when the table already existed cluster-wide and
		// only this coordinator was behind, and without this read the state would
		// report "created" over a table whose columns are not the declared ones.
		live, found, err = readTable(ctx, s, want.keyspace, want.name)
		if err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		if !found {
			return sendFailure(stream, fmt.Sprintf("table %s is still absent after CREATE and schema agreement",
				qualify(want.keyspace, want.name)))
		}
		if differences := want.diff(live); len(differences) > 0 {
			return sendFailure(stream, tableMismatch(want, differences))
		}
		return sendOutcome(stream, true, fmt.Sprintf("table %s created", qualify(want.keyspace, want.name)), tableOutput(want))

	case convergeMatches:
		// Waits on the no-op path for the reason spelled out in keyspace.go: the
		// three-way read finishes a missing statement, not a missing agreement, and a
		// run killed between its CREATE and its wait leaves exactly this shape.
		if err := awaitSchema(ctx, s, "table "+qualify(want.keyspace, want.name)); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		return sendOutcome(stream, false, fmt.Sprintf("table %s already matches (no-op)", qualify(want.keyspace, want.name)), tableOutput(want))

	default:
		return sendFailure(stream, tableMismatch(want, want.diff(live)))
	}
}

// tableMismatch is the refusal, in one place because two paths reach it: a table
// that was already there, and one the CREATE ... IF NOT EXISTS turned out not to
// have created.
func tableMismatch(want tableSpec, differences []string) string {
	return fmt.Sprintf(
		"table %s exists and does not match the declaration: %s. "+
			"This state does not reconcile an existing table — Cassandra cannot change a primary key or a column type, "+
			"and re-creating the table would destroy its data. Resolve it deliberately (ALTER via command.run, or a migration).",
		qualify(want.keyspace, want.name), strings.Join(differences, "; "))
}

// tableConverge is the three-way read of schema.go. The third answer is where this
// object differs from `keyspace`: differing means REFUSE, not ALTER.
func tableConverge(found bool, live, want tableSpec) converge {
	switch {
	case !found:
		return convergeAbsent
	case len(want.diff(live)) == 0:
		return convergeMatches
	default:
		return convergeDiffers
	}
}

// tableOutput is the one shape every non-failing outcome of `present` reports.
func tableOutput(spec tableSpec) map[string]any {
	return map[string]any{
		"keyspace":       spec.keyspace,
		"table":          spec.name,
		"partition_key":  strings.Join(spec.partitionKey, ","),
		"clustering_key": strings.Join(spec.clusteringKey, ","),
		"columns":        int64(len(spec.columns)),
	}
}

// applyTableAbsent drops a table. Idempotent: one that is not there is a no-op.
func (m *CassandraModule) applyTableAbsent(ctx context.Context, stream eventStream, s cassSession, params *structpb.Struct) error {
	f := params.GetFields()
	password := stringOrEmpty(f["password"])

	keyspace := stringOrEmpty(f["keyspace"])
	name := stringOrEmpty(f["name"])
	if err := checkIdentifier("params.keyspace", keyspace); err != nil {
		return sendFailure(stream, err.Error())
	}
	if err := checkIdentifier("params.name", name); err != nil {
		return sendFailure(stream, err.Error())
	}

	if _, found, err := readTable(ctx, s, keyspace, name); err != nil {
		return sendFailure(stream, redactError(err, password))
	} else if !found {
		if err := awaitSchema(ctx, s, "table "+qualify(keyspace, name)); err != nil {
			return sendFailure(stream, redactError(err, password))
		}
		return sendOutcome(stream, false, fmt.Sprintf("table %s is already absent (no-op)", qualify(keyspace, name)),
			map[string]any{"keyspace": keyspace, "table": name})
	}

	if err := s.Exec(ctx, "DROP TABLE IF EXISTS "+qualify(keyspace, name)); err != nil {
		return sendFailure(stream, "DROP TABLE: "+redactError(err, password))
	}
	if err := awaitSchema(ctx, s, "table "+qualify(keyspace, name)); err != nil {
		return sendFailure(stream, redactError(err, password))
	}
	return sendOutcome(stream, true, fmt.Sprintf("table %s dropped", qualify(keyspace, name)),
		map[string]any{"keyspace": keyspace, "table": name})
}

func validateTablePresent(f map[string]*structpb.Value) []string {
	errs := validateConnect(f)
	if _, err := parseTableSpec(f); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}

func validateTableAbsent(f map[string]*structpb.Value) []string {
	errs := validateConnect(f)
	if err := checkIdentifier("params.keyspace", stringOrEmpty(f["keyspace"])); err != nil {
		errs = append(errs, err.Error())
	}
	if err := checkIdentifier("params.name", stringOrEmpty(f["name"])); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}
