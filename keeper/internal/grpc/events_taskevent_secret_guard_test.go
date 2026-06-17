package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// Security guard-инвариант (ИБ-аудит, low/secrets): RenderedTask.Params и
// resolved-Vault-значения НИКОГДА не попадают в наблюдаемые каналы (audit_log /
// SSE / логи). Тесты фиксируют это на полном пути handleTaskEvent (audit + SSE
// одновременно) — регресс ловится здесь.
//
// Структурный фундамент инварианта: TaskEvent (Soul→Keeper, apply.proto) НЕ
// несёт params вовсе — keeper-side handler физически не может их транслировать.
// А register_data/error.message no_log-задачи (единственный остаточный канал
// произвольного секрета, который MaskSecrets по vault-ref не ловит) подавляются
// формой payload до записи. Эти тесты проверяют оба факта на стыке handler→
// audit/SSE.

// TestHandleTaskEvent_NoLogSecretNeverReachesObservableChannels — no_log-задача
// с секретом в register_data И в error.message: ни audit-payload, ни SSE-фрейм
// не несут plaintext. В audit вместо них маркер suppressed:"no_log"; в SSE
// register не публикуется никогда, error без message.
func TestHandleTaskEvent_NoLogSecretNeverReachesObservableChannels(t *testing.T) {
	const secret = "S3cr3t-PlainText-Password"

	aw := &recordingAudit{}
	bus := applybus.NewBus(discardLogger(t))
	h := newTestHandlerWithBusAudit(t, aw, bus)

	rd, err := structpb.NewStruct(map[string]any{"password": secret})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	ev := &keeperv1.TaskEvent{
		ApplyId: "01HAPPLY",
		TaskIdx: 0,
		Status:  keeperv1.TaskStatus_TASK_STATUS_FAILED,
		NoLog:   true,
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.vault.kv-read",
			Message: "leaked " + secret,
		},
		RegisterData: rd,
	}

	sseEv, ok := collectSSE(t, bus, "01HAPPLY", func() {
		h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)
	})
	if !ok {
		t.Fatal("no SSE event published")
	}

	// Audit-канал: payload не несёт секрет и не несёт register-ключ.
	auditEvents := aw.snapshot()
	if len(auditEvents) != 1 {
		t.Fatalf("audit events = %d, want 1", len(auditEvents))
	}
	auditBlob, _ := json.Marshal(auditEvents[0].Payload)
	if strings.Contains(string(auditBlob), secret) {
		t.Errorf("no_log secret leaked into audit payload: %s", auditBlob)
	}
	if _, present := auditEvents[0].Payload["register_data"]; present {
		t.Errorf("no_log task must not carry register_data in audit: %v", auditEvents[0].Payload)
	}
	if auditEvents[0].Payload["suppressed"] != "no_log" {
		t.Errorf("no_log marker missing in audit: %v", auditEvents[0].Payload)
	}

	// SSE-канал: фрейм не несёт секрет, register и error.message.
	sseBlob, _ := json.Marshal(sseEv.Payload)
	if strings.Contains(string(sseBlob), secret) {
		t.Errorf("no_log secret leaked into SSE frame: %s", sseBlob)
	}
	ssePayload, _ := sseEv.Payload.(map[string]any)
	if _, present := ssePayload["register_data"]; present {
		t.Errorf("SSE frame must never carry register_data: %v", ssePayload)
	}
}

// TestHandleTaskEvent_NoParamsKeyInAnyChannel — структурный guard: на полном
// пути handler→audit/SSE ни в одном канале не возникает ключа, содержащего
// "param". TaskEvent params не несёт (apply.proto), поэтому источника нет —
// тест ловит регресс, если кто-то проложит params в audit/SSE.
func TestHandleTaskEvent_NoParamsKeyInAnyChannel(t *testing.T) {
	aw := &recordingAudit{}
	bus := applybus.NewBus(discardLogger(t))
	h := newTestHandlerWithBusAudit(t, aw, bus)

	rd, err := structpb.NewStruct(map[string]any{"changed": true})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	ev := &keeperv1.TaskEvent{
		ApplyId:      "01HAPPLY",
		TaskIdx:      1,
		Status:       keeperv1.TaskStatus_TASK_STATUS_CHANGED,
		RegisterData: rd,
	}

	sseEv, ok := collectSSE(t, bus, "01HAPPLY", func() {
		h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)
	})
	if !ok {
		t.Fatal("no SSE event published")
	}

	auditEvents := aw.snapshot()
	if len(auditEvents) != 1 {
		t.Fatalf("audit events = %d, want 1", len(auditEvents))
	}
	assertNoParamKeyAnyLevel(t, "audit", auditEvents[0].Payload)
	if ssePayload, ok := sseEv.Payload.(map[string]any); ok {
		assertNoParamKeyAnyLevel(t, "sse", ssePayload)
	}
}

// assertNoParamKeyAnyLevel — рекурсивная проверка отсутствия param-shaped ключа
// в map-payload-е (case-insensitive «param»). channel — для диагностики.
func assertNoParamKeyAnyLevel(t *testing.T, channel string, m map[string]any) {
	t.Helper()
	for k, v := range m {
		if strings.Contains(strings.ToLower(k), "param") {
			t.Errorf("%s channel carries forbidden param-shaped key %q (RenderedTask.Params must never reach observable channels)", channel, k)
		}
		if nested, ok := v.(map[string]any); ok {
			assertNoParamKeyAnyLevel(t, channel, nested)
		}
	}
}

// newTestHandlerWithBusAudit — handler с подключёнными ApplyBus И recording-audit
// для guard-тестов полного пути handleTaskEvent (оба наблюдаемых канала сразу).
func newTestHandlerWithBusAudit(t *testing.T, aw *recordingAudit, bus *applybus.EventBus) *eventStreamHandler {
	t.Helper()
	deps := EventStreamDeps{
		SeedDB:      &fakeSeedDB{},
		AuditWriter: aw,
		KID:         "kid-test",
		ApplyBus:    bus,
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}
