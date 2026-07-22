package audit

import (
	"strings"
	"testing"
)

// TestBuildTaskExecutedPayload_BaseFields — base form: sid/apply_id/task_idx/
// plan_index/status present; without error/register_data those keys are not added.
func TestBuildTaskExecutedPayload_BaseFields(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID:       "h.local",
		ApplyID:   "apply-1",
		TaskIdx:   3,
		PlanIndex: 7,
		Status:    "TASK_STATUS_CHANGED",
	})
	if p["sid"] != "h.local" || p["apply_id"] != "apply-1" || p["task_idx"] != 3 {
		t.Errorf("base fields = %v", p)
	}
	if p["status"] != "TASK_STATUS_CHANGED" {
		t.Errorf("status = %v, want TASK_STATUS_CHANGED", p["status"])
	}
	for _, absent := range []string{"error", "register_data", "suppressed"} {
		if _, present := p[absent]; present {
			t.Errorf("payload unexpectedly carries %q: %v", absent, p[absent])
		}
	}
}

// TestBuildTaskExecutedPayload_PlanIndexEmitted — T3: the GLOBAL plan_index rides
// in the payload additively alongside the local task_idx (correlation key of a
// CHANGED task with the plan in auditpg.SelectChangedTaskKeys under
// staged/per-host-where). Under staged plan_index ≠ task_idx — both are set, the
// rollup keys on plan_index.
func TestBuildTaskExecutedPayload_PlanIndexEmitted(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 2, PlanIndex: 9, Status: "TASK_STATUS_CHANGED",
	})
	if p["plan_index"] != 9 {
		t.Errorf("plan_index = %v, want 9 (global sequential index)", p["plan_index"])
	}
	if p["task_idx"] != 2 {
		t.Errorf("task_idx = %v, want 2 (local, kept for observability)", p["task_idx"])
	}
}

// TestBuildTaskExecutedPayload_ErrorMessageForNonNoLog — error.message is set for a
// NON-no_log task (masking is on the write-path), code/module present.
func TestBuildTaskExecutedPayload_ErrorMessageForNonNoLog(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_FAILED",
		Error: &TaskExecutedError{Code: "E1", Module: "core.pkg.installed", Message: "boom"},
	})
	em, ok := p["error"].(map[string]any)
	if !ok {
		t.Fatalf("error type = %T", p["error"])
	}
	if em["message"] != "boom" || em["module"] != "core.pkg.installed" || em["code"] != "E1" {
		t.Errorf("error map = %v", em)
	}
}

// TestBuildTaskExecutedPayload_NoLogSuppression — a no_log task: error.message and
// register_data are suppressed, the suppressed:"no_log" marker is present. The root
// of suppressing an arbitrary secret leak (MaskSecrets by vault-ref won't catch it).
func TestBuildTaskExecutedPayload_NoLogSuppression(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_FAILED",
		NoLog:        true,
		Error:        &TaskExecutedError{Module: "core.vault.kv-read", Message: "plaintext secret"},
		RegisterData: `{"password":"hunter2"}`,
	})
	if p["suppressed"] != "no_log" {
		t.Errorf("suppressed = %v, want no_log", p["suppressed"])
	}
	if _, present := p["register_data"]; present {
		t.Errorf("register_data leaked for no_log: %v", p["register_data"])
	}
	em, ok := p["error"].(map[string]any)
	if !ok {
		t.Fatalf("error type = %T", p["error"])
	}
	if _, present := em["message"]; present {
		t.Errorf("error.message leaked for no_log: %v (must be suppressed)", em["message"])
	}
	if em["module"] != "core.vault.kv-read" {
		t.Errorf("error.module = %v, want core.vault.kv-read (module is not suppressed)", em["module"])
	}
}

// TestBuildTaskExecutedPayload_RegisterDataForNonNoLog — register_data is set for a
// NON-no_log task when the value is non-empty (a Soul-side protojson string).
func TestBuildTaskExecutedPayload_RegisterDataForNonNoLog(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_CHANGED",
		RegisterData: `{"changed":true}`,
	})
	if p["register_data"] != `{"changed":true}` {
		t.Errorf("register_data = %v, want {\"changed\":true}", p["register_data"])
	}
}

// TestBuildTaskExecutedPayload_NoParamsKey — security guard invariant:
// RenderedTask.Params (rendered task params, potentially carrying resolved Vault
// values) NEVER reach the task.executed audit payload.
//
// Structural barrier: TaskExecutedInput has no Params field (params are rendered
// Keeper-side and go Soul→ApplyRequest, but are NOT returned back in TaskEvent —
// the apply.proto TaskEvent message carries only task_idx/status/register_data/
// error/no_log). The test asserts that even with a maximally filled input no
// payload key equals or contains "param" — a regression (someone adds Params to
// input and threads it into the payload) is caught here.
func TestBuildTaskExecutedPayload_NoParamsKey(t *testing.T) {
	inputs := []TaskExecutedInput{
		{SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_CHANGED",
			RegisterData: `{"changed":true}`},
		{SID: "h", ApplyID: "a", TaskIdx: 1, Status: "TASK_STATUS_FAILED",
			Error: &TaskExecutedError{Code: "E", Module: "core.pkg.installed", Message: "boom"}},
		{SID: "h", ApplyID: "a", TaskIdx: 2, Status: "TASK_STATUS_FAILED",
			NoLog:        true,
			Error:        &TaskExecutedError{Module: "core.vault.kv-read", Message: "plaintext"},
			RegisterData: `{"password":"hunter2"}`},
	}
	for _, in := range inputs {
		p := BuildTaskExecutedPayload(in)
		assertNoParamKey(t, p)
	}
}

// assertNoParamKey checks that no level of the map payload has a key containing
// "param" (case-insensitive). Recurses into nested maps (payload["error"] is a
// nested map). The invariant is about the key, not the value: params must not
// appear as a payload field at all.
func assertNoParamKey(t *testing.T, m map[string]any) {
	t.Helper()
	for k, v := range m {
		if strings.Contains(strings.ToLower(k), "param") {
			t.Errorf("payload carries forbidden param-shaped key %q (RenderedTask.Params must never reach audit)", k)
		}
		if nested, ok := v.(map[string]any); ok {
			assertNoParamKey(t, nested)
		}
	}
}
