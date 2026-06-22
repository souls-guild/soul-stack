package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// secretPass — пароль, который НИКОГДА не должен утечь в события/ошибки/stderr
// (ИБ-инвариант ADR-010). Длинный/уникальный, чтобы поиск подстроки был надёжен.
const secretPass = "vault-resolved-supersecret-9f3a7c1e2b"

// fakeConn — in-memory redisConn: пишет каждый вызов Do, отдаёт скриптованные
// ответы. seenPassword фиксируем, чтобы доказать: пароль уходит в коннект, но НЕ
// в аргументы команд.
type fakeConn struct {
	cfg     connConfig
	calls   [][]any
	results map[string]string // ключ — args[0]; "" → echo "OK"
	doErr   error
	closed  bool
}

func (f *fakeConn) Do(_ context.Context, args ...any) (string, error) {
	f.calls = append(f.calls, args)
	if f.doErr != nil {
		return "", f.doErr
	}
	if len(args) == 0 {
		return "", nil
	}
	verb, _ := args[0].(string)
	if r, ok := f.results[verb]; ok {
		return r, nil
	}
	return "OK", nil
}

func (f *fakeConn) Close() error { f.closed = true; return nil }

// newModule собирает RedisModule с инъектированным fakeConn и возвращает оба.
func newModule(conn *fakeConn) *RedisModule {
	return &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			conn.cfg = cfg
			return conn, nil
		},
	}
}

// applyStream — локальный fake grpc-stream (паритет с sdk fakeApplyStream).
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

// --- Validate ---

func TestValidate_CommandRejectsEmptyArgs(t *testing.T) {
	m := &RedisModule{}
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "args": []any{}}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой args")
	}
}

func TestValidate_ConfigRejectsEmptyMap(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "config",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "config": map[string]any{}}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой config")
	}
}

func TestValidate_RejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "", "args": []any{"PING"}}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr")
	}
}

func TestValidate_RejectsUnknownState(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "failover",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на нереализованный state")
	}
}

func TestValidate_HappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "args": []any{"PING"}}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

// --- Apply: command ---

func TestApplyCommand_HappyPath_ChangedFalseByDefault(t *testing.T) {
	conn := &fakeConn{results: map[string]string{"PING": "PONG"}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
			"args":     []any{"PING"},
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
		t.Error("changed должен быть false по умолчанию (probe-семантика)")
	}
	if got := fin.GetOutput().GetFields()["result"].GetStringValue(); got != "PONG" {
		t.Errorf("result=%q, ждали PONG", got)
	}
	// Пароль ушёл в коннект, но НЕ в аргументы команды.
	if conn.cfg.password != secretPass {
		t.Errorf("пароль не доехал до коннекта: %q", conn.cfg.password)
	}
	assertNoCommandCarriesSecret(t, conn)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

func TestApplyCommand_ChangedTrueWhenRequested(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:6379",
			"args":    []any{"FLUSHDB"},
			"changed": true,
		}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Changed {
		t.Fatalf("ждали changed=true при changed:true в params, got %+v", fin)
	}
}

func TestApplyCommand_RedisErrorIsFailure(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("WRONGPASS invalid username-password pair")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "args": []any{"PING"}}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку Redis, got %+v", fin)
	}
}

// --- Apply: config ---

func TestApplyConfig_HappyPath_ChangedTrue(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
			"config": map[string]any{
				"maxmemory":        "256mb",
				"maxmemory-policy": "allkeys-lru",
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	// Две директивы → два CONFIG SET (детерминированный порядок по ключу).
	wantSets := [][]any{
		{"CONFIG", "SET", "maxmemory", "256mb"},
		{"CONFIG", "SET", "maxmemory-policy", "allkeys-lru"},
	}
	assertCalls(t, conn, wantSets)
	assertNoCommandCarriesSecret(t, conn)
}

func TestApplyConfig_NumericValueStringified(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":   "127.0.0.1:6379",
			"config": map[string]any{"maxclients": 20000},
		}),
	}, stream)

	if fin := stream.final(); fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if len(conn.calls) != 1 || conn.calls[0][3] != "20000" {
		t.Errorf("ждали CONFIG SET maxclients 20000 (стрингифицировано), got %v", conn.calls)
	}
}

func TestApplyConfig_RewriteCallsConfigRewrite(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:6379",
			"config":  map[string]any{"maxmemory": "512mb"},
			"rewrite": true,
		}),
	}, stream)

	last := conn.calls[len(conn.calls)-1]
	if len(last) != 2 || last[0] != "CONFIG" || last[1] != "REWRITE" {
		t.Errorf("ждали финальный CONFIG REWRITE, got %v", last)
	}
}

// --- Apply: unix-socket addr ---

func TestApply_UnixSocketAddrParsed(t *testing.T) {
	var captured connConfig
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			captured = cfg
			return &fakeConn{}, nil
		},
	}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "unix:/var/run/redis/redis-server.sock", "args": []any{"PING"}}),
	}, &applyStream{})

	if captured.addr != "unix:/var/run/redis/redis-server.sock" {
		t.Errorf("addr не доехал до коннекта: %q", captured.addr)
	}
}

// --- ИБ-инвариант: пароль не утекает ---

// TestApply_ConnectFailure_DoesNotLeakPassword — ошибка коннекта, чей текст
// СОДЕРЖИТ пароль, должна быть санитизирована в событии (redactError).
func TestApply_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			// Моделируем worst-case: драйвер положил пароль в текст ошибки.
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
			"args":     []any{"PING"},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApply_NoEventCarriesSecret — сквозная проверка: ни одно событие
// (command + config happy-path) не содержит пароль ни в Message, ни в Output.
func TestApply_NoEventCarriesSecret(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  string
		params map[string]any
	}{
		{"command", "command", map[string]any{"addr": "127.0.0.1:6379", "password": secretPass, "args": []any{"GET", "key"}}},
		{"config", "config", map[string]any{"addr": "127.0.0.1:6379", "password": secretPass, "config": map[string]any{"maxmemory": "256mb"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &fakeConn{}
			m := newModule(conn)
			stream := &applyStream{}
			_ = m.Apply(&pluginv1.ApplyRequest{State: tc.state, Params: mustStruct(t, tc.params)}, stream)
			assertEventsNoSecret(t, stream)
			assertNoCommandCarriesSecret(t, conn)
		})
	}
}

// --- assert-хелперы ---

func assertEventsNoSecret(t *testing.T, stream *applyStream) {
	t.Helper()
	for i, ev := range stream.sent {
		if strings.Contains(ev.GetMessage(), secretPass) {
			t.Errorf("событие[%d].Message содержит пароль: %q", i, ev.GetMessage())
		}
		if out := ev.GetOutput(); out != nil {
			for k, v := range out.GetFields() {
				if strings.Contains(v.GetStringValue(), secretPass) {
					t.Errorf("событие[%d].Output[%s] содержит пароль", i, k)
				}
			}
		}
	}
}

func assertNoCommandCarriesSecret(t *testing.T, conn *fakeConn) {
	t.Helper()
	for i, call := range conn.calls {
		for _, a := range call {
			if s, ok := a.(string); ok && strings.Contains(s, secretPass) {
				t.Errorf("команда[%d] несёт пароль в аргументах: %v", i, call)
			}
		}
	}
}

func assertCalls(t *testing.T, conn *fakeConn, want [][]any) {
	t.Helper()
	if len(conn.calls) != len(want) {
		t.Fatalf("вызовов %d, ждали %d: %v", len(conn.calls), len(want), conn.calls)
	}
	for i := range want {
		if len(conn.calls[i]) != len(want[i]) {
			t.Fatalf("вызов[%d] арность %d, ждали %d: %v", i, len(conn.calls[i]), len(want[i]), conn.calls[i])
		}
		for j := range want[i] {
			if conn.calls[i][j] != want[i][j] {
				t.Errorf("вызов[%d][%d]=%v, ждали %v", i, j, conn.calls[i][j], want[i][j])
			}
		}
	}
}
