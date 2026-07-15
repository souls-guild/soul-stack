package render

import (
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
)

// TestStateChangesVars_KeeperRegisterVisible — ★ GUARD for the live
// provisioned_vm_ids bug (ADR-056 amendment 2026-07-02). register of keeper-side
// tasks (on: keeper) lives in the bucket RegisterByHost[KeeperTargetSID];
// state_changes-scope must see it as a run-level underlay — otherwise
// `${ register.<keeper-task>.* }` in sets → "no such key" → error_locked AFTER a
// successful deploy.
func TestStateChangesVars_KeeperRegisterVisible(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	in := RenderInput{
		RegisterByHost: map[string]map[string]any{
			KeeperTargetSID: {"provision": map[string]any{"vm_ids": []any{"vm-1", "vm-2"}}},
			host.SID:        {"probe": map[string]any{"stdout": "master"}},
		},
	}

	vars := stateChangesVars(in, host)
	if _, ok := vars.Register["provision"]; !ok {
		t.Errorf("★ stateChangesVars.Register = %v, want keeper-register 'provision' (run-level подложка state_changes-scope)", vars.Register)
	}
	if _, ok := vars.Register["probe"]; !ok {
		t.Errorf("stateChangesVars.Register = %v, want per-host register 'probe' рядом с keeper-подложкой", vars.Register)
	}
}

// TestStateChangesVars_HostWinsOnCollision — on a register-name collision, the
// per-host key WINS over the keeper underlay (host-wins; collisions are actually
// impossible due to the register-duplicate validator, this is a formal safety net).
func TestStateChangesVars_HostWinsOnCollision(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	in := RenderInput{
		RegisterByHost: map[string]map[string]any{
			KeeperTargetSID: {"x": map[string]any{"src": "keeper"}},
			host.SID:        {"x": map[string]any{"src": "host"}},
		},
	}

	vars := stateChangesVars(in, host)
	got, _ := vars.Register["x"].(map[string]any)
	if got == nil || got["src"] != "host" {
		t.Errorf("stateChangesVars.Register[x] = %v, want host-значение (host-wins при коллизии)", vars.Register["x"])
	}
}

// TestStateChangesVars_NoKeeperBucket_BitForBit — back-compat: without a
// keeper-bucket, Register is exactly the per-host bucket (the same map, no
// merge-copy); a host without a bucket → nil (as before: `register.*` in sets →
// plain "no such key").
func TestStateChangesVars_NoKeeperBucket_BitForBit(t *testing.T) {
	host := &topology.HostFacts{SID: "host-a.example.com"}
	bucket := map[string]any{"probe": map[string]any{"stdout": "master"}}
	in := RenderInput{
		RegisterByHost: map[string]map[string]any{host.SID: bucket},
	}

	vars := stateChangesVars(in, host)
	if len(vars.Register) != len(bucket) {
		t.Fatalf("stateChangesVars.Register = %v, want ровно per-host bucket %v", vars.Register, bucket)
	}
	// Same map, not a copy: mutating bucket is visible through vars (no extra allocation).
	bucket["late"] = true
	if _, ok := vars.Register["late"]; !ok {
		t.Errorf("stateChangesVars.Register — копия per-host bucket-а, want та же карта (бит-в-бит без keeper-подложки)")
	}

	noBucket := &topology.HostFacts{SID: "host-b.example.com"}
	if got := stateChangesVars(in, noBucket).Register; got != nil {
		t.Errorf("stateChangesVars.Register (хост без bucket-а) = %v, want nil", got)
	}
}
