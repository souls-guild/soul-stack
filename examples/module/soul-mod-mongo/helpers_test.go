package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"go.mongodb.org/mongo-driver/bson"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// secretPass is a password that must NEVER leak into events/errors/stderr
// (security invariant ADR-010). Long/unique so substring search is reliable.
const secretPass = "vault-resolved-supersecret-9f3a7c1e2b"

// cmdCall records a RunCommand call (db + command document) to check command
// order/presence and that password did not leak into arguments.
type cmdCall struct {
	db  string
	cmd bson.D
}

// fakeConn is an in-memory mongoConn: records every call and returns scripted
// responses. Lets tests prove localhost-exception fallback, idempotency, and
// secret invariants without a live mongod.
type fakeConn struct {
	cfg    connConfig
	calls  []cmdCall
	pinged bool
	closed bool

	pingErr error
	// cmdErrByName is the error for a command with this first key (usersInfo/createUser/...).
	cmdErrByName map[string]error
	// rawByName is the raw response for a command with this first key.
	rawByName map[string]bson.Raw
}

func (f *fakeConn) Ping(_ context.Context) error {
	f.pinged = true
	return f.pingErr
}

func (f *fakeConn) RunCommand(_ context.Context, db string, cmd bson.D) (bson.Raw, error) {
	f.calls = append(f.calls, cmdCall{db: db, cmd: cmd})
	name := ""
	if len(cmd) > 0 {
		name = cmd[0].Key
	}
	if f.cmdErrByName != nil {
		if err, ok := f.cmdErrByName[name]; ok {
			return nil, err
		}
	}
	if f.rawByName != nil {
		if raw, ok := f.rawByName[name]; ok {
			return raw, nil
		}
	}
	// Default is successful {ok: 1}.
	return okRaw(), nil
}

func (f *fakeConn) Close(_ context.Context) error { f.closed = true; return nil }

// okRaw is bson response {ok: 1} (successful command).
func okRaw() bson.Raw {
	b, _ := bson.Marshal(bson.D{{Key: "ok", Value: int32(1)}})
	return b
}

// usersRaw is usersInfo response with users array of given length (n>0 -> user exists).
func usersRaw(n int) bson.Raw {
	arr := bson.A{}
	for i := 0; i < n; i++ {
		arr = append(arr, bson.D{{Key: "user", Value: "u"}})
	}
	b, _ := bson.Marshal(bson.D{{Key: "users", Value: arr}, {Key: "ok", Value: int32(1)}})
	return b
}

// applyStream is a local fake grpc-stream (parity with sdk fakeApplyStream).
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

// hasCommand reports whether RunCommand was called with first key name (in any DB).
func hasCommand(calls []cmdCall, name string) bool {
	for _, c := range calls {
		if len(c.cmd) > 0 && c.cmd[0].Key == name {
			return true
		}
	}
	return false
}

// commandValue is the first field value of command name (for example user name in createUser).
func commandValue(calls []cmdCall, name string) (any, bool) {
	for _, c := range calls {
		if len(c.cmd) > 0 && c.cmd[0].Key == name {
			return c.cmd[0].Value, true
		}
	}
	return nil, false
}

// commandField is field value of command with first key cmdName (for example pwd
// in createUser document). Used to check pwd = password of the CREATED user.
func commandField(calls []cmdCall, cmdName, field string) (any, bool) {
	for _, c := range calls {
		if len(c.cmd) == 0 || c.cmd[0].Key != cmdName {
			continue
		}
		for _, e := range c.cmd {
			if e.Key == field {
				return e.Value, true
			}
		}
	}
	return nil, false
}

// assertEventsNoSecret verifies no event (Message/serialized Output) contains
// password (security invariant ADR-010).
func assertEventsNoSecret(t *testing.T, s *applyStream) {
	t.Helper()
	for _, e := range s.sent {
		if strings.Contains(e.GetMessage(), secretPass) {
			t.Errorf("password leaked into event Message: %q", e.GetMessage())
		}
		if e.GetOutput() != nil {
			if strings.Contains(e.GetOutput().String(), secretPass) {
				t.Errorf("password leaked into event Output: %q", e.GetOutput().String())
			}
		}
	}
}

// commandCarriesSecretExcept checks whether password appears in ARGUMENTS of any
// command EXCEPT createUser (where pwd is a legitimate part of mongo contract, but
// goes over the wire, NOT into events). Used to check "password does not leak into
// unexpected places".
func commandCarriesSecretOutsideCreateUser(calls []cmdCall, secret string) bool {
	for _, c := range calls {
		name := ""
		if len(c.cmd) > 0 {
			name = c.cmd[0].Key
		}
		if name == "createUser" {
			continue // pwd is expected here (createUser contract)
		}
		for _, e := range c.cmd {
			if s, ok := e.Value.(string); ok && strings.Contains(s, secret) {
				return true
			}
		}
	}
	return false
}
