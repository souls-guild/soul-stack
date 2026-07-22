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

// Security guard invariant (security audit, low/secrets): RenderedTask.Params and
// resolved Vault values NEVER reach observable channels (audit_log /
// SSE / logs). These tests pin that down on the full handleTaskEvent path (audit + SSE
// at once) — a regression is caught here.
//
// Structural foundation of the invariant: TaskEvent (Soul→Keeper, apply.proto) does NOT
// carry params at all — the keeper-side handler physically cannot forward them.
// And a no_log task's register_data/error.message (the only remaining channel for
// an arbitrary secret that MaskSecrets can't catch by vault-ref) are suppressed
// by the payload shape before writing. These tests verify both facts at the handler→
// audit/SSE.

// TestHandleTaskEvent_NoLogSecretNeverReachesObservableChannels — a no_log task
// with a secret in register_data AND in error.message: neither the audit payload nor the SSE frame
// carries plaintext. audit gets a suppressed:"no_log" marker instead; SSE
// never publishes register, and error has no message.
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

	// Audit channel: the payload carries neither the secret nor the register key.
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

	// SSE channel: the frame carries neither the secret, register, nor error.message.
	sseBlob, _ := json.Marshal(sseEv.Payload)
	if strings.Contains(string(sseBlob), secret) {
		t.Errorf("no_log secret leaked into SSE frame: %s", sseBlob)
	}
	ssePayload, _ := sseEv.Payload.(map[string]any)
	if _, present := ssePayload["register_data"]; present {
		t.Errorf("SSE frame must never carry register_data: %v", ssePayload)
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
