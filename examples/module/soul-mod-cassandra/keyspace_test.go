package main

import (
	"errors"
	"strings"
	"testing"
)

// keyspaceParams is a converged declaration: NetworkTopologyStrategy, dc1=3.
func keyspaceParams(own map[string]any) map[string]any {
	base := map[string]any{
		"name":        "app",
		"replication": map[string]any{"class": networkStrategy, "dc1": 3},
	}
	for k, v := range own {
		base[k] = v
	}
	return withParams(base)
}

// --- the three converge branches ---

func TestKeyspacePresent_CreatesWhenAbsent(t *testing.T) {
	// Two reads, not one: the state re-reads after its CREATE, because
	// IF NOT EXISTS makes the statement a server-side no-op when the coordinator
	// merely lagged behind a keyspace that already existed.
	s := &fakeSession{answer: answerSequence(
		nil,
		keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true),
	)}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	e := assertOutcome(t, stream, true, false)
	stmts := execedStatements(s)
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "CREATE KEYSPACE IF NOT EXISTS app ") {
		t.Fatalf("expected one CREATE KEYSPACE, got %q", stmts)
	}
	if !strings.Contains(stmts[0], "{'class': 'NetworkTopologyStrategy', 'dc1': '3'}") {
		t.Errorf("replication not rendered as declared: %q", stmts[0])
	}
	if !strings.Contains(stmts[0], "DURABLE_WRITES = true") {
		t.Errorf("durable_writes should default to true: %q", stmts[0])
	}
	// A keyspace created now holds no data, so nothing is owed.
	if e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("repair_required must be false on a freshly created keyspace")
	}
	assertNoSecretInEvents(t, stream)
	assertNoSecretInStatementText(t, s)
}

func TestKeyspacePresent_NoOpWhenAlreadyMatching(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("a converged keyspace must issue no statement, got %q", execedStatements(s))
	}
}

func TestKeyspacePresent_AltersWhenDeclarationDiffers(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
	}}
	// Same factor for dc1, but a datacenter the live keyspace does not replicate to.
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(map[string]any{
		"replication": map[string]any{"class": networkStrategy, "dc1": 3, "dc2": 2},
	}))

	assertOutcome(t, stream, true, false)
	stmts := execedStatements(s)
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "ALTER KEYSPACE app ") {
		t.Fatalf("expected one ALTER KEYSPACE, got %q", stmts)
	}
}

func TestKeyspacePresent_AltersOnDurableWritesAlone(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(map[string]any{"durable_writes": false}))

	assertOutcome(t, stream, true, false)
	if stmts := execedStatements(s); len(stmts) != 1 || !strings.Contains(stmts[0], "DURABLE_WRITES = false") {
		t.Fatalf("durable_writes drift must be reconciled, got %q", stmts)
	}
}

// --- ★ replication factor and repair ---

// TestKeyspacePresent_RaisingTheFactorDemandsRepair is the guard on the decision that
// repair is NOT part of this state. Raising a factor changes the schema and moves no
// data, so the state must say so — in Output for a scenario to gate on, and in the
// message for a human reading the run.
func TestKeyspacePresent_RaisingTheFactorDemandsRepair(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "1"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil)) // dc1: 1 -> 3

	e := assertOutcome(t, stream, true, false)
	if !e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("raising a replication factor must set repair_required")
	}
	if !strings.Contains(e.GetMessage(), "REPAIR REQUIRED") {
		t.Errorf("the message must say repair is owed, got %q", e.GetMessage())
	}
}

// TestKeyspacePresent_LoweringTheFactorDoesNotDemandRepair — surplus copies are still
// correct copies. What is owed there is `nodetool cleanup`, which is a disk-space
// matter, not a correctness one, and flagging it as repair would train an operator to
// ignore the flag.
func TestKeyspacePresent_LoweringTheFactorDoesNotDemandRepair(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "5"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil)) // dc1: 5 -> 3

	e := assertOutcome(t, stream, true, false)
	if e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("lowering a replication factor must NOT set repair_required")
	}
}

// --- ★ schema agreement ---

// TestKeyspacePresent_FailsWhenTheClusterDoesNotAgree is the invariant this object
// exists to hold. The DDL succeeded; the nodes did not converge on it. Reporting
// changed=true here is what leaves the NEXT step of the run failing on a keyspace
// that "does not exist" on whichever node it reaches.
func TestKeyspacePresent_FailsWhenTheClusterDoesNotAgree(t *testing.T) {
	s := &fakeSession{agreeErr: errors.New("gossip timed out")}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	e := assertOutcome(t, stream, false, true)
	if !strings.Contains(e.GetMessage(), "schema agreement") {
		t.Errorf("the failure must name schema agreement, got %q", e.GetMessage())
	}
	if s.agreed != 1 {
		t.Errorf("the state must wait for agreement exactly once, waited %d times", s.agreed)
	}
}

func TestKeyspacePresent_WaitsForAgreementAfterEveryDDL(t *testing.T) {
	for _, tc := range []struct {
		name string
		live func(string, []any) ([]map[string]any, error)
	}{
		{"create", nil},
		{"alter", func(string, []any) ([]map[string]any, error) {
			return keyspaceRows(networkStrategy, map[string]string{"dc1": "1"}, true), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeSession{answer: tc.live}
			apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))
			if s.agreed != 1 {
				t.Errorf("a DDL-issuing branch must wait for schema agreement, waited %d times", s.agreed)
			}
		})
	}
}

// TestKeyspacePresent_NoOpStillWaitsForAgreement — the no-op path waits, and this is
// the case the wait exists for.
//
// The converge read asks ONE coordinator. A previous run killed between its CREATE
// and its wait leaves the keyspace present on that coordinator and absent on a
// lagging replica; reporting no-op without waiting would hand the next step exactly
// the failure the mechanism is meant to prevent. The three-way read finishes a
// missing STATEMENT — it cannot finish a missing AGREEMENT.
func TestKeyspacePresent_NoOpStillWaitsForAgreement(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Errorf("a no-op must issue no statement, got %q", execedStatements(s))
	}
	if s.agreed != 1 {
		t.Errorf("a no-op must still confirm schema agreement, waited %d times", s.agreed)
	}
}

// TestKeyspacePresent_NoOpFailsWhenTheClusterHasNotAgreed — and the wait is load
// bearing: if agreement cannot be reached, a no-op is a FAILURE, not a green step.
func TestKeyspacePresent_NoOpFailsWhenTheClusterHasNotAgreed(t *testing.T) {
	s := &fakeSession{
		answer: func(string, []any) ([]map[string]any, error) {
			return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
		},
		agreeErr: errors.New("gocql: cluster schema versions not consistent"),
	}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))
	assertOutcome(t, stream, false, true)
}

// --- absent ---

func TestKeyspaceAbsent_DropsWhenPresent(t *testing.T) {
	s := &fakeSession{answer: func(string, []any) ([]map[string]any, error) {
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true), nil
	}}
	stream := apply(t, newModule(s).keyspace(), "absent", withParams(map[string]any{"name": "app"}))

	assertOutcome(t, stream, true, false)
	if stmts := execedStatements(s); len(stmts) != 1 || stmts[0] != "DROP KEYSPACE IF EXISTS app" {
		t.Fatalf("expected DROP KEYSPACE IF EXISTS app, got %q", stmts)
	}
	if s.agreed != 1 {
		t.Error("a drop is a schema change and must wait for agreement too")
	}
}

func TestKeyspaceAbsent_NoOpWhenAlreadyGone(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).keyspace(), "absent", withParams(map[string]any{"name": "app"}))

	assertOutcome(t, stream, false, false)
	if len(s.execed) != 0 {
		t.Fatalf("dropping an absent keyspace must issue nothing, got %q", execedStatements(s))
	}
}

// --- replication declaration ---

func TestParseReplication_Refusals(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{
			// The NIM-778 rule at the nested level: a factor is a count, and "3"
			// beside 3 is the coercion the whole artifact refuses.
			name: "a factor written as a string",
			in:   map[string]any{"class": networkStrategy, "dc1": "3"},
			want: "must be an integer replication factor",
		},
		{
			name: "a fractional factor",
			in:   map[string]any{"class": networkStrategy, "dc1": 2.5},
			want: "must be an integer replication factor",
		},
		{
			name: "a negative factor",
			in:   map[string]any{"class": networkStrategy, "dc1": -1},
			want: "must not be negative",
		},
		{
			name: "a strategy this artifact does not manage",
			in:   map[string]any{"class": "OldNetworkTopologyStrategy", "dc1": 3},
			want: "is not a strategy this plugin manages",
		},
		{
			name: "SimpleStrategy without its factor",
			in:   map[string]any{"class": simpleStrategy, "dc1": 3},
			want: "takes exactly one option",
		},
		{
			name: "NetworkTopologyStrategy without a datacenter",
			in:   map[string]any{"class": networkStrategy},
			want: "takes at least one datacenter",
		},
		{
			// The key is concatenated into a quoted CQL literal, so a quote in it
			// would end the literal early.
			name: "a datacenter name that could break out of the literal",
			in:   map[string]any{"class": networkStrategy, "dc1', 'x": 3},
			want: "is not a usable datacenter name",
		},
		{
			name: "no class at all",
			in:   map[string]any{"dc1": 3},
			want: "params.replication.class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustStruct(t, map[string]any{"replication": tc.in})
			_, err := parseReplication(s.GetFields()["replication"])
			if err == nil {
				t.Fatalf("expected a refusal, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestReplicationLiteralIsDeterministic — the same declaration must render the same
// statement on every run, or a diff between two runs means nothing.
func TestReplicationLiteralIsDeterministic(t *testing.T) {
	spec := replicationSpec{class: networkStrategy, factors: map[string]int{"dc2": 2, "dc1": 3, "dc3": 1}}
	const want = "{'class': 'NetworkTopologyStrategy', 'dc1': '3', 'dc2': '2', 'dc3': '1'}"
	for i := 0; i < 10; i++ {
		if got := spec.literal(); got != want {
			t.Fatalf("literal() = %q, want %q", got, want)
		}
	}
}

// TestLiveReplicationStripsTheClassPrefix — Cassandra stores the fully qualified
// class name and an author writes the short one; without normalizing them the state
// would see drift on every run and ALTER a converged keyspace forever.
func TestLiveReplicationStripsTheClassPrefix(t *testing.T) {
	live, err := liveReplication(map[string]string{
		"class": classPrefix + networkStrategy,
		"dc1":   "3",
	})
	if err != nil {
		t.Fatalf("liveReplication: %v", err)
	}
	want := replicationSpec{class: networkStrategy, factors: map[string]int{"dc1": 3}}
	if !live.equal(want) {
		t.Errorf("live %+v does not equal the declared %+v", live, want)
	}
}
