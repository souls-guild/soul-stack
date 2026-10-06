package scenario

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

type tlsDestinyResolver struct{ d *render.ResolvedDestiny }

func (r tlsDestinyResolver) Resolve(_ context.Context, name string) (*render.ResolvedDestiny, error) {
	if name != r.d.Name {
		return nil, fmt.Errorf("unknown destiny %q", name)
	}
	return r.d, nil
}

// rotateTLSPassagePlan renders the shape of
// examples/service/dragonfly/scenario/rotate_tls with the applier and its
// consumers forced onto opposite sides of a Passage boundary: the consumers
// read a probe register in `where:`, the applier does not. It is the full plan
// run.go hands newCrossPassageGate when it dispatches Passage 1, plus the
// applier's terminal and the two child indices.
func rotateTLSPassagePlan(t *testing.T) (tasks []*render.RenderedTask, plans []render.DispatchPlan, terminal *render.RenderedTask, children []int) {
	t.Helper()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	exec := func(cmd string) *config.ModuleTask {
		return &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": cmd}}
	}
	scn := &config.ScenarioManifest{Name: "rotate_tls", Tasks: []config.Task{
		{Name: "Probe role", Register: "role", ChangedWhen: "false", Module: exec("detect-role")},
		{Name: "Re-render TLS PEM material", Register: "tls_certs", Apply: &config.ApplyTask{Destiny: "tls"}},
		{Name: "Re-read the rotated TLS cert", Where: "register.role.stdout == 'primary'", OnChanges: []string{"tls_certs"}, Module: exec("config-set-tls")},
		{Name: "Restore the previous TLS material", Where: "register.role.stdout == 'primary'", OnFail: []string{"tls_certs"}, Module: exec("restore-tls")},
	}}
	file := func(name, path string) config.Task {
		return config.Task{Name: name, Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{"path": path, "content": "pem"}}}
	}
	destiny := &render.ResolvedDestiny{Name: "tls", Tasks: []config.Task{
		file("Lay down the certificate", "/etc/df/tls/dragonfly.crt"),
		file("Lay down the key", "/etc/df/tls/dragonfly.key"),
	}}

	passage, err := render.Stratify(scn.Tasks)
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	if want := []int{0, 0, 1, 1}; !slices.Equal(passage.TaskPassage, want) {
		t.Fatalf("TaskPassage = %v, want %v: the applier and its consumers must sit in different Passages or the gate is not exercised", passage.TaskPassage, want)
	}

	primary := map[string]any{"role": map[string]any{"stdout": "primary"}}
	tasks, plans, err = render.NewPipeline(nil, engine, nil, nil).Render(context.Background(), render.RenderInput{
		Scenario:       scn,
		Incarnation:    render.IncarnationMeta{ID: "df"},
		Hosts:          []*topology.HostFacts{{SID: "a.example.com", Coven: []string{"df"}}, {SID: "b.example.com", Coven: []string{"df"}}},
		Destiny:        tlsDestinyResolver{d: destiny},
		RegisterByHost: map[string]map[string]any{"a.example.com": primary, "b.example.com": primary},
		TaskPassage:    passage.TaskPassage,
		ActivePassage:  1,
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, rt := range tasks {
		if rt.Register == "tls_certs" {
			terminal = rt
		}
	}
	if terminal == nil || terminal.Module != "core.noop.run" || terminal.Passage != 0 || len(terminal.AggregateOf) != 2 {
		t.Fatalf("applier terminal = %+v, want a Passage-0 core.noop.run aggregating 2 children", terminal)
	}
	return tasks, plans, terminal, terminal.AggregateOf
}

// TestCrossPassageGate_ApplierSourceResolvedByChildren is the NIM-931 guard.
// The facts are what the audit log really holds for an applier: the children's
// statuses, and the terminal reporting OK whatever they did — so the terminal's
// own key is never in either set. Reading the source off that key would drop
// the reload after a real rotation and never fire the restore.
func TestCrossPassageGate_ApplierSourceResolvedByChildren(t *testing.T) {
	const reload, restore = "Re-read the rotated TLS cert", "Restore the previous TLS material"
	tasks, plans, terminal, children := rotateTLSPassagePlan(t)
	// The LAST child carries the fact: a resolution that stops at the first
	// child, or at the terminal, misses it.
	last := children[len(children)-1]
	first := children[0]

	cases := []struct {
		name            string
		changed, failed map[auditpg.ChangedTaskKey]struct{}
		want            map[string][]string // sid → tasks dispatched in Passage 1; absent = host dropped
	}{
		{
			name:    "a child changed on one host",
			changed: keySet(key("a.example.com", last)),
			want:    map[string][]string{"a.example.com": {reload}},
		},
		{
			name:    "no child changed",
			changed: keySet(),
			want:    map[string][]string{},
		},
		// The two onfail cases check the gate alone: today a failed child fails
		// the run at the Passage barrier, so no later Passage is built to carry
		// the rescue (ADR-056 R3, open limit).
		{
			name:   "a child failed on one host",
			failed: keySet(key("a.example.com", last)),
			want:   map[string][]string{"a.example.com": {restore}},
		},
		{
			name:    "one child changed, the next failed",
			changed: keySet(key("b.example.com", first)),
			failed:  keySet(key("b.example.com", last)),
			want:    map[string][]string{"b.example.com": {reload, restore}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pTasks, pPlans := tasksForPassage(tasks, plans, 1)
			perHost := groupByHost(pTasks, pPlans)
			if len(perHost) != 2 {
				t.Fatalf("Passage 1 targets %d hosts before the gate, want 2", len(perHost))
			}
			got := newCrossPassageGate(tasks, tc.changed, tc.failed).applyGate(perHost, 1)

			for _, sid := range []string{"a.example.com", "b.example.com"} {
				want, wantHost := tc.want[sid]
				names := taskNames(got[sid])
				if !wantHost {
					if _, present := got[sid]; present {
						t.Errorf("%s dispatches %v, want the host dropped (no child of tls_certs changed or failed there)", sid, names)
					}
					continue
				}
				if !slices.Equal(names, want) {
					t.Errorf("%s dispatches %v, want %v", sid, names, want)
				}
				for _, rt := range got[sid] {
					if slices.Contains(rt.OnChangesIdx, terminal.Index) || slices.Contains(rt.OnFailIdx, terminal.Index) {
						t.Errorf("%s: %q still carries the applier terminal %d on the wire; Soul would re-gate it as an absent source and skip", sid, rt.Name, terminal.Index)
					}
				}
			}
		})
	}
}
