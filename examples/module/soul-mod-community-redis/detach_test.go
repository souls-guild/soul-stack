package main

import (
	"context"
	"errors"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// --- Validate: detached требует только addr ---

func TestValidate_DetachedRejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "detached",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr (detached)")
	}
}

func TestValidate_DetachedHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "detached",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

// --- Apply detached: slave → master (REPLICAOF NO ONE, changed=true) ---

// TestApplyDetached_SlavePromotedToMaster — инстанс был репликой → REPLICAOF NO
// ONE, changed=true, previous_master несёт прежний master host:port.
func TestApplyDetached_SlavePromotedToMaster(t *testing.T) {
	conn := &replConn{infoReply: "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:10.0.0.1\r\n" +
		"master_port:6379\r\n" +
		"master_link_status:up\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "detached",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:6379",
			"password": secretPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (slave промоутнут в master), got %+v", fin)
	}
	if !hasCall(conn.calls, "REPLICAOF", "NO", "ONE") {
		t.Errorf("нет REPLICAOF NO ONE: %v", conn.calls)
	}
	if got := fin.GetOutput().GetFields()["previous_master"].GetStringValue(); got != "10.0.0.1:6379" {
		t.Errorf("previous_master=%q, ждали 10.0.0.1:6379", got)
	}
	assertEventsNoSecret(t, stream)
}

// --- Apply detached: master → no-op (идемпотентность) ---

// TestApplyDetached_AlreadyMasterNoOp — инстанс уже master → changed=false,
// REPLICAOF НЕ вызывается (идемпотентно, безопасно к повтору сценария).
func TestApplyDetached_AlreadyMasterNoOp(t *testing.T) {
	conn := &replConn{infoReply: "# Replication\r\nrole:master\r\nconnected_slaves:2\r\n"}
	m := replModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "detached",
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
		t.Error("ждали changed=false: инстанс уже master (no-op идемпотентно)")
	}
	if hasCall(conn.calls, "REPLICAOF") {
		t.Errorf("no-op нарушен: REPLICAOF вызван на уже-master: %v", conn.calls)
	}
	if got := fin.GetOutput().GetFields()["previous_master"].GetStringValue(); got != "" {
		t.Errorf("previous_master=%q, ждали пусто (уже master)", got)
	}
}

// TestApplyDetached_InfoErrorIsFailure — INFO replication упал → failed (не
// трогаем инстанс вслепую без знания роли).
func TestApplyDetached_InfoErrorIsFailure(t *testing.T) {
	conn := &replConn{failOnVerb: "INFO"}
	m := replModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "detached",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку INFO replication, got %+v", fin)
	}
	if hasCall(conn.calls, "REPLICAOF") {
		t.Errorf("REPLICAOF не должен вызываться при недоступном INFO: %v", conn.calls)
	}
}

// TestApplyDetached_ConnectFailure_DoesNotLeakPassword — коннект-ошибка с
// паролем в тексте санитизируется (ИБ-инвариант ADR-010).
func TestApplyDetached_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "detached",
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
