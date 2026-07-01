package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// newModule собирает MongoModule, отдающий один и тот же fakeConn на любой
// коннект (для pinged/command — single-connect states). cfg последнего коннекта
// фиксируется в conn.cfg (проверка, что пароль доехал до коннекта).
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
		t.Fatal("ждали Ok=false на пустой addr (pinged)")
	}
}

func TestValidate_PingedHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_UserRejectsEmptyName(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустое name (user)")
	}
}

func TestValidate_UserRejectsUnknownState(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": "alice", "state": "weird"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на неизвестный state юзера")
	}
}

func TestValidate_UserHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "user",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "name": "alice", "state": "present"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_CommandRejectsEmptyCommand(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "command": map[string]any{}}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой command")
	}
}

func TestValidate_CommandHappyPath(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017", "command": map[string]any{"ping": 1}}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_RejectsUnknownState(t *testing.T) {
	m := &MongoModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replicaset",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на нереализованный state")
	}
}

// --- Apply: pinged ---

// TestApplyPinged_HappyPath_ChangedFalse — Ping ok → Output.ok=true, changed=false
// КОНСТРУКТИВНО (probe-семантика).
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
		t.Fatalf("ждали успешное финальное событие, got %+v", fin)
	}
	if fin.Changed {
		t.Error("pinged: changed обязан быть false конструктивно (probe, не изменение)")
	}
	if !fin.GetOutput().GetFields()["ok"].GetBoolValue() {
		t.Error("Output.ok=false, ждали true")
	}
	if !conn.pinged {
		t.Error("Ping не вызван")
	}
	// Пароль ушёл в коннект, но НЕ в события.
	if conn.cfg.password != secretPass {
		t.Errorf("пароль не доехал до коннекта: %q", conn.cfg.password)
	}
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyPinged_PingErrorIsFailure — Ping с ошибкой → failed=true (health-gate
// увидит провал через failed_when/until).
func TestApplyPinged_PingErrorIsFailure(t *testing.T) {
	conn := &fakeConn{pingErr: errors.New("server selection timeout")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:27017"}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку Ping, got %+v", fin)
	}
}

// TestApplyPinged_ConnectFailure_DoesNotLeakPassword — коннект-ошибка с паролем в
// тексте санитизируется (redactError) — ИБ-инвариант ADR-010.
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
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyPinged_PingErrorRedactsPassword — ошибка на самом Ping (драйвер эхнул
// пароль из коннекта) редактируется через redactError по password.
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
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("пароль утёк в Message ошибки Ping: %q", fin.GetMessage())
	}
}

// --- Apply: command ---

// TestApplyCommand_HappyPath_ChangedFalseByDefault — raw-команда → Output.ok=true,
// changed=false по умолчанию (probe-семантика). db дефолт admin.
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
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("command: changed=false по умолчанию (probe)")
	}
	if !hasCommand(conn.calls, "serverStatus") {
		t.Errorf("ждали вызов serverStatus, got %v", conn.calls)
	}
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyCommand_ChangedTrueWhenRequested — changed=true при changed:true в params.
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
		t.Fatalf("ждали changed=true при changed:true в params, got %+v", fin)
	}
}

// TestApplyCommand_TargetsRequestedDB — db из params используется как целевая БД
// runCommand (не всегда admin).
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
		t.Fatalf("ждали runCommand в БД appdb, got %+v", conn.calls)
	}
}

// TestApplyCommand_ErrorIsFailure — ошибка команды сервера → failed=true.
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
		t.Fatalf("ждали failed=true на ошибку команды, got %+v", fin)
	}
}
