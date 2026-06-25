package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// sentinelConn — fake redisConn для sentinel-тестов. Отвечает на:
//   - SENTINEL MASTER <name>  → masterReply (flat "k v k v"; пусто → "No such
//     master" ошибка, моделируя неизвестный master для action=add);
//   - SENTINEL CONFIG GET <p> → configReply[p] как "p value" (пусто → "p ").
//
// Пишет каждый вызов (assert MONITOR/REMOVE/SET/CONFIG SET).
const secretAuthPass = "vault-resolved-sentinel-pass-7c2f9a"

type sentinelConn struct {
	cfg          connConfig
	masterReply  string            // ответ SENTINEL MASTER (flat "k v ..."); "" → No such master
	masterExists bool              // true → master известен (даже если masterReply задан)
	configReply  map[string]string // SENTINEL CONFIG GET <p> → значение
	calls        [][]any
	closed       bool
}

func (c *sentinelConn) Do(_ context.Context, args ...any) (string, error) {
	c.calls = append(c.calls, args)
	switch {
	case isSub(args, "SENTINEL", "MASTER"):
		if c.masterReply == "" && !c.masterExists {
			return "", errors.New("ERR No such master with that name")
		}
		return c.masterReply, nil
	case isSub(args, "SENTINEL", "CONFIG") && len(args) >= 4 && strings.EqualFold(str(args[2]), "GET"):
		p := str(args[3])
		if v, ok := c.configReply[p]; ok {
			return p + " " + v, nil
		}
		return p + " ", nil
	}
	return "OK", nil
}

// ConfigGet — sentinel-state читает SENTINEL CONFIG GET (через Do), а не plain
// CONFIG GET; стаб под интерфейс redisConn.
func (c *sentinelConn) ConfigGet(_ context.Context, param string) (map[string]string, error) {
	return map[string]string{param: ""}, nil
}

// GetKeysInSlot — sentinel-state слоты не мигрирует, стаб под интерфейс redisConn.
func (c *sentinelConn) GetKeysInSlot(_ context.Context, _, _ int) ([]string, error) {
	return nil, nil
}

// AclList — sentinel-state ACL не трогает, стаб под интерфейс redisConn.
func (c *sentinelConn) AclList(_ context.Context) ([]string, error) { return nil, nil }

func (c *sentinelConn) Close() error { c.closed = true; return nil }

func str(v any) string { s, _ := v.(string); return s }

func isSub(args []any, want ...string) bool {
	if len(args) < len(want) {
		return false
	}
	for i, w := range want {
		if !strings.EqualFold(str(args[i]), w) {
			return false
		}
	}
	return true
}

func sentinelModule(conn *sentinelConn) *RedisModule {
	return &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			conn.cfg = cfg
			return conn, nil
		},
	}
}

// --- classifyConfig (чистая функция, перенос classify_config) ---

func TestClassifyConfig_SplitsGlobalsAndPerMaster(t *testing.T) {
	config := map[string]string{
		"loglevel":             "notice",
		"sentinel announce-ip": "10.0.0.9",
		"sentinel down-after-milliseconds mymaster": "12000",
		"sentinel failover-timeout other":           "70000",          // другой master → игнор
		"dir":                                       "/var/lib/redis", // startup-only → игнор
		"port":                                      "26379",          // startup-only → игнор
	}
	globals, perMaster := classifyConfig(config, "mymaster")

	if globals["loglevel"] != "notice" {
		t.Errorf("loglevel должен быть global: %v", globals)
	}
	if globals["announce-ip"] != "10.0.0.9" {
		t.Errorf("announce-ip должен быть global: %v", globals)
	}
	if perMaster["down-after-milliseconds"] != "12000" {
		t.Errorf("down-after-milliseconds должен быть per-master: %v", perMaster)
	}
	if _, ok := perMaster["failover-timeout"]; ok {
		t.Error("failover-timeout для ДРУГОГО master не должен попасть в per-master")
	}
	if _, ok := globals["dir"]; ok {
		t.Error("startup-only dir не должен попасть в globals")
	}
	if _, ok := globals["port"]; ok {
		t.Error("startup-only port не должен попасть в globals")
	}
}

// --- supportedGlobals: loglevel version-gate + секрет-фильтр ---

func TestSupportedGlobals_LoglevelVersionGate(t *testing.T) {
	g := map[string]string{"loglevel": "notice", "announce-ip": "10.0.0.1"}

	on70 := supportedGlobals(g, "7.4.1")
	if on70["loglevel"] != "notice" {
		t.Errorf("loglevel должен пройти на 7.4.1: %v", on70)
	}
	on62 := supportedGlobals(g, "6.2.0")
	if _, ok := on62["loglevel"]; ok {
		t.Errorf("loglevel НЕ должен пройти на 6.2.0: %v", on62)
	}
	if on62["announce-ip"] != "10.0.0.1" {
		t.Errorf("announce-ip должен проходить на любой версии: %v", on62)
	}
	// Пустая версия → loglevel отбрасывается fail-closed.
	if _, ok := supportedGlobals(g, "")["loglevel"]; ok {
		t.Error("loglevel НЕ должен проходить на пустой версии")
	}
}

func TestSupportedGlobals_DropsSecretGlobals(t *testing.T) {
	g := map[string]string{"sentinel-pass": secretAuthPass, "announce-ip": "10.0.0.1"}
	out := supportedGlobals(g, "7.4.1")
	if _, ok := out["sentinel-pass"]; ok {
		t.Error("sentinel-pass (секрет) не должен попадать в applied globals (diff невозможен)")
	}
}

// --- versionTuple / versionGE: epoch, v-префикс, мусорный суффикс, 2-компонент ---

func TestVersionTuple_NormalizesDistroForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want [3]int
	}{
		{"8.0.3", [3]int{8, 0, 3}},
		{"v8.0.3", [3]int{8, 0, 3}},              // v-префикс
		{"6.2", [3]int{6, 2, 0}},                 // 2-компонент → patch=0
		{"7", [3]int{7, 0, 0}},                   // только major
		{"5:7.0.15-1~deb12u7", [3]int{7, 0, 15}}, // deb epoch + revision: major=7, НЕ 5
		{"7.0.15-1ubuntu0.1", [3]int{7, 0, 15}},  // revision-хвост срезан
		{"  7.4.1  ", [3]int{7, 4, 1}},           // окружающие пробелы
		{"7.0.15~rc1", [3]int{7, 0, 15}},         // мусорный суффикс в чанке (ведущие цифры)
		{"", [3]int{0, 0, 0}},                    // пусто → (0,0,0)
		{"garbage", [3]int{0, 0, 0}},             // не-цифры → (0,0,0)
	} {
		if got := versionTuple(tc.in); got != tc.want {
			t.Errorf("versionTuple(%q)=%v, ждали %v", tc.in, got, tc.want)
		}
	}
}

// TestVersionGE_EpochDoesNotMisgate — регресс к m6: distro epoch не должен ронять
// major до epoch-числа. "5:7.0.15..." обязан считаться >= 7.0 (а не как 5.x).
func TestVersionGE_EpochDoesNotMisgate(t *testing.T) {
	target := [3]int{7, 0, 0}
	if !versionGE("5:7.0.15-1~deb12u7", target) {
		t.Error("epoch-форма 5:7.0.15 должна быть >= 7.0 (epoch не major)")
	}
	if versionGE("5:6.2.7-1", target) {
		t.Error("epoch-форма 5:6.2.7 НЕ должна быть >= 7.0 (epoch не делает её 7+)")
	}
	if versionGE("6.2.0", target) {
		t.Error("6.2.0 НЕ должна быть >= 7.0")
	}
	if !versionGE("7.0.0", target) {
		t.Error("7.0.0 должна быть >= 7.0 (нижняя граница включительно)")
	}
	if versionGE("", target) {
		t.Error("пустая версия → (0,0,0) НЕ >= 7.0 (fail-closed)")
	}
}

// TestSentinelGlobals_SkippedPre70 — m1: на <7.0 reconcileGlobals целиком НЕ
// выполняется (SENTINEL CONFIG GET/SET недоступен), непустые globals → no-op +
// warning-действие. На 6.2 announce-ip НЕ должен попасть в CONFIG GET/SET.
func TestSentinelGlobals_SkippedPre70(t *testing.T) {
	conn := &sentinelConn{
		masterExists: true,
		masterReply:  "name mymaster ip 10.0.0.1 port 6379 quorum 2",
		configReply:  map[string]string{"announce-ip": "10.0.0.9"},
	}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":          "127.0.0.1:26379",
			"master_name":   "mymaster",
			"redis_version": "6.2.7", // <7.0 → globals целиком скипаются
			"monitor":       map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
			"config": map[string]any{
				"sentinel announce-ip": "10.0.0.1",
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех (не падение на 6.2), got %+v", fin)
	}
	if fin.Changed {
		t.Error("ждали changed=false: globals скипнуты на <7.0, монитор уже на месте")
	}
	// КЛЮЧЕВОЕ: SENTINEL CONFIG GET/SET НЕ должны вызываться на 6.2.
	if hasCall(conn.calls, "SENTINEL", "CONFIG", "GET") {
		t.Errorf("SENTINEL CONFIG GET не должен вызываться на 6.2: %v", conn.calls)
	}
	if hasCall(conn.calls, "SENTINEL", "CONFIG", "SET") {
		t.Errorf("SENTINEL CONFIG SET не должен вызываться на 6.2: %v", conn.calls)
	}
	if got := fin.GetOutput().GetFields()["actions"].GetStringValue(); !strings.Contains(got, "globals_skipped_pre_7.0") {
		t.Errorf("ждали warning-действие globals_skipped_pre_7.0 в actions, got %q", got)
	}
}

// TestValidate_SentinelRejectsZeroQuorum — m2: quorum:0 при наличии монитора
// отвергается (SENTINEL MONITOR ... 0 не принимает Redis).
func TestValidate_SentinelRejectsZeroQuorum(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:26379",
			"monitor": map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 0},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false: quorum=0 недопустим")
	}
}

func TestValidate_SentinelRejectsNegativeQuorum(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:26379",
			"monitor": map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": -1},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false: отрицательный quorum недопустим")
	}
}

// TestValidate_SentinelQuorumOmittedOK — quorum не задан → проходит (default 1
// в reconcileMonitor); guard срабатывает только при ЯВНОМ некорректном значении.
func TestValidate_SentinelQuorumOmittedOK(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:26379",
			"monitor": map[string]any{"ip": "10.0.0.1", "port": 6379},
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true: quorum опционален, got %+v", reply)
	}
}

// --- computeMonitorAction ---

func TestComputeMonitorAction(t *testing.T) {
	if a := computeMonitorAction(nil, "10.0.0.1", "6379"); a != "add" {
		t.Errorf("неизвестный master → add, got %q", a)
	}
	cur := map[string]string{"ip": "10.0.0.1", "port": "6379"}
	if a := computeMonitorAction(cur, "10.0.0.1", "6379"); a != "none" {
		t.Errorf("совпавший адрес → none, got %q", a)
	}
	if a := computeMonitorAction(cur, "10.0.0.2", "6379"); a != "readd" {
		t.Errorf("сменился IP → readd, got %q", a)
	}
}

// --- computeSetUpdates: только отличия, детерминированный порядок ---

func TestComputeSetUpdates_OnlyDiffsSorted(t *testing.T) {
	desired := map[string]string{"quorum": "2", "down-after-milliseconds": "12000", "failover-timeout": "70000"}
	current := map[string]string{"quorum": "2", "down-after-milliseconds": "5000"} // quorum совпал, dam отличается, ft отсутствует
	keys, values := computeSetUpdates(desired, current)

	// quorum совпал → не в апдейтах; остальные два → есть; порядок отсортирован.
	want := []string{"down-after-milliseconds", "failover-timeout"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("keys=%v, ждали %v (без quorum, sorted)", keys, want)
	}
	if values["down-after-milliseconds"] != "12000" || values["failover-timeout"] != "70000" {
		t.Errorf("неверные значения апдейтов: %v", values)
	}
}

// --- Validate: sentinel ---

func TestValidate_SentinelRejectsEmptyAddr(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "sentinel",
		Params: mustStruct(t, map[string]any{"addr": ""}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на пустой addr")
	}
}

func TestValidate_SentinelRejectsMonitorWithoutIP(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:26379",
			"monitor": map[string]any{"port": 6379, "quorum": 2},
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false: monitor без ip")
	}
}

func TestValidate_SentinelHappy(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":    "127.0.0.1:26379",
			"monitor": map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true, got %+v", reply)
	}
}

// --- Apply sentinel: новый монитор (MONITOR + auth-set) ---

func TestApplySentinel_MonitorAddWithAuth(t *testing.T) {
	// master неизвестен → action=add.
	conn := &sentinelConn{configReply: map[string]string{}}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:26379",
			"master_name": "mymaster",
			"monitor":     map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
			"auth_user":   "sentinel",
			"auth_pass":   secretAuthPass,
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if !hasCall(conn.calls, "SENTINEL", "MONITOR", "mymaster", "10.0.0.1", "6379", "2") {
		t.Errorf("нет SENTINEL MONITOR mymaster 10.0.0.1 6379 2: %v", conn.calls)
	}
	if !hasCall(conn.calls, "SENTINEL", "SET", "mymaster", "auth-user", "sentinel") {
		t.Errorf("нет SENTINEL SET auth-user: %v", conn.calls)
	}
	if !hasCall(conn.calls, "SENTINEL", "SET", "mymaster", "auth-pass") {
		t.Errorf("нет SENTINEL SET auth-pass: %v", conn.calls)
	}
	// auth_pass не должен утечь в события.
	assertSentinelNoSecret(t, stream)
}

// --- Apply sentinel: монитор уже на месте → idempotent (no MONITOR) ---

func TestApplySentinel_MonitorIdempotent(t *testing.T) {
	// master известен на нужном адресе → action=none; конфиг пуст → нет SET/CONFIG SET.
	conn := &sentinelConn{
		masterExists: true,
		masterReply:  "name mymaster ip 10.0.0.1 port 6379 quorum 2",
		configReply:  map[string]string{},
	}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:26379",
			"master_name": "mymaster",
			"monitor":     map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
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
		t.Error("ждали changed=false: монитор уже на нужном адресе, quorum совпал (no-op)")
	}
	if hasCall(conn.calls, "SENTINEL", "MONITOR") {
		t.Errorf("idempotent нарушен: MONITOR вызван: %v", conn.calls)
	}
	if hasCall(conn.calls, "SENTINEL", "SET") {
		t.Errorf("idempotent нарушен: SET вызван при совпавшем quorum: %v", conn.calls)
	}
}

// --- Apply sentinel: смена адреса master → REMOVE + MONITOR (readd) ---

func TestApplySentinel_MonitorReadd(t *testing.T) {
	conn := &sentinelConn{
		masterExists: true,
		masterReply:  "name mymaster ip 10.0.0.5 port 6379 quorum 2", // старый адрес
		configReply:  map[string]string{},
	}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:26379",
			"master_name": "mymaster",
			"monitor":     map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2}, // новый адрес
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true, got %+v", fin)
	}
	if !hasCall(conn.calls, "SENTINEL", "REMOVE", "mymaster") {
		t.Errorf("нет SENTINEL REMOVE при смене адреса: %v", conn.calls)
	}
	if !hasCall(conn.calls, "SENTINEL", "MONITOR", "mymaster", "10.0.0.1", "6379", "2") {
		t.Errorf("нет SENTINEL MONITOR с новым адресом: %v", conn.calls)
	}
}

// --- Apply sentinel: per-master SET reconcile (down-after отличается) ---

func TestApplySentinel_PerMasterSetReconcile(t *testing.T) {
	conn := &sentinelConn{
		masterExists: true,
		masterReply:  "name mymaster ip 10.0.0.1 port 6379 quorum 2 down-after-milliseconds 5000",
		configReply:  map[string]string{},
	}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:26379",
			"master_name": "mymaster",
			"monitor":     map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
			"config": map[string]any{
				"sentinel down-after-milliseconds mymaster": "12000",
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (down-after изменился), got %+v", fin)
	}
	if !hasCall(conn.calls, "SENTINEL", "SET", "mymaster", "down-after-milliseconds", "12000") {
		t.Errorf("нет SENTINEL SET down-after-milliseconds 12000: %v", conn.calls)
	}
	// quorum совпал (2) → его в SET быть не должно.
	if hasCall(conn.calls, "SENTINEL", "SET", "mymaster", "quorum") {
		t.Errorf("quorum совпал — SET quorum не должен вызываться: %v", conn.calls)
	}
}

// --- Apply sentinel: globals reconcile через SENTINEL CONFIG SET ---

func TestApplySentinel_GlobalsConfigSet(t *testing.T) {
	conn := &sentinelConn{
		masterExists: true,
		masterReply:  "name mymaster ip 10.0.0.1 port 6379 quorum 2",
		configReply:  map[string]string{"announce-ip": "10.0.0.9"}, // текущее значение отличается
	}
	m := sentinelModule(conn)
	stream := &applyStream{}

	err := m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":          "127.0.0.1:26379",
			"master_name":   "mymaster",
			"redis_version": "7.4.1", // SENTINEL CONFIG GET/SET доступен с 7.0
			"monitor":       map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
			"config": map[string]any{
				"sentinel announce-ip": "10.0.0.1", // желаемое != текущему 10.0.0.9
			},
		}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fin := stream.final()
	if fin == nil || fin.Failed || !fin.Changed {
		t.Fatalf("ждали успех changed=true (announce-ip изменился), got %+v", fin)
	}
	if !hasCall(conn.calls, "SENTINEL", "CONFIG", "SET", "announce-ip", "10.0.0.1") {
		t.Errorf("нет SENTINEL CONFIG SET announce-ip: %v", conn.calls)
	}
}

// --- Apply sentinel: auth_pass не течёт (события + коннект-фейл) ---

func TestApplySentinel_NoSecretLeak(t *testing.T) {
	conn := &sentinelConn{configReply: map[string]string{}}
	m := sentinelModule(conn)
	stream := &applyStream{}

	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:26379",
			"master_name": "mymaster",
			"monitor":     map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
			"auth_pass":   secretAuthPass,
		}),
	}, stream)

	assertSentinelNoSecret(t, stream)
}

func TestApplySentinel_ConnectFailureNoLeak(t *testing.T) {
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			return nil, errors.New("dial failed for AUTH " + cfg.password)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "sentinel",
		Params: mustStruct(t, map[string]any{
			"addr":      "127.0.0.1:26379",
			"password":  secretPass,
			"auth_pass": secretAuthPass,
			"monitor":   map[string]any{"ip": "10.0.0.1", "port": 6379, "quorum": 2},
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true, got %+v", fin)
	}
	assertSentinelNoSecret(t, stream)
}

// assertSentinelNoSecret — ни одно событие не несёт auth_pass/sentinel-pass.
func assertSentinelNoSecret(t *testing.T, stream *applyStream) {
	t.Helper()
	for i, ev := range stream.sent {
		if strings.Contains(ev.GetMessage(), secretAuthPass) {
			t.Errorf("событие[%d].Message содержит auth_pass: %q", i, ev.GetMessage())
		}
		if out := ev.GetOutput(); out != nil {
			for k, v := range out.GetFields() {
				if strings.Contains(v.GetStringValue(), secretAuthPass) {
					t.Errorf("событие[%d].Output[%s] содержит auth_pass", i, k)
				}
			}
		}
	}
}
