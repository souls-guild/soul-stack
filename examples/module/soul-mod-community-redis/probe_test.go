package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// --- Validate: pinged / role требуют только addr ---

func TestValidate_PingedRejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr (pinged)")
	}
}

func TestValidate_PingedHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_RoleRejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "role",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr (role)")
	}
}

func TestValidate_RoleHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "role",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_ReplicaSyncedRejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replica-synced",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr (replica-synced)")
	}
}

func TestValidate_ReplicaSyncedHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replica-synced",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

// --- Apply: pinged ---

// TestApplyPinged_HappyPath_PongChangedFalse — PING → Output.result == PONG,
// changed=false КОНСТРУКТИВНО (probe-семантика). Поле result совместимо с прежним
// community.redis.command args:[PING] (register.self.result в health-gate).
func TestApplyPinged_HappyPath_PongChangedFalse(t *testing.T) {
	conn := &fakeConn{results: map[string]string{"PING": "PONG"}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
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
	if got := fin.GetOutput().GetFields()["result"].GetStringValue(); got != "PONG" {
		t.Errorf("Output.result=%q, ждали PONG (совместимость с register.self.result)", got)
	}
	// Пароль ушёл в коннект, но НЕ в аргументы PING.
	if conn.cfg.password != secretPass {
		t.Errorf("пароль не доехал до коннекта: %q", conn.cfg.password)
	}
	assertNoCommandCarriesSecret(t, conn)
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyPinged_RedisErrorIsFailure — PING с ошибкой сервера (LOADING и т.п.)
// → failed=true (health-gate увидит провал через failed_when/until).
func TestApplyPinged_RedisErrorIsFailure(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("LOADING Redis is loading the dataset in memory")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "pinged",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку PING, got %+v", fin)
	}
}

// TestApplyPinged_ConnectFailure_DoesNotLeakPassword — коннект-ошибка, чей текст
// содержит пароль, санитизируется (redactError) — ИБ-инвариант ADR-010.
func TestApplyPinged_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// --- Apply: role ---

// TestApplyRole_Master — INFO replication role:master → Output.role == master,
// changed=false. Доказывает where-таргетинг master-ветки rolling-restart.
func TestApplyRole_Master(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nrole:master\r\nconnected_slaves:2\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "role",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
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
		t.Error("role: changed обязан быть false конструктивно (probe)")
	}
	if got := fin.GetOutput().GetFields()["role"].GetStringValue(); got != "master" {
		t.Errorf("Output.role=%q, ждали master", got)
	}
	assertNoCommandCarriesSecret(t, conn)
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyRole_Slave — INFO replication role:slave → Output.role == slave
// (значение Redis; redis-cli role-shell отдавал то же). Доказывает slave-ветку.
func TestApplyRole_Slave(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nrole:slave\r\nmaster_host:10.0.0.1\r\nmaster_link_status:up\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "role",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["role"].GetStringValue(); got != "slave" {
		t.Errorf("Output.role=%q, ждали slave", got)
	}
	// INFO replication действительно вызван (а не что-то иное).
	if !hasCall(conn.calls, "INFO", "replication") {
		t.Errorf("ждали вызов INFO replication, got %v", conn.calls)
	}
}

// TestApplyRole_MissingRoleField — нештатный INFO (без поля role) → failed, а не
// пустая роль в where (иначе молчаливый пропуск рестарта на этом хосте).
func TestApplyRole_MissingRoleField(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nconnected_slaves:0\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "role",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	if fin := stream.final(); fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на INFO без поля role, got %+v", fin)
	}
}

// TestApplyRole_ConnectFailure_DoesNotLeakPassword — коннект-ошибка с паролем в
// тексте санитизируется (ИБ-инвариант ADR-010).
func TestApplyRole_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "role",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyRole_InfoErrorRedactsPassword — ошибка на самой INFO replication
// (драйвер эхнул пароль из коннекта) редактируется через redactError по password.
func TestApplyRole_InfoErrorRedactsPassword(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("READONLY context for AUTH " + secretPass)}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "role",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("пароль утёк в Message ошибки INFO replication: %q", fin.GetMessage())
	}
}

// --- Apply: replica-synced ---

// TestApplyReplicaSynced_LinkUp — master_link_status:up → synced=true,
// changed=false. Строгий ресинк-гейт реплики (реплика догнала master-а после
// рестарта); until: register.self.synced == true проходит.
func TestApplyReplicaSynced_LinkUp(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nrole:slave\r\nmaster_host:10.0.0.1\r\nmaster_link_status:up\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica-synced",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
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
		t.Error("replica-synced: changed обязан быть false конструктивно (probe)")
	}
	if !fin.GetOutput().GetFields()["synced"].GetBoolValue() {
		t.Errorf("Output.synced=%v, ждали true (master_link_status:up)", fin.GetOutput().GetFields()["synced"])
	}
	if got := fin.GetOutput().GetFields()["master_link_status"].GetStringValue(); got != "up" {
		t.Errorf("Output.master_link_status=%q, ждали up", got)
	}
	assertNoCommandCarriesSecret(t, conn)
	assertEventsNoSecret(t, stream)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyReplicaSynced_LinkDown — master_link_status:down → synced=false (но
// success-event, не failed: health-gate сам решает через until/failed_when по
// register.self.synced). Реплика ещё не догнала master-а.
func TestApplyReplicaSynced_LinkDown(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nrole:slave\r\nmaster_host:10.0.0.1\r\nmaster_link_status:down\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "replica-synced",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали success-event (synced=false, не failed), got %+v", fin)
	}
	if fin.GetOutput().GetFields()["synced"].GetBoolValue() {
		t.Error("Output.synced=true, ждали false (master_link_status:down)")
	}
	if got := fin.GetOutput().GetFields()["master_link_status"].GetStringValue(); got != "down" {
		t.Errorf("Output.master_link_status=%q, ждали down", got)
	}
}

// TestApplyReplicaSynced_MasterHasNoLinkField — у master-а поля master_link_status
// НЕТ (граница master/slave): synced=false с ПОНЯТНОЙ причиной в Message (НЕ тихий
// success). State предназначен для slave-пути — health-gate реплики не должен
// пройти на master-е.
func TestApplyReplicaSynced_MasterHasNoLinkField(t *testing.T) {
	conn := &fakeConn{results: map[string]string{
		"INFO": "# Replication\r\nrole:master\r\nconnected_slaves:2\r\n",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State:  "replica-synced",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil {
		t.Fatal("ждали финальное событие")
	}
	if fin.GetOutput().GetFields()["synced"].GetBoolValue() {
		t.Error("★ Output.synced=true на master-е (нет master_link_status) — должно быть false")
	}
	// Причина обязана быть явной (НЕ тихий success): Message упоминает отсутствие поля.
	if !strings.Contains(fin.GetMessage(), "master_link_status") {
		t.Errorf("★ ждали понятную причину в Message (нет master_link_status), got %q", fin.GetMessage())
	}
}

// TestApplyReplicaSynced_ConnectFailure_DoesNotLeakPassword — коннект-ошибка с
// паролем в тексте санитизируется (ИБ-инвариант ADR-010).
func TestApplyReplicaSynced_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica-synced",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// TestApplyReplicaSynced_InfoErrorRedactsPassword — ошибка на самой INFO
// replication (драйвер эхнул пароль из коннекта) редактируется redactError.
func TestApplyReplicaSynced_InfoErrorRedactsPassword(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("READONLY context for AUTH " + secretPass)}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica-synced",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("пароль утёк в Message ошибки INFO replication: %q", fin.GetMessage())
	}
}

// TestApplyPinged_RedisErrorRedactsPassword — nit #2: applyPinged на ошибке PING
// редактирует пароль через redactError (defense-in-depth/единообразие с role/config).
func TestApplyPinged_RedisErrorRedactsPassword(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("ERR context for AUTH " + secretPass)}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "pinged",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("пароль утёк в Message ошибки PING: %q", fin.GetMessage())
	}
}
