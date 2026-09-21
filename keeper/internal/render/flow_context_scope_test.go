package render

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// flow_context scope (NIM-813). The snapshot used to carry the run's WHOLE input
// — `secret: true` fields included — to every targeted host on every task, tasks
// with no predicate among them, which never read the snapshot at all. One
// compromised host in a 200-host run then yielded every secret the run was given,
// not the ones its own tasks used. These tests pin both halves of the fix:
//
//   - a task with no flow-control predicate carries no operator-supplied section;
//   - a task with one carries exactly the fields its predicates name.
//
// Everything here reads the WIRE value (RenderedTask.FlowContext, what
// ToProtoTasks hands to the ApplyRequest), not an intermediate.

// fcSection returns a flow_context section's keys, sorted.
func fcSection(t *testing.T, rt *RenderedTask, key string) []string {
	t.Helper()
	if rt.FlowContext == nil {
		t.Fatal("FlowContext == nil - Soul needs the snapshot for its own evalWhen")
	}
	sec, ok := rt.FlowContext.GetFields()[key]
	if !ok {
		t.Fatalf("flow_context has no %q section - the key is always present, empty or not", key)
	}
	keys := make([]string, 0)
	for k := range sec.GetStructValue().GetFields() {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// twoSecretInputs — a run whose input carries two declared-secret fields, the
// shape of the ticket's failure scenario (admin_password / replica_password).
func twoSecretInputs() map[string]any {
	return map[string]any{
		"admin_password":   "s3cret-admin",
		"replica_password": "s3cret-replica",
		"action":           "apply",
	}
}

func secretInputRender(manifest *config.ScenarioManifest, hosts ...*topology.HostFacts) RenderInput {
	if len(hosts) == 0 {
		hosts = []*topology.HostFacts{host("a.example.com", []string{"svc"}, nil)}
	}
	return RenderInput{
		Scenario:    manifest,
		Input:       twoSecretInputs(),
		Incarnation: IncarnationMeta{ID: "svc", Service: "redis"},
		State:       map[string]any{"root_token": "s3cret-state"},
		Hosts:       hosts,
	}
}

// TestFlowContextScope_NoPredicate_ShipsNoOperatorInput — ★ invariant 1: a task
// with no when:/changed_when:/failed_when:/until: gets NO input, vars or
// incarnation. Soul's only readers of flow_context are evalWhen and
// evalFlowPredicate (applyrunner.go), so such a task never opens the snapshot —
// there is nothing for the value to be needed for on that host.
func TestFlowContextScope_NoPredicate_ShipsNoOperatorInput(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "no-predicate",
		Tasks: []config.Task{
			{
				Name:   "restart",
				Vars:   map[string]any{"port": "${ input.action }"},
				Module: &config.ModuleTask{Module: "core.service.restarted", Params: map[string]any{"name": "redis"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	for _, section := range []string{"input", "vars", "incarnation"} {
		if got := fcSection(t, tasks[0], section); len(got) != 0 {
			t.Errorf("flow_context.%s = %v on a task with no predicate, want empty - it is never read there", section, got)
		}
	}
	// self stays whole: the receiving host's own facts widen nothing, and both
	// fail-closed host-invariance layers are defined against its presence. A
	// presence check alone would pass on an empty section, which is exactly what
	// pulling self into flowContextRoots would produce here (no predicate → no
	// fields), so assert a key soulprintSelfMap always places.
	self := fcSection(t, tasks[0], "self")
	if len(self) == 0 {
		t.Fatal("flow_context.self is empty - it is not part of the narrowing")
	}
	if !slices.Contains(self, "sid") {
		t.Errorf("flow_context.self = %v, want the host's own facts (sid among them) - self ships whole", self)
	}
}

// TestFlowContextScope_Predicate_ShipsOnlyWhatItReads — ★ invariant 2: a
// predicate naming one secret input gets that one, and the OTHER secret of the
// same run does not travel.
func TestFlowContextScope_Predicate_ShipsOnlyWhatItReads(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "one-of-two",
		Tasks: []config.Task{
			{
				Name:   "set-admin",
				When:   "input.admin_password != ''",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := fcSection(t, tasks[0], "input")
	if strings.Join(got, ",") != "admin_password" {
		t.Errorf("flow_context.input = %v, want exactly [admin_password] - the predicate names no other field", got)
	}
}

// TestFlowContextScope_AllFourPredicates_Union — the shipped set is the union
// over when/changed_when/failed_when/retry.until, and nothing beyond it.
// retry.until is the one a three-field gate forgets: Soul evaluates it against
// the same snapshot through the same engine (runTaskWithRetry →
// evalFlowPredicate), so leaving it out would break `until:` on the host rather
// than here.
func TestFlowContextScope_AllFourPredicates_Union(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "union",
		Tasks: []config.Task{
			{
				Name:        "probe",
				Vars:        map[string]any{"a": "1", "b": "2", "c": "3", "d": "4", "unused": "5"},
				When:        "vars.a != ''",
				ChangedWhen: "vars.b != ''",
				FailedWhen:  "vars.c != ''",
				Retry:       &config.RetrySpec{Count: 3, Delay: "1ms", Until: "vars.d != ''"},
				Module:      &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := fcSection(t, tasks[0], "vars")
	if strings.Join(got, ",") != "a,b,c,d" {
		t.Errorf("flow_context.vars = %v, want [a b c d] - the union of the four predicates, without `unused`", got)
	}
}

// TestFlowContextScope_UntilOnly_ShipsWhatUntilReads — the same point on its own:
// a task whose ONLY predicate is retry.until still gets what that predicate
// needs. Gating on when/changed_when/failed_when alone leaves this one empty and
// the run fails on the host at the first retry.
func TestFlowContextScope_UntilOnly_ShipsWhatUntilReads(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "until-only",
		Tasks: []config.Task{
			{
				Name:   "wait",
				Retry:  &config.RetrySpec{Count: 3, Delay: "1ms", Until: "input.action == 'apply'"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := fcSection(t, tasks[0], "input")
	if strings.Join(got, ",") != "action" {
		t.Errorf("flow_context.input = %v, want [action] - retry.until reads it", got)
	}
}

// TestFlowContextScope_IncarnationStateNarrowed — incarnation.state is a captured
// snapshot and holds secrets by the same rules as input ([ADR-0084]). A predicate
// on incarnation.id gets id and NOT state.
func TestFlowContextScope_IncarnationStateNarrowed(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "incarnation-scope",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "incarnation.id == 'svc'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	got := fcSection(t, tasks[0], "incarnation")
	if strings.Join(got, ",") != "id" {
		t.Errorf("flow_context.incarnation = %v, want [id] - state must not ride along", got)
	}
}

// TestFlowContextScope_SoulReplayAgreesWithFullContext — the narrowing must not
// change a single predicate's ANSWER. Each case is evaluated Soul-side (the same
// flow-control engine and the same flowControlVars shape) TWICE: against the
// snapshot that actually shipped, and against the same snapshot built whole
// (allFlowContextReads). Both must agree with each other and with want. A
// narrowed key that the predicate needed shows up as a disagreement or an error
// here, not as a wrong task decision on a host.
//
// The full-context leg is what makes this differential rather than nine
// hardcoded expectations: want alone would still pass if narrowing and the full
// snapshot were BOTH wrong in the same direction.
//
// `has(input.x)` and the retired `incarnation.name` are the two shapes that go
// wrong QUIETLY rather than loudly: has() over a dropped key is false, not an
// error, and `name` is derived from `id` at activation rather than stored.
func TestFlowContextScope_SoulReplayAgreesWithFullContext(t *testing.T) {
	cases := []struct {
		name string
		when string
		want bool
	}{
		{"select", "input.action == 'apply'", true},
		{"has-present", "has(input.admin_password)", true},
		{"has-absent", "has(input.nope)", false},
		{"legacy-incarnation-id", "incarnation.name == 'svc'", true},
		{"incarnation-id", "incarnation.id == 'svc'", true},
		{"whole-input", "size(input) == 3", true},
		{"index-form", "input['action'] == 'apply'", true},
		{"nested-state", "incarnation.state.root_token != ''", true},
		{"mixed", "input.action == 'apply' && incarnation.service == 'redis'", true},
	}

	soulEngine, err := cel.NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := &config.ScenarioManifest{
				Name: "replay",
				Tasks: []config.Task{
					{
						// The predicate is FailedWhen, not When, and that is what
						// keeps it off the static-when placeholder path:
						// staticWhenSkips reads task.When alone. Moving these cases
						// to When: would render placeholders and quietly change
						// which snapshot is under test.
						Name:       "t",
						FailedWhen: tc.when,
						Module:     &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
					},
				},
			}
			in := secretInputRender(manifest)
			p := NewPipeline(nil, newEngine(t), nil, nil)
			tasks, _, rerr := p.Render(context.Background(), in)
			if rerr != nil {
				t.Fatalf("Render: %v", rerr)
			}

			shipped, serr := soulEngine.EvalPredicate(tc.when, flowControlVarsFromStruct(tasks[0].FlowContext, nil))
			if serr != nil {
				t.Fatalf("Soul-side eval against the SHIPPED flow_context: %v", serr)
			}

			// The same snapshot, unnarrowed — the behavior before NIM-813.
			full, ferr := buildFlowContext(in, in.Hosts[0], hostVars(in, in.Hosts[0], len(in.Hosts)), len(in.Hosts), allFlowContextReads())
			if ferr != nil {
				t.Fatalf("buildFlowContext (whole): %v", ferr)
			}
			whole, werr := soulEngine.EvalPredicate(tc.when, flowControlVarsFromStruct(full, nil))
			if werr != nil {
				t.Fatalf("Soul-side eval against the WHOLE flow_context: %v", werr)
			}

			if shipped != whole {
				t.Errorf("predicate %q disagrees across snapshots: shipped = %v, whole = %v - the narrowing changed an answer", tc.when, shipped, whole)
			}
			if shipped != tc.want {
				t.Errorf("predicate %q on the shipped snapshot = %v, want %v", tc.when, shipped, tc.want)
			}
		})
	}
}

// TestFlowContextScope_UntilHostVariantVars_Rejected — hasFlowControl gates the
// second fail-closed layer (flowContextHostInvariant) and used to count three
// predicates, not four. A task whose ONLY predicate is `until: vars.x` over a
// host-variant vars.x therefore evaluated every host against the FIRST host's
// value with neither layer objecting — the predicate text carries no
// "soulprint", so the regex guard misses it too.
func TestFlowContextScope_UntilHostVariantVars_Rejected(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "until-laundering",
		Tasks: []config.Task{
			{
				Name:   "t",
				Vars:   map[string]any{"is_debian": "${ soulprint.self.os.family == 'debian' }"},
				Retry:  &config.RetrySpec{Count: 3, Delay: "1ms", Until: "vars.is_debian"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := secretInputRender(manifest,
		host("a.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "debian"}}),
		host("b.example.com", []string{"svc"}, map[string]any{"os": map[string]any{"family": "rhel"}}),
	)
	_, _, err := p.Render(context.Background(), in)
	if err == nil {
		t.Fatal("Render: expected a fail-closed vars-laundering error for until:, got nil")
	}
	if !strings.Contains(err.Error(), "host-variant flow_context") {
		t.Errorf("error text is not about vars-laundering flow_context: %q", err.Error())
	}
}

// TestFlowContextScope_StaticSkipPlaceholder_Narrowed — the placeholder of a
// statically-false when: ships flow_context too (Soul re-evaluates the same
// predicate for its own SKIPPED). It is narrowed like any other wire snapshot,
// and still carries what that predicate reads.
func TestFlowContextScope_StaticSkipPlaceholder_Narrowed(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "static-skip",
		Tasks: []config.Task{
			{
				Name:   "t",
				When:   "input.action == 'destroy'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
		},
	}
	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), secretInputRender(manifest))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if tasks[0].Params != nil {
		t.Fatal("expected a static-when skip placeholder (Params == nil)")
	}

	got := fcSection(t, tasks[0], "input")
	if strings.Join(got, ",") != "action" {
		t.Errorf("placeholder flow_context.input = %v, want [action] - narrowed, but complete for its own when:", got)
	}
}

// TestProjectFlowSection_UnnamedRootShipsWhole — the projection's fail direction.
// Of the two ways to be wrong about a section, shipping it costs reach; dropping
// it breaks a predicate on the host, mid-run. A root absent from the reads map
// (a caller that built the set by hand and missed one — neither constructor
// does today) must therefore land on the first.
func TestProjectFlowSection_UnnamedRootShipsWhole(t *testing.T) {
	section := map[string]any{"admin_password": "s3cret", "action": "apply"}

	got := projectFlowSection(section, flowContextReads{}, flowContextInputKey)
	if len(got) != len(section) {
		t.Errorf("projectFlowSection with %q unnamed = %v, want the whole section - a missing entry must not read as an empty one", flowContextInputKey, got)
	}

	// The zero value of RootReads is the OPPOSITE case and must stay narrow:
	// taskFlowContextReads gives a predicate-less task exactly this.
	named := flowContextReads{flowContextInputKey: cel.RootReads{Fields: map[string]bool{}}}
	if got := projectFlowSection(section, named, flowContextInputKey); len(got) != 0 {
		t.Errorf("projectFlowSection on a named-but-empty root = %v, want empty - that is a task with no predicate", got)
	}

	if got := projectFlowSection(section, allFlowContextReads(), flowContextInputKey); len(got) != len(section) {
		t.Errorf("projectFlowSection with allFlowContextReads = %v, want the whole section", got)
	}
}
