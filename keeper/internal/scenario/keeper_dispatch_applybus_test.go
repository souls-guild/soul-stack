package scenario

// Guard invariants for ADR-068 §A2 (keeper-side task.executed → applybus): keeper-side
// `on: keeper` progress is visible on the operator SSE SYMMETRICALLY to Soul-side
// (grpc/events_taskevent.go), WITHOUT leaking secrets. Tests are white-box unit
// (real applybus.NewBus + subscriber), no PG/network.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// forbiddenSSEKeys — fields that must NEVER reach a keeper-side SSE frame
// (secret hygiene, ADR-068 §A2): vault-resolved output/register/stderr.
var forbiddenSSEKeys = []string{"output", "register_data", "register", "message"}

// publishAndCapture subscribes to the bus, publishes a keeper-side
// task.executed event, and returns the delivered event (or fails the test on timeout).
func publishAndCapture(t *testing.T, applyID string, passage int, rt *render.RenderedTask, changed, failed bool) applybus.Event {
	t.Helper()
	bus := applybus.NewBus(slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := bus.Subscribe(ctx, applyID)

	r := &Runner{deps: Deps{ApplyBus: bus}}
	r.publishKeeperTaskExecuted(applyID, passage, rt, changed, failed)

	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("applybus: keeper-side task.executed event not delivered")
		return applybus.Event{}
	}
}

// TestPublishKeeperTaskExecuted_ChangedSymmetry — guard (2): a changed keeper
// task is published with sid="keeper" and a payload shape symmetric to
// Soul-side (same key set and status literal).
func TestPublishKeeperTaskExecuted_ChangedSymmetry(t *testing.T) {
	rt := &render.RenderedTask{Index: 2, Module: "core.cloud.provisioned"}
	ev := publishAndCapture(t, "01APPLYCHANGED0000000000000", 0, rt, true, false)

	if ev.Kind != applybus.KindTaskExecuted {
		t.Fatalf("kind = %q, want %q", ev.Kind, applybus.KindTaskExecuted)
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type %T, want map[string]any", ev.Payload)
	}
	// sid=keeper — synthetic address of the keeper target (render.KeeperTargetSID).
	if got := p["sid"]; got != render.KeeperTargetSID {
		t.Errorf("sid = %v, want %q", got, render.KeeperTargetSID)
	}
	// task_idx / passage — int32 (type parity with Soul-side proto getters).
	if got := p["task_idx"]; got != int32(2) {
		t.Errorf("task_idx = %#v, want int32(2)", got)
	}
	if got := p["passage"]; got != int32(0) {
		t.Errorf("passage = %#v, want int32(0)", got)
	}
	// task_status — same literal as Soul-side (ev.GetStatus().String()).
	if got := p["task_status"]; got != "TASK_STATUS_CHANGED" {
		t.Errorf("task_status = %v, want TASK_STATUS_CHANGED", got)
	}
	if got := p["kind"]; got != string(applybus.KindTaskExecuted) {
		t.Errorf("kind field = %v, want %q", got, applybus.KindTaskExecuted)
	}
	// Exactly the key set Soul-side uses for a non-failed task (no error).
	wantKeys := map[string]bool{"apply_id": true, "kind": true, "sid": true, "task_idx": true, "task_status": true, "passage": true}
	for k := range p {
		if !wantKeys[k] {
			t.Errorf("extra key %q in changed-task payload", k)
		}
	}
	if _, has := p["error"]; has {
		t.Error("changed task must not carry error")
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_FailedSecretHygiene — guard (3): a failed
// keeper task carries ONLY error{code,module} (no message/output/register) —
// secret hygiene.
func TestPublishKeeperTaskExecuted_FailedSecretHygiene(t *testing.T) {
	rt := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read"}
	ev := publishAndCapture(t, "01APPLYFAILED00000000000000", 3, rt, false, true)

	p := ev.Payload.(map[string]any)
	if got := p["task_status"]; got != "TASK_STATUS_FAILED" {
		t.Errorf("task_status = %v, want TASK_STATUS_FAILED", got)
	}
	errObj, ok := p["error"].(map[string]any)
	if !ok {
		t.Fatalf("error type %T, want map[string]any", p["error"])
	}
	// error carries ONLY code+module (no message = stderr).
	if errObj["module"] != "core.vault.kv-read" {
		t.Errorf("error.module = %v, want core.vault.kv-read", errObj["module"])
	}
	if _, hasCode := errObj["code"]; !hasCode {
		t.Error("error must carry the code key (Soul-side symmetry)")
	}
	if _, hasMsg := errObj["message"]; hasMsg {
		t.Error("error.message (stderr) must not end up in the SSE frame (secret hygiene)")
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_NoLogSuppressed — guard: a no_log task carries
// a suppressed marker without register/output.
func TestPublishKeeperTaskExecuted_NoLogSuppressed(t *testing.T) {
	rt := &render.RenderedTask{Index: 0, Module: "core.soul.registered", NoLog: true}
	ev := publishAndCapture(t, "01APPLYNOLOG000000000000000", 0, rt, true, false)

	p := ev.Payload.(map[string]any)
	if got := p["suppressed"]; got != "no_log" {
		t.Errorf("suppressed = %v, want no_log", got)
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_NilBusNoop — guard: ApplyBus=nil → no-op
// without panicking (single-Keeper dev / unit without SSE), like Soul-side.
func TestPublishKeeperTaskExecuted_NilBusNoop(t *testing.T) {
	r := &Runner{deps: Deps{ApplyBus: nil}}
	// Must not panic.
	r.publishKeeperTaskExecuted("01APPLYNIL0000000000000000", 0, &render.RenderedTask{Index: 0, Module: "core.cloud.provisioned"}, true, false)
}

func assertNoSecretKeys(t *testing.T, p map[string]any) {
	t.Helper()
	for _, k := range forbiddenSSEKeys {
		if _, has := p[k]; has {
			t.Errorf("secret key %q must not end up in the keeper-side SSE frame", k)
		}
	}
}
