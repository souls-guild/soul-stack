package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// newModule builds MongoModule that returns the same fakeConn for any connection
// (for pinged/command single-connect states). Last connection cfg is recorded in
// conn.cfg (checks that password reached the connection).
func newModule(conn *fakeConn) *MongoModule {
	return &MongoModule{
		connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
			conn.cfg = cfg
			return conn, nil
		},
	}
}

// --- Validate ---

func TestValidate_PingedRejectsEmptyAddr(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("expected Ok=false for empty addr (pinged)")
	}
}

func TestValidate_PingedHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("expected Ok=true without errors, got %+v", reply)
	}
}

func TestValidate_UserRejectsEmptyName(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": ""}),
	})
	if reply.Ok {
		t.Fatal("expected Ok=false for empty name (user)")
	}
}

func TestValidate_UserRejectsUnknownState(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": "alice", "state": "weird"}),
	})
	if reply.Ok {
		t.Fatal("expected Ok=false for unknown user state")
	}
}

func TestValidate_UserHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": "alice", "state": "present"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("expected Ok=true without errors, got %+v", reply)
	}
}

func TestValidate_CommandRejectsEmptyCommand(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "command": map[string]any{}}),
	})
	if reply.Ok {
		t.Fatal("expected Ok=false for empty command")
	}
}

func TestValidate_CommandHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "command": map[string]any{"ping": 1}}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("expected Ok=true without errors, got %+v", reply)
	}
}

func TestValidate_RejectsUnknownState(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replicaset",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	})
	if reply.Ok {
		t.Fatal("expected Ok=false for unimplemented state")
	}
}

// --- Apply: pinged ---

// TestApplyPinged_HappyPath_ChangedFalse — Ping ok → Output.ok=true, changed=false
// CONSTRUCTIVELY (probe semantics).
func TestApplyPinged_HappyPath_ChangedFalse(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"username": "default_admin",
			"password": secretPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected successful final event, got %+v", fin)
	}
	if fin.Changed {
		t.Error("pinged: changed must be false constructively (probe, not a change)")
	}
	if !fin.GetOutput().GetFields()["ok"].GetBoolValue() {
		t.Error("Output.ok=false, expected true")
	}
	if !conn.pinged {
		t.Error("Ping not called")
	}
	// Password went into connection, but NOT into events.
	if conn.cfg.password != secretPass {
		t.Errorf("password did not reach connection: %q", conn.cfg.password)
	}
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("connection not closed")
	}
}

// TestApplyPinged_PingErrorIsFailure - Ping with error -> failed=true (health-gate
// sees failure through failed_when/until).
func TestApplyPinged_PingErrorIsFailure(t *testing.T) {
	conn := &fakeConn{pingErr: errors.New("server selection timeout")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true on Ping error, got %+v", fin)
	}
}

// TestApplyPinged_ConnectFailure_DoesNotLeakPassword - connection error with
// password in text is sanitized (redactError), security invariant ADR-010.
func TestApplyPinged_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &MongoModule{
		connect: func(_ context.Context, cfg connConfig) (mongoConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyPinged_PingErrorRedactsPassword - error from Ping itself (driver echoed
// connection password) is redacted through redactError by password.
func TestApplyPinged_PingErrorRedactsPassword(t *testing.T) {
	conn := &fakeConn{pingErr: errors.New("context for AUTH " + secretPass)}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("password leaked into Ping error Message: %q", fin.GetMessage())
	}
}

// --- Apply: command ---

// TestApplyCommand_HappyPath_ChangedFalseByDefault - raw command -> Output.ok=true,
// changed=false by default (probe semantics). Default db is admin.
func TestApplyCommand_HappyPath_ChangedFalseByDefault(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:27017",
			"password": secretPass,
			"command":  map[string]any{"serverStatus": 1},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("expected success, got %+v", fin)
	}
	if fin.Changed {
		t.Error("command: changed=false by default (probe)")
	}
	if !hasCommand(conn.calls, "serverStatus") {
		t.Errorf("expected serverStatus call, got %v", conn.calls)
	}
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("connection not closed")
	}
}

// TestApplyCommand_ChangedTrueWhenRequested - changed=true when params has changed:true.
func TestApplyCommand_ChangedTrueWhenRequested(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:27017",
			"command": map[string]any{"fsync": 1},
			"changed": true,
		}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Changed {
		t.Fatalf("expected changed=true when params has changed:true, got %+v", fin)
	}
}

// TestApplyCommand_TargetsRequestedDB - db from params is used as runCommand
// target DB (not always admin).
func TestApplyCommand_TargetsRequestedDB(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:27017",
			"db":      "appdb",
			"command": map[string]any{"collStats": "events"},
		}),
	}, stream)

	if len(conn.calls) == 0 || conn.calls[0].db != "appdb" {
		t.Fatalf("expected runCommand in DB appdb, got %+v", conn.calls)
	}
}

// TestApplyCommand_ErrorIsFailure - server command error -> failed=true.
func TestApplyCommand_ErrorIsFailure(t *testing.T) {
	conn := &fakeConn{cmdErrByName: map[string]error{"serverStatus": errors.New("not authorized on admin")}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:27017",
			"command": map[string]any{"serverStatus": 1},
		}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("expected failed=true on command error, got %+v", fin)
	}
}
