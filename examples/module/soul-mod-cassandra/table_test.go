package main

import (
	"strings"
	"testing"
)

// tableParams is a converged declaration: one partition key, one clustering column,
// one regular column.
func tableParams(own map[string]any) map[string]any {
	base := map[string]any{
		"keyspace":       "app",
		"name":           "events",
		"columns":        map[string]any{"id": "uuid", "at": "timestamp", "payload": "text"},
		"partition_key":  []any{"id"},
		"clustering_key": []any{"at"},
	}
	for k, v := range own {
		base[k] = v
	}
	return withParams(base)
}

// liveEvents is the same table as system_schema.columns reports it.
func liveEvents(extra ...map[string]any) []map[string]any {
	rows := []map[string]any{
		columnRow("id", "uuid", kindPartitionKey, 0),
		columnRow("at", "timestamp", kindClustering, 0),
		columnRow("payload", "text", "regular", -1),
	}
	return append(rows, extra...)
}

func TestTablePresent_CreatesWhenAbsent(t *testing.T) {
	// Two reads: the state re-reads after its CREATE ... IF NOT EXISTS, which is a
	// server-side no-op when the coordinator merely lagged behind an existing table.
	s := &fakeSession{answer: answerSequence(nil, liveEvents())}
	stream := apply(t, newModule(s).table(), "present", tableParams(nil))

	assertOutcome(t, stream, true, false)
	stmts := execedStatements(s)
	if len(stmts) != 1 {
		t.Fatalf("expected one CREATE TABLE, got %q", stmts)
	}
	// Column order is deterministic: partition key, clustering key, then the rest
	// sorted — so the same declaration always renders the same statement.
	const want = "CREATE TABLE IF NOT EXISTS app.events (id uuid, at timestamp, payload text, PRIMARY KEY ((id), at))"
	if stmts[0] != want {
		t.Errorf("statement =\n  %q\nwant\n  %q", stmts[0], want)
	}
	if s.agreed != 1 {
		t.Errorf("a CREATE TABLE must wait for schema agreement, waited %d times", s.agreed)
	}
}

func TestTablePresent_RendersACompositePartitionKey(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).table(), "present", tableParams(map[string]any{
		"columns":        map[string]any{"tenant": "text", "id": "uuid", "at": "timestamp"},
		"partition_key":  []any{"tenant", "id"},
		"clustering_key": []any{"at"},
	}))

	if got := execedStatements(s)[0]; !strings.Contains(got, "PRIMARY KEY ((tenant, id), at)") {
		t.Errorf("composite partition key not rendered: %q", got)
	}
}

func TestTablePresent_NoOpWhenAlreadyMatching(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return liveEvents(), nil
	}}
	stream := apply(t, newModule(s).table(), "present", tableParams(nil))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("a converged table must issue no statement, got %q", execedStatements(s))
	}
	// The no-op waits for the reason keyspace_test spells out: the three-way read
	// finishes a missing statement, not a missing agreement.
	if s.agreed != 1 {
		t.Errorf("a no-op must still confirm schema agreement, waited %d times", s.agreed)
	}
}

// TestTablePresent_NormalizesTypeWhitespace — `map<text,int>` as an author writes it
// and `map<text, int>` as Cassandra reports it are one type. Without normalizing,
// the state would call a converged table drift and fail on every run.
func TestTablePresent_NormalizesTypeWhitespace(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return []map[string]any{
			columnRow("id", "uuid", kindPartitionKey, 0),
			columnRow("tags", "map<text, text>", "regular", -1),
		}, nil
	}}
	stream := apply(t, newModule(s).table(), "present", tableParams(map[string]any{
		"columns":        map[string]any{"id": "uuid", "tags": "map<text,text>"},
		"partition_key":  []any{"id"},
		"clustering_key": []any{},
	}))

	assertOutcome(t, stream, false, false)
}

// --- ★ an existing table that does not match is a failure ---

// TestTablePresent_RefusesToReconcileAnExistingTable is the guard on the decision NOT
// to ALTER. Cassandra cannot change a primary key or a column type; a state that
// added the columns it could while ignoring the key it could not would report success
// on a table that is not the declared one.
func TestTablePresent_RefusesToReconcileAnExistingTable(t *testing.T) {
	cases := []struct {
		name string
		live []map[string]any
		want string
	}{
		{
			name: "a column has another type",
			live: []map[string]any{
				columnRow("id", "uuid", kindPartitionKey, 0),
				columnRow("at", "timestamp", kindClustering, 0),
				columnRow("payload", "blob", "regular", -1),
			},
			want: "column payload is blob, declared text",
		},
		{
			name: "a declared column is missing",
			live: []map[string]any{
				columnRow("id", "uuid", kindPartitionKey, 0),
				columnRow("at", "timestamp", kindClustering, 0),
			},
			want: "column payload (text) is missing",
		},
		{
			name: "the partition key is different",
			live: []map[string]any{
				columnRow("payload", "text", kindPartitionKey, 0),
				columnRow("at", "timestamp", kindClustering, 0),
				columnRow("id", "uuid", "regular", -1),
			},
			want: "partition key is (payload), declared (id)",
		},
		{
			name: "the clustering key is different",
			live: []map[string]any{
				columnRow("id", "uuid", kindPartitionKey, 0),
				columnRow("at", "timestamp", "regular", -1),
				columnRow("payload", "text", "regular", -1),
			},
			want: "clustering key is (), declared (at)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) { return tc.live, nil }}
			stream := apply(t, newModule(s).table(), "present", tableParams(nil))

			e := assertOutcome(t, stream, false, true)
			if !strings.Contains(e.GetMessage(), tc.want) {
				t.Errorf("the failure must name the difference %q, got %q", tc.want, e.GetMessage())
			}
			if len(s.execed) != 0 {
				t.Errorf("a mismatched table must not be touched, got %q", execedStatements(s))
			}
		})
	}
}

// TestTablePresent_IgnoresAColumnItDoesNotDeclare — the declaration is the columns
// this service needs, not the complete set the table may carry. Treating someone
// else's column as drift would make the object unusable beside anything that adds one.
func TestTablePresent_IgnoresAColumnItDoesNotDeclare(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return liveEvents(columnRow("added_by_someone_else", "text", "regular", -1)), nil
	}}
	stream := apply(t, newModule(s).table(), "present", tableParams(nil))

	assertOutcome(t, stream, false, false)
}

// --- absent ---

func TestTableAbsent_DropsWhenPresent(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) { return liveEvents(), nil }}
	stream := apply(t, newModule(s).table(), "absent", withParams(map[string]any{"keyspace": "app", "name": "events"}))

	assertOutcome(t, stream, true, false)
	if stmts := execedStatements(s); len(stmts) != 1 || stmts[0] != "DROP TABLE IF EXISTS app.events" {
		t.Fatalf("expected DROP TABLE IF EXISTS app.events, got %q", stmts)
	}
	if s.agreed != 1 {
		t.Error("a drop is a schema change and must wait for agreement too")
	}
}

func TestTableAbsent_NoOpWhenAlreadyGone(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).table(), "absent", withParams(map[string]any{"keyspace": "app", "name": "events"}))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("dropping an absent table must issue nothing, got %q", execedStatements(s))
	}
}

// TestTable_DoesNotConnectIntoTheKeyspace — `params.keyspace` names where the table
// lives, not a session keyspace. The driver issues a USE on connect, so carrying it
// into the connection would fail the CONNECTION for a keyspace that does not exist
// yet, reporting an error about something the author did not write.
func TestTable_DoesNotConnectIntoTheKeyspace(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).table(), "present", tableParams(nil))
	if s.cfg.keyspace != "" {
		t.Errorf("table actions must not set a session keyspace, got %q", s.cfg.keyspace)
	}
}

// --- the declaration ---

func TestParseTableSpec_Refusals(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			name: "a key column that is not declared",
			in:   map[string]any{"partition_key": []any{"nope"}},
			want: `"nope" is not one of params.columns`,
		},
		{
			name: "a column in both halves of the primary key",
			in:   map[string]any{"partition_key": []any{"id"}, "clustering_key": []any{"id"}},
			want: "appears twice in the primary key",
		},
		{
			name: "no partition key",
			in:   map[string]any{"partition_key": []any{}},
			want: "must name at least one column",
		},
		{
			name: "no columns",
			in:   map[string]any{"columns": map[string]any{}},
			want: "params.columns",
		},
		{
			// A type expression is concatenated into the statement, so anything that
			// could close it is refused rather than escaped.
			name: "a type expression that could carry a second statement",
			in:   map[string]any{"columns": map[string]any{"id": "uuid; DROP TABLE x"}, "partition_key": []any{"id"}, "clustering_key": []any{}},
			want: "is not a usable CQL type expression",
		},
		{
			// Cassandra folds an unquoted identifier to lowercase, so an uppercase
			// name would be created and then never found again.
			name: "an uppercase table name",
			in:   map[string]any{"name": "Events"},
			want: "is not a usable CQL identifier here",
		},
		{
			name: "a column type that is not a string",
			in:   map[string]any{"columns": map[string]any{"id": 3}, "partition_key": []any{"id"}, "clustering_key": []any{}},
			want: "must be a string CQL type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTableSpec(mustStruct(t, tableParams(tc.in)).GetFields())
			if err == nil {
				t.Fatal("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}
