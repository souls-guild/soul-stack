package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// keeper.voyage.start answers a scenario target holding an error_locked
// incarnation with the code keeper.incarnation.run uses for the same state, not
// the not-cancellable code a 409 from cancel means (NIM-989).
func TestToolFromVoyageProblem_ConflictByType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, typ, want string
	}{
		{"incarnation locked", problem.TypeIncarnationLocked, mcpCodeIncarnationLocked},
		{"cancel conflict", problem.TypeErrandNotCancellable, mcpCodeErrandNotCancellable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			rec.WriteHeader(http.StatusConflict)
			_, _ = rec.WriteString(`{"type":"` + tc.typ + `","status":409,"detail":"refused"}`)
			resp := (&Handler{}).toolFromVoyageProblem([]byte(`1`), "keeper.voyage.start", rec, http.StatusConflict)
			if resp.Error == nil {
				t.Fatal("want a tool error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}
