package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// secretPass is the connection password that must NEVER leak into events, errors or
// stderr (security invariant ADR-010). Long and unique so a substring search over an
// event is reliable.
const secretPass = "vault-resolved-supersecret-9f3a7c1e2b"

// secretRolePass is the password OF A MANAGED ROLE. Unlike [secretPass] it is
// allowed in ONE statement — the CREATE ROLE that sets it — because the driver binds
// no values on a non-DML statement and a literal is the only thing that works
// (cql.go). Everywhere else, and in every event, it is a leak.
const secretRolePass = "vault-resolved-rolepass-4d8e6a0b1c"

// stmtCall records one statement and the values bound to it, so a test can prove
// both what was run and what was NOT put into the text of it.
type stmtCall struct {
	stmt string
	args []any
}

// fakeSession is an in-memory [cassSession]: it records every statement and returns
// scripted rows, which is what lets the converge branches, the schema-agreement wait
// and the secret invariants all be proven without a live cluster.
type fakeSession struct {
	cfg connConfig

	// answer returns the rows for a Query. nil means "no rows, no error", which is
	// how a test spells an absent keyspace, role or table.
	answer func(stmt string, args []any) ([]map[string]any, error)
	// execErr fails every Exec — a server refusing the DDL.
	execErr error
	// agreeErr fails the schema-agreement wait — the cluster accepting a change and
	// not converging on it.
	agreeErr error

	queried []stmtCall
	execed  []stmtCall
	agreed  int
	closed  bool
}

func (f *fakeSession) Query(_ context.Context, stmt string, args ...any) ([]map[string]any, error) {
	f.queried = append(f.queried, stmtCall{stmt: stmt, args: args})
	if f.answer == nil {
		return nil, nil
	}
	return f.answer(stmt, args)
}

func (f *fakeSession) Exec(_ context.Context, stmt string, args ...any) error {
	f.execed = append(f.execed, stmtCall{stmt: stmt, args: args})
	return f.execErr
}

func (f *fakeSession) AwaitSchemaAgreement(_ context.Context) error {
	f.agreed++
	return f.agreeErr
}

func (f *fakeSession) Close() { f.closed = true }

// newModule builds a CassandraModule handing out the same fake session, and records
// the connection config on it so a test can prove the password reached the
// connection and nothing else.
func newModule(s *fakeSession) *CassandraModule {
	return &CassandraModule{
		connect: func(_ context.Context, cfg connConfig) (cassSession, error) {
			s.cfg = cfg
			return s, nil
		},
	}
}

// applyStream collects the events an action sends.
type applyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	sent []*pluginv1.ApplyEvent
}

func (s *applyStream) Send(e *pluginv1.ApplyEvent) error { s.sent = append(s.sent, e); return nil }
func (s *applyStream) Context() context.Context          { return context.Background() }

func (s *applyStream) final() *pluginv1.ApplyEvent {
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[len(s.sent)-1]
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// apply runs one action of one object end to end through the real dispatcher, so a
// test exercises the param-type check and the connect path rather than calling the
// applyXxx method directly.
func apply(t *testing.T, obj *object, state string, params map[string]any) *applyStream {
	t.Helper()
	stream := &applyStream{}
	if err := obj.Apply(&pluginv1.ApplyRequest{State: state, Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply returned a transport error: %v", err)
	}
	if stream.final() == nil {
		t.Fatal("Apply sent no final event")
	}
	return stream
}

// assertOutcome checks the final event against what the action promised.
func assertOutcome(t *testing.T, s *applyStream, wantChanged, wantFailed bool) *pluginv1.ApplyEvent {
	t.Helper()
	e := s.final()
	if e.GetFailed() != wantFailed {
		t.Fatalf("failed = %t, want %t (message: %q)", e.GetFailed(), wantFailed, e.GetMessage())
	}
	if e.GetChanged() != wantChanged {
		t.Errorf("changed = %t, want %t (message: %q)", e.GetChanged(), wantChanged, e.GetMessage())
	}
	return e
}

// assertNoSecretInEvents is the ADR-010 guard: neither password may appear in any
// event's message or output.
func assertNoSecretInEvents(t *testing.T, s *applyStream) {
	t.Helper()
	for _, e := range s.sent {
		for _, secret := range []string{secretPass, secretRolePass} {
			if strings.Contains(e.GetMessage(), secret) {
				t.Errorf("secret leaked into event Message: %q", e.GetMessage())
			}
			if e.GetOutput() != nil && strings.Contains(e.GetOutput().String(), secret) {
				t.Errorf("secret leaked into event Output: %q", e.GetOutput().String())
			}
		}
	}
}

// assertNoSecretInStatementText is the guard that keeps a credential out of a
// statement STRING, which can reach a server-side query log or come back inside a
// syntax error.
//
// The CONNECTION password has no exception anywhere: it belongs in the credentials
// and there is no statement that needs it. The ROLE password has exactly one — the
// CREATE ROLE that sets it — and the exception is written here, narrowly, rather
// than by dropping the check on that action: if the literal ever appears in an
// ALTER, a DROP or a system_auth read, this still fails.
func assertNoSecretInStatementText(t *testing.T, s *fakeSession) {
	t.Helper()
	for _, call := range append(append([]stmtCall{}, s.queried...), s.execed...) {
		if strings.Contains(call.stmt, secretPass) {
			t.Errorf("the CONNECTION password leaked into statement text: %q", call.stmt)
		}
		if strings.Contains(call.stmt, secretRolePass) && !strings.HasPrefix(call.stmt, "CREATE ROLE ") {
			t.Errorf("the role password appeared outside a CREATE ROLE: %q", call.stmt)
		}
	}
}

// connParams is the connection surface every action needs, with the password that
// must not leak.
func connParams() map[string]any {
	return map[string]any{
		"hosts":    []any{"10.0.0.1", "10.0.0.2:9042"},
		"username": "cassandra",
		"password": secretPass,
	}
}

// withParams merges an action's own params onto [connParams].
func withParams(own map[string]any) map[string]any {
	out := connParams()
	for k, v := range own {
		out[k] = v
	}
	return out
}

// execedStatements is the statement text of every Exec, for asserting which DDL ran.
func execedStatements(s *fakeSession) []string {
	out := make([]string, 0, len(s.execed))
	for _, call := range s.execed {
		out = append(out, call.stmt)
	}
	return out
}

// --- scripted rows ---

// keyspaceRows is what system_schema.keyspaces returns for an existing keyspace.
func keyspaceRows(class string, factors map[string]string, durable bool) []map[string]any {
	replication := map[string]string{"class": classPrefix + class}
	for k, v := range factors {
		replication[k] = v
	}
	return []map[string]any{{"durable_writes": durable, "replication": replication}}
}

// roleRows is what system_auth.roles returns for an existing role.
func roleRows(login, superuser bool) []map[string]any {
	return []map[string]any{{"can_login": login, "is_superuser": superuser}}
}

// columnRow is one row of system_schema.columns.
func columnRow(name, cqlType, kind string, position int) map[string]any {
	return map[string]any{"column_name": name, "type": cqlType, "kind": kind, "position": position}
}
