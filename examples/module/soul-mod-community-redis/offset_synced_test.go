package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// offsetConn — fake redisConn для offset-synced: отвечает на INFO replication
// заданной секцией и на DBSIZE заданным числом; прочие verb → "OK". Пишет вызовы.
type offsetConn struct {
	cfg       connConfig
	infoReply string // ответ INFO replication
	dbsize    int64  // ответ DBSIZE
	calls     [][]any
	closed    bool
	failVerb  string // непусто → ошибка на этот verb (args[0])
}

func (c *offsetConn) Do(_ context.Context, args ...any) (string, error) {
	c.calls = append(c.calls, args)
	verb, _ := args[0].(string)
	if c.failVerb != "" && strings.EqualFold(verb, c.failVerb) {
		return "", errors.New("redis error on " + verb)
	}
	switch {
	case strings.EqualFold(verb, "INFO"):
		return c.infoReply, nil
	case strings.EqualFold(verb, "DBSIZE"):
		return strconv.FormatInt(c.dbsize, 10), nil
	default:
		return "OK", nil
	}
}

func (c *offsetConn) ConfigGet(_ context.Context, p string) (map[string]string, error) {
	return map[string]string{p: ""}, nil
}
func (c *offsetConn) GetKeysInSlot(_ context.Context, _, _ int) ([]string, error) { return nil, nil }
func (c *offsetConn) AclList(_ context.Context) ([]string, error)                 { return nil, nil }
func (c *offsetConn) Close() error                                                { c.closed = true; return nil }

// offsetModule собирает RedisModule, роутящий коннект по cfg.addr: selfAddr →
// self, иначе → source. Так applyOffsetSynced получает РАЗНЫЕ инстансы для своего
// addr и для source_addr (второй коннект).
func offsetModule(selfAddr string, self, source *offsetConn) *RedisModule {
	return &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			if cfg.addr == selfAddr {
				self.cfg = cfg
				return self, nil
			}
			source.cfg = cfg
			return source, nil
		},
	}
}

// --- Validate: offset-synced ---

func TestValidate_OffsetSyncedRequiresAddrAndSource(t *testing.T) {
	m := &RedisModule{}
	// нет source_addr
	r1, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "offset-synced",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379"}),
	})
	if r1.Ok {
		t.Error("ждали Ok=false без source_addr")
	}
	// нет addr
	r2, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "offset-synced",
		Params: mustStruct(t, map[string]any{"source_addr": "203.0.113.5:6379"}),
	})
	if r2.Ok {
		t.Error("ждали Ok=false без addr")
	}
}

func TestValidate_OffsetSyncedHappyPath(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "offset-synced",
		Params: mustStruct(t, map[string]any{
			"addr":        "127.0.0.1:6379",
			"source_addr": "203.0.113.5:6379",
		}),
	})
	if !reply.Ok || len(reply.Errors) != 0 {
		t.Fatalf("ждали Ok=true без ошибок, got %+v", reply)
	}
}

func TestValidate_OffsetSyncedRejectsNegativeLag(t *testing.T) {
	m := &RedisModule{}
	reply, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "offset-synced",
		Params: mustStruct(t, map[string]any{
			"addr":          "127.0.0.1:6379",
			"source_addr":   "203.0.113.5:6379",
			"lag_threshold": -1,
		}),
	})
	if reply.Ok {
		t.Fatal("ждали Ok=false на отрицательный lag_threshold")
	}
}

// --- Apply offset-synced ---

// selfReplInfo — INFO replication своего инстанса (реплики).
func selfReplInfo(slaveOffset int, link string, syncInProgress int) string {
	return "# Replication\r\n" +
		"role:slave\r\n" +
		"master_host:203.0.113.5\r\n" +
		"master_link_status:" + link + "\r\n" +
		"master_sync_in_progress:" + strconv.Itoa(syncInProgress) + "\r\n" +
		"slave_repl_offset:" + strconv.Itoa(slaveOffset) + "\r\n"
}

// sourceReplInfo — INFO replication внешнего master-источника.
func sourceReplInfo(masterOffset int) string {
	return "# Replication\r\n" +
		"role:master\r\n" +
		"master_repl_offset:" + strconv.Itoa(masterOffset) + "\r\n"
}

const (
	selfAddr   = "127.0.0.1:6379"
	sourceAddr = "203.0.113.5:6379"
)

func applyOffset(t *testing.T, m *RedisModule, extra map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	params := map[string]any{
		"addr":            selfAddr,
		"source_addr":     sourceAddr,
		"password":        secretPass,
		"source_password": masterSourcePass,
	}
	for k, v := range extra {
		params[k] = v
	}
	stream := &applyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{State: "offset-synced", Params: mustStruct(t, params)}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return stream.final()
}

// TestApplyOffsetSynced_CaughtUp — link up + sync done + lag<=thr → caught_up=true,
// changed=false конструктивно. Главный happy-path safety-gate.
func TestApplyOffsetSynced_CaughtUp(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0), dbsize: 42}
	source := &offsetConn{infoReply: sourceReplInfo(1000), dbsize: 42}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true})
	if fin == nil || fin.Failed {
		t.Fatalf("ждали успех, got %+v", fin)
	}
	if fin.Changed {
		t.Error("offset-synced: changed обязан быть false конструктивно (probe)")
	}
	out := fin.GetOutput().GetFields()
	if !out["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=true (link up + sync done + lag 0)")
	}
	if got := out["lag_bytes"].GetNumberValue(); got != 0 {
		t.Errorf("lag_bytes=%v, ждали 0 (offset равны)", got)
	}
	if out["master_sync_in_progress"].GetBoolValue() {
		t.Error("master_sync_in_progress=true, ждали false")
	}
}

// TestApplyOffsetSynced_LagExceedsThreshold — lag > lag_threshold → caught_up=false
// (link up, sync done, но данные ещё не догнаны). Ключевой кейс gate.
func TestApplyOffsetSynced_LagExceedsThreshold(t *testing.T) {
	// master впереди на 500 байт, порог 100 → не догнал.
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1500)}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true, "lag_threshold": 100})
	out := fin.GetOutput().GetFields()
	if out["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=false: lag 500 > threshold 100")
	}
	if got := out["lag_bytes"].GetNumberValue(); got != 500 {
		t.Errorf("lag_bytes=%v, ждали 500", got)
	}
}

// TestApplyOffsetSynced_LagWithinThreshold — lag <= lag_threshold → caught_up=true
// (допуск отставания принят оператором через lag_threshold).
func TestApplyOffsetSynced_LagWithinThreshold(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1050)} // lag 50
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true, "lag_threshold": 100})
	if !fin.GetOutput().GetFields()["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=true: lag 50 <= threshold 100")
	}
}

// TestApplyOffsetSynced_SyncInProgress — master_sync_in_progress==1 → caught_up=false
// ДАЖЕ если offset сошлись (идёт full-sync, данные не консистентны).
func TestApplyOffsetSynced_SyncInProgress(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 1)} // sync идёт
	source := &offsetConn{infoReply: sourceReplInfo(1000)}      // offset сошлись
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true})
	out := fin.GetOutput().GetFields()
	if out["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=false: master_sync_in_progress==1 (идёт full-sync)")
	}
	if !out["master_sync_in_progress"].GetBoolValue() {
		t.Error("ждали master_sync_in_progress=true в Output")
	}
}

// TestApplyOffsetSynced_LinkDown — master_link_status:down → caught_up=false.
func TestApplyOffsetSynced_LinkDown(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "down", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true})
	if fin.GetOutput().GetFields()["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=false: master_link_status:down")
	}
}

// TestApplyOffsetSynced_ChecksumDBSize — !skip_checksum → DBSIZE обоих в Output.
func TestApplyOffsetSynced_ChecksumDBSize(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0), dbsize: 40}
	source := &offsetConn{infoReply: sourceReplInfo(1000), dbsize: 42}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, nil) // skip_checksum=false (default)
	out := fin.GetOutput().GetFields()
	if got := out["dbsize_source"].GetNumberValue(); got != 42 {
		t.Errorf("dbsize_source=%v, ждали 42", got)
	}
	if got := out["dbsize_replica"].GetNumberValue(); got != 40 {
		t.Errorf("dbsize_replica=%v, ждали 40", got)
	}
	// DBSIZE вызван на обоих коннектах.
	if !hasCall(source.calls, "DBSIZE") {
		t.Errorf("DBSIZE не вызван на источнике: %v", source.calls)
	}
	if !hasCall(self.calls, "DBSIZE") {
		t.Errorf("DBSIZE не вызван на реплике: %v", self.calls)
	}
}

// TestApplyOffsetSynced_SkipChecksum — skip_checksum=true → DBSIZE НЕ вызывается,
// dbsize-полей в Output нет.
func TestApplyOffsetSynced_SkipChecksum(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true})
	if hasCall(self.calls, "DBSIZE") || hasCall(source.calls, "DBSIZE") {
		t.Errorf("skip_checksum: DBSIZE не должен вызываться: self=%v source=%v", self.calls, source.calls)
	}
	if _, ok := fin.GetOutput().GetFields()["dbsize_source"]; ok {
		t.Error("skip_checksum: dbsize_source не должен быть в Output")
	}
}

// TestApplyOffsetSynced_SecondConnectUsesSourceSecrets — второй коннект уходит на
// source_addr с source_password (НЕ свой password). Доказывает изоляцию кредов.
func TestApplyOffsetSynced_SecondConnectUsesSourceSecrets(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	_ = applyOffset(t, m, map[string]any{"skip_checksum": true})
	if source.cfg.addr != sourceAddr {
		t.Errorf("второй коннект ушёл не на source_addr: %q", source.cfg.addr)
	}
	if source.cfg.password != masterSourcePass {
		t.Errorf("второй коннект не несёт source_password: %q", source.cfg.password)
	}
	if self.cfg.password != secretPass {
		t.Errorf("первый коннект не несёт свой password: %q", self.cfg.password)
	}
	if !source.closed {
		t.Error("второй коннект (source) не закрыт")
	}
}

// TestApplyOffsetSynced_SourceTLS_SecondConnectUsesTLS — source_tls=true:
// ВТОРОЙ коннект (к внешнему источнику) идёт по TLS с source_tls_ca, а коннект к
// СВОЕМУ инстансу — по своим tls-параметрам. Доказывает доведённый TLS-к-источнику
// на offset-synced: source-секреты и source-TLS изолированы от своих.
func TestApplyOffsetSynced_SourceTLS_SecondConnectUsesTLS(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	const sourceCA = "-----BEGIN CERTIFICATE-----\nSOURCE-CA-OFFSET\n-----END CERTIFICATE-----"
	_ = applyOffset(t, m, map[string]any{
		"skip_checksum": true,
		"source_tls":    true,
		"source_tls_ca": sourceCA,
	})

	// Второй коннект (источник) — TLS включён, CA источника проброшен.
	if !source.cfg.tls.enabled {
		t.Error("source_tls=true не доехал до второго коннекта (TLS к источнику не включён)")
	}
	if source.cfg.tls.caPEM != sourceCA {
		t.Errorf("source_tls_ca не доехал до второго коннекта: %q", source.cfg.tls.caPEM)
	}
	// Свой коннект НЕ должен унаследовать source-TLS (изоляция): tls не задан → false.
	if self.cfg.tls.enabled {
		t.Error("свой коннект ошибочно включил TLS из source_tls (нет изоляции)")
	}
}

// TestApplyOffsetSynced_OwnTLSIndependentOfSource — свой tls=true и source_tls=true
// читаются из РАЗНЫХ полей: свой коннект берёт tls/tls_ca, источник —
// source_tls/source_tls_ca. Доказывает раздельность двух TLS-контекстов.
func TestApplyOffsetSynced_OwnTLSIndependentOfSource(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	const ownCA = "-----BEGIN CERTIFICATE-----\nOWN-CA\n-----END CERTIFICATE-----"
	const srcCA = "-----BEGIN CERTIFICATE-----\nSRC-CA\n-----END CERTIFICATE-----"
	_ = applyOffset(t, m, map[string]any{
		"skip_checksum": true,
		"tls":           true,
		"tls_ca":        ownCA,
		"source_tls":    true,
		"source_tls_ca": srcCA,
	})

	if !self.cfg.tls.enabled || self.cfg.tls.caPEM != ownCA {
		t.Errorf("свой коннект: ждали tls с ownCA, got enabled=%v ca=%q", self.cfg.tls.enabled, self.cfg.tls.caPEM)
	}
	if !source.cfg.tls.enabled || source.cfg.tls.caPEM != srcCA {
		t.Errorf("коннект источника: ждали tls с srcCA, got enabled=%v ca=%q", source.cfg.tls.enabled, source.cfg.tls.caPEM)
	}
}

// TestApplyOffsetSynced_NoSourceTLS_SecondConnectPlaintext — без source_tls
// второй коннект plaintext (back-compat: tls=false → enabled=false).
func TestApplyOffsetSynced_NoSourceTLS_SecondConnectPlaintext(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	_ = applyOffset(t, m, map[string]any{"skip_checksum": true})
	if source.cfg.tls.enabled {
		t.Error("без source_tls второй коннект не должен включать TLS")
	}
}

// TestApplyOffsetSynced_NoSecretLeak — ни свой, ни source-пароль не утекают в
// события (ИБ-инвариант ADR-010).
func TestApplyOffsetSynced_NoSecretLeak(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0), dbsize: 5}
	source := &offsetConn{infoReply: sourceReplInfo(1000), dbsize: 5}
	m := offsetModule(selfAddr, self, source)

	params := map[string]any{
		"addr":            selfAddr,
		"source_addr":     sourceAddr,
		"password":        secretPass,
		"source_password": masterSourcePass,
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{State: "offset-synced", Params: mustStruct(t, params)}, stream)

	assertEventsNoSecret(t, stream)
	for i, ev := range stream.sent {
		if strings.Contains(ev.GetMessage(), masterSourcePass) {
			t.Errorf("событие[%d].Message несёт source_password: %q", i, ev.GetMessage())
		}
	}
}

// TestApplyOffsetSynced_SourceConnectFailure_DoesNotLeak — коннект к источнику
// упал, текст содержит source_password → санитизируется.
func TestApplyOffsetSynced_SourceConnectFailure_DoesNotLeak(t *testing.T) {
	self := &offsetConn{infoReply: selfReplInfo(1000, "up", 0)}
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			if cfg.addr == selfAddr {
				self.cfg = cfg
				return self, nil
			}
			return nil, errors.New("dial failed for AUTH " + cfg.password) // source-пароль в тексте
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "offset-synced",
		Params: mustStruct(t, map[string]any{
			"addr":            selfAddr,
			"source_addr":     sourceAddr,
			"password":        secretPass,
			"source_password": masterSourcePass,
		}),
	}, stream)

	fin := stream.final()
	if fin == nil || !fin.Failed {
		t.Fatalf("ждали failed=true на коннект-фейл источника, got %+v", fin)
	}
	assertEventsNoSecret(t, stream)
	for i, ev := range stream.sent {
		if strings.Contains(ev.GetMessage(), masterSourcePass) {
			t.Errorf("событие[%d].Message несёт source_password при коннект-фейле: %q", i, ev.GetMessage())
		}
	}
}

// TestApplyOffsetSynced_MissingOffset_NotCaughtUp — на своём INFO нет
// slave_repl_offset (нештатный ввод: addr не реплика) → caught_up=false, не паника.
func TestApplyOffsetSynced_MissingOffset_NotCaughtUp(t *testing.T) {
	self := &offsetConn{infoReply: "# Replication\r\nrole:master\r\nmaster_link_status:up\r\n"} // нет slave_repl_offset
	source := &offsetConn{infoReply: sourceReplInfo(1000)}
	m := offsetModule(selfAddr, self, source)

	fin := applyOffset(t, m, map[string]any{"skip_checksum": true})
	if fin.GetOutput().GetFields()["caught_up"].GetBoolValue() {
		t.Error("ждали caught_up=false: slave_repl_offset отсутствует (addr не реплика)")
	}
}
