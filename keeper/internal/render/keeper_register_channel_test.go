package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// The keeper register channel (ADR-056 Slice 2, [RenderInput.KeeperRegister])
// carries the register results of keeper-side tasks from previous Passages.
//
// ★ The invariant CHANGED in NIM-698. Slice 2 kept the channel fully isolated
// from host tasks: hostRegister with an empty per-host bucket fell back to the
// flat in.Register and never to the keeper bucket, so a host could not read
// `register.<keeper-task>.*` at all. [ADR-0083] §5 needs exactly that read — a
// service state field carrying declared secrets is written by a keeper-side task
// (`core.state.present`), and the Soul-side task that consumes the result reads
// `register.<name>.effective` off it.
//
// What Slice 2 was protecting against was the FALLBACK, not the visibility: an
// empty per-host bucket used to be replaced WHOLESALE by the keeper bucket, so
// `register.<name>` on a host could resolve to a keeper task's data by accident.
// The union keeps that from coming back — the host's own bucket wins every
// collision — and the tests below pin both halves.
func TestKeeperRegisterChannel_KeeperVisibleToHosts(t *testing.T) {
	in := RenderInput{
		Register:       map[string]any{"hostprobe": map[string]any{"stdout": "ok"}},
		KeeperRegister: map[string]any{"redis_users": map[string]any{"effective": []any{"alice"}}},
	}

	// A keeper task sees KeeperRegister (NOT the flat Register) — unchanged.
	kv := keeperVars(in)
	if _, ok := kv.Register["redis_users"]; !ok {
		t.Errorf("keeperVars.Register = %v, want it to contain keeper-register 'redis_users'", kv.Register)
	}
	if _, ok := kv.Register["hostprobe"]; ok {
		t.Errorf("keeperVars.Register leaked host-register 'hostprobe' -- channel not isolated: %v", kv.Register)
	}

	// A host task now READS the keeper register ([ADR-0083] §5): without this the
	// scenario that writes users.acl has no way to reach what core.state.present
	// wrote.
	host := &topology.HostFacts{SID: "host-a.example.com"}
	hr := hostRegister(in, host)
	if _, ok := hr["redis_users"]; !ok {
		t.Fatalf("hostRegister = %v, want the keeper register 'redis_users' visible to a host task", hr)
	}
	// The flat Register is NOT unioned in: it is the fallback for callers that set
	// nothing else (trial/push), not a third source.
	if _, ok := hr["hostprobe"]; ok {
		t.Errorf("hostRegister = %v, want the flat Register used only as a fallback", hr)
	}
}

// TestKeeperRegisterChannel_PerHostBucketWins — a name present in BOTH buckets
// resolves to the host's own. A register name is unique within a scenario, so a
// collision means something is already wrong; the host's own probe result is the
// more specific fact, and shadowing it would silently change what `where:` sees.
func TestKeeperRegisterChannel_PerHostBucketWins(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	in := RenderInput{
		Register:       map[string]any{"flat": map[string]any{"v": 1}},
		KeeperRegister: map[string]any{"role": map[string]any{"stdout": "keeper-side"}, "provision": map[string]any{"ip": "10.0.0.7"}},
		RegisterByHost: map[string]map[string]any{
			"host-a.example.com": {"role": map[string]any{"stdout": "master"}},
		},
	}
	hr := hostRegister(in, host)
	role, _ := hr["role"].(map[string]any)
	if role["stdout"] != "master" {
		t.Errorf("hostRegister['role'] = %v, want the host's own bucket to win the collision", hr["role"])
	}
	if _, ok := hr["provision"]; !ok {
		t.Errorf("hostRegister = %v, want the non-colliding keeper register still present", hr)
	}
	if _, ok := hr["flat"]; ok {
		t.Errorf("hostRegister = %v, want the flat Register NOT unioned in when a bucket exists", hr)
	}
}

// TestKeeperRegisterChannel_FlatFallbackUnchanged — ★ the guard that keeps the
// pre-NIM-698 shape for every caller that sets only the flat Register (Trial,
// push, unit callers): with both buckets empty, hostRegister returns in.Register
// itself, bit-for-bit.
func TestKeeperRegisterChannel_FlatFallbackUnchanged(t *testing.T) {
	flat := map[string]any{"prev": map[string]any{"out": "x"}}
	in := RenderInput{Register: flat}
	host := &topology.HostFacts{SID: "host-a.example.com"}

	hr := hostRegister(in, host)
	if _, ok := hr["prev"]; !ok {
		t.Fatalf("hostRegister = %v, want the flat Register when both buckets are empty", hr)
	}
	// The same map, not a copy: nothing was allocated on the untouched path.
	if len(hr) != len(flat) {
		t.Errorf("hostRegister has %d keys, want the flat Register's %d", len(hr), len(flat))
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
