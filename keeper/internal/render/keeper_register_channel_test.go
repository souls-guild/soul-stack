package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// TestKeeperRegisterChannel_Isolated — ★ HOST-FALLBACK GUARD for Slice 2
// (handoff flag from the Slice 1 review). keeper→keeper register chaining
// pours previous Passages' keeper-task registers into an ISOLATED
// RenderInput.KeeperRegister channel. Invariant:
//   - keeperVars sees KeeperRegister (a keeper task in the active Passage
//     reads register.<prev>.* from keeper tasks of past Passages);
//   - hostRegister with an EMPTY per-host bucket does NOT see KeeperRegister,
//     stays on the flat in.Register — a host task in a mixed Passage must
//     NOT accidentally read the keeper register via fallback (otherwise a
//     host would get register.provision.* from a keeper task it never
//     referenced).
//
// Before the channels were split (Slice 1 poured the keeper bucket into the
// flat Register, read by BOTH keeperVars and host-fallback), this test would
// have caught the leak.
func TestKeeperRegisterChannel_Isolated(t *testing.T) {
	keeperReg := map[string]any{
		"provision": map[string]any{"ip": "10.0.0.7", "changed": true},
	}
	flatReg := map[string]any{
		"hostprobe": map[string]any{"stdout": "ok"},
	}

	in := RenderInput{
		Register:       flatReg,
		KeeperRegister: keeperReg,
	}

	// A keeper task sees KeeperRegister (NOT the flat Register).
	kv := keeperVars(in)
	if _, ok := kv.Register["provision"]; !ok {
		t.Errorf("keeperVars.Register = %v, want it to contain keeper-register 'provision'", kv.Register)
	}
	if _, ok := kv.Register["hostprobe"]; ok {
		t.Errorf("keeperVars.Register leaked host-register 'hostprobe' -- channel not isolated: %v", kv.Register)
	}

	// A host task with an EMPTY per-host bucket → falls back to the flat
	// Register, NOT to KeeperRegister. The keeper register doesn't leak to it.
	host := &topology.HostFacts{SID: "host-a.example.com"}
	hr := hostRegister(in, host)
	if _, ok := hr["provision"]; ok {
		t.Fatalf("* hostRegister leaked keeper-register 'provision' via fallback -- host would accidentally read register.provision.* (handoff flag): %v", hr)
	}
	if _, ok := hr["hostprobe"]; !ok {
		t.Errorf("hostRegister = %v, want the flat Register (fallback 'hostprobe') when per-host bucket is empty", hr)
	}
}

// TestKeeperRegisterChannel_PerHostBucketWins — a host task with its OWN
// per-host bucket takes that (not KeeperRegister, not the flat Register): the
// keeper channel doesn't override the host's real per-host register.
func TestKeeperRegisterChannel_PerHostBucketWins(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	in := RenderInput{
		Register:       map[string]any{"flat": map[string]any{"v": 1}},
		KeeperRegister: map[string]any{"provision": map[string]any{"ip": "10.0.0.7"}},
		RegisterByHost: map[string]map[string]any{
			"host-a.example.com": {"role": map[string]any{"stdout": "master"}},
		},
	}
	hr := hostRegister(in, host)
	if _, ok := hr["role"]; !ok {
		t.Errorf("hostRegister = %v, want per-host bucket ('role')", hr)
	}
	if _, ok := hr["provision"]; ok {
		t.Errorf("hostRegister leaked keeper-register 'provision' over the per-host bucket: %v", hr)
	}
}

// TestKeeperVars_FallbackToFlatRegister — backward-compat: KeeperRegister
// empty (P0 / N=1 / not staged / host-only Passage) → keeperVars degrades to
// the flat Register (trial/push/other callers that only set Register see the
// register the same way, BIT-FOR-BIT).
func TestKeeperVars_FallbackToFlatRegister(t *testing.T) {
	in := RenderInput{
		Register: map[string]any{"prev": map[string]any{"out": "x"}},
		// KeeperRegister == nil
	}
	kv := keeperVars(in)
	if _, ok := kv.Register["prev"]; !ok {
		t.Errorf("keeperVars.Register = %v, want fallback to the flat Register ('prev') when KeeperRegister is empty", kv.Register)
	}
}
