//go:build e2e_live

package e2e_live_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// TestL3bBootstrap_OneSoul - minimal smoke L3b-2: a real Bootstrap flow for
// one soul container. Checks:
//   - soul init (CSR -> Keeper.Bootstrap) completes without errors;
//   - soul run reaches souls.status='connected' (waitForSoulConnected inside
//     SpawnSoulContainer);
//   - the `soul.bootstrapped` audit event is recorded by the Keeper handler (this
//     is coverage L3a with a stub-soul doesn't give - there the Bootstrap RPC isn't
//     called).
//
// Further L3b slices (3+) will bring up a real apply (nginx etc.) on this infra;
// here we stop at onboarding.
func TestL3bBootstrap_OneSoul(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		// ExamplePath is not needed for L3b-2 (we don't run apply); the field stays
		// empty - git-seed is skipped by NewStack's default.
		Souls: 1,
	})
	defer stack.Cleanup()

	if got := len(stack.SoulContainers); got != 1 {
		t.Fatalf("expected 1 soul container, got %d", got)
	}
	sc := stack.SoulContainers[0]
	const wantSID = "soul-live-a.example.com"
	if sc.SID != wantSID {
		t.Errorf("SoulContainer.SID = %q, expected %q", sc.SID, wantSID)
	}

	// soul.bootstrapped is written by the keeper-side bootstrapHandler after
	// a successful COMMIT of the "burn token + insert seed + status flip" transaction.
	// The subset includes SID (a stable payload field).
	stack.AssertAuditEvent(t, "soul.bootstrapped", map[string]any{
		"sid": wantSID,
	})

	// Sanity-check: the souls row's connected status is visible directly (waitFor*
	// inside Spawn already checked it, this is an extra guarantee after the full-Cleanup
	// gate: the snapshot is taken before teardown).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var status string
	if err := stack.DB().QueryRow(ctx,
		"SELECT status FROM souls WHERE sid = $1", wantSID).Scan(&status); err != nil {
		t.Fatalf("SELECT souls.status: %v", err)
	}
	if status != "connected" {
		t.Errorf("souls.status = %q, expected connected", status)
	}
}
