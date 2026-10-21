package render

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/config"
)

// What a linear run needs from its Souls before its first step (NIM-946, ADR-0091
// item 10).

func needHosts() []*topology.HostFacts {
	return []*topology.HostFacts{
		host(sidA, []string{"svc"}, nil),
		host(sidB, []string{"svc", "db"}, nil),
	}
}

// surveyInput is the first pass of a linear run over m.
func surveyInput(m *config.ScenarioManifest, hosts []*topology.HostFacts) RenderInput {
	plan, steps := config.LinearPassage(m.Tasks, m.Settings)
	return RenderInput{
		Scenario:      m,
		Input:         map[string]any{"want": "ok"},
		Incarnation:   IncarnationMeta{ID: "svc"},
		Hosts:         hosts,
		TaskPassage:   plan.TaskPassage,
		ActivePassage: 0,
		Steps:         steps,
		HeldTargets:   NewHeldTargets(),
		SoulNeeds:     NewSoulNeeds(),
	}
}

func surveyNeeds(t *testing.T, in RenderInput) map[string]SoulNeed {
	t.Helper()
	if _, _, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), in); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := map[string]SoulNeed{}
	for _, n := range in.SoulNeeds.List() {
		if _, dup := out[n.Task]; dup {
			t.Fatalf("task %q surveyed twice", n.Task)
		}
		out[n.Task] = n
	}
	return out
}

const needsPlan = `
name: needs
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: check
    module: core.exec.run
    failed_when: register.self.stdout != register.probe.stdout
    params: { cmd: check }
  - name: plain
    module: core.exec.run
    changed_when: register.self.rc != 0
    params: { cmd: plain }
  - name: decided
    module: core.exec.run
    when: register.probe.stdout == 'yes'
    params: { cmd: decided }
  - name: retried
    module: core.exec.run
    retry: { count: 3, until: "register.self.rc == 0" }
    params: { cmd: retried }
  - name: each
    module: core.exec.run
    loop: { items: "${ input.items }", as: it }
    failed_when: register.self.out != input.want
    params: { cmd: "${ it }" }
`

// ★ GUARD. The first pass states every step's needs, not only its own: a later step's
// predicate that the Keeper will substitute into needs the rendered_predicate bit, one
// that reads only register.self does not, and a `when:` the Keeper decides travels
// blank and needs nothing.
//
// Mutations: survey only the active step → check is missing; mark every predicate
// rendered → plain is; require flow_control for a decided `when:` → decided does.
func TestSoulNeeds_WholePlanBeforeTheFirstStep(t *testing.T) {
	in := surveyInput(loadStagedManifest(t, needsPlan), needHosts())
	in.Input["items"] = []any{"x", "y"}
	got := surveyNeeds(t, in)
	both := []string{sidA, sidB}
	want := map[string]SoulNeed{
		"probe":   {Step: 0, Task: "probe", Module: "core.exec.run", TargetSIDs: both},
		"check":   {Step: 1, Task: "check", Module: "core.exec.run", FlowControl: true, RenderedPredicate: true, TargetSIDs: both},
		"plain":   {Step: 2, Task: "plain", Module: "core.exec.run", FlowControl: true, TargetSIDs: both},
		"decided": {Step: 3, Task: "decided", Module: "core.exec.run", TargetSIDs: both},
		"retried": {Step: 4, Task: "retried", Module: "core.exec.run", Retry: true, TargetSIDs: both},
		"each":    {Step: 5, Task: "each", Module: "core.exec.run", FlowControl: true, RenderedPredicate: true, TargetSIDs: both},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("needs:\n got %+v\nwant %+v", got, want)
	}
}

// ★ GUARD. A host a static `where:` or `on:` excludes is not asked; a task whose
// targeting reads a register is left to its step; what the render drops before its
// step — a static `when:` false, an `assert:` — and a keeper-side task send nothing.
//
// by_probe's `where:` evaluates without the probe — to every host — and to fewer once
// the probe ran: an up-front answer would ask hosts its step never sends to; so does
// by_hosts_literal's, whose read sits inside a `soulprint.hosts.where("…")` string, and
// by_var_hosts_literal's, whose var does. A register read in `vars:` matters only to a
// `where:`: vars_no_where's hosts are known.
//
// Mutations: resolve every task against the whole roster → only_a and only_db name
// both hosts; survey a register-reading `where:` → by_probe appears; read the
// `where:` without inlining the hosts literal → by_hosts_literal appears; read a var
// by the quote-blind regex alone → by_var_hosts_literal appears; judge `vars:` without
// a `where:` → vars_no_where is missing.
func TestSoulNeeds_TargetingKnownBeforeTheRun(t *testing.T) {
	m := loadStagedManifest(t, `
name: targeting
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: only_a
    module: core.exec.run
    where: soulprint.self.sid == 'a.example.com'
    params: { cmd: a }
  - name: only_db
    module: core.exec.run
    on: [db]
    params: { cmd: db }
  - name: by_probe
    module: core.exec.run
    where: "!has(register.probe) || register.probe.stdout == 'yes'"
    params: { cmd: p }
  - name: by_var
    module: core.exec.run
    vars: { pick: "${ register.probe.stdout }" }
    where: vars.pick == 'yes'
    params: { cmd: v }
  - name: vars_no_where
    module: core.exec.run
    vars: { c: "${ register.probe.stdout }" }
    params: { cmd: "${ vars.c }" }
  - name: by_hosts_literal
    module: core.exec.run
    where: soulprint.hosts.where("!has(register.probe)").exists(h, h.sid == soulprint.self.sid)
    params: { cmd: h }
  - name: by_var_hosts_literal
    module: core.exec.run
    vars: { pending: "${ soulprint.hosts.where(\"!has(register.probe)\") }" }
    where: size(vars.pending) > 0
    params: { cmd: p }
  - name: off
    module: core.exec.run
    when: input.want == 'other'
    params: { cmd: off }
  - name: record
    module: core.state.set
    params: { field: f, value: v }
`)
	got := surveyNeeds(t, surveyInput(m, needHosts()))
	var names []string
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"only_a", "only_db", "probe", "vars_no_where"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("surveyed %v, want %v", names, want)
	}
	if sids := got["only_a"].TargetSIDs; !reflect.DeepEqual(sids, []string{sidA}) {
		t.Errorf("only_a targets %v, want [%s]", sids, sidA)
	}
	if sids := got["only_db"].TargetSIDs; !reflect.DeepEqual(sids, []string{sidB}) {
		t.Errorf("only_db targets %v, want [%s]", sids, sidB)
	}
}

// statePlan reads incarnation.state in a static `when:`, an include's `when:` and a
// `where:`; capture puts a capture before them, after them, or nowhere.
func statePlan(capture string) string {
	const record = `  - name: record
    module: core.state.set
    params: { field: done, value: "yes" }
`
	src := `
name: state
tasks:
  - name: probe
    module: core.exec.run
    params: { cmd: probe }
`
	if capture == "before" {
		src += record
	}
	src += `  - name: after_done
    module: core.exec.run
    when: has(incarnation.state.done)
    params: { cmd: after }
  - name: included
    module: core.exec.run
    params: { cmd: included }
  - name: before_done
    module: core.exec.run
    where: "!has(incarnation.state.done)"
    params: { cmd: before }
`
	if capture == "after" {
		src += record
	}
	return src
}

// ★ GUARD. A predicate over incarnation.state is decided against the state of its own
// step, which only a capture at an earlier step changes: after one, a static `when:`
// or an include's `when:` reading the state counts as sent, and a `where:` reading it
// leaves the task's hosts to the step. With the capture after them, or none, the state
// at their step is the run's snapshot, and all three are decided up front.
//
// Mutations: let a static `when:` over moving state drop the task → after_done is
// missing with the capture before; evaluate the include's `when:` regardless →
// included is; count a `where:` over moving state as known → before_done appears; treat
// a capture anywhere in the plan as moving the state → the "after" case surveys
// after_done and included, and not before_done.
func TestSoulNeeds_StateACaptureChangesIsDecidedAtTheStep(t *testing.T) {
	for _, c := range []struct {
		capture string
		want    []string
	}{
		{capture: "before", want: []string{"after_done", "included", "probe"}},
		{capture: "after", want: []string{"before_done", "probe"}},
		{capture: "none", want: []string{"before_done", "probe"}},
	} {
		m := loadStagedManifest(t, statePlan(c.capture))
		for i := range m.Tasks {
			if m.Tasks[i].Name == "included" {
				m.Tasks[i].IncludeGroupID, m.Tasks[i].IncludeWhen = 1, "has(incarnation.state.done)"
			}
		}
		in := surveyInput(m, needHosts())
		in.State = map[string]any{}
		got := surveyNeeds(t, in)
		var names []string
		for name := range got {
			names = append(names, name)
		}
		sort.Strings(names)
		if !reflect.DeepEqual(names, c.want) {
			t.Errorf("capture %s: surveyed %v, want %v", c.capture, names, c.want)
		}
	}
}

// ★ GUARD. The state moves from the first capture the render keeps — a dropped one
// writes nothing — at its step, not its position in the task list; and a predicate
// reads it through a task's `vars:` and through `compute:` as well as directly, while
// a service var does not move with it.
//
// A `when:` cannot read compute at all — the flow context carries none — so one that
// tries fails its task at the step and sends nothing.
//
// Mutations: take the last capture → late_by_var and late_by_compute appear and
// late_static_var does not; take the task's index for its step, or count the dropped
// capture → early_static appears; stop following `vars:` or `compute:` → late_by_var
// or late_by_compute appears; stop following one compute entry, one var or a var's
// compute through another → the matching *_chain / late_by_var_of_compute appears;
// let a static `when:` over a moving var drop its task → late_static_var is missing;
// treat every var as moving → late_by_service_var is; let a `when:` read moving
// compute → late_when_compute appears.
func TestSoulNeeds_StateMovesFromTheFirstKeptCapture(t *testing.T) {
	m := loadStagedManifest(t, `
name: moving
compute:
  ready: "${ has(incarnation.state.done) }"
  ready_too: "${ compute.ready }"
tasks:
  - name: pair
    block:
      - { name: one, module: core.exec.run, params: { cmd: one } }
      - { name: two, module: core.exec.run, params: { cmd: two } }
      - { name: three, module: core.exec.run, params: { cmd: three } }
  - name: early_by_var
    module: core.exec.run
    vars: { flag: "${ has(incarnation.state.done) }" }
    where: "!vars.flag"
    params: { cmd: early }
  - name: skipped_capture
    module: core.state.set
    when: input.want == 'other'
    params: { field: done, value: "no" }
  - name: early_static
    module: core.exec.run
    when: has(incarnation.state.done)
    params: { cmd: static }
  - name: record
    module: core.state.set
    params: { field: done, value: "yes" }
  - name: late_by_var
    module: core.exec.run
    vars: { flag: "${ has(incarnation.state.done) }" }
    where: "!vars.flag"
    params: { cmd: late }
  - name: late_by_compute
    module: core.exec.run
    where: "!compute.ready"
    params: { cmd: computed }
  - name: late_static_var
    module: core.exec.run
    vars: { flag: "${ has(incarnation.state.done) }" }
    when: vars.flag
    params: { cmd: flagged }
  - name: late_by_service_var
    module: core.exec.run
    where: "vars.region == 'eu'"
    params: { cmd: region }
  - name: late_by_compute_chain
    module: core.exec.run
    where: "!compute.ready_too"
    params: { cmd: chain }
  - name: late_by_var_chain
    module: core.exec.run
    vars: { a: "${ has(incarnation.state.done) }", b: "${ vars.a }" }
    where: "!vars.b"
    params: { cmd: varchain }
  - name: late_by_var_of_compute
    module: core.exec.run
    vars: { c: "${ compute.ready }" }
    where: "!vars.c"
    params: { cmd: varcompute }
  - name: late_when_compute
    module: core.exec.run
    when: compute.ready
    params: { cmd: whencompute }
  - name: record_again
    module: core.state.set
    params: { field: again, value: "yes" }
`)
	in := surveyInput(m, needHosts())
	in.State = map[string]any{}
	in.ServiceVars = serviceView(t, map[string]any{"region": "eu"})
	got := surveyNeeds(t, in)
	var names []string
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"early_by_var", "late_by_service_var", "late_static_var", "one", "three", "two"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("surveyed %v, want %v", names, want)
	}
}

// ★ GUARD. Inside a block rolled as one group the group's registers stay open: a
// child's `when:` reading a sibling travels and needs flow_control, a predicate reading
// only the group is not rendered, one reading anything else is. A default block's
// children are surveyed at their own steps, each with its own `where:`.
//
// Mutations: treat a grouped child like a step (its `when:` decided) → start has no
// flow_control; close the group's registers → check's failed_when reads as rendered.
func TestSoulNeeds_Blocks(t *testing.T) {
	m := loadStagedManifest(t, `
name: blocks
tasks:
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: stop
        module: core.exec.run
        register: stop
        params: { cmd: stop }
      - name: start
        module: core.exec.run
        when: register.stop.rc == 0
        params: { cmd: start }
      - name: check
        module: core.exec.run
        failed_when: register.self.rc != register.stop.rc
        params: { cmd: check }
      - name: verify
        module: core.exec.run
        failed_when: register.self.out != input.want
        params: { cmd: verify }
  - name: steps
    block:
      - name: first
        module: core.exec.run
        params: { cmd: first }
      - name: second
        module: core.exec.run
        where: soulprint.self.sid == 'b.example.com'
        params: { cmd: second }
`)
	got := surveyNeeds(t, surveyInput(m, needHosts()))
	both := []string{sidA, sidB}
	want := map[string]SoulNeed{
		"stop":   {Step: 0, Task: "stop", Module: "core.exec.run", TargetSIDs: both},
		"start":  {Step: 0, Task: "start", Module: "core.exec.run", FlowControl: true, TargetSIDs: both},
		"check":  {Step: 0, Task: "check", Module: "core.exec.run", FlowControl: true, TargetSIDs: both},
		"verify": {Step: 0, Task: "verify", Module: "core.exec.run", FlowControl: true, RenderedPredicate: true, TargetSIDs: both},
		"first":  {Step: 1, Task: "first", Module: "core.exec.run", TargetSIDs: both},
		"second": {Step: 2, Task: "second", Module: "core.exec.run", TargetSIDs: []string{sidB}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("needs:\n got %+v\nwant %+v", got, want)
	}
}

// ★ GUARD (user decision 2026-10-08, option A). A later applier's destiny is resolved
// up front and its tasks are surveyed on the applier's hosts with their own
// predicates, which the Soul evaluates; a task behind a static `when:`, or narrowing
// its own hosts with `where:`, is left to the step, where the destiny's input decides it.
//
// Mutations: skip appliers → install is missing; count a static-`when:` task → tls
// appears; count a destiny task's own `where:` → debian_only appears on every host.
func TestSoulNeeds_ApplierDestinyResolvedUpFront(t *testing.T) {
	m := loadStagedManifest(t, `
name: applier
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: deploy
    register: deploy
    apply:
      destiny: redis
      input: { tls: false }
`)
	in := surveyInput(m, needHosts())
	in.Destiny = &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name:  "redis",
		Input: config.InputSchemaMap{"tls": {Type: "boolean", Default: false}},
		Tasks: []config.Task{
			{Name: "install", Module: &config.ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "redis"}}},
			{Name: "tls", When: "input.tls", Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{"path": "/tls"}}},
			{Name: "unit", When: "register.install.changed", Module: &config.ModuleTask{Module: "core.service.running", Params: map[string]any{"name": "redis"}}},
			{Name: "debian_only", Where: "soulprint.self.os.family == 'debian'", Module: &config.ModuleTask{Module: "core.sysctl.present", Params: map[string]any{"name": "vm.x"}}},
		},
	}}
	got := surveyNeeds(t, in)
	both := []string{sidA, sidB}
	want := map[string]SoulNeed{
		"probe":          {Step: 0, Task: "probe", Module: "core.exec.run", TargetSIDs: both},
		"deploy/install": {Step: 1, Task: "deploy/install", Module: "core.pkg.installed", TargetSIDs: both},
		"deploy/unit":    {Step: 1, Task: "deploy/unit", Module: "core.service.running", FlowControl: true, TargetSIDs: both},
		"deploy":         {Step: 1, Task: "deploy", Module: applierRegisterModule, TargetSIDs: both},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("needs:\n got %+v\nwant %+v", got, want)
	}
}

// A destiny that does not resolve up front is left to the applier's step, which
// reports the error as it does without the survey: the first pass does not fail.
func TestSoulNeeds_UnresolvedDestinyIsLeftToItsStep(t *testing.T) {
	m := loadStagedManifest(t, `
name: applier
tasks:
  - name: probe
    module: core.exec.run
    params: { cmd: probe }
  - name: deploy
    apply: { destiny: missing }
`)
	in := surveyInput(m, needHosts())
	in.Destiny = &stubDestinyResolver{err: context.DeadlineExceeded}
	got := surveyNeeds(t, in)
	if len(got) != 1 || got["probe"].Module == "" {
		t.Errorf("needs = %+v, want probe alone", got)
	}
}

// The survey runs once per run: a later pass leaves what the first one collected.
func TestSoulNeeds_FilledByTheFirstPassOnly(t *testing.T) {
	in := surveyInput(loadStagedManifest(t, needsPlan), needHosts())
	in.Input["items"] = []any{"x"}
	first := surveyNeeds(t, in)
	in.ActivePassage = 2
	in.RegisterByHost = map[string]map[string]any{sidA: {"probe": map[string]any{"stdout": "a"}}, sidB: {"probe": map[string]any{"stdout": "b"}}}
	in.Hosts = needHosts()[:1]
	if again := surveyNeeds(t, in); !reflect.DeepEqual(again, first) {
		t.Errorf("a later pass changed the survey:\n got %+v\nwant %+v", again, first)
	}
}

// ★ GUARD (the survey and the render cannot drift). For a plan whose targeting is
// known before the run, what the first pass says each task sends is what the pass of
// that task's step does send: the same hosts, and on every host the Keeper does not
// skip, the same features off the wire. No task of this plan fails before dispatch, so
// none escapes the comparison that way.
//
// Mutation: change any decision of the survey (hosts, a feature, a step) without the
// render → red.
func TestSoulNeeds_AgreesWithEachStepsRender(t *testing.T) {
	m := loadStagedManifest(t, needsPlan+`
  - name: only_db
    module: core.exec.run
    on: [db]
    failed_when: register.self.rc != vars.limit
    vars: { limit: 3 }
    params: { cmd: db }
  - name: canary
    serial: 1
    settings: { serial_scope: block }
    block:
      - name: stop
        module: core.exec.run
        register: stop
        params: { cmd: stop }
      - name: start
        module: core.exec.run
        when: register.stop.rc == 0
        retry: { count: 2, until: "register.self.rc == register.stop.rc" }
        params: { cmd: start }
  - name: again
    module: core.exec.run
    retry: { count: 2 }
    params: { cmd: again }
  - name: deploy
    register: deploy
    apply: { destiny: redis }
`)
	in := surveyInput(m, needHosts())
	in.Input["items"] = []any{"x", "y"}
	in.Destiny = &stubDestinyResolver{resolved: &ResolvedDestiny{
		Name: "redis",
		Tasks: []config.Task{
			{Name: "install", Register: "install", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "install"}}},
			{Name: "unit", When: "register.install.changed", FailedWhen: "register.self.rc != 0", Module: &config.ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "unit"}}},
		},
	}}
	needs := surveyNeeds(t, in)
	// The render names a destiny task by itself and the applier's register by its
	// terminal task; the survey names both after the applier.
	needFor := func(rt *RenderedTask) (SoulNeed, string, bool) {
		for _, name := range []string{rt.Name, "deploy/" + rt.Name} {
			if n, ok := needs[name]; ok {
				return n, name, true
			}
		}
		if rt.Name == "applier-register deploy" {
			n, ok := needs["deploy"]
			return n, "deploy", ok
		}
		return SoulNeed{}, "", false
	}

	reg := map[string]map[string]any{
		sidA: {"probe": map[string]any{"stdout": "yes"}},
		sidB: {"probe": map[string]any{"stdout": "yes"}},
	}
	plan, _ := config.LinearPassage(m.Tasks, m.Settings)
	seen := map[string]bool{}
	for step := 0; step < plan.Count; step++ {
		pass := in
		pass.SoulNeeds = nil
		pass.ActivePassage = step
		pass.RegisterByHost = reg
		tasks, plans, err := NewPipeline(nil, newEngine(t), nil, nil).Render(context.Background(), pass)
		if err != nil {
			t.Fatalf("Render at step %d: %v", step, err)
		}
		targets := map[int][]string{}
		for _, pl := range plans {
			targets[pl.TaskIndex] = pl.TargetSIDs
		}
		for _, rt := range tasks {
			if rt.Passage != step || len(targets[rt.Index]) == 0 {
				continue
			}
			need, name, ok := needFor(rt)
			if !ok {
				t.Errorf("step %d sends %q, which the survey does not name", step, rt.Name)
				continue
			}
			seen[name] = true
			if need.Step != step || need.Module != rt.Module || !reflect.DeepEqual(need.TargetSIDs, targets[rt.Index]) {
				t.Errorf("%q: survey {step %d %s %v}, render {step %d %s %v}", rt.Name, need.Step, need.Module, need.TargetSIDs, step, rt.Module, targets[rt.Index])
			}
			for _, sid := range targets[rt.Index] {
				hf := rt.FlowBySID[sid]
				if hf.Failure != nil {
					t.Errorf("%q fails on %s before dispatch (%s): the comparison would skip it", rt.Name, sid, hf.Failure.Code)
					continue
				}
				if hf.Skip {
					continue
				}
				f := SentFlow(rt, sid)
				flow := f.When != "" || f.ChangedWhen != "" || f.FailedWhen != ""
				retry := f.Until != "" || rt.RetryCount > 1
				if flow != need.FlowControl || retry != need.Retry || f.Rendered != need.RenderedPredicate {
					t.Errorf("%q on %s: survey {flow %v retry %v rendered %v}, wire {flow %v retry %v rendered %v}",
						rt.Name, sid, need.FlowControl, need.Retry, need.RenderedPredicate, flow, retry, f.Rendered)
				}
			}
		}
	}
	for name := range needs {
		if !seen[name] {
			t.Errorf("the survey names %q, which no step sent", name)
		}
	}
}

// surveyedNames is the sorted task names a survey states needs for.
func surveyedNames(got map[string]SoulNeed) []string {
	names := make([]string, 0, len(got))
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ★ GUARD (ADR-0082 amendment 2026-10-15, point 16). A predicate reads moving state
// through a name of the task's VIEW, not only of its own `vars:`: a scenario var over
// incarnation.state moves once a capture has run. So a `where:` reading it leaves the
// task's hosts to the step, and a static `when:` or an include's `when:` reading it
// counts as sent — the include's through the service and scenario rungs it reads. A
// string nested in a definition counts, and so does a name read through another.
// Before the capture the state is the run's snapshot, and early is decided up front.
//
// Mutations: close moving vars over the task's own `vars:` only → by_where, by_nested
// and by_chain appear, by_static_when, by_nested_when and by_include are missing; give
// the include's `when:` no vars → by_include is missing; read a definition's top-level
// strings only → by_nested appears and by_nested_when is missing.
func TestSoulNeeds_AScenarioVarOverStateMovesItsReaders(t *testing.T) {
	m := loadStagedManifest(t, `
name: scenario-moving
vars:
  phase: "${ has(incarnation.state.done) ? 'late' : 'early' }"
  nested: { flag: "${ has(incarnation.state.done) }" }
  label: "${ vars.phase }"
tasks:
  - name: early
    module: core.exec.run
    where: "vars.phase == 'early'"
    params: { cmd: e }
  - name: record
    module: core.state.set
    params: { field: done, value: "yes" }
  - name: by_where
    module: core.exec.run
    where: "vars.phase == 'early'"
    params: { cmd: w }
  - name: by_nested
    module: core.exec.run
    where: "!vars.nested.flag"
    params: { cmd: n }
  - name: by_chain
    module: core.exec.run
    where: "vars.label == 'early'"
    params: { cmd: c }
  - name: by_static_when
    module: core.exec.run
    when: "vars.phase == 'late'"
    params: { cmd: s }
  - name: by_nested_when
    module: core.exec.run
    when: vars.nested.flag
    params: { cmd: nw }
  - name: by_include
    module: core.exec.run
    params: { cmd: i }
`)
	for i := range m.Tasks {
		if m.Tasks[i].Name == "by_include" {
			m.Tasks[i].IncludeGroupID, m.Tasks[i].IncludeWhen = 1, "vars.phase == 'late'"
		}
	}
	in := surveyInput(m, needHosts())
	in.State = map[string]any{}
	want := []string{"by_include", "by_nested_when", "by_static_when", "early"}
	if got := surveyedNames(surveyNeeds(t, in)); !reflect.DeepEqual(got, want) {
		t.Errorf("surveyed %v, want %v", got, want)
	}
}

// ★ GUARD (point 16). targetingKnown walks a task's `vars:` at any depth and follows a
// `where:`'s names through the view: a register read nested in a task var leaves the
// task's hosts to its step, and so does moving state nested in a scenario var that a
// task var reads by name. A nested literal changes nothing.
//
// Mutations: scan a task var's top-level strings only for a register → deep_register
// appears; close moving vars over the task's own `vars:` only → deep_through_name
// appears.
func TestSoulNeeds_TargetingKnownWalksVarsAtDepthThroughNames(t *testing.T) {
	m := loadStagedManifest(t, `
name: deep-targeting
vars:
  gate: { open: "${ has(incarnation.state.done) }" }
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: probe }
  - name: record
    module: core.state.set
    params: { field: done, value: "yes" }
  - name: deep_register
    module: core.exec.run
    vars: { cfg: { who: "${ has(register.probe) ? register.probe.stdout : 'none' }" } }
    where: "vars.cfg.who == 'none'"
    params: { cmd: r }
  - name: deep_through_name
    module: core.exec.run
    vars: { mine: "${ vars.gate }" }
    where: "!vars.mine.open"
    params: { cmd: n }
  - name: deep_literal
    module: core.exec.run
    vars: { cfg: { who: none } }
    where: "vars.cfg.who == 'none'"
    params: { cmd: l }
`)
	in := surveyInput(m, needHosts())
	in.State = map[string]any{}
	want := []string{"deep_literal", "probe"}
	if got := surveyedNames(surveyNeeds(t, in)); !reflect.DeepEqual(got, want) {
		t.Errorf("surveyed %v, want %v", got, want)
	}
}
