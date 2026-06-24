package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// replConn — fake redisConn для replica-тестов: отвечает на INFO replication
// заданной секцией, пишет каждый вызов (для assert REPLICAOF/CONFIG SET).
type replConn struct {
	cfg        connConfig
	infoReply  string // ответ на INFO replication
	calls      [][]any
	closed     bool
	failOnVerb string // если непусто — Do возвращает ошибку на этот verb (args[0])
}

func (c *replConn) Do(_ context.Context, args ...any) (string, error) {
	c.calls = append(c.calls, args)
	verb, _ := args[0].(string)
	if c.failOnVerb != "" && strings.EqualFold(verb, c.failOnVerb) {
		return "", errors.New("redis error on " + verb)
	}
	if strings.EqualFold(verb, "INFO") {
		return c.infoReply, nil
	}
	return "OK", nil
}

// ConfigGet — replica-state не вызывает CONFIG GET (диагностика через INFO),
// поэтому стаб для удовлетворения интерфейса redisConn.
func (c *replConn) ConfigGet(_ context.Context, param string) (map[string]string, error) {
	return map[string]string{param: ""}, nil
}

// GetKeysInSlot — replica-state слоты не мигрирует, стаб под интерфейс redisConn.
func (c *replConn) GetKeysInSlot(_ context.Context, _, _ int) ([]string, error) {
	return nil, nil
}

func (c *replConn) Close() error { c.closed = true; return nil }

func replModule(conn *replConn) *RedisModule {
	return &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			conn.cfg = cfg
			return conn, nil
		},
	}
}

// hasCall — был ли вызов с заданными первыми токенами (префикс-матч).
func hasCall(calls [][]any, want ...string) bool {
	for _, call := range calls {
		if len(call) < len(want) {
			continue
		}
		ok := true
		for i, w := range want {
			s, _ := call[i].(string)
			if !strings.EqualFold(s, w) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// --- Validate: replica ---

func TestValidate_ReplicaRequiresMasterAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replica",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false без master_addr")
	}
}

func TestValidate_ReplicaHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "replica",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "master_addr": "10.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply replica: REPLICAOF + masterauth ---

func TestApplyReplica_SetsReplicaofAndMasterauth(t *testing.T) {
	// INFO: инстанс пока master (не реплицирует) → нужно настроить.
	conn := &replConn{infoReply: "# Replication\r\nrole:master\r\nconnected_slaves:0\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	// masterauth ДО REPLICAOF, REPLICAOF c host port.
	if !hasCall(conn.calls, "CONFIG", "SET", "masterauth") {
		t.Errorf("нет CONFIG SET masterauth: %v", conn.calls)
	}
	if !hasCall(conn.calls, "REPLICAOF", "10.0.0.1", "6379") {
		t.Errorf("нет REPLICAOF 10.0.0.1 6379: %v", conn.calls)
	}
	// Пароль не утёк ни в события, ни в аргументы команд (masterauth — отдельный
	// инвариант: значение пароля идёт аргументом CONFIG SET, это допустимо к
	// Redis, но НЕ должно попасть в события).
	assertEventsNoSecret(t, stream)
}

// --- Apply replica: идемпотентность (уже реплика нужного master) ---

func TestApplyReplica_AlreadyReplicaNoOp(t *testing.T) {
	conn := &replConn{infoReply: "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:10.0.0.1\r\n" +
		"master_port:6379\r\n" +
		"master_link_status:up\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
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
		t.Error("ждали changed=false: уже реплика нужного master (no-op)")
	}
	// No-op: ни REPLICAOF, ни CONFIG SET masterauth.
	if hasCall(conn.calls, "REPLICAOF") {
		t.Errorf("no-op нарушен: REPLICAOF вызван: %v", conn.calls)
	}
	if hasCall(conn.calls, "CONFIG", "SET", "masterauth") {
		t.Errorf("no-op нарушен: masterauth установлен: %v", conn.calls)
	}
}

// --- Apply replica: addr == master_addr → master не реплицирует себя ---

// TestApplyReplica_SelfIsMasterNoOp тестирует ПЛАГИН-GUARD (addr==master_addr →
// no-op) как defense-in-depth. ВАЖНО: эта addr==master_addr-комбинация в prod НЕ
// возникает — в sentinel.yml addr=127.0.0.1:6379, master_addr=primary_ip (напр.
// 10.0.0.1), они никогда не равны. Реальная защита от «master реплицирует сам
// себя» — scenario `where:` (master исключён по SID), доказана L0-кейсом
// sentinel-create-1master-2replica (на master-хосте задачи replica НЕТ).
func TestApplyReplica_SelfIsMasterNoOp(t *testing.T) {
	// Тот же endpoint, что master_addr (но через другую форму записи host).
	conn := &replConn{}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "10.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
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
		t.Error("ждали changed=false: addr==master_addr (master, no-op)")
	}
	// master-guard срабатывает ДО INFO: не должно быть ни одной команды.
	if len(conn.calls) != 0 {
		t.Errorf("master-guard не сработал, вызваны команды: %v", conn.calls)
	}
	if got := fin.GetOutput().GetFields()["role"].GetStringValue(); got != "master" {
		t.Errorf("role=%q, ждали master", got)
	}
}

// --- Apply replica: пароль не течёт (события + ошибка коннекта) ---

func TestApplyReplica_NoSecretLeak(t *testing.T) {
	conn := &replConn{infoReply: "role:master\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)

	assertEventsNoSecret(t, stream)
	if conn.cfg.password != secretPass {
		t.Errorf("пароль не доехал до коннекта: %q", conn.cfg.password)
	}
}

func TestApplyReplica_ConnectFailureNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
}

// --- Apply replica: username → CONFIG SET masteruser ---

func TestApplyReplica_SetsMasteruserWhenUsernameGiven(t *testing.T) {
	conn := &replConn{infoReply: "role:master\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
			"username":    "replicator",
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if !hasCall(conn.calls, "CONFIG", "SET", "masteruser", "replicator") {
		t.Errorf("нет CONFIG SET masteruser replicator: %v", conn.calls)
	}
	if !hasCall(conn.calls, "REPLICAOF", "10.0.0.1", "6379") {
		t.Errorf("нет REPLICAOF после masteruser: %v", conn.calls)
	}
}

func TestApplyReplica_NoUsernameSkipsMasteruser(t *testing.T) {
	conn := &replConn{infoReply: "role:master\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)

	if hasCall(conn.calls, "CONFIG", "SET", "masteruser") {
		t.Errorf("без username masteruser не должен ставиться: %v", conn.calls)
	}
}

// --- Apply replica: re-point на ЧУЖОЙ master (был slave другого) → changed=true ---

func TestApplyReplica_RepointFromOtherMaster(t *testing.T) {
	// Уже реплика, но ДРУГОГО master (10.0.0.9) → должен перенастроиться на 10.0.0.1.
	conn := &replConn{infoReply: "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:10.0.0.9\r\n" +
		"master_port:6379\r\n" +
		"master_link_status:up\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали changed=true: реплика чужого master должна перенастроиться, got %+v", fin)
	}
	if !hasCall(conn.calls, "REPLICAOF", "10.0.0.1", "6379") {
		t.Errorf("re-point: нет REPLICAOF на нужный master: %v", conn.calls)
	}
}

// --- Apply replica: правильный master, но link НЕ up → changed=true (re-sync) ---

func TestApplyReplica_RightMasterLinkDownRepoints(t *testing.T) {
	// master тот же, но link down → НЕ идемпотентно (надо переустановить REPLICAOF).
	conn := &replConn{infoReply: "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:10.0.0.1\r\n" +
		"master_port:6379\r\n" +
		"master_link_status:down\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
			"password":    secretPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали changed=true: link down → не no-op, got %+v", fin)
	}
	if !hasCall(conn.calls, "REPLICAOF", "10.0.0.1", "6379") {
		t.Errorf("link down: ожидался повторный REPLICAOF: %v", conn.calls)
	}
}

// --- Apply replica: empty password → masterauth НЕ ставится ---

func TestApplyReplica_NoPasswordSkipsMasterauth(t *testing.T) {
	conn := &replConn{infoReply: "role:master\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "replica",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"master_addr": "10.0.0.1:6379",
		}),
	}, stream)

	if hasCall(conn.calls, "CONFIG", "SET", "masterauth") {
		t.Errorf("пустой пароль: masterauth не должен ставиться: %v", conn.calls)
	}
	if !hasCall(conn.calls, "REPLICAOF", "10.0.0.1", "6379") {
		t.Errorf("REPLICAOF должен выполниться и без пароля: %v", conn.calls)
	}
}
