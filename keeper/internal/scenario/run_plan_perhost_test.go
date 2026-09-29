package scenario

import (
	"encoding/json"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

func planStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	st, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return st
}

// The run plan carries ONE params map per task beside per-host RESULTS. Since
// NIM-908 a task can render different params on different hosts, and showing the
// first host's as if it were everyone's is the substitution this ticket removed
// from the run itself — reappearing in the view an operator opens to check what ran.
//
// A differing cell is MARKED; a host-invariant one stays visible, because blanking
// the map to cover one key would cost the view its purpose.
func TestMaskRunPlanParams_PerHostCellsAreMarked(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "echo",
		Module: "core.exec.run",
		Params: planStruct(t, map[string]any{"cmd": "echo a", "port": "6379"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"cmd": "echo a", "port": "6379"}),
			"b.example.com": planStruct(t, map[string]any{"cmd": "echo b", "port": "6379"}),
		},
	}

	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, nil), &got); err != nil {
		t.Fatalf("unmarshal the persisted plan params: %v", err)
	}
	if got["cmd"] != render.PerHostParamValue {
		t.Errorf("params.cmd = %v, want %q — the plan row shows one host's value as everyone's", got["cmd"], render.PerHostParamValue)
	}
	if got["port"] != "6379" {
		t.Errorf("params.port = %v, want %q — a host-invariant cell must stay visible", got["port"], "6379")
	}
}

// The golden path is untouched: a task with no per-host params persists exactly
// what it always did.
func TestMaskRunPlanParams_GoldenPathUnchanged(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "echo",
		Module: "core.exec.run",
		Params: planStruct(t, map[string]any{"cmd": "echo a"}),
	}
	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, nil), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["cmd"] != "echo a" {
		t.Errorf("params.cmd = %v, want %q", got["cmd"], "echo a")
	}
}

// A sealed cell is masked whether or not it varies: the seal marks PATHS, so
// per-host values under the same path are all covered. The per-host marker must not
// become a way for a secret to reach the row unmasked.
func TestMaskRunPlanParams_SealedPerHostCellStaysMasked(t *testing.T) {
	rt := &render.RenderedTask{
		Name:   "write",
		Module: "core.file.present",
		Params: planStruct(t, map[string]any{"content": "pw-for-a"}),
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": planStruct(t, map[string]any{"content": "pw-for-a"}),
			"b.example.com": planStruct(t, map[string]any{"content": "pw-for-b"}),
		},
	}
	var got map[string]any
	if err := json.Unmarshal(maskRunPlanParams(rt, map[string]bool{"content": true}), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, leaked := range []string{"pw-for-a", "pw-for-b"} {
		if got["content"] == leaked {
			t.Fatalf("a sealed per-host cell reached the plan row in the clear: %v", got["content"])
		}
	}
	// ★ And it is MASKED, not merely replaced by the per-host marker. Asserting
	// "neither plaintext" would pass whether the seal ran or not, because the marker
	// alone satisfies it — so an ordering regression (mark after mask, or skip
	// masking for marked cells) would go unnoticed. The seal runs last and wins.
	if got["content"] != audit.MaskedValue {
		t.Fatalf("params.content = %v, want %q — the seal must win over the per-host marker", got["content"], audit.MaskedValue)
	}
}
