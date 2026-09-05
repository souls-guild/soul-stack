package main

import (
	"fmt"
	"testing"
)

func TestCommandRun_BindsArgsInsteadOfBuildingTheStatement(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).command(), "run", withParams(map[string]any{
		"cql":  "SELECT payload FROM app.events WHERE id = ?",
		"args": []any{"7f3a"},
	}))

	if len(s.queried) != 1 {
		t.Fatalf("expected one statement, got %d", len(s.queried))
	}
	call := s.queried[0]
	if call.stmt != "SELECT payload FROM app.events WHERE id = ?" {
		t.Errorf("the statement was rewritten: %q", call.stmt)
	}
	if len(call.args) != 1 || call.args[0] != "7f3a" {
		t.Errorf("args were not bound: %v", call.args)
	}
}

// TestCommandRun_IsAProbeUnlessDeclaredOtherwise — the driver cannot tell whether a
// statement mutated anything, so the default is the honest one and `changed` is the
// author's declaration.
func TestCommandRun_IsAProbeUnlessDeclaredOtherwise(t *testing.T) {
	for _, tc := range []struct{ declared, want bool }{{false, false}, {true, true}} {
		t.Run(fmt.Sprintf("changed=%t", tc.declared), func(t *testing.T) {
			s := &fakeSession{}
			stream := apply(t, newModule(s).command(), "run", withParams(map[string]any{
				"cql": "UPDATE app.events SET payload = 'x' WHERE id = 1", "changed": tc.declared,
			}))
			assertOutcome(t, stream, tc.want, false)
		})
	}
}

func TestCommandRun_RendersRowsAsText(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return []map[string]any{{"n": int64(3), "name": "app"}}, nil
	}}
	stream := apply(t, newModule(s).command(), "run", withParams(map[string]any{"cql": "SELECT n, name FROM t"}))

	e := assertOutcome(t, stream, false, false)
	fields := e.GetOutput().GetFields()
	if fields["row_count"].GetNumberValue() != 1 {
		t.Errorf("row_count = %v, want 1", fields["row_count"].GetNumberValue())
	}
	row := fields["rows"].GetListValue().GetValues()[0].GetStructValue().GetFields()
	if row["n"].GetStringValue() != "3" || row["name"].GetStringValue() != "app" {
		t.Errorf("rows not rendered as text: %v", row)
	}
}

// TestCommandRun_BoundsWhatOneEventCarries — an unbounded SELECT over a real table
// would exhaust the plugin and flood the stream, so the bound is hit and SAID rather
// than a prefix being returned as if it were everything.
func TestCommandRun_BoundsWhatOneEventCarries(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		rows := make([]map[string]any, maxCommandRows+50)
		for i := range rows {
			rows[i] = map[string]any{"i": int64(i)}
		}
		return rows, nil
	}}
	stream := apply(t, newModule(s).command(), "run", withParams(map[string]any{"cql": "SELECT i FROM t"}))

	fields := assertOutcome(t, stream, false, false).GetOutput().GetFields()
	if got := int(fields["row_count"].GetNumberValue()); got != maxCommandRows {
		t.Errorf("row_count = %d, want the %d bound", got, maxCommandRows)
	}
	if !fields["truncated"].GetBoolValue() {
		t.Error("truncated must say the bound was hit")
	}
}

// TestCommandRun_ConnectsIntoTheDeclaredKeyspace — this is the one object where
// params.keyspace is a SESSION keyspace, so unqualified table names resolve.
func TestCommandRun_ConnectsIntoTheDeclaredKeyspace(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).command(), "run", withParams(map[string]any{"cql": "SELECT * FROM events", "keyspace": "app"}))

	if s.cfg.keyspace != "app" {
		t.Errorf("session keyspace = %q, want app", s.cfg.keyspace)
	}
}
