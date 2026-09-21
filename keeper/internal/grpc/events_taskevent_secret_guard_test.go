package grpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Security guard invariant (security audit, low/secrets): RenderedTask.Params and
// resolved Vault values NEVER reach observable channels (audit_log /
// SSE / logs). These tests pin that down on the full handleTaskEvent path (audit + SSE
// at once) — a regression is caught here.
//
// Structural foundation of the invariant: TaskEvent (Soul→Keeper, apply.proto) does NOT
// carry params at all — the keeper-side handler physically cannot forward them.
// Register output is the one channel that does carry module-produced values, and the
// fields a module declared secret are masked there per field ([ADR-0083] §8) before
// either channel sees the payload. These tests verify both facts at the handler→
// audit/SSE boundary.

// TestHandleTaskEvent_SecretOutputNeverReachesObservableChannels — a task whose
// module declared `password` a secret output ([ADR-0083] §8) returns it in
// register_data and fails with the same value in stderr. Neither observable channel
// carries the register plaintext: audit writes the field masked (the rest of the
// output survives), and SSE never publishes register_data at all, nor `message` for
// a failed task.
//
// What this test does NOT claim: that error.message is stripped. §8 stopped
// suppressing it per task — the barriers left on it are the write-path
// MaskSecrets/MaskSecretsSealed (applied in auditpg, past this fake writer) and the
// SSE floor asserted below. A module that prints its own credential to stderr is
// covered by neither, which is why the declaration is on the OUTPUT field.
func TestHandleTaskEvent_SecretOutputNeverReachesObservableChannels(t *testing.T) {
	const secret = "S3cr3t-PlainText-Password"

	aw := &recordingAudit{}
	bus := applybus.NewBus(discardLogger(t))
	h := newTestHandlerWithBusAudit(t, aw, bus)

	rd, err := structpb.NewStruct(map[string]any{"password": secret, "path": "secret/app/cfg"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	ev := &keeperv1.TaskEvent{
		ApplyId:      "01HAPPLY",
		TaskIdx:      0,
		Status:       keeperv1.TaskStatus_TASK_STATUS_FAILED,
		SecretOutput: []string{"password"},
		Error: &keeperv1.TaskError{
			Code: "module.failed", Module: "core.vault.kv-read",
			Message: "write rejected",
		},
		RegisterData: rd,
	}

	sseEv, ok := collectSSE(t, bus, "01HAPPLY", func() {
		h.handleTaskEvent(context.Background(), "host.example.com", "session-1", ev)
	})
	if !ok {
		t.Fatal("no SSE event published")
	}

	// Audit channel: the declared field is masked, the rest of the output is intact.
	auditEvents := aw.snapshot()
	if len(auditEvents) != 1 {
		t.Fatalf("audit events = %d, want 1", len(auditEvents))
	}
	auditBlob, _ := json.Marshal(auditEvents[0].Payload)
	if strings.Contains(string(auditBlob), secret) {
		t.Errorf("declared-secret output leaked into audit payload: %s", auditBlob)
	}
	rdMap := decodeRegisterData(t, auditEvents[0].Payload)
	if rdMap["password"] != audit.MaskedValue {
		t.Errorf("register_data.password = %v, want %q", rdMap["password"], audit.MaskedValue)
	}
	if rdMap["path"] != "secret/app/cfg" {
		t.Errorf("non-secret output field lost from audit: %v", rdMap)
	}

	// The live payload is untouched — the next task reads what this one produced.
	if got := ev.GetRegisterData().GetFields()["password"].GetStringValue(); got != secret {
		t.Errorf("live register payload was mutated: password = %q", got)
	}

	// SSE channel: no register_data, no stderr for a failed task.
	sseBlob, _ := json.Marshal(sseEv.Payload)
	if strings.Contains(string(sseBlob), secret) {
		t.Errorf("declared-secret output leaked into SSE frame: %s", sseBlob)
	}
	ssePayload, _ := sseEv.Payload.(map[string]any)
	if _, present := ssePayload["register_data"]; present {
		t.Errorf("SSE frame must never carry register_data: %v", ssePayload)
	}
	if errMap, ok := ssePayload["error"].(map[string]any); ok {
		if _, present := errMap["message"]; present {
			t.Errorf("SSE floor broken: a failed task published stderr: %v", errMap)
		}
	}
}

// TestHandleTaskEvent_NoParamsKeyInAnyChannel — a structural guard: on the full
// handler→audit/SSE path, no channel ever produces a key containing
// "param". TaskEvent doesn't carry params (apply.proto), so there's no source —
// this test catches a regression if someone routes params into audit/SSE.
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

// assertNoParamKeyAnyLevel — recursively checks for the absence of a param-shaped key
// in a map payload (case-insensitive "param"). channel is for diagnostics.
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

// newTestHandlerWithBusAudit — a handler with both ApplyBus AND recording-audit wired up
// for guard tests of the full handleTaskEvent path (both observable channels at once).
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

// decodeRegisterData returns the audit payload's register_data, which travels as
// the protojson text of the register Struct rather than a nested map.
func decodeRegisterData(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, ok := payload["register_data"].(string)
	if !ok {
		t.Fatalf("audit register_data type = %T, want string (§8 masks the field, it does not drop the block)", payload["register_data"])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("register_data is not JSON: %v (%q)", err, raw)
	}
	return out
}
