package scenario

// Guard-инварианты ADR-068 §A2 (keeper-side task.executed → applybus): keeper-side
// прогресс `on: keeper` виден на operator-SSE СИММЕТРИЧНО Soul-side
// (grpc/events_taskevent.go), БЕЗ утечки секретов. Тесты — white-box unit (real
// applybus.NewBus + subscriber), без PG/сети.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// forbiddenSSEKeys — поля, которые НИКОГДА не должны попасть в keeper-side SSE-frame
// (секрет-гигиена ADR-068 §A2): vault-резолвленный output/register/stderr.
var forbiddenSSEKeys = []string{"output", "register_data", "register", "message"}

// publishAndCapture подписывается на шину, публикует keeper-side task.executed и
// возвращает доставленное событие (или валит тест на таймауте).
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
		t.Fatal("applybus: событие keeper-side task.executed не доставлено")
		return applybus.Event{}
	}
}

// TestPublishKeeperTaskExecuted_ChangedSymmetry — guard (2): changed keeper-задача
// публикуется с sid="keeper" и формой payload, симметричной Soul-side (тот же набор
// ключей и литерал статуса).
func TestPublishKeeperTaskExecuted_ChangedSymmetry(t *testing.T) {
	rt := &render.RenderedTask{Index: 2, Module: "core.cloud.provisioned"}
	ev := publishAndCapture(t, "01APPLYCHANGED0000000000000", 0, rt, true, false)

	if ev.Kind != applybus.KindTaskExecuted {
		t.Fatalf("kind = %q, want %q", ev.Kind, applybus.KindTaskExecuted)
	}
	p, ok := ev.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload тип %T, want map[string]any", ev.Payload)
	}
	// sid=keeper — синтетический адрес keeper-target-а (render.KeeperTargetSID).
	if got := p["sid"]; got != render.KeeperTargetSID {
		t.Errorf("sid = %v, want %q", got, render.KeeperTargetSID)
	}
	// task_idx / passage — int32 (паритет типов с Soul-side proto-getter-ами).
	if got := p["task_idx"]; got != int32(2) {
		t.Errorf("task_idx = %#v, want int32(2)", got)
	}
	if got := p["passage"]; got != int32(0) {
		t.Errorf("passage = %#v, want int32(0)", got)
	}
	// task_status — тот же литерал, что Soul-side (ev.GetStatus().String()).
	if got := p["task_status"]; got != "TASK_STATUS_CHANGED" {
		t.Errorf("task_status = %v, want TASK_STATUS_CHANGED", got)
	}
	if got := p["kind"]; got != string(applybus.KindTaskExecuted) {
		t.Errorf("kind-поле = %v, want %q", got, applybus.KindTaskExecuted)
	}
	// Ровно тот набор ключей, что Soul-side для не-упавшей задачи (без error).
	wantKeys := map[string]bool{"apply_id": true, "kind": true, "sid": true, "task_idx": true, "task_status": true, "passage": true}
	for k := range p {
		if !wantKeys[k] {
			t.Errorf("лишний ключ %q в payload changed-задачи", k)
		}
	}
	if _, has := p["error"]; has {
		t.Error("changed-задача не должна нести error")
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_FailedSecretHygiene — guard (3): упавшая keeper-задача
// несёт ТОЛЬКО error{code,module} (без message/output/register) — секрет-гигиена.
func TestPublishKeeperTaskExecuted_FailedSecretHygiene(t *testing.T) {
	rt := &render.RenderedTask{Index: 1, Module: "core.vault.kv-read"}
	ev := publishAndCapture(t, "01APPLYFAILED00000000000000", 3, rt, false, true)

	p := ev.Payload.(map[string]any)
	if got := p["task_status"]; got != "TASK_STATUS_FAILED" {
		t.Errorf("task_status = %v, want TASK_STATUS_FAILED", got)
	}
	errObj, ok := p["error"].(map[string]any)
	if !ok {
		t.Fatalf("error тип %T, want map[string]any", p["error"])
	}
	// error несёт ТОЛЬКО code+module (без message = stderr).
	if errObj["module"] != "core.vault.kv-read" {
		t.Errorf("error.module = %v, want core.vault.kv-read", errObj["module"])
	}
	if _, hasCode := errObj["code"]; !hasCode {
		t.Error("error должен нести ключ code (симметрия Soul-side)")
	}
	if _, hasMsg := errObj["message"]; hasMsg {
		t.Error("error.message (stderr) не должен попадать в SSE-frame (секрет-гигиена)")
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_NoLogSuppressed — guard: no_log-задача несёт маркер
// suppressed без register/output.
func TestPublishKeeperTaskExecuted_NoLogSuppressed(t *testing.T) {
	rt := &render.RenderedTask{Index: 0, Module: "core.soul.registered", NoLog: true}
	ev := publishAndCapture(t, "01APPLYNOLOG000000000000000", 0, rt, true, false)

	p := ev.Payload.(map[string]any)
	if got := p["suppressed"]; got != "no_log" {
		t.Errorf("suppressed = %v, want no_log", got)
	}
	assertNoSecretKeys(t, p)
}

// TestPublishKeeperTaskExecuted_NilBusNoop — guard: ApplyBus=nil → no-op без паники
// (single-Keeper dev / unit без SSE), как Soul-side.
func TestPublishKeeperTaskExecuted_NilBusNoop(t *testing.T) {
	r := &Runner{deps: Deps{ApplyBus: nil}}
	// Не должно паниковать.
	r.publishKeeperTaskExecuted("01APPLYNIL0000000000000000", 0, &render.RenderedTask{Index: 0, Module: "core.cloud.provisioned"}, true, false)
}

func assertNoSecretKeys(t *testing.T, p map[string]any) {
	t.Helper()
	for _, k := range forbiddenSSEKeys {
		if _, has := p[k]; has {
			t.Errorf("секрет-ключ %q не должен попадать в keeper-side SSE-frame", k)
		}
	}
}
