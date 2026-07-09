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
//
// configLive моделирует текущие значения CONFIG GET <param> (для честного diff
// config-state): пусто/нет ключа → пара "param " (значение "").
type fakeConn struct {
	cfg        connConfig
	calls      [][]any
	results    map[string]string // ключ — args[0]; "" → echo "OK"
	configLive map[string]string // CONFIG GET <param> → текущее значение
	doErr      error
	closed     bool

	// aclSeq — последовательные ответы ACL LIST (по одному на вызов: before, after).
	// Если длиннее не нужно — повторяет последний. nil → пустой список.
	aclSeq   [][]string
	aclCalls int
	aclErr   error // ошибка ACL LIST (для error-path acl-state)
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

// ConfigGet — типизированный путь: отдаёт {param: value} из configLive БЕЗ
// space-join, поэтому многословные значения (save) сохраняются целиком (паритет
// с реальным go-redis ConfigGet → map[string]string). Записываем вызов в calls,
// чтобы diff-проба CONFIG GET оставалась видимой для assert.
func (f *fakeConn) ConfigGet(_ context.Context, param string) (map[string]string, error) {
	f.calls = append(f.calls, []any{"CONFIG", "GET", param})
	if f.doErr != nil {
		return nil, f.doErr
	}
	return map[string]string{param: f.configLive[param]}, nil
}

// GetKeysInSlot — command/config-тесты слоты не мигрируют, стаб под интерфейс.
func (f *fakeConn) GetKeysInSlot(_ context.Context, _, _ int) ([]string, error) {
	return nil, nil
}

// AclList — отдаёт следующий элемент aclSeq (before/after для diff acl-state),
// фиксируя порядковый вызов. Записывает вызов в calls (видим для assert порядка
// LIST → LOAD → LIST). Длиннее aclSeq — повторяет последний.
func (f *fakeConn) AclList(_ context.Context) ([]string, error) {
	f.calls = append(f.calls, []any{"ACL", "LIST"})
	if f.aclErr != nil {
		return nil, f.aclErr
	}
	i := f.aclCalls
	f.aclCalls++
	if i >= len(f.aclSeq) {
		if len(f.aclSeq) == 0 {
			return nil, nil
		}
		return f.aclSeq[len(f.aclSeq)-1], nil
	}
	return f.aclSeq[i], nil
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
	// Честный diff: live пуст → обе директивы отличаются → GET перед каждым SET,
	// затем SET (детерминированный порядок по ключу).
	wantCalls := [][]any{
		{"CONFIG", "GET", "maxmemory"},
		{"CONFIG", "SET", "maxmemory", "256mb"},
		{"CONFIG", "GET", "maxmemory-policy"},
		{"CONFIG", "SET", "maxmemory-policy", "allkeys-lru"},
	}
	assertCalls(t, conn, wantCalls)
	assertNoCommandCarriesSecret(t, conn)
}

// TestApplyConfig_NoOpWhenLiveMatches — честный diff: live уже на желаемом
// значении → CONFIG SET НЕ вызывается, changed=false (идемпотентность, M3-фикс).
func TestApplyConfig_NoOpWhenLiveMatches(t *testing.T) {
	conn := &fakeConn{configLive: map[string]string{
		"maxmemory":        "256mb",
		"maxmemory-policy": "allkeys-lru",
	}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr": "127.0.0.1:6379",
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
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: live уже на желаемом (no-op, M3 честный diff)")
	}
	if hasCall(conn.calls, "CONFIG", "SET") {
		t.Errorf("no-op нарушен: CONFIG SET вызван при совпадении live: %v", conn.calls)
	}
	// GET по обеим директивам всё равно прошёл (diff-проба).
	if !hasCall(conn.calls, "CONFIG", "GET", "maxmemory") {
		t.Errorf("CONFIG GET maxmemory должен выполниться для diff: %v", conn.calls)
	}
}

// TestApplyConfig_PartialDiff — часть директив совпала, часть нет: SET только для
// реально отличающихся, changed=true, count — по применённым.
func TestApplyConfig_PartialDiff(t *testing.T) {
	conn := &fakeConn{configLive: map[string]string{
		"maxmemory":        "256mb",      // совпадёт → no-op
		"maxmemory-policy": "noeviction", // отличается → SET
	}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr": "127.0.0.1:6379",
			"config": map[string]any{
				"maxmemory":        "256mb",
				"maxmemory-policy": "allkeys-lru",
			},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (одна директива отличается), got %+v", fin)
	}
	if hasCall(conn.calls, "CONFIG", "SET", "maxmemory", "256mb") {
		t.Errorf("совпавшую директиву не должны SET-ить: %v", conn.calls)
	}
	if !hasCall(conn.calls, "CONFIG", "SET", "maxmemory-policy", "allkeys-lru") {
		t.Errorf("отличающуюся директиву должны SET-ить: %v", conn.calls)
	}
	if got := fin.GetOutput().GetFields()["count"].GetNumberValue(); got != 1 {
		t.Errorf("count=%v, ждали 1 (одна применённая директива)", got)
	}
}

// TestApplyConfig_MultiwordValueNoOp — P3-регресс: многословное значение
// (save "900 1 300 10 60 10000") при space-join+strings.Fields рассыпалось бы в
// перепутанные пары → current != want → ложный CONFIG SET (потеря
// идемпотентности на update_config). Типизированный ConfigGet сохраняет
// значение целиком → live == want → no-op.
func TestApplyConfig_MultiwordValueNoOp(t *testing.T) {
	const save = "900 1 300 10 60 10000"
	conn := &fakeConn{configLive: map[string]string{"save": save}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":   "127.0.0.1:6379",
			"config": map[string]any{"save": save},
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
		t.Error("ждали changed=false: live save совпадает с желаемым (многословное значение сохранено целиком)")
	}
	if hasCall(conn.calls, "CONFIG", "SET") {
		t.Errorf("ложный CONFIG SET на совпавшем многословном значении (P3): %v", conn.calls)
	}
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
	// GET (live пуст) + SET со стрингифицированным значением.
	if !hasCall(conn.calls, "CONFIG", "SET", "maxclients", "20000") {
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

// TestApplyConfig_StartupOnlyDirectivesSkipped — денилист startup-only: CONFIG SET
// их отвергает, операционный сценарий рендерит ПОЛНЫЙ redis.conf (с ними), поэтому плагин их
// ПРОПУСКАЕТ (не падает). Hot-settable директивы того же вызова применяются как
// обычно. Проверяем: ни CONFIG GET, ни CONFIG SET по startup-only НЕ вызваны;
// hot-settable применена; skipped/skippedCount в Output корректны.
func TestApplyConfig_StartupOnlyDirectivesSkipped(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr": "127.0.0.1:6379",
			"config": map[string]any{
				// startup-only — должны быть пропущены (CONFIG SET их отвергает):
				"port":            "6379",
				"dir":             "/var/lib/redis",
				"aclfile":         "/etc/redis/users.acl",
				"cluster-enabled": "yes",
				"loadmodule":      "/x.so",
				// hot-settable — должна примениться:
				"maxmemory": "512mb",
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (startup-only пропущены, не падение), got %+v", fin)
	}
	if !fin.Changed {
		t.Error("ждали changed=true: hot-settable maxmemory применена")
	}

	// Ни одна startup-only-директива не должна дойти ни до GET, ни до SET.
	for _, k := range []string{"port", "dir", "aclfile", "cluster-enabled", "loadmodule"} {
		if hasCall(conn.calls, "CONFIG", "GET", k) || hasCall(conn.calls, "CONFIG", "SET", k) {
			t.Errorf("startup-only %q не должна попадать в CONFIG GET/SET: %v", k, conn.calls)
		}
	}
	// Hot-settable применена.
	if !hasCall(conn.calls, "CONFIG", "SET", "maxmemory", "512mb") {
		t.Errorf("hot-settable maxmemory должна примениться: %v", conn.calls)
	}
	// Output: applied/count по hot-settable, skipped/skippedCount по startup-only.
	out := fin.GetOutput().GetFields()
	if got := out["count"].GetNumberValue(); got != 1 {
		t.Errorf("count=%v, ждали 1 (одна hot-settable)", got)
	}
	if got := out["skippedCount"].GetNumberValue(); got != 5 {
		t.Errorf("skippedCount=%v, ждали 5 (port/dir/aclfile/cluster-enabled/loadmodule)", got)
	}
	skipped := out["skipped"].GetStringValue()
	for _, k := range []string{"port", "dir", "aclfile", "cluster-enabled", "loadmodule"} {
		if !strings.Contains(skipped, k) {
			t.Errorf("skipped не содержит %q: %q", k, skipped)
		}
	}
}

// TestApplyConfig_AllStartupOnly_NoChange — если ВСЕ директивы startup-only,
// CONFIG SET не вызывается ни разу, changed=false (нечего применять hot), прогон не
// падает. Граничный кейс денилиста: rewrite тоже не дёргается (len(applied)==0).
func TestApplyConfig_AllStartupOnly_NoChange(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:6379",
			"config":  map[string]any{"port": "6379", "dir": "/var/lib/redis"},
			"rewrite": true,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (все startup-only пропущены), got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: нечего применять hot (все директивы startup-only)")
	}
	if hasCall(conn.calls, "CONFIG", "SET") {
		t.Errorf("CONFIG SET не должен вызываться при всех startup-only: %v", conn.calls)
	}
	if hasCall(conn.calls, "CONFIG", "REWRITE") {
		t.Errorf("CONFIG REWRITE не должен вызываться при пустом applied: %v", conn.calls)
	}
}

// --- Validate: acl ---

func TestValidate_AclRequiresAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr для acl")
	}
}

func TestValidate_AclHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

// --- Apply: acl ---

// TestApplyACL_SendsAclLoad — базовый контракт: acl-state шлёт ACL LOAD между
// двумя ACL LIST (diff-проба), коннект получает пароль, аргументы команд его не
// несут, соединение закрывается.
func TestApplyACL_SendsAclLoad(t *testing.T) {
	conn := &fakeConn{aclSeq: [][]string{
		{"user default on nopass ~* +@all"},
		{"user default on nopass ~* +@all", "user alice on #abc ~app:* +@read"}, // после LOAD добавился alice
	}}
	m := newModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "acl",
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
	// Порядок: ACL LIST (before) → ACL LOAD → ACL LIST (after).
	wantCalls := [][]any{
		{"ACL", "LIST"},
		{"ACL", "LOAD"},
		{"ACL", "LIST"},
	}
	assertCalls(t, conn, wantCalls)
	if conn.cfg.password != secretPass {
		t.Errorf("пароль не доехал до коннекта: %q", conn.cfg.password)
	}
	assertNoCommandCarriesSecret(t, conn)
	if !conn.closed {
		t.Error("соединение не закрыто")
	}
}

// TestApplyACL_ChangedTrueWhenAclDiffers — ACL LIST до/после отличается (LOAD
// реально перечитал файл с новыми правилами) → changed=true, users — по after.
func TestApplyACL_ChangedTrueWhenAclDiffers(t *testing.T) {
	conn := &fakeConn{aclSeq: [][]string{
		{"user default on nopass ~* +@all"},
		{"user default on nopass ~* +@all", "user alice on #abc ~app:* +@read"},
	}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (ACL изменился), got %+v", fin)
	}
	if got := fin.GetOutput().GetFields()["users"].GetNumberValue(); got != 2 {
		t.Errorf("users=%v, ждали 2 (after-список)", got)
	}
}

// TestApplyACL_ChangedFalseWhenAclUnchanged — живой инстанс уже совпадал с
// aclfile: ACL LIST до/после идентичен → changed=false (no-op, идемпотентность
// как config/cluster/sentinel). ACL LOAD при этом всё равно выполняется
// (приведение к декларированному — by construction).
func TestApplyACL_ChangedFalseWhenAclUnchanged(t *testing.T) {
	same := []string{"user default on nopass ~* +@all", "user alice on #abc ~app:* +@read"}
	conn := &fakeConn{aclSeq: [][]string{same, same}}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: ACL LIST до/после совпал (no-op)")
	}
	if !hasCall(conn.calls, "ACL", "LOAD") {
		t.Errorf("ACL LOAD должен выполниться даже на no-op (приведение by construction): %v", conn.calls)
	}
}

// TestApplyACL_LoadErrorIsFailure — ACL LOAD упал (битый/несконфигурированный
// aclfile) → failed=true с внятным сообщением.
func TestApplyACL_LoadErrorIsFailure(t *testing.T) {
	conn := &fakeConn{doErr: errors.New("ERR This Redis instance is not configured to use an ACL file.")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку ACL LOAD, got %+v", fin)
	}
	if !strings.Contains(fin.GetMessage(), "ACL LOAD") {
		t.Errorf("ждали внятный prefix 'ACL LOAD' в сообщении, got %q", fin.GetMessage())
	}
}

// TestApplyACL_ListErrorIsFailure — ACL LIST (before) недоступен → failed, ACL
// LOAD НЕ выполняется (нет точки сравнения — не трогаем живой инстанс вслепую).
func TestApplyACL_ListErrorIsFailure(t *testing.T) {
	conn := &fakeConn{aclErr: errors.New("WRONGPASS invalid username-password pair")}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "acl",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на ошибку ACL LIST, got %+v", fin)
	}
	if hasCall(conn.calls, "ACL", "LOAD") {
		t.Errorf("ACL LOAD не должен выполняться при недоступном ACL LIST (before): %v", conn.calls)
	}
}

// TestApplyACL_ConnectFailure_DoesNotLeakPassword — ошибка коннекта, чей текст
// СОДЕРЖИТ пароль, санитизируется (общий путь Apply, redactError).
func TestApplyACL_ConnectFailure_DoesNotLeakPassword(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "acl",
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

// TestApplyACL_TLSParamsReachConnect — TLS-параметры (tls/tls_ca) доезжают до
// коннекта (acl читает тот же набор, что config/command — общий путь).
func TestApplyACL_TLSParamsReachConnect(t *testing.T) {
	var captured connConfig
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			captured = cfg
			return &fakeConn{}, nil
		},
	}
	const caPEM = "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----"
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "acl",
		Params: mustStruct(t, map[string]any{
			"addr":   "127.0.0.1:6379",
			"tls":    true,
			"tls_ca": caPEM,
		}),
	}, &applyStream{})

	if !captured.tls.enabled {
		t.Error("tls=true не доехал до коннекта acl-state")
	}
	if captured.tls.caPEM != caPEM {
		t.Errorf("tls_ca не доехал до коннекта: %q", captured.tls.caPEM)
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
		{"acl", "acl", map[string]any{"addr": "127.0.0.1:6379", "password": secretPass}},
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

// TestApplyConfig_SecretLeakInDriverError_Redacted — драйвер вернул ошибку,
// СОДЕРЖАЩУЮ значение директивы (requirepass): applyConfig обязан вырезать его
// через redactError. Доказывает, что error-path config-state симметричен
// replica/cluster/sentinel (M4, defense-in-depth).
func TestApplyConfig_SecretLeakInDriverError_Redacted(t *testing.T) {
	conn := &leakyConfigConn{secret: secretPass}
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) { return conn, nil },
	}
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr":   "127.0.0.1:6379",
			"config": map[string]any{"requirepass": secretPass},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	if strings.Contains(fin.GetMessage(), secretPass) {
		t.Errorf("значение директивы (секрет) утекло в Message ошибки: %q", fin.GetMessage())
	}
	if !strings.Contains(fin.GetMessage(), "***") {
		t.Errorf("ждали маску *** в редактированной ошибке, got %q", fin.GetMessage())
	}
}

// leakyConfigConn моделирует драйвер, эхающий значение SET-аргумента в текст
// ошибки (worst-case для проверки redactError на error-path config-state).
type leakyConfigConn struct {
	secret string
}

func (c *leakyConfigConn) Do(_ context.Context, args ...any) (string, error) {
	if len(args) >= 4 && args[0] == "CONFIG" && args[1] == "SET" {
		val, _ := args[3].(string)
		return "", errors.New("ERR could not set value to " + val) // эхает секрет
	}
	return "OK", nil
}

// ConfigGet — live пуст → diff сработает, applyConfig дойдёт до CONFIG SET.
func (c *leakyConfigConn) ConfigGet(_ context.Context, param string) (map[string]string, error) {
	return map[string]string{param: ""}, nil
}
func (c *leakyConfigConn) GetKeysInSlot(_ context.Context, _, _ int) ([]string, error) {
	return nil, nil
}
func (c *leakyConfigConn) AclList(_ context.Context) ([]string, error) { return nil, nil }
func (c *leakyConfigConn) Close() error                                { return nil }

// --- redactError: регресс для коротких паролей-подстрок ---

// TestRedactError_ShortPasswordSubstring — пароль-подстрока, совпадающая с частью
// безобидного текста ("6379" в "127.0.0.1:6379"), при ReplaceAll затрёт И адрес.
// Тест ФИКСИРУЕТ текущее поведение (хрупкость ReplaceAll на коротких секретах):
// маскируется ЛЮБОЕ вхождение подстроки. Инвариант ИБ соблюдён (секрет не виден),
// ценой возможной чрезмерной маскировки диагностики. Регресс ловит изменение.
func TestRedactError_ShortPasswordSubstring(t *testing.T) {
	err := errors.New("dial 127.0.0.1:6379: connection refused")
	got := redactError(err, "6379")
	if strings.Contains(got, "6379") {
		t.Errorf("секрет-подстрока должна быть вырезана отовсюду, got %q", got)
	}
	// Документируем over-masking: адрес тоже задет — ОЖИДАЕМО при коротком секрете.
	if !strings.Contains(got, "127.0.0.1:***") {
		t.Errorf("ждали затёртый порт (over-masking фиксируется): %q", got)
	}
	// Пустой пароль — no-op.
	if redactError(errors.New("plain error"), "") != "plain error" {
		t.Error("пустой пароль должен быть no-op для redactError")
	}
}

// --- valueToString / args со значением, содержащим пробел ---

// TestStringList_PreservesValueWithSpace — guard: значение элемента args с
// пробелом ("hello world") приходит ОДНИМ аргументом команды и не теряет
// структуру (каждый элемент списка = отдельный arg, без склейки/Fields).
func TestStringList_PreservesValueWithSpace(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr": "127.0.0.1:6379",
			"args": []any{"SET", "greeting", "hello world"},
		}),
	}, stream)

	if len(conn.calls) != 1 {
		t.Fatalf("ждали один вызов команды, got %v", conn.calls)
	}
	call := conn.calls[0]
	if len(call) != 3 {
		t.Fatalf("ждали 3 аргумента (SET greeting 'hello world'), got %d: %v", len(call), call)
	}
	if call[2] != "hello world" {
		t.Errorf("значение с пробелом потеряло структуру: arg[2]=%v (ждали 'hello world')", call[2])
	}
}

// TestStringMap_NumericAndStringValuesStringified — guard valueToString в составе
// config-map: число и bool стрингифицируются предсказуемо (256 не 256.000000;
// true не пусто) — Redis ждёт строки.
func TestStringMap_NumericAndStringValuesStringified(t *testing.T) {
	conn := &fakeConn{}
	m := newModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "config",
		Params: mustStruct(t, map[string]any{
			"addr": "127.0.0.1:6379",
			"config": map[string]any{
				"maxclients":             20000,      // число → "20000"
				"appendfsync":            "everysec", // строка как есть
				"lazyfree-lazy-eviction": true,       // bool → "true" (оператор предупреждён: Redis ждёт yes/no — это его выбор)
			},
		}),
	}, stream)

	if !hasCall(conn.calls, "CONFIG", "SET", "maxclients", "20000") {
		t.Errorf("число должно стать '20000': %v", conn.calls)
	}
	if !hasCall(conn.calls, "CONFIG", "SET", "appendfsync", "everysec") {
		t.Errorf("строка как есть: %v", conn.calls)
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
