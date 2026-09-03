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

// TestBuildTaskExecutedPayload_ErrorMessage — error.message is always set
// (masking is on the write path), code/module present. There is no per-task
// suppression switch any more ([ADR-0083] §8 removed `no_log:`).
func TestBuildTaskExecutedPayload_ErrorMessage(t *testing.T) {
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

// TestBuildTaskExecutedPayload_NoSuppressionMarker — the payload builder has no
// suppression switch left ([ADR-0083] §8 removed `no_log:`): whatever the caller
// hands over is written. error.message and register_data reach the payload, and
// the `suppressed` marker that used to stand in their place is gone.
//
// This is the load-bearing half of the replacement: masking now happens BEFORE
// this function is called — per output field from the module manifest
// (grpc.redactSecretOutput) and per param cell from the seal
// (audit.MaskSecretsSealed). A builder that dropped fields on its own would hide
// whether that masking actually ran.
func TestBuildTaskExecutedPayload_NoSuppressionMarker(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_FAILED",
		Error:        &TaskExecutedError{Module: "core.vault.kv-read", Message: "boom"},
		RegisterData: `{"data":"` + MaskedValue + `"}`,
	})
	if _, present := p["suppressed"]; present {
		t.Errorf("suppressed marker = %v, want it gone with no_log", p["suppressed"])
	}
	if p["register_data"] != `{"data":"`+MaskedValue+`"}` {
		t.Errorf("register_data = %v, want the caller's already-masked value passed through", p["register_data"])
	}
	em, ok := p["error"].(map[string]any)
	if !ok {
		t.Fatalf("error type = %T", p["error"])
	}
	if em["message"] != "boom" {
		t.Errorf("error.message = %v, want it written (no per-task suppression)", em["message"])
	}
	if em["module"] != "core.vault.kv-read" {
		t.Errorf("error.module = %v, want core.vault.kv-read", em["module"])
	}
}

// TestBuildTaskExecutedPayload_RegisterData — register_data is set when the
// value is non-empty (a Soul-side protojson string).
func TestBuildTaskExecutedPayload_RegisterData(t *testing.T) {
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
// error/secret_output). The test asserts that even with a maximally filled input no
// payload key equals or contains "param" — a regression (someone adds Params to
// input and threads it into the payload) is caught here.
func TestBuildTaskExecutedPayload_NoParamsKey(t *testing.T) {
	inputs := []TaskExecutedInput{
		{SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_CHANGED",
			RegisterData: `{"changed":true}`},
		{SID: "h", ApplyID: "a", TaskIdx: 1, Status: "TASK_STATUS_FAILED",
			Error: &TaskExecutedError{Code: "E", Module: "core.pkg.installed", Message: "boom"}},
		{SID: "h", ApplyID: "a", TaskIdx: 2, Status: "TASK_STATUS_FAILED",
			Error:        &TaskExecutedError{Module: "core.vault.kv-read", Message: "boom"},
			RegisterData: `{"password":"` + MaskedValue + `"}`},
		{SID: "h", ApplyID: "a", TaskIdx: 3, Status: "TASK_STATUS_CHANGED",
			Notices: []TaskExecutedNotice{{
				Code: "deprecated_param", Module: "redis.instance.pinged",
				Param: "address", Message: `param "address" is deprecated`,
			}}},
	}
	for _, in := range inputs {
		p := BuildTaskExecutedPayload(in)
		assertNoParamKey(t, p)
	}
}

// noticeParamPath — the single legitimate "param"-shaped key in the payload.
// TaskNotice.param (NIM-237) is the NAME of a manifest parameter, and the
// invariant below is about RenderedTask.Params — the VALUES, which may carry a
// secret. A name read out of a manifest carries none, and the operator cannot
// act on "something is deprecated" without being told what.
const noticeParamPath = "notices[].param"

// assertNoParamKey checks that no level of the map payload has a key containing
// "param" (case-insensitive), except [noticeParamPath]. The invariant is about
// the key, not the value: params must not appear as a payload field at all.
//
// Recursion covers nested maps AND lists of maps. It used to stop at maps, so a
// payload key that grew into a list of objects (notices did exactly that) would
// have been waved through unread — the guard would still pass while no longer
// guarding, which is worse than not having it.
func assertNoParamKey(t *testing.T, m map[string]any) {
	t.Helper()
	assertNoParamKeyAt(t, "", m)
}

func assertNoParamKeyAt(t *testing.T, path string, m map[string]any) {
	t.Helper()
	for k, v := range m {
		at := k
		if path != "" {
			at = path + "." + k
		}
		if strings.Contains(strings.ToLower(k), "param") && at != noticeParamPath {
			t.Errorf("payload carries forbidden param-shaped key %q at %q (RenderedTask.Params must never reach audit)", k, at)
		}
		switch nested := v.(type) {
		case map[string]any:
			assertNoParamKeyAt(t, at, nested)
		case []map[string]any:
			for _, item := range nested {
				assertNoParamKeyAt(t, at+"[]", item)
			}
		case []any:
			for _, item := range nested {
				if im, ok := item.(map[string]any); ok {
					assertNoParamKeyAt(t, at+"[]", im)
				}
			}
		}
	}
}

// Notices are rendered from the manifest (param name, versions, replacement) and
// never from task output or input values, so no masking applies to them. They
// travel beside a register_data whose secret fields the caller already masked —
// the point of the test is that the two are independent: nothing about a task
// handling secrets blinds the operator to a contract change on it, which is
// exactly what the old all-or-nothing `no_log:` did.
func TestBuildTaskExecutedPayload_NoticesTravelWithMaskedOutput(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_CHANGED",
		RegisterData: `{"password":"` + MaskedValue + `"}`,
		Notices: []TaskExecutedNotice{{
			Code: "deprecated_param", Module: "redis.instance.pinged",
			Param: "address", Message: `param "address" stops working in 0.6.0`,
		}},
	})

	if p["register_data"] != `{"password":"`+MaskedValue+`"}` {
		t.Fatalf("register_data = %v, want the masked value passed through untouched", p["register_data"])
	}
	got, ok := p["notices"].([]map[string]any)
	if !ok || len(got) != 1 {
		t.Fatalf("notices = %#v, want the one notice present", p["notices"])
	}
	if got[0]["param"] != "address" || got[0]["code"] != "deprecated_param" {
		t.Errorf("notice = %#v, want the param name and code intact", got[0])
	}
}

// The other direction: a task with nothing to report writes the payload it
// always wrote. An empty list would still be a new key for every consumer of
// task.executed, and the changed_tasks rollup reads this shape.
func TestBuildTaskExecutedPayload_NoNoticesKeyWhenSilent(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_OK",
	})
	if _, present := p["notices"]; present {
		t.Error("a task with no findings emitted a notices key")
	}
}
