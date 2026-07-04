//go:build e2e_live

package harness

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Unlock снимает блокирующий статус инкарнации (error_locked/migration_failed →
// ready) через Operator API POST /v1/incarnations/{name}/unlock (ADR-009, 200 OK,
// reason required). Нужен, чтобы после НАМЕРЕННОГО fail-closed-прогона (негатив
// module_not_allowed) перезапустить сценарий: lockRun отклоняет запуск из
// error_locked (ErrLocked, run.go). state не меняется (unlock — только статус).
func (s *Stack) Unlock(t *testing.T, incarnationName, reason string) {
	t.Helper()
	c := s.opClient(t)
	path := fmt.Sprintf("/v1/incarnations/%s/unlock", incarnationName)
	resp, status, err := c.post(context.Background(), path, map[string]any{"reason": reason})
	if err != nil {
		t.Fatalf("Unlock %s: http: %v", incarnationName, err)
	}
	if status != http.StatusOK {
		t.Fatalf("Unlock %s: status %d, body=%s", incarnationName, status, string(resp))
	}
}
