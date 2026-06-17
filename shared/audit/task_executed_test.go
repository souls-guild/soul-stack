package audit

import (
	"strings"
	"testing"
)

// TestBuildTaskExecutedPayload_BaseFields — базовая форма: sid/apply_id/task_idx/
// status присутствуют; без error/register_data ключи не добавляются.
func TestBuildTaskExecutedPayload_BaseFields(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID:     "h.local",
		ApplyID: "apply-1",
		TaskIdx: 3,
		Status:  "TASK_STATUS_CHANGED",
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

// TestBuildTaskExecutedPayload_ErrorMessageForNonNoLog — error.message кладётся
// для НЕ-no_log задачи (маскинг — на write-path), code/module присутствуют.
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

// TestBuildTaskExecutedPayload_NoLogSuppression — no_log-задача: error.message и
// register_data подавлены, маркер suppressed:"no_log" присутствует. Корень
// подавления утечки произвольного секрета (MaskSecrets по vault-ref его не ловит).
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
		t.Errorf("error.module = %v, want core.vault.kv-read (module не подавляется)", em["module"])
	}
}

// TestBuildTaskExecutedPayload_RegisterDataForNonNoLog — register_data кладётся
// для НЕ-no_log при непустом значении (Soul-side protojson-строка).
func TestBuildTaskExecutedPayload_RegisterDataForNonNoLog(t *testing.T) {
	p := BuildTaskExecutedPayload(TaskExecutedInput{
		SID: "h", ApplyID: "a", TaskIdx: 0, Status: "TASK_STATUS_CHANGED",
		RegisterData: `{"changed":true}`,
	})
	if p["register_data"] != `{"changed":true}` {
		t.Errorf("register_data = %v, want {\"changed\":true}", p["register_data"])
	}
}

// TestBuildTaskExecutedPayload_NoParamsKey — security guard-инвариант:
// RenderedTask.Params (рендеренные параметры задачи, потенциально несущие
// resolved-Vault-значения) НИКОГДА не попадают в audit-payload task.executed.
//
// Структурный barrier: TaskExecutedInput не имеет поля Params (params рендерятся
// Keeper-side и едут Soul→в ApplyRequest, но обратно в TaskEvent НЕ возвращаются —
// apply.proto message TaskEvent несёт только task_idx/status/register_data/error/
// no_log). Тест фиксирует, что даже при максимально заполненном input ни один
// ключ payload не равен и не содержит "param" — регрессия (кто-то добавит
// Params в input и проложит его в payload) будет поймана здесь.
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

// assertNoParamKey проверяет, что ни на одном уровне map-payload-а нет ключа,
// содержащего "param" (case-insensitive). Рекурсивно обходит вложенные map-ы
// (payload["error"] — вложенный map). Адрес инварианта — ключ, а не значение:
// params не должны появляться как поле payload вообще.
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
