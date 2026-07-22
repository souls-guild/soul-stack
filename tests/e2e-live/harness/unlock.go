//go:build e2e_live

package harness

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Unlock clears a blocking incarnation status (error_locked/migration_failed ->
// ready) via Operator API POST /v1/incarnations/{name}/unlock (ADR-009, 200 OK,
// reason required). Needed to re-run a scenario after an INTENTIONAL fail-closed
// run (module_not_allowed negative test): lockRun rejects runs while in
// error_locked (ErrLocked, run.go). State is unchanged (unlock only clears status).
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
