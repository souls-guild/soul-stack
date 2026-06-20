package auditpg

import (
	"strings"
	"testing"
)

// TestSelectTaskKeysByStatusSQL_Filters — docker-free guard формы запроса свёртки
// (T3 changed / R3 failed): ловит регресс, если имя JSONB-ключа status/sid/
// plan_index/task_idx или колонки фильтра (correlation_id/event_type) изменится.
// Источник — строго `task.executed`; status фильтруется по НАБОРУ имён (`= ANY($3)`,
// один SQL обслуживает CHANGED- и FAILED-выборки); адресные поля — sid +
// COALESCE(plan_index, task_idx) (глобальный ключ корреляции с планом, fallback на
// локальный для старых строк); payload-значения (register_data) НЕ читаются.
func TestSelectTaskKeysByStatusSQL_Filters(t *testing.T) {
	for _, want := range []string{
		"correlation_id = $1",
		"event_type = $2",
		"payload->>'status' = ANY($3)",
		"payload->>'sid'",
		// plan_index приоритетен (ключ корреляции с RenderedTask.Index под staged/
		// per-host); task_idx — fallback для строк до T3 (COALESCE).
		"COALESCE(payload->>'plan_index', payload->>'task_idx')",
		"payload->>'plan_index'",
		"payload->>'task_idx'",
		"FROM audit_log",
	} {
		if !strings.Contains(selectTaskKeysByStatusSQL, want) {
			t.Errorf("selectTaskKeysByStatusSQL missing %q", want)
		}
	}
	// Секрет-гигиена: register_data / params не должны читаться запросом.
	for _, forbidden := range []string{"register_data", "params", "'error'"} {
		if strings.Contains(selectTaskKeysByStatusSQL, forbidden) {
			t.Errorf("selectTaskKeysByStatusSQL reads payload value %q — secret hygiene violated", forbidden)
		}
	}
}

// TestTaskStatusConsts — статус-строки совпадают с keeperv1.TaskStatus-именами
// (handler кладёт Status().String()). Жёстко прибиваем константы — рассинхрон с
// proto-enum молча обнулил бы свёртку (changed) либо cross-passage onfail-gating
// (failed/timed_out, ADR-056 R3).
func TestTaskStatusConsts(t *testing.T) {
	cases := map[string]string{
		taskStatusChanged:  "TASK_STATUS_CHANGED",
		taskStatusFailed:   "TASK_STATUS_FAILED",
		taskStatusTimedOut: "TASK_STATUS_TIMED_OUT",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("status const = %q, want %q", got, want)
		}
	}
}
