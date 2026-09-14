package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The task's `transport:` (NIM-870) reaches the DISPATCH PLAN, not the rendered
// task: a transport decides how the Keeper gets to a host, and RenderedTask is
// the part that crosses the wire to the host that was already reached. These
// pin the seam — one `apply:` is many rendered tasks and one transport decision,
// and every plan of the expansion has to carry the same answer, or a dispatcher
// reading a later plan would dial differently from one reading the first.

func transportPlans(t *testing.T, tasks ...config.Task) []DispatchPlan {
	t.Helper()
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := RenderInput{
		Scenario:    &config.ScenarioManifest{Name: "s", Tasks: tasks},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts: []*topology.HostFacts{
			host("a.example.com", []string{"svc"}, nil),
			host("b.example.com", []string{"svc"}, nil),
		},
		Destiny: staticResolver{&ResolvedDestiny{
			Name:  "d",
			Tasks: []config.Task{moduleTask("d-1", "core.exec.run"), moduleTask("d-2", "core.exec.run")},
		}},
	}
	_, plans, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return plans
}

func TestTransport_ScalarReachesThePlan(t *testing.T) {
	plans := transportPlans(t, config.Task{
		Name:      "install",
		Transport: "ssh",
		Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}},
	})
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if plans[0].TransportName != config.TransportSSH {
		t.Errorf("TransportName = %q, want %q", plans[0].TransportName, config.TransportSSH)
	}
	if len(plans[0].TransportParams) != 0 {
		t.Errorf("TransportParams = %v, want empty for the scalar form", plans[0].TransportParams)
	}
}

// The whole apply expansion carries one decision. A dispatcher may read any
// plan of the group; they must not disagree.
func TestTransport_EveryPlanOfAnApplyExpansionCarriesIt(t *testing.T) {
	plans := transportPlans(t, config.Task{
		Name:      "install",
		Transport: map[string]any{"ssh": map[string]any{"ssh_provider": "vault-bastion", "user": "deploy"}},
		Apply:     &config.ApplyTask{Destiny: "d"},
	})
	if len(plans) != 2 {
		t.Fatalf("len(plans) = %d, want 2 (the destiny has two tasks)", len(plans))
	}
	for i, p := range plans {
		if p.TransportName != config.TransportSSH {
			t.Errorf("plans[%d].TransportName = %q, want ssh", i, p.TransportName)
		}
		if got := p.TransportParams["ssh_provider"]; got != "vault-bastion" {
			t.Errorf("plans[%d] ssh_provider = %v, want vault-bastion", i, got)
		}
		if got := p.TransportParams["user"]; got != "deploy" {
			t.Errorf("plans[%d] user = %v, want deploy", i, got)
		}
	}
}

// A block's key is an INHERITED DEFAULT, in the same direction as `where:` and
// `serial:`; a descendant that names its own wins. The opposite direction —
// the block clobbering a child — is the bug this pins: it would silently send a
// task through a bastion its author explicitly routed around.
func TestTransport_BlockInheritsAndTheChildWins(t *testing.T) {
	plans := transportPlans(t, config.Task{
		Name:      "group",
		Transport: map[string]any{"ssh": map[string]any{"ssh_provider": "block-bastion"}},
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("inherits", "core.exec.run"),
			func() config.Task {
				own := moduleTask("overrides", "core.exec.run")
				own.Transport = map[string]any{"ssh": map[string]any{"ssh_provider": "child-bastion"}}
				return own
			}(),
		}},
	})
	if len(plans) != 2 {
		t.Fatalf("len(plans) = %d, want 2", len(plans))
	}
	if got := plans[0].TransportParams["ssh_provider"]; got != "block-bastion" {
		t.Errorf("inheriting child ssh_provider = %v, want block-bastion", got)
	}
	if got := plans[1].TransportParams["ssh_provider"]; got != "child-bastion" {
		t.Errorf("overriding child ssh_provider = %v, want child-bastion (the block must not clobber it)", got)
	}
}

// Nesting: the NEAREST enclosing block wins. The bug this pins is the outer
// block's key reaching a grandchild past an inner block that said something
// else — a task routed through the wrong bastion with both lines reading
// correctly on their own.
func TestTransport_NestedBlockBeatsTheOuterOne(t *testing.T) {
	plans := transportPlans(t, config.Task{
		Name:      "outer",
		Transport: map[string]any{"ssh": map[string]any{"ssh_provider": "outer-bastion"}},
		Block: &config.BlockTask{Block: []config.Task{
			moduleTask("outer-child", "core.exec.run"),
			{
				Name:      "inner",
				Transport: map[string]any{"ssh": map[string]any{"ssh_provider": "inner-bastion"}},
				Block: &config.BlockTask{Block: []config.Task{
					moduleTask("inner-child", "core.exec.run"),
					func() config.Task {
						own := moduleTask("inner-child-own", "core.exec.run")
						own.Transport = map[string]any{"ssh": map[string]any{"ssh_provider": "leaf-bastion"}}
						return own
					}(),
				}},
			},
		}},
	})
	if len(plans) != 3 {
		t.Fatalf("len(plans) = %d, want 3", len(plans))
	}
	want := []string{"outer-bastion", "inner-bastion", "leaf-bastion"}
	for i, w := range want {
		if got := plans[i].TransportParams["ssh_provider"]; got != w {
			t.Errorf("plans[%d] ssh_provider = %v, want %v", i, got, w)
		}
	}
}

func TestTransport_AbsentKeyLeavesThePlanEmpty(t *testing.T) {
	plans := transportPlans(t, config.Task{
		Name:   "install",
		Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}},
	})
	if plans[0].TransportName != "" {
		t.Errorf("TransportName = %q, want empty — an unwritten key must leave the registry in charge", plans[0].TransportName)
	}
}

// A key that is WRITTEN but does not decode fails the render. Offline
// validation refuses every such shape, so a linted scenario never reaches this —
// but pushorch hands the raw DSL value in from its own API, and there the
// alternative to failing is a run that quietly falls back to the registry the
// key exists to beat, reported as a success.
func TestTransport_MalformedKeyFailsTheRender(t *testing.T) {
	cases := []struct {
		name      string
		transport any
	}{
		{name: "unregistered scalar", transport: "rsh"},
		{name: "two transports", transport: map[string]any{"ssh": nil, "agent": nil}},
		{name: "params not a mapping", transport: map[string]any{"ssh": "vault-bastion"}},
		{name: "a list", transport: []any{"ssh"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
					Name:      "install",
					Transport: tc.transport,
					Module:    &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{}},
				}}},
				Incarnation: IncarnationMeta{ID: "svc"},
				Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
			})
			if !errors.Is(err, ErrUnsupportedDSL) {
				t.Fatalf("Render err = %v, want ErrUnsupportedDSL — a written-but-undecodable key must not fall back to the registry", err)
			}
		})
	}
}

// The merge must not swallow a descendant's MALFORMED key. Asking the decoder
// inside mergeTransport looked harmless and opened the worst hole the key can
// have: the child's value answered "not a transport", the BLOCK's value took its
// place, and the descendant was dispatched through a bastion its own line does
// not name — or, with no key on the block, fell straight back to the registry
// the key exists to beat, with the run reported green.
func TestTransport_MalformedChildKeyInABlockIsNotSwallowed(t *testing.T) {
	malformed := map[string]any{"ssh": map[string]any{"ssh_provider": "bastion-b"}, "agent": nil}

	for _, tc := range []struct {
		name  string
		block any
	}{
		{name: "block names a transport", block: map[string]any{"ssh": map[string]any{"ssh_provider": "bastion-a"}}},
		{name: "block names none", block: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), RenderInput{
				Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
					Name:      "group",
					Transport: tc.block,
					Block: &config.BlockTask{Block: []config.Task{
						func() config.Task {
							c := moduleTask("child", "core.exec.run")
							c.Transport = malformed
							return c
						}(),
					}},
				}}},
				Incarnation: IncarnationMeta{ID: "svc"},
				Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
			})
			if !errors.Is(err, ErrUnsupportedDSL) {
				t.Fatalf("Render err = %v, want ErrUnsupportedDSL — the child's malformed key must not be replaced by the block's", err)
			}
		})
	}
}

// A destiny is rendered per host and shipped whole to the ONE transport the
// scenario task chose, so a destiny task naming a second one has nothing to act
// on. Refused rather than ignored: `serial:`/`run_once:`/`on:` are refused in
// that position for the same reason, and a silently dropped `transport: ssh` is
// a file that says one thing while the run dials another.
func TestTransport_InADestinyIsRefused(t *testing.T) {
	inner := moduleTask("d-1", "core.exec.run")
	inner.Transport = "ssh"
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), RenderInput{
		Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
			Name:  "install",
			Apply: &config.ApplyTask{Destiny: "d"},
		}}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
		Destiny:     staticResolver{&ResolvedDestiny{Name: "d", Tasks: []config.Task{inner}}},
	})
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("Render err = %v, want ErrUnsupportedDSL", err)
	}
	if !strings.Contains(err.Error(), "transport:") {
		t.Errorf("err = %q, want it to name the key", err)
	}
}

// Defense in depth behind the offline `transport_on_keeper_invalid`: a plan
// that did not come through the validator must still be refused rather than
// rendered with a transport nothing will read.
func TestTransport_OnAKeeperTaskIsRefusedAtRender(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), RenderInput{
		Scenario: &config.ScenarioManifest{Name: "s", Tasks: []config.Task{{
			Name:      "register",
			Transport: "ssh",
			Module:    &config.ModuleTask{Module: "core.soul.registered", Params: map[string]any{"sid": "a.example.com"}},
		}}},
		Incarnation: IncarnationMeta{ID: "svc"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)},
	})
	if !errors.Is(err, ErrUnsupportedDSL) {
		t.Fatalf("Render err = %v, want ErrUnsupportedDSL", err)
	}
	if !strings.Contains(err.Error(), "transport:") {
		t.Errorf("err = %q, want it to name the key", err)
	}
}
