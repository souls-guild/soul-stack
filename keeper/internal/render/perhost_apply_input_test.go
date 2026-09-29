package render

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// NIM-908. `apply: input:` used to render ONCE, in the environment of the first
// targeted host by SID, and the result was handed to every host — while the
// per-host roots (`soulprint.self.*`, `register.*`) were available in that
// environment and not refused. Nine hosts all received the first one's address,
// silently, and the run went green.
//
// These tests pin the three halves of the answer:
//
//  1. the value is now this host's, everywhere the wire can carry a per-host value;
//  2. an ordinary module param gets it too, because the same change dispatches
//     params per host (ParamsBySID) — open Q #25 closed for params;
//  3. where there is no host to render for at all (the loop axis, `on:`, a static
//     `when:`, an include's `when:`), the reference is refused by name.

// perHostHosts — two hosts with different primary_ip, deliberately NOT in
// lexicographic order so "the first by SID" is not "the first in the list": the old
// behaviour substituted a.example.com's address, and a test whose roster order
// already matched could not tell that apart from a correct per-host render.
func perHostHosts() []*topology.HostFacts {
	return []*topology.HostFacts{
		hostWithRole("b.example.com", "replica", []string{"redis"},
			map[string]any{"primary_ip": "10.0.0.2"}, map[string]any{"family": "debian"}),
		hostWithRole("a.example.com", "master", []string{"redis"},
			map[string]any{"primary_ip": "10.0.0.1"}, map[string]any{"family": "debian"}),
	}
}

// perHostApplyInput builds the scenario from the ticket: one applier passing a
// per-host expression into a destiny.
func perHostApplyInput(destinyTasks []config.Task, schema config.InputSchemaMap, applyInput map[string]any) (*config.ScenarioManifest, *stubDestinyResolver) {
	res := &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "redis",
		Input: schema,
		Tasks: destinyTasks,
	}}
	return applyScenario("redis", applyInput), res
}

func perHostRenderInput(manifest *config.ScenarioManifest, res *stubDestinyResolver, hosts []*topology.HostFacts) RenderInput {
	return RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{},
		Incarnation: IncarnationMeta{ID: "redis-prod"},
		Hosts:       hosts,
		Destiny:     res,
	}
}

// ★ THE TICKET, FIXED. `apply: input:` reads soulprint.self, the destiny writes a
// config file from it, and each host's file carries ITS OWN address.
//
// core.file.rendered is the channel this can travel on: render_context is
// materialized per SID (RenderedTask.RenderContextBySID) and overlaid when a
// specific host's ApplyRequest is built (ToProtoTasksForHost). Before NIM-908 the
// same scenario rendered one render_context — a.example.com's — for both hosts.
//
// NON-VACUITY: revert `rc["input"] = orEmptyMap(inputForHost(in, host))` in
// buildRenderContext to `in.Input`, or revert hostVars's Input to `in.Input`, and
// b.example.com gets 10.0.0.1 here.
func TestApplyInput_PerHost_ReachesRenderedTemplate(t *testing.T) {
	const tmplPath = "templates/redis.conf.tmpl"
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name: "write redis.conf",
			Module: &config.ModuleTask{
				Module: moduleFileRendered,
				Params: map[string]any{"path": "/etc/redis/redis.conf", "template": tmplPath},
			},
		}},
		config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
		map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
	)
	res.resolved.Templates = fakeReader{files: map[string][]byte{
		tmplPath: []byte("replicaof {{ .input.master_addr }} 6379\n"),
	}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one task, got %d", len(tasks))
	}

	want := map[string]string{"a.example.com": "10.0.0.1", "b.example.com": "10.0.0.2"}
	byHost := tasks[0].RenderContextBySID
	if len(byHost) != len(want) {
		t.Fatalf("RenderContextBySID has %d entries, want %d — the input did not travel per host", len(byHost), len(want))
	}
	for sid, addr := range want {
		rc, ok := byHost[sid]
		if !ok {
			t.Fatalf("no render_context for %s", sid)
		}
		got := rc.AsMap()["input"].(map[string]any)["master_addr"]
		if got != addr {
			t.Errorf("%s: render_context.input.master_addr = %v, want %q — this host received another host's address", sid, got, addr)
		}
	}
}

// ★ THE OTHER HALF: an ordinary module param, which before NIM-908 rendered GREEN
// with `echo 10.0.0.1` dispatched to BOTH hosts. Each host now gets its own params
// struct, and the wire carries it — this is open Q #25 closed for params
// (ParamsBySID → ToProtoTasksForHost), not a refusal.
func TestApplyInput_PerHost_OrdinaryParamsDispatchedPerHost(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "announce master",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.master_addr }"}},
		}},
		config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
		map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := map[string]string{"a.example.com": "echo 10.0.0.1", "b.example.com": "echo 10.0.0.2"}
	for sid, cmd := range want {
		st, ok := tasks[0].ParamsBySID[sid]
		if !ok {
			t.Fatalf("no per-host params for %s: %v", sid, tasks[0].ParamsBySID)
		}
		if got := st.GetFields()["cmd"].GetStringValue(); got != cmd {
			t.Errorf("%s: cmd = %q, want %q — this host acted on another host's address", sid, got, cmd)
		}
	}
	// And the wire actually carries it: the converter for each SID must answer with
	// that SID's struct, not the plan's golden-path one.
	for sid, cmd := range want {
		wire := ToProtoTasksForHost(tasks, sid)
		if got := wire[0].Params.GetFields()["cmd"].GetStringValue(); got != cmd {
			t.Errorf("%s: wire cmd = %q, want %q", sid, got, cmd)
		}
	}
}

// A single host renders exactly as it always did: there is one environment, the
// value is unambiguous, and nothing is refused. The counterweight to the test above
// — a guard that failed here would have made per-host input useless on the rosters
// where it is trivially correct.
func TestApplyInput_PerHost_SingleHostStillRenders(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "announce master",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.master_addr }"}},
		}},
		config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
		map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	solo := []*topology.HostFacts{perHostHosts()[1]} // a.example.com
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, solo))
	if err != nil {
		t.Fatalf("Render on one host: %v", err)
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo 10.0.0.1" {
		t.Fatalf("params.cmd = %q, want %q", got, "echo 10.0.0.1")
	}
}

// A host-INVARIANT apply.input is untouched by all of this: same single
// RenderedTask, same value, no per-host map, no guard armed. This is the shape
// nearly every scenario in the tree has, so "NIM-908 costs it nothing" is the
// property worth pinning.
func TestApplyInput_HostInvariant_Unchanged(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "announce",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ input.marker }"}},
		}},
		config.InputSchemaMap{"marker": {Type: "string", Required: true}},
		map[string]any{"marker": "${ incarnation.id }-fixed"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, plans, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected one task, got %d", len(tasks))
	}
	if got := tasks[0].Params.GetFields()["cmd"].GetStringValue(); got != "echo redis-prod-fixed" {
		t.Fatalf("params.cmd = %q, want %q", got, "echo redis-prod-fixed")
	}
	if len(plans[0].TargetSIDs) != 2 {
		t.Fatalf("plan targets %v, want both hosts", plans[0].TargetSIDs)
	}
}

// A destiny's own `vars.yml` already resolved per host — that was never the defect.
// What it received was one host's input. Now the file-var layer sees THIS host's.
func TestApplyInput_PerHost_ReachesDestinyVars(t *testing.T) {
	const tmplPath = "templates/conf.tmpl"
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name: "write",
			Module: &config.ModuleTask{
				Module: moduleFileRendered,
				Params: map[string]any{"path": "/etc/app.conf", "template": tmplPath},
			},
		}},
		config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
		map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
	)
	res.resolved.Vars = map[string]any{"endpoint": "${ input.master_addr }:6379"}
	res.resolved.Templates = fakeReader{files: map[string][]byte{
		tmplPath: []byte("endpoint {{ .vars.endpoint }}\n"),
	}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := map[string]string{"a.example.com": "10.0.0.1:6379", "b.example.com": "10.0.0.2:6379"}
	for sid, endpoint := range want {
		rc, ok := tasks[0].RenderContextBySID[sid]
		if !ok {
			t.Fatalf("no render_context for %s", sid)
		}
		got := rc.AsMap()["vars"].(map[string]any)["endpoint"]
		if got != endpoint {
			t.Errorf("%s: vars.endpoint = %v, want %q", sid, got, endpoint)
		}
	}
}

// A destiny task's `where:` is evaluated per host, so a per-host input narrows the
// roster the way its author meant. Under the old behaviour every host was compared
// against the first host's value, so the predicate was either true everywhere or
// false everywhere — the filter silently did nothing.
func TestApplyInput_PerHost_ReachesDestinyWhere(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "only the master",
			Where:  "input.self_addr == '10.0.0.1'",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo master"}},
		}},
		config.InputSchemaMap{"self_addr": {Type: "string", Required: true}},
		map[string]any{"self_addr": "${ soulprint.self.network.primary_ip }"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, plans, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("expected one plan, got %d", len(plans))
	}
	if got := plans[0].TargetSIDs; len(got) != 1 || got[0] != "a.example.com" {
		t.Fatalf("where: selected %v, want only a.example.com — the predicate saw one host's value for everyone", got)
	}
}

// `register.*` is per-host too ([hostRegister] keys the run's buckets by SID), and
// the ticket names it alongside soulprint.self. It is classified as host-variant
// off the TEXT, so the classification does not depend on whether this particular
// run happened to register different values.
func TestApplyInput_RegisterCountsAsPerHost(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "fan",
			Loop:   &config.LoopSpec{Items: "${ input.role_list }", As: "item"},
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
		}},
		config.InputSchemaMap{"role_list": {Type: "array"}},
		map[string]any{"role_list": "${ [register.probe.role] }"},
	)

	in := perHostRenderInput(manifest, res, perHostHosts())
	in.RegisterByHost = map[string]map[string]any{
		"a.example.com": {"probe": map[string]any{"role": "master"}},
		"b.example.com": {"probe": map[string]any{"role": "replica"}},
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, cel.ErrNoHostBound) {
		t.Fatalf("expected ErrNoHostBound for a register-derived input on the loop axis, got: %v", err)
	}
}

// hostFreeCases — the contexts that have no host to render for. Each reads a
// per-host destiny input and each must refuse BY NAME. Before NIM-908 every one of
// them would have quietly taken the roster's first host's value: that is the
// original defect one layer down, which is why direction (2) was required whatever
// direction (1) turned out to deliver.
func TestApplyInput_PerHost_RefusedWhereNoHostIsBound(t *testing.T) {
	cases := []struct {
		name    string
		task    config.Task
		context string
	}{
		{
			name: "loop.items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: "${ [input.master_addr] }", As: "item"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
			},
			context: "loop.items:/loop.when:",
		},
		{
			name: "loop.when",
			task: config.Task{
				Name: "fan",
				Loop: &config.LoopSpec{Items: []any{"a"}, As: "item", When: "input.master_addr != ''"},
				// A bare `${ item }` would make params host-invariant and let the
				// task through on the params check; the refusal must come from the
				// loop axis itself.
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
			},
			context: "loop.items:/loop.when:",
		},
		{
			name: "on covens",
			task: config.Task{
				Name:   "roll",
				On:     []any{"redis-${ input.master_addr }"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "on: [covens]",
		},
		{
			name: "static when",
			task: config.Task{
				Name:   "maybe",
				When:   "input.master_addr == '10.0.0.1'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "a static when:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, res := perHostApplyInput(
				[]config.Task{tc.task},
				config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
				map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
			)
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
			if !errors.Is(err, cel.ErrNoHostBound) {
				t.Fatalf("expected ErrNoHostBound, got: %v", err)
			}
			if !strings.Contains(err.Error(), "input.master_addr") {
				t.Errorf("the refusal must name the input: %v", err)
			}
			if !strings.Contains(err.Error(), tc.context) {
				t.Errorf("the refusal must name the %s context: %v", tc.context, err)
			}
			if strings.Contains(err.Error(), "no such key") {
				t.Errorf("the message blames a key that is spelled correctly: %v", err)
			}
		})
	}
}

// The same host-free contexts reading a host-INVARIANT input still work. Without
// this the guard above could pass by refusing every input read, which would break
// `items: ${ input.acl_users }` — a documented, used shape.
func TestApplyInput_HostFreeContextsStillReadInvariantInputs(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "fan",
			Loop:   &config.LoopSpec{Items: "${ input.acl_users }", As: "item"},
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "acl ${ item }"}},
		}},
		config.InputSchemaMap{
			"acl_users":   {Type: "array"},
			"master_addr": {Type: "string", Required: true},
		},
		map[string]any{
			"acl_users": []any{"alice", "bob"},
			// Per-host, and present in the very same apply.input: the guard must
			// refuse by NAME, not arm itself for the whole namespace.
			"master_addr": "${ soulprint.self.network.primary_ip }",
		},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("a host-invariant input on the loop axis must still resolve: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected two loop iterations, got %d", len(tasks))
	}
}

// An include group is spliced into the plan or dropped from it for every host, so
// its `when:` cannot be decided from one host's input. Separate from the table
// above because include-when is evaluated through the flow-control engine, which
// never sees cel.Vars.HostScope — the refusal is raised at the decision site.
func TestApplyInput_PerHost_RefusedInIncludeWhen(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:           "conditional",
			IncludeGroupID: 7,
			IncludeWhen:    "input.master_addr == '10.0.0.1'",
			Module:         &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		}},
		config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
		map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if !errors.Is(err, cel.ErrNoHostBound) {
		t.Fatalf("expected ErrNoHostBound for include-when, got: %v", err)
	}
	if !strings.Contains(err.Error(), "include") {
		t.Errorf("the refusal must name the include context: %v", err)
	}
}

// The destiny's `input:` contract runs per host since NIM-908, because
// `required_when` and `validate:` are predicates OVER the values: a rule can hold
// for one host and fail for another. The error has to say which host, or an
// operator reads it as an intermittent failure of the whole run.
func TestApplyInput_PerHost_ContractErrorNamesTheHost(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "noop",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		}},
		config.InputSchemaMap{"role": {Type: "string", Enum: []any{"master"}}},
		map[string]any{"role": "${ soulprint.self.role }"},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if !errors.Is(err, ErrDestinyInputInvalid) {
		t.Fatalf("expected ErrDestinyInputInvalid, got: %v", err)
	}
	// b.example.com is the replica — the value that fails the enum. It is NOT the
	// first host by SID, so naming it also proves every host is checked and not
	// just the one the old code rendered on.
	if !strings.Contains(err.Error(), "b.example.com") {
		t.Fatalf("the contract failure must name the host whose values broke it: %v", err)
	}
}

// ★ The seal invariant NIM-811 established, under a per-host apply.input: the
// sealed set must not move with the ROSTER. apply.input now renders N times instead
// of once, and the seal is collected on each of those renders — if any part of that
// collection depended on the VALUE rather than the expression, two hosts would seal
// differently from one and a two-host run would mask differently from a one-host
// run of the same plan.
//
// The existing TestRender_SealedSetDoesNotMoveWithTheRoster covers a scenario task;
// this is the applier, which is the path NIM-908 changed.
//
// ★ Honest about what it is: a TRIP-WIRE, not evidence. Every seal contribution on
// this path is collected once per task OUTSIDE the per-host loop
// ([Pipeline.renderTaskIter]), every source is schema- or raw-text-derived
// ([Pipeline.taskSealSources]), and resolveApplyInput adds nothing to Sealed at all —
// so nothing NIM-908 changed could have moved the set, and this test could not have
// failed before the change either. It exists for the edit that makes a seal source
// value-derived or roster-dependent; mutation case 24b confirms it would catch one.
func TestApplyInput_SealedSetDoesNotMoveWithTheRoster(t *testing.T) {
	build := func() (*config.ScenarioManifest, *stubDestinyResolver) {
		manifest, res := perHostApplyInput(
			[]config.Task{{
				Name: "write",
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{"content": "requirepass ${ input.pw }"},
				},
			}},
			config.InputSchemaMap{"pw": {Type: "string", Required: true, Secret: true}},
			// Host-variant BY TEXT (it reads soulprint.self) while the secret it
			// carries is not: exactly the asymmetry that would expose a
			// value-derived seal.
			map[string]any{"pw": "${ size(soulprint.self.sid) > 0 ? input.admin_password : '' }"},
		)
		return manifest, res
	}

	run := func(hosts []*topology.HostFacts) map[string]bool {
		t.Helper()
		manifest, res := build()
		p := NewPipeline(nil, newEngine(t), nil, nil)
		in := perHostRenderInput(manifest, res, hosts)
		in.Input = map[string]any{"admin_password": "s3cret"}
		in.Scenario.Input = config.InputSchemaMap{"admin_password": {Type: "string", Secret: true}}
		in.Sealed = NewSealedSet()
		if _, _, err := p.Render(context.Background(), in); err != nil {
			t.Fatalf("Render on %d host(s): %v", len(hosts), err)
		}
		return in.Sealed.Paths()
	}

	solo := run([]*topology.HostFacts{perHostHosts()[1]})
	pair := run(perHostHosts())

	if len(solo) == 0 {
		t.Fatalf("one host: nothing sealed at all — the test cannot detect a move: %v", solo)
	}
	if len(solo) != len(pair) {
		t.Fatalf("sealed set moved with the roster: one host %v, two hosts %v", solo, pair)
	}
	for path := range solo {
		if !pair[path] {
			t.Errorf("sealed path %q present on one host and absent on two", path)
		}
	}
}

// An `input:` value is not always a scalar. A list of addresses or a map of per-host
// overrides carries its `${ … }` one level down, and a classification that stopped at
// the top would read exactly the structured cases as host-invariant — which is the
// defect, on the shapes most likely to be per-host in the first place.
func TestApplyInput_NestedValueIsClassified(t *testing.T) {
	cases := map[string]any{
		"in a list": []any{"${ soulprint.self.network.primary_ip }", "127.0.0.1"},
		"in a map":  map[string]any{"addr": "${ soulprint.self.network.primary_ip }"},
		"in a list inside a map": map[string]any{
			"peers": []any{"${ soulprint.self.sid }"},
		},
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			manifest, res := perHostApplyInput(
				[]config.Task{{
					Name:   "fan",
					Loop:   &config.LoopSpec{Items: "${ [string(input.endpoints)] }", As: "item"},
					Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
				}},
				config.InputSchemaMap{"endpoints": {}},
				map[string]any{"endpoints": value},
			)
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
			if !errors.Is(err, cel.ErrNoHostBound) {
				t.Fatalf("a nested per-host expression was not classified: %v", err)
			}
		})
	}
}

// ★ The projection itself, asserted directly rather than through a refusal. A host-free
// context is protected twice — the name is absent from Input AND naming it is refused —
// and the two are independent: mutating either alone leaves every end-to-end test green,
// because the other still holds. That is the property worth having and the reason this
// test exists at the unit level.
//
// Input is what a context with no host reads, so a per-host name appearing in it is the
// original defect restored: a value that is real and belongs to the first host by SID.
func TestApplyInput_InvariantProjectionOmitsPerHostNames(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "noop",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		}},
		config.InputSchemaMap{
			"master_addr": {Type: "string", Required: true},
			"os_family":   {Type: "string"},
			"acl_users":   {Type: "array"},
			"port":        {Type: "string", Default: "6379"},
		},
		map[string]any{
			"master_addr": "${ soulprint.self.network.primary_ip }",
			// Per-host by TEXT; both hosts are debian, so the values coincide.
			"os_family": "${ soulprint.self.os.family }",
			"acl_users": []any{"alice"},
		},
	)

	p := NewPipeline(nil, newEngine(t), nil, nil)
	hosts := perHostHosts()
	applier := manifest.Tasks[0]
	byHost, invariant, _, err := p.resolveApplyInput(perHostRenderInput(manifest, res, hosts), applier, res.resolved, hosts,
		hostVariantInputNames(p.cel, applier, keeperRegisterNames(manifest)))
	if err != nil {
		t.Fatalf("resolveApplyInput: %v", err)
	}

	if _, leaked := invariant["master_addr"]; leaked {
		t.Errorf("the host-invariant projection carries the per-host name: %v — a host-free context would read the first host's address", invariant)
	}
	// The counterweight: it must carry everything else, schema defaults included, or a
	// host-free context loses names that never varied.
	for _, name := range []string{"acl_users", "port"} {
		if _, ok := invariant[name]; !ok {
			t.Errorf("the host-invariant projection dropped %q: %v", name, invariant)
		}
	}
	// ★ A name the TEXT calls per-host whose VALUES happen to coincide. Both hosts are
	// debian here, so the value safety net sees no difference and only the text
	// classification can keep it out of the projection — which is the point: the rule
	// is what the author wrote, not what this roster happened to produce. Without this
	// case the two layers cover for each other and neither is actually pinned.
	if _, leaked := invariant["os_family"]; leaked {
		t.Errorf("a per-host name whose values coincide leaked into the invariant projection: %v", invariant)
	}

	// And the per-host map carries every name, for every host, with each host's value.
	want := map[string]string{"a.example.com": "10.0.0.1", "b.example.com": "10.0.0.2"}
	for sid, addr := range want {
		if got := byHost[sid]["master_addr"]; got != addr {
			t.Errorf("%s: master_addr = %v, want %q", sid, got, addr)
		}
		if _, ok := byHost[sid]["acl_users"]; !ok {
			t.Errorf("%s: the per-host map dropped a host-invariant name: %v", sid, byHost[sid])
		}
	}
}

// ★ THE FIX'S OWN HOLE, closed. An applier can hoist the per-host expression into its
// own `vars:` and pass the VAR into the destiny:
//
//	vars: {addr: "${ soulprint.self.network.primary_ip }"}
//	apply: {input: {master_addr: "${ vars.addr }"}}
//
// The input's own text then names no per-host root, so a classification that scanned
// `apply: input:` alone read `master_addr` as invariant — and reproduced the exact
// defect NIM-908 fixes, one hop over. Measured on this tree before hostVariantVarNames
// existed: two hosts, `loop.items: ${ input.master_addr }`, `echo 10.0.0.1` dispatched
// to both, green.
//
// The closure is transitive and order-independent: `vars:` has no declaration order
// (vars.md), so `b: ${ vars.a }` may be visited before `a`.
func TestApplyInput_VarsHopIsClassifiedPerHost(t *testing.T) {
	cases := map[string]map[string]any{
		"one hop": {
			"addr": "${ soulprint.self.network.primary_ip }",
		},
		"two hops, declared in the wrong order": {
			"endpoint": "${ vars.addr }:6379",
			"addr":     "${ soulprint.self.network.primary_ip }",
		},
		"through register": {
			"addr": "${ register.probe.role }",
		},
	}
	varName := map[string]string{
		"one hop":                               "addr",
		"two hops, declared in the wrong order": "endpoint",
		"through register":                      "addr",
	}

	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			manifest, res := perHostApplyInput(
				[]config.Task{{
					Name:   "fan",
					Loop:   &config.LoopSpec{Items: "${ [input.master_addr] }", As: "item"},
					Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
				}},
				config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
				map[string]any{"master_addr": "${ vars." + varName[name] + " }"},
			)
			manifest.Tasks[0].Vars = vars

			in := perHostRenderInput(manifest, res, perHostHosts())
			in.RegisterByHost = map[string]map[string]any{
				"a.example.com": {"probe": map[string]any{"role": "master"}},
				"b.example.com": {"probe": map[string]any{"role": "replica"}},
			}
			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), in)
			if !errors.Is(err, cel.ErrNoHostBound) {
				t.Fatalf("a per-host value hoisted through the applier's vars: was read as invariant: %v", err)
			}
		})
	}
}

// The counterweight, and the reason `vars` is judged per NAME and not as a root: the
// SERVICE vars layer resolves once per run and is host-invariant by construction
// (ADR-0082). `apply: input:` is the documented channel for forwarding it into a
// destiny, so counting the whole `vars` root would refuse the shape the key exists for.
func TestApplyInput_ServiceVarsHopStaysInvariant(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "fan",
			Loop:   &config.LoopSpec{Items: "${ input.acl_users }", As: "item"},
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "acl ${ item }"}},
		}},
		config.InputSchemaMap{"acl_users": {Type: "array"}},
		map[string]any{"acl_users": "${ vars.users }"},
	)

	in := perHostRenderInput(manifest, res, perHostHosts())
	in.ServiceVars = map[string]any{"users": []any{"alice", "bob"}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("a service var forwarded through apply.input must stay host-invariant: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected two loop iterations, got %d", len(tasks))
	}
}

// hostVariantVarNames closes over a map, and a Go map has no iteration order, so a
// single pass is right or wrong by luck: with `b: ${ vars.a }` visited before `a`, one
// pass never learns that `b` is per-host. The loop is a fixpoint for that reason.
//
// Asserted directly and repeated, because an end-to-end test of the same property is a
// coin flip — it would catch a single-pass regression in about half its runs and pass
// in the other half, which is the shape of a guard nobody trusts. Over a three-link
// chain and 64 draws, a single-pass version is caught with certainty for practical
// purposes.
func TestApplyInput_VarsClosureIsTransitive(t *testing.T) {
	engine := newEngine(t)
	raw := map[string]any{
		"addr":     "${ soulprint.self.network.primary_ip }",
		"endpoint": "${ vars.addr }:6379",
		"conn":     "redis://${ vars.endpoint }",
		"timeout":  "30s",
	}
	for i := 0; i < 64; i++ {
		got := hostVariantVarNames(engine, raw, nil, nil)
		for _, name := range []string{"addr", "endpoint", "conn"} {
			if !got[name] {
				t.Fatalf("draw %d: %q not closed as per-host: %v", i, name, got)
			}
		}
		if got["timeout"] {
			t.Fatalf("draw %d: a literal var classified as per-host: %v", i, got)
		}
	}
}

// ★ THE CORPUS CASE, and the reason `register` is judged per NAME. Measured on
// `wb/service/redis` (2026-09-29): its `apply: input:` builds `config` from
// `register.system_acl_users.effective`, written by a `core.state.present` step —
// keeper-side, so one value every host reads identically — and the redis destiny
// gates a task on `when: … has(input.config.unixsocket)`, a static predicate.
//
// Under a whole-root `register` rule that `config` classified as host-variant and
// the working `when:` became a refusal: a real service, green today, red after the
// change. The price of "fail immediately" has to be zero on a correct service.
func TestApplyInput_KeeperRegisterStaysInvariant(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "gate",
			When:   "has(input.config)",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		}},
		config.InputSchemaMap{"config": {}},
		map[string]any{"config": "${ register.system_acl_users.effective }"},
	)
	// The producer: keeper-side, so its register is one value for the whole run.
	manifest.Tasks = append([]config.Task{{
		Name:     "mint",
		Register: "system_acl_users",
		Module: &config.ModuleTask{
			Module: "core.state.present",
			Params: map[string]any{"field": "system_acl_users", "value": "x"},
		},
	}}, manifest.Tasks...)

	in := perHostRenderInput(manifest, res, perHostHosts())
	in.KeeperRegister = map[string]any{"system_acl_users": map[string]any{"effective": map[string]any{"unixsocket": "/run/redis.sock"}}}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	if _, _, err := p.Render(context.Background(), in); err != nil {
		t.Fatalf("a keeper-side register forwarded through apply.input must stay host-invariant: %v", err)
	}
}

// The counterweight: a SOUL-side register is the host's own probe result, and
// forwarding it keeps the input per-host. Same shape, one word different in the
// producing task's module address.
func TestApplyInput_SoulRegisterStaysPerHost(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "fan",
			Loop:   &config.LoopSpec{Items: "${ [input.role] }", As: "item"},
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
		}},
		config.InputSchemaMap{"role": {}},
		map[string]any{"role": "${ register.probe.role }"},
	)
	manifest.Tasks = append([]config.Task{{
		Name:     "probe the role",
		Register: "probe",
		Module:   &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "redis-cli role"}},
	}}, manifest.Tasks...)

	in := perHostRenderInput(manifest, res, perHostHosts())
	in.RegisterByHost = map[string]map[string]any{
		"a.example.com": {"probe": map[string]any{"role": "master"}},
		"b.example.com": {"probe": map[string]any{"role": "replica"}},
	}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	_, _, err := p.Render(context.Background(), in)
	if !errors.Is(err, cel.ErrNoHostBound) {
		t.Fatalf("a Soul-side register is per-host; expected ErrNoHostBound on the loop axis, got: %v", err)
	}
}

// ★ THE SEAL, under PER-HOST PARAMS — the property most likely to break quietly
// once a task can render N different params structs (NIM-908, open Q #25).
//
// The claim being tested: sealing marks PATHS, not values, so per-host VALUES under
// the same path are fine and the sealed SET stays host-invariant. Verified rather
// than assumed — `collectSealed` walks the RAW params (`task.Module.Params`, the
// author's `${ … }` text) exactly once per task, before the per-host loop, and every
// source it consults is schema- or text-derived. Nothing it reads is a rendered
// value, so N renders cannot move it.
//
// The task here is genuinely per-host (`host` reads soulprint.self) AND carries a
// sealed cell (`content` reads a secret input), which is the combination that did
// not exist before this ticket: previously such a task was refused outright, so no
// test could have covered a sealed cell on a per-host task.
//
// Mutation 27 moves the collection inside the per-host loop and feeds it the
// RENDERED struct — the shape a "seal what we actually shipped" edit would take —
// and the non-vacuity assertion below is what catches it.
func TestPerHostParams_SealedSetDoesNotMoveWithTheRoster(t *testing.T) {
	scenario := func() *config.ScenarioManifest {
		return &config.ScenarioManifest{
			Name:  "seal-per-host",
			Input: config.InputSchemaMap{"admin_password": {Type: "string", Secret: true}},
			Tasks: []config.Task{{
				Name: "write redis.conf",
				Module: &config.ModuleTask{
					Module: "core.file.present",
					Params: map[string]any{
						"content": "requirepass ${ input.admin_password }",
						"host":    "${ soulprint.self.network.primary_ip }",
					},
				},
			}},
		}
	}

	run := func(hosts []*topology.HostFacts) (map[string]bool, *RenderedTask) {
		t.Helper()
		p := NewPipeline(nil, newEngine(t), nil, nil)
		in := RenderInput{
			Scenario:    scenario(),
			Input:       map[string]any{"admin_password": "s3cret"},
			Incarnation: IncarnationMeta{ID: "redis-prod"},
			Hosts:       hosts,
			Sealed:      NewSealedSet(),
		}
		tasks, _, err := p.Render(context.Background(), in)
		if err != nil {
			t.Fatalf("Render on %d host(s): %v", len(hosts), err)
		}
		return in.Sealed.Paths(), tasks[0]
	}

	solo, soloTask := run([]*topology.HostFacts{perHostHosts()[1]})
	pair, pairTask := run(perHostHosts())

	// NON-VACUITY: a seal collection that read rendered VALUES instead of the raw
	// expressions would find no `${ … }` and seal nothing — equal on both rosters,
	// and silently useless. The set has to be non-empty for the comparison to mean
	// anything.
	if !solo["content"] {
		t.Fatalf("one host: the secret-reading cell is not sealed: %v", solo)
	}
	if len(solo) != len(pair) {
		t.Fatalf("sealed set moved with the roster: one host %v, two hosts %v", solo, pair)
	}
	for path := range solo {
		if !pair[path] {
			t.Errorf("sealed path %q present on one host and absent on two", path)
		}
	}

	// And the premise: the two-host render really is per-host, or the comparison
	// above is between two host-invariant plans and proves nothing about NIM-908.
	if len(soloTask.ParamsBySID) != 0 {
		t.Errorf("one host must keep the golden path, got ParamsBySID=%v", soloTask.ParamsBySID)
	}
	if len(pairTask.ParamsBySID) != 2 {
		t.Fatalf("two hosts: expected per-host params, got %v", pairTask.ParamsBySID)
	}
	// The sealed cell itself is the SAME on both hosts (the secret does not vary);
	// only `host` does. A seal that had followed the value rather than the path
	// would have had to decide which host's `content` it was marking.
	for _, sid := range []string{"a.example.com", "b.example.com"} {
		if got := pairTask.ParamsBySID[sid].GetFields()["content"].GetStringValue(); got != "requirepass s3cret" {
			t.Errorf("%s: content = %q, want the same secret on every host", sid, got)
		}
	}
}

// ★ THE MIRROR OF THE APPLIER-VARS HOP, one layer down, and it was open until it
// was measured. A DESTINY's own `vars.yml` resolves per host, so a local reading a
// per-host `input.<name>` is per-host itself — and the reference that reaches a
// host-free decision says only `vars.<name>`, which looks like any other.
//
// Measured on this tree before hostVariantDestinyVars existed, with
// `when: "vars.addr == '10.0.0.2'"` — TRUE on b.example.com, FALSE on
// a.example.com, decided on a (first by SID) and the task skipped for BOTH.
// `targets=[]`, no error, green. b should have run it.
func TestDestinyVars_PerHostHopIsRefusedInHostFreeContexts(t *testing.T) {
	cases := []struct {
		name    string
		task    config.Task
		context string
	}{
		{
			name: "static when",
			task: config.Task{
				Name:   "gate",
				When:   "vars.addr == '10.0.0.2'",
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "a static when:",
		},
		{
			name: "include-when",
			task: config.Task{
				Name:           "gate",
				IncludeGroupID: 7,
				IncludeWhen:    "vars.addr == '10.0.0.1'",
				Module:         &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			context: "a conditional include's when:",
		},
		{
			name: "loop.items",
			task: config.Task{
				Name:   "fan",
				Loop:   &config.LoopSpec{Items: "${ [vars.addr] }", As: "item"},
				Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "echo ${ item }"}},
			},
			context: "loop.items:/loop.when:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest, res := perHostApplyInput(
				[]config.Task{tc.task},
				config.InputSchemaMap{"master_addr": {Type: "string", Required: true}},
				map[string]any{"master_addr": "${ soulprint.self.network.primary_ip }"},
			)
			// The hop: the destiny's own local reads the per-host input.
			res.resolved.Vars = map[string]any{"addr": "${ input.master_addr }"}

			p := NewPipeline(nil, newEngine(t), nil, nil)
			_, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
			if !errors.Is(err, cel.ErrNoHostBound) {
				t.Fatalf("a per-host value laundered through the destiny's vars.yml was not refused: %v", err)
			}
			if !strings.Contains(err.Error(), "vars.addr") {
				t.Errorf("the refusal must name the destiny local: %v", err)
			}
			if !strings.Contains(err.Error(), tc.context) {
				t.Errorf("the refusal must name the %s context: %v", tc.context, err)
			}
		})
	}
}

// The counterweight: a destiny local built from a HOST-INVARIANT input is not
// per-host, and every host-free context still reads it. Without this the closure
// could pass by marking every destiny var, which would refuse the ordinary shape
// `vars.yml` exists for.
func TestDestinyVars_InvariantHopStaysReadable(t *testing.T) {
	manifest, res := perHostApplyInput(
		[]config.Task{{
			Name:   "gate",
			When:   "vars.tier == 'prod'",
			Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
		}},
		config.InputSchemaMap{
			"tier":        {Type: "string"},
			"master_addr": {Type: "string", Required: true},
		},
		map[string]any{
			"tier": "prod",
			// Per-host, and in the very same apply.input: the closure must judge by
			// NAME, not arm itself for the whole destiny.
			"master_addr": "${ soulprint.self.network.primary_ip }",
		},
	)
	res.resolved.Vars = map[string]any{"tier": "${ input.tier }"}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	tasks, _, err := p.Render(context.Background(), perHostRenderInput(manifest, res, perHostHosts()))
	if err != nil {
		t.Fatalf("a destiny local over a host-invariant input must stay readable: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected the task to render, got %d", len(tasks))
	}
}

// The register classification, isolated from the VALUE safety net that sits under it
// ([hostInvariantInput] promotes a name whose values turn out to differ). Both layers
// refuse a Soul-side register, so an end-to-end test cannot tell which one did it —
// and a mutation deleting the text rule would survive on the net alone.
//
// Here the two hosts register the SAME value, so only the text classification can
// speak: `probe` is Soul-side and must be per-host anyway, `mint` is keeper-side and
// must not be.
func TestApplyInput_RegisterClassificationIsTextual(t *testing.T) {
	manifest := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{
			{Name: "probe", Register: "probe", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "role"}}},
			{Name: "mint", Register: "mint", Module: &config.ModuleTask{Module: "core.state.present", Params: map[string]any{"field": "x", "value": "y"}}},
			{Name: "apply", Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{
				"from_soul":   "${ register.probe.role }",
				"from_keeper": "${ register.mint.effective }",
				"plain":       "literal",
			}}},
		},
	}
	got := hostVariantInputNames(newEngine(t), manifest.Tasks[2], keeperRegisterNames(manifest))

	want := map[string]bool{"from_soul": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("%q classified per-host; only a Soul-side register read should be", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%q not classified per-host — a Soul-side register is the host's own bucket", name)
	}
}

// ★ The classification itself, asserted directly and isolated from the VALUE safety
// net under it ([hostInvariantInput] promotes a name whose values turn out to
// differ). Both layers refuse, so an end-to-end test cannot say which one did — and a
// mutation deleting the text rule would survive on the net alone. This one cannot:
// it calls the classifier and reads its answer.
func TestApplyInput_TextualClassificationCoversEveryRoute(t *testing.T) {
	applier := config.Task{
		Name: "apply",
		Vars: map[string]any{
			"hop":   "${ soulprint.self.network.primary_ip }",
			"plain": "fixed",
		},
		Apply: &config.ApplyTask{Destiny: "d", Input: map[string]any{
			"direct":     "${ soulprint.self.sid }",
			"via_vars":   "${ vars.hop }",
			"nested_map": map[string]any{"addr": "${ soulprint.self.sid }"},
			"nested_lst": []any{"${ soulprint.self.sid }", "x"},
			"invariant":  "${ incarnation.id }",
			"from_vars":  "${ vars.plain }",
		}},
	}
	got := hostVariantInputNames(newEngine(t), applier, nil)

	want := map[string]bool{"direct": true, "via_vars": true, "nested_map": true, "nested_lst": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("%q classified per-host; it reaches no per-host root", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%q NOT classified per-host — the text reaches soulprint.self, directly, through vars: or one level down", name)
	}
}
