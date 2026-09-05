// Guards on the invariants that are cheap to state and expensive to lose: secrets
// out of every error path, a type expression that cannot declare a second column,
// and the two places the driver's binding rule leaks into this artifact's contract.
//
// Each of these replaced a test that could not fail. The redaction guard is the
// clearest case: the suite asserted "no secret in the events" while injecting an
// error that never contained one, so [redactError] could have been deleted whole and
// everything would have stayed green.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// leakyError is what a driver error looks like when the server or the connection
// layer echoed the credentials back. It carries exactly the secrets the action was
// GIVEN — derived from its own params rather than fixed — so the assertion is about
// redaction and not about an action forwarding a value it never received.
func leakyError(params map[string]any) error {
	msg := "gocql: connection failed"
	for _, key := range sortedKeys(params) {
		if s, ok := params[key].(string); ok && (s == secretPass || s == secretRolePass) {
			msg += " " + key + "=" + s
		}
	}
	return errors.New(msg + " (server echoed the credentials back)")
}

// TestLeakyErrorCarriesTheSecrets is the floor under the redaction guard: an
// injected error that contained no secret would make every assertion below vacuous,
// which is precisely how the version this replaced passed while asserting nothing.
func TestLeakyErrorCarriesTheSecrets(t *testing.T) {
	if !strings.Contains(leakyError(validParams("keyspace", "present")).Error(), secretPass) {
		t.Error("the injected error must carry the connection password")
	}
	if !strings.Contains(leakyError(validParams("role", "present")).Error(), secretRolePass) {
		t.Error("the injected error must carry the role password on role.present")
	}
}

// TestEveryErrorPathIsRedacted — every action, both the read error and the write
// error, with an error that actually carries the secrets.
//
// This is the guard the ADR-010 requirement asks for. Without an error that contains
// a secret the assertion is vacuous: the previous version injected
// `errors.New("no connections available")` and would have passed against a plugin
// with no redaction at all.
func TestEveryErrorPathIsRedacted(t *testing.T) {
	for name, obj := range allObjects(&CassandraModule{}) {
		for _, state := range obj.states() {
			for _, failure := range []string{"read", "write"} {
				t.Run(name+"."+state+"/"+failure, func(t *testing.T) {
					params := validParams(name, state)
					s := &fakeSession{}
					switch failure {
					case "read":
						s.answer = func(string, []any) ([]map[string]any, error) { return nil, leakyError(params) }
					case "write":
						s.execErr = leakyError(params)
						// Reaching a write means getting past the converge read, and
						// the two halves need OPPOSITE reads to do it: `present`
						// writes when the subject is absent (the fake's default
						// empty answer), `absent` writes when it is there. Feeding
						// both the empty answer sent every `absent` case down the
						// no-op path, where it issued nothing and proved nothing.
						if state == "absent" {
							s.answer = answerSequence(presentRows(name))
						}
					}

					stream := apply(t, allObjects(newModule(s))[name], state, params)
					assertNoSecretInEvents(t, stream)

					// A "write" subtest over an action that issues no statement
					// asserts nothing. `instance` and `command` only ever Query, so
					// they are named here; anything ELSE reaching this branch without
					// an Exec means a write path stopped being exercised and the
					// subtest quietly went vacuous.
					if failure == "write" && len(s.execed) == 0 && name != "instance" && name != "command" {
						t.Errorf("no statement was issued, so this subtest proved nothing about the write path")
					}
				})
			}
		}
	}
}

// TestAgreementFailureIsRedacted — the schema-agreement wait is an error path like
// any other, and it is the one added last, so it is the one most likely to have been
// wired up without redaction.
func TestAgreementFailureIsRedacted(t *testing.T) {
	s := &fakeSession{agreeErr: leakyError(keyspaceParams(nil))}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	assertOutcome(t, stream, false, true)
	assertNoSecretInEvents(t, stream)
}

// TestTypeExpressionCannotDeclareASecondColumn — a column TYPE is concatenated into
// the CREATE TABLE, so it has to be one type and not a fragment of a column list.
//
// `int, b text` was admitted by the character-class check that came before: it
// created an undeclared column AND left the state permanently unconvergeable, since
// what comes back for that column is `int` while the declaration normalizes to
// `int,btext`. `text static` is the same hole with a space instead of a comma.
func TestTypeExpressionCannotDeclareASecondColumn(t *testing.T) {
	for _, bad := range []string{"int, b text", "text static", "map<text,int> x", "list<int"} {
		t.Run(bad, func(t *testing.T) {
			params := tableParams(map[string]any{
				"columns": map[string]any{"id": "uuid", "a": bad},
			})

			reply, _ := newModule(&fakeSession{}).table().Validate(context.Background(),
				&pluginv1.ValidateRequest{State: "present", Params: mustStruct(t, params)})
			if reply.GetOk() {
				t.Error("Validate accepted a type expression that declares more than one column")
			}

			s := &fakeSession{}
			stream := apply(t, newModule(s).table(), "present", params)
			assertOutcome(t, stream, false, true)
			if len(s.execed) != 0 {
				t.Errorf("nothing may be issued after the refusal, got %q", execedStatements(s))
			}
		})
	}
}

// TestTypeExpressionAdmitsARealCollection — the floor under the test above: the
// commas and spaces INSIDE a collection's angle brackets are legitimate, and a check
// that refused them would refuse most real schemas.
func TestTypeExpressionAdmitsARealCollection(t *testing.T) {
	for _, good := range []string{"uuid", "map<text, int>", "frozen<list<text>>", "tuple<int, text>"} {
		if err := checkTypeExpr("params.columns.x", good); err != nil {
			t.Errorf("%q must be accepted: %v", good, err)
		}
	}
}

// TestCommandRefusesArgsTheDriverWouldDiscard — ★ the driver binds values only on
// DML and drops them SILENTLY on everything else (cql.go). Passing them through
// would leave the ? markers unbound and the statement a syntax error whose cause
// appears nowhere in the message, so the refusal is the contract.
func TestCommandRefusesArgsTheDriverWouldDiscard(t *testing.T) {
	params := withParams(map[string]any{
		"cql":  "ALTER ROLE app_rw WITH PASSWORD = ?",
		"args": []any{"whatever"},
	})

	reply, _ := newModule(&fakeSession{}).command().Validate(context.Background(),
		&pluginv1.ValidateRequest{State: "run", Params: mustStruct(t, params)})
	if reply.GetOk() {
		t.Error("Validate accepted args on a statement the driver will not bind")
	}

	s := &fakeSession{}
	stream := apply(t, newModule(s).command(), "run", params)
	e := assertOutcome(t, stream, false, true)
	if !strings.Contains(e.GetMessage(), "params.args") {
		t.Errorf("the refusal must name params.args, got %q", e.GetMessage())
	}
	if len(s.queried) != 0 {
		t.Errorf("nothing may be sent after the refusal, got %d statements", len(s.queried))
	}
}

// TestCommandBindsArgsOnDML — and the floor under it: on the statements the driver
// does prepare, args are passed through and the values stay out of the text.
func TestCommandBindsArgsOnDML(t *testing.T) {
	s := &fakeSession{}
	apply(t, newModule(s).command(), "run", withParams(map[string]any{
		"cql":  "SELECT * FROM app.events WHERE id = ?",
		"args": []any{"7f3a"},
	}))

	if len(s.queried) != 1 {
		t.Fatalf("expected one query, got %d", len(s.queried))
	}
	if len(s.queried[0].args) != 1 || s.queried[0].args[0] != "7f3a" {
		t.Errorf("the value must be bound, args = %v", s.queried[0].args)
	}
}

// TestCommandWaitsForAgreementOnlyWhenItMightHaveChangedSchema — a statement the
// driver would not prepare may be DDL, and DDL propagates through gossip. Without
// this, command.run would be the one action of the artifact that reports success
// before the cluster agrees.
func TestCommandWaitsForAgreementOnlyWhenItMightHaveChangedSchema(t *testing.T) {
	ddl := &fakeSession{}
	apply(t, newModule(ddl).command(), "run", withParams(map[string]any{
		"cql": "CREATE INDEX ON app.events (payload)", "changed": true,
	}))
	if ddl.agreed != 1 {
		t.Errorf("a statement that may be DDL must wait for agreement, waited %d times", ddl.agreed)
	}

	dml := &fakeSession{}
	apply(t, newModule(dml).command(), "run", withParams(map[string]any{
		"cql": "SELECT release_version FROM system.local",
	}))
	if dml.agreed != 0 {
		t.Errorf("a SELECT changes no schema and must not wait, waited %d times", dml.agreed)
	}
}

// TestValidateRefusesBrokenTLSBeforeTheRun — ★ NIM-786 on the connection surface.
// [buildTLSConfig] runs inside the driver's connect on the Apply path, so without
// Validate building it too, every one of these refusals would land in the middle of
// a run — which is the phase-defeating shape the rule names.
func TestValidateRefusesBrokenTLSBeforeTheRun(t *testing.T) {
	cases := map[string]map[string]any{
		"half an mTLS pair":  {"tls": true, "tls_cert": "-----BEGIN CERTIFICATE-----"},
		"unparseable CA PEM": {"tls": true, "tls_ca": "this is not a PEM"},
	}
	for name, own := range cases {
		t.Run(name, func(t *testing.T) {
			reply, _ := newModule(&fakeSession{}).instance().Validate(context.Background(),
				&pluginv1.ValidateRequest{State: "pinged", Params: mustStruct(t, withParams(own))})
			if reply.GetOk() {
				t.Error("Validate accepted TLS material the connect path would refuse")
			}
		})
	}
}

// --- gaps a mutation pass found: assertions that were missing, not wrong ---

// TestRepairRequiredOnAStrategyChange — deleting the class-change arm of
// [replicationSpec.raisesOver] left the whole suite green, so the arm was
// unprotected. The two strategies do not share an option vocabulary, so comparing
// key by key across a class change reads an absent `replication_factor` as 0 and
// answers whatever the numbers happen to say.
// The case has to be one where NO factor rises, or the key-by-key comparison
// answers true by accident and the test proves nothing. NetworkTopologyStrategy
// accepts `replication_factor` as a cluster-wide shorthand, so this pair shares its
// only key at the SAME value: key by key there is nothing to see, and the answer
// still has to be "repair owed", because the two strategies place replicas
// differently — NTS is rack- and datacenter-aware, SimpleStrategy walks the ring.
func TestRepairRequiredOnAStrategyChange(t *testing.T) {
	s := &fakeSession{answer: answerSequence(
		keyspaceRows(networkStrategy, map[string]string{replicationFactorKey: "3"}, true),
	)}
	stream := apply(t, newModule(s).keyspace(), "present", withParams(map[string]any{
		"name":        "app",
		"replication": map[string]any{"class": simpleStrategy, replicationFactorKey: 3},
	}))

	e := assertOutcome(t, stream, true, false)
	if !e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("a strategy change relocates replicas and owes a repair, even with every factor unchanged")
	}
	if !strings.Contains(e.GetMessage(), "REPAIR REQUIRED") {
		t.Errorf("the message must carry it too: %q", e.GetMessage())
	}
}

// TestNoRepairWhenNothingRises is the floor under it: a flag that fired on
// everything would pass the test above and teach an operator to ignore it.
func TestNoRepairWhenNothingRises(t *testing.T) {
	s := &fakeSession{answer: answerSequence(
		keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true),
	)}
	stream := apply(t, newModule(s).keyspace(), "present", withParams(map[string]any{
		"name":        "app",
		"replication": map[string]any{"class": networkStrategy, "dc1": 2},
	}))

	e := assertOutcome(t, stream, true, false)
	if e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("lowering a factor owes a cleanup, not a repair")
	}
}

// TestRolePresent_ApplyRefusesAPasswordOnANonLoginRole — the APPLY half of the
// symmetry. Validate refusing alone is not the contract: a runner need not call
// Validate, and the test that covered this called validateRolePresent directly.
func TestRolePresent_ApplyRefusesAPasswordOnANonLoginRole(t *testing.T) {
	s := &fakeSession{}
	stream := apply(t, newModule(s).role(), "present", withParams(map[string]any{
		"name": "grp", "login": false, "role_password": secretRolePass,
	}))

	assertOutcome(t, stream, false, true)
	if len(s.execed) != 0 {
		t.Errorf("nothing may be issued after the refusal, got %q", execedStatements(s))
	}
	assertNoSecretInEvents(t, stream)
}

// TestAbsentNoOpStillWaitsForAgreement — the `absent` half of the no-op wait.
// Deleting both awaitSchema calls on the already-absent path left the suite green:
// the two no-op tests asserted only that nothing was executed.
func TestAbsentNoOpStillWaitsForAgreement(t *testing.T) {
	cases := map[string]struct {
		object *object
		params map[string]any
	}{
		"keyspace": {nil, withParams(map[string]any{"name": "app"})},
		"table":    {nil, withParams(map[string]any{"keyspace": "app", "name": "events"})},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &fakeSession{}
			stream := apply(t, allObjects(newModule(s))[name], "absent", tc.params)

			assertOutcome(t, stream, false, false)
			if len(s.execed) != 0 {
				t.Errorf("dropping an absent subject must issue nothing, got %q", execedStatements(s))
			}
			if s.agreed != 1 {
				t.Errorf("an absent no-op must still confirm schema agreement, waited %d times", s.agreed)
			}
		})
	}
}

// TestIsDMLMirrorsTheDriver pins the boundary of the rule copied out of gocql,
// including the single-token case the driver answers false for — the one direction
// where a divergence is SILENT, because it accepts args that are then discarded.
func TestIsDMLMirrorsTheDriver(t *testing.T) {
	dml := []string{
		"SELECT * FROM t", "insert into t (a) values (?)", "  UPDATE t SET a = 1 ;",
		"DELETE FROM t WHERE a = ?", "BEGIN BATCH ... APPLY BATCH",
	}
	notDML := []string{
		"CREATE TABLE t (a int)", "ALTER ROLE r WITH PASSWORD = 'x'", "DROP KEYSPACE k",
		"GRANT SELECT ON k.t TO r", "REVOKE SELECT ON k.t FROM r", "CREATE INDEX ON t (a)",
		// No interior whitespace: the driver leaves its verb empty here and answers
		// false. Answering true would accept args and then drop them.
		"select", "delete", "insert;", "  update  ",
	}
	for _, stmt := range dml {
		if !isDML(stmt) {
			t.Errorf("%q must be DML (the driver prepares and binds it)", stmt)
		}
	}
	for _, stmt := range notDML {
		if isDML(stmt) {
			t.Errorf("%q must NOT be DML — the driver would discard any bound value", stmt)
		}
	}
}

// answerSequence returns a different scripted answer per Query call, so a test can
// script the state BEFORE a statement and the state AFTER it.
func answerSequence(answers ...[]map[string]any) func(string, []any) ([]map[string]any, error) {
	call := 0
	return func(string, []any) ([]map[string]any, error) {
		i := call
		call++
		if i < len(answers) {
			return answers[i], nil
		}
		return answers[len(answers)-1], nil
	}
}

// TestKeyspacePresent_CreateThatWasASilentNoOpIsStillConverged — ★ the cost of
// IF NOT EXISTS, paid rather than left in.
//
// The converge read asks ONE coordinator. If it lags behind a keyspace the cluster
// already has, the read says absent and the CREATE ... IF NOT EXISTS is a server-side
// no-op. Without the re-read the state would report "created (NTS,dc1=3)" and
// repair_required=false over a keyspace that still carries SimpleStrategy and holds
// data — wrong on both fields, where before the guard it was a loud AlreadyExists.
func TestKeyspacePresent_CreateThatWasASilentNoOpIsStillConverged(t *testing.T) {
	s := &fakeSession{answer: answerSequence(
		nil, // the lagging coordinator: "no such keyspace"
		keyspaceRows(simpleStrategy, map[string]string{"replication_factor": "1"}, true),
	)}
	stream := apply(t, newModule(s).keyspace(), "present", keyspaceParams(nil))

	e := assertOutcome(t, stream, true, false)
	stmts := execedStatements(s)
	if len(stmts) != 2 || !strings.HasPrefix(stmts[1], "ALTER KEYSPACE app ") {
		t.Fatalf("the no-op CREATE must be followed by an ALTER, got %q", stmts)
	}
	if !e.GetOutput().GetFields()["repair_required"].GetBoolValue() {
		t.Error("replication changed on a keyspace holding data — repair is owed and must be reported")
	}
}

// TestTablePresent_CreateThatWasASilentNoOpIsRefused — the same race on `table`,
// where the answer is a refusal rather than an ALTER, because Cassandra cannot
// reshape an existing table.
func TestTablePresent_CreateThatWasASilentNoOpIsRefused(t *testing.T) {
	s := &fakeSession{answer: answerSequence(
		nil, // the lagging coordinator: "no such table"
		[]map[string]any{
			columnRow("id", "uuid", kindPartitionKey, 0),
			columnRow("at", "timestamp", kindClustering, 0),
			columnRow("payload", "int", "regular", 0), // declared text
		},
	)}
	stream := apply(t, newModule(s).table(), "present", tableParams(nil))

	e := assertOutcome(t, stream, false, true)
	if !strings.Contains(e.GetMessage(), "does not match the declaration") {
		t.Errorf("the refusal must name the mismatch, got %q", e.GetMessage())
	}
}

// presentRows is what the object's own system-table read returns when the subject
// EXISTS — the read an `absent` action needs to get past its no-op and reach the
// DROP.
func presentRows(object string) []map[string]any {
	switch object {
	case "keyspace":
		return keyspaceRows(networkStrategy, map[string]string{"dc1": "3"}, true)
	case "role":
		return roleRows(true, false)
	case "table":
		return liveEvents()
	default:
		return nil
	}
}
