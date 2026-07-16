//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/tests/e2e/internal/soulstub"
)

// TaskResponse — soul-stub's scripted response to one ApplyRequest task by
// task_name (success-only helper; a harness wrapper over
// soulstub.ScriptEntry so the test doesn't import internal/soulstub and the
// keeperv1 enum directly).
//
// StateChanges goes into RunResult.state_changes (a per-task artifact for
// register/drift); incarnation.state is mutated separately — by the
// keeper-side render of scenario.state_changes.sets AFTER the barrier
// (run.go section 8), NOT from RunResult. So StateChanges here documents
// the task's expected effect on the host but does not affect the
// incarnation_state assert.
type TaskResponse struct {
	TaskName     string
	StateChanges map[string]any
}

// LoadApplyScript loads the stub with scripted-success responses for the
// listed tasks (matched by task_name). Tasks not covered by the script
// (e.g. collectors.yml steps under when:, which are not actually executed
// at L3a) are caught by SetApplyDefaultSuccess — otherwise an unscripted
// task would produce FAILED. Mirrors
// stub-responses.yaml::scenarios.<name>.apply_responses, but loaded inline
// (a YAML-loader for fixtures is not implemented — the pilot pattern, as
// with hello-world).
func LoadApplyScript(stub *soulstub.Stub, scenario string, tasks []TaskResponse) {
	entries := make([]soulstub.ScriptEntry, 0, len(tasks))
	for _, t := range tasks {
		entries = append(entries, soulstub.ScriptEntry{
			TaskName:     t.TaskName,
			Status:       keeperv1.RunStatus_RUN_STATUS_SUCCESS,
			StateChanges: t.StateChanges,
		})
	}
	stub.LoadScript(map[string][]soulstub.ScriptEntry{scenario: entries})
	// Covers when:-tasks (collectors.yml) not in the scripted table: L3a
	// does not check per-task realism, only the apply_runs lifecycle matters.
	stub.SetApplyDefaultSuccess(true)
}

// ConnectSoulStub opens a live EventStream from soul-stub to the Keeper for
// the i-th pre-auth Soul (see Config.Souls / SoulSID). This turns a "row in
// souls with status=connected" into a real gRPC mTLS stream: on session-open
// the Keeper acquires the Redis SID lease, and dispatch (Errand/Apply) is
// routed to this SID's local Outbound. Without an open stream, dispatch
// returns ErrSoulNotConnected (errand -> spawn_error, apply -> orphaned).
//
// The stub responds to ApplyRequest with a scripted RunResult and to
// ErrandRequest with an ErrandResult of status SUCCESS (see
// soulstub.SetErrandStatus for other branches). Stream closure is
// registered in Stack.Cleanup (LIFO).
//
// Returns *soulstub.Stub — the caller can read Messages() / change the
// errand status before dispatch.
func (s *Stack) ConnectSoulStub(t *testing.T, soulIndex int) *soulstub.Stub {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("ConnectSoulStub(%d): out of range (%d souls created)", soulIndex, len(s.souls))
	}
	id := s.souls[soulIndex]

	stub := soulstub.New(id.SID, s.KeeperGRPCAddr, id.Cert, id.Key, s.caBundle)

	ctx, cancel := context.WithCancel(context.Background())
	if err := stub.Open(ctx); err != nil {
		cancel()
		t.Fatalf("ConnectSoulStub(%s): open stream: %v", id.SID, err)
	}
	s.cleanups = append(s.cleanups, func() {
		_ = stub.Close()
		cancel()
	})

	// Wait for HelloReply: the Keeper sends it AFTER acquiring the Redis
	// SID lease (eventstream.go: presence online = live lease acquired
	// before HelloReply). So seeing HelloReply in Messages() guarantees
	// dispatch can already route Errand/Apply to this SID's local Outbound.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range stub.Messages() {
			if m.Kind == "HelloReply" {
				return stub
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("ConnectSoulStub(%s): HelloReply not received within 10s (lease/handshake incomplete)", id.SID)
	return nil
}
