package auditpg

import (
	"strings"
	"testing"
)

// TestSelectChangedTaskKeysSQL_Filters — docker-free guard формы запроса свёртки
// changed (T3): ловит регресс, если имя JSONB-ключа status/sid/task_idx или
// колонки фильтра (correlation_id/event_type) изменится. Источник changed —
// строго `task.executed`+CHANGED; адресные поля — sid + task_idx, payload-
// значения (register_data) НЕ читаются (секрет-гигиена).
func TestSelectChangedTaskKeysSQL_Filters(t *testing.T) {
	for _, want := range []string{
		"correlation_id = $1",
		"event_type = $2",
		"payload->>'status' = $3",
		"payload->>'sid'",
		"payload->>'task_idx'",
		"FROM audit_log",
	} {
		if !strings.Contains(selectChangedTaskKeysSQL, want) {
			t.Errorf("selectChangedTaskKeysSQL missing %q", want)
		}
	}
	// Секрет-гигиена: register_data / params не должны читаться запросом.
	for _, forbidden := range []string{"register_data", "params", "'error'"} {
		if strings.Contains(selectChangedTaskKeysSQL, forbidden) {
			t.Errorf("selectChangedTaskKeysSQL reads payload value %q — secret hygiene violated", forbidden)
		}
	}
}

// TestTaskStatusChangedConst — статус-строка совпадает с keeperv1.TaskStatus
// CHANGED-именем (handler кладёт Status().String()). Жёстко прибиваем константу
// — рассинхрон с proto-enum молча обнулил бы свёртку.
func TestTaskStatusChangedConst(t *testing.T) {
	if taskStatusChanged != "TASK_STATUS_CHANGED" {
		t.Errorf("taskStatusChanged = %q, want TASK_STATUS_CHANGED", taskStatusChanged)
	}
}
