package trial

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
)

// An L2 case applies ONE ApplyRequest to ONE stand. Since NIM-908 a plan can carry
// per-host params, and there is no host to convert for — the request would be built
// from the first host by SID and the case would pass while proving something about
// a host it never applied to.
//
// Unit-level on purpose: the gate is docker-free, so a test that started a stand
// would not run there — which is exactly where a guard is worth having.
func TestL2_PerHostPlanRefused(t *testing.T) {
	plain := []*render.RenderedTask{{Name: "echo", Module: "core.exec.run"}}
	if got := l2PerHostPlanRefusal(plain); got != "" {
		t.Errorf("a host-invariant plan must run at L2, got refusal: %q", got)
	}

	st, err := structpb.NewStruct(map[string]any{"cmd": "echo a"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	perHost := []*render.RenderedTask{{
		Name:   "echo",
		Module: "core.exec.run",
		Params: st,
		ParamsBySID: map[string]*structpb.Struct{
			"a.example.com": st,
			"b.example.com": st,
		},
	}}
	got := l2PerHostPlanRefusal(perHost)
	if got == "" {
		t.Fatal("a per-host plan must be refused at L2, not flattened onto the single stand")
	}
	if !strings.Contains(got, "L0") {
		t.Errorf("the refusal must say where a multi-host case belongs: %q", got)
	}
}
