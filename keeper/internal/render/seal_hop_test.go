package render

import (
	"context"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

// The seal must survive a HOP. A secret reaches a params cell either directly
// (`${ input.pw }`) or through a name that holds it — a task `vars:`, a run-level
// `compute:`, a destiny's `vars.yml`, a destiny's own `input:`. Every test here
// renders the same secret through one hop and asserts the cell is sealed; the
// asymmetry tests render it with NO hop, which was already sealed before NIM-811
// and NIM-812 and is what made the hole invisible — hoisting a repeated
// expression into a var reads as a purely cosmetic refactor and silently removed
// the masking.
//
// Everything goes through the real [Pipeline.Render] rather than [collectSealed]
// directly: the defect was never in the detector, it was in what the pipeline
// handed it, so a test that builds its own [cel.SealSources] would pass on the
// broken tree.

const hopSecret = "s3cr3t-admin-pw"

// hopScenario — a one-task scenario over a `secret: true` input, with whatever
// task the case is about.
func hopScenario(tasks ...config.Task) *config.ScenarioManifest {
	return &config.ScenarioManifest{
		Name: "create",
		Input: config.InputSchemaMap{
			"admin_password": {Type: "string", Secret: true},
			"acl_users":      {Type: "array", Secret: true},
			"port":           {Type: "string"},
		},
		Tasks: tasks,
	}
}

// hopInput — the values the run was started with.
func hopInput() map[string]any {
	return map[string]any{
		"admin_password": hopSecret,
		// The redis idiom a `loop:` exists for: a list of users carrying
		// per-user credentials, declared secret as a whole.
		"acl_users": []any{
			map[string]any{"name": "alice", "password": hopSecret},
		},
		"port": "6379",
	}
}

// renderHop runs one scenario and returns the sealed paths plus the rendered
// params of the task at index i.
func renderHop(t *testing.T, p *Pipeline, in RenderInput, i int) (map[string]bool, map[string]any) {
	t.Helper()
	in.Sealed = NewSealedSet()
	tasks, _, err := p.Render(context.Background(), in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(tasks) <= i {
		t.Fatalf("len(tasks) = %d, want at least %d", len(tasks), i+1)
	}
	return in.Sealed.Paths(), tasks[i].Params.AsMap()
}

func hopRenderInput(scn *config.ScenarioManifest) RenderInput {
	return RenderInput{
		Scenario:    scn,
		Input:       hopInput(),
		Incarnation: IncarnationMeta{ID: "redis-prod", Service: "demo-service-redis"},
		Hosts:       []*topology.HostFacts{host("a.example.com", []string{"redis"}, nil)},
	}
}

// ★ NIM-811, the ticket's own failure scenario: a secret hoisted into a task
// `vars:` and read back in params. `direct` is the same secret with no hop — it
// was sealed before the fix and must stay sealed, which is the asymmetry that
// made the defect invisible to an author.
func TestRender_SealFollowsTaskVarsHop(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "write redis.conf",
		Vars: map[string]any{"pw": "${ input.admin_password }"},
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"path":    "/etc/redis/redis.conf",
				"content": "requirepass ${ vars.pw }",
				"direct":  "requirepass ${ input.admin_password }",
				"port":    "${ input.port }",
			},
		},
	}))

	paths, params := renderHop(t, p, in, 0)

	if !paths["content"] {
		t.Errorf("the cell reading vars.pw is NOT sealed: %v — a secret routed through vars: loses its masking", paths)
	}
	if !paths["direct"] {
		t.Errorf("the cell reading input.admin_password directly is not sealed: %v", paths)
	}
	if paths["path"] || paths["port"] {
		t.Errorf("a non-secret cell was sealed: %v", paths)
	}

	// The point of the seal: this is what the run-plan write sees.
	masked := audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths})
	if got := masked["content"]; got == params["content"] {
		t.Errorf("content survived masking as %v — the plaintext lands in apply_run_plan.params", got)
	}
}

// ★ NIM-811, the `compute:` half: run-level, resolved once, and equally a hop.
func TestRender_SealFollowsComputeHop(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	scn := hopScenario(config.Task{
		Name: "write redis.conf",
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"content": "requirepass ${ compute.pw }",
				"banner":  "listening on ${ compute.endpoint }",
			},
		},
	})
	scn.Compute = config.ComputeBlock{
		{Name: "pw", Value: "${ input.admin_password }"},
		{Name: "endpoint", Value: "127.0.0.1:${ input.port }"},
	}

	paths, _ := renderHop(t, p, hopRenderInput(scn), 0)

	if !paths["content"] {
		t.Errorf("the cell reading compute.pw is NOT sealed: %v", paths)
	}
	if paths["banner"] {
		t.Errorf("the cell reading a non-secret compute entry was sealed: %v", paths)
	}
}

// A compute entry may read an earlier one, so the taint must accumulate in
// declaration order — the same order resolveCompute evaluates in.
func TestRender_SealComputeTransitiveInDeclarationOrder(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	scn := hopScenario(config.Task{
		Name:   "write redis.conf",
		Module: &config.ModuleTask{Module: "core.file.present", Params: map[string]any{"content": "${ compute.line }"}},
	})
	scn.Compute = config.ComputeBlock{
		{Name: "pw", Value: "${ input.admin_password }"},
		{Name: "line", Value: "requirepass ${ compute.pw }"},
	}

	paths, _ := renderHop(t, p, hopRenderInput(scn), 0)

	if !paths["content"] {
		t.Errorf("compute.line derived from compute.pw did not carry the taint: %v", paths)
	}
}

// `${ vault(…) }` hoisted into a task var — the shape `examples/` uses most, and
// the one the dragonfly scenario annotates by hand ("vault()-in-cell (seal
// masking)") precisely because the hop used to drop the seal.
func TestRender_SealFollowsVaultInTaskVar(t *testing.T) {
	kv := stubKVRender{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": hopSecret},
	}}
	e, err := cel.New(cel.WithVault(kv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	p := NewPipeline(kv, e, nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "write redis.conf",
		Vars: map[string]any{"pw": "${ vault('secret/redis/admin#password') }"},
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{"content": "requirepass ${ vars.pw }"},
		},
	}))

	paths, params := renderHop(t, p, in, 0)

	if !paths["content"] {
		t.Errorf("the cell reading a vault()-backed var is NOT sealed: %v", paths)
	}
	if got := params["content"]; got != "requirepass "+hopSecret {
		t.Fatalf("content = %v — the test is not carrying the secret it claims to", got)
	}
}

// Transitivity WITHIN the vars layer: `b` reads `a`, and a var is evaluated when it
// is read rather than in map order, so the taint cannot be decided in one pass over
// a map. Both spellings are asserted so the test does
// not pass by accident of iteration order.
func TestRender_SealTaskVarsTransitiveWithinLayer(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "write redis.conf",
		Vars: map[string]any{
			"pw":   "${ input.admin_password }",
			"line": "requirepass ${ vars.pw }",
			"tail": "${ vars.line } # generated",
		},
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"content": "${ vars.line }",
				"footer":  "${ vars.tail }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if !paths["content"] || !paths["footer"] {
		t.Errorf("a var derived from a sealed var did not carry the taint: %v", paths)
	}
}

// No over-seal: a var that reads nothing secret leaves its readers alone. Without
// this the fix could "pass" by sealing every cell that mentions vars at all,
// which would mask an operator's whole diagnostic surface.
func TestRender_SealTaskVarsNoOverSeal(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "write redis.conf",
		Vars: map[string]any{
			"conf_dir": "/etc/redis",
			"endpoint": "127.0.0.1:${ input.port }",
		},
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"path":    "${ vars.conf_dir }/redis.conf",
				"content": "bind ${ vars.endpoint }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if len(paths) != 0 {
		t.Errorf("sealed paths = %v, want none — no var in this task reads a secret", paths)
	}
}

// The hop on the keeper-side path (renderKeeperTask), which collects its seal at
// its own call site: a fix applied to one of the two leaves the other open.
func TestRender_SealFollowsTaskVarsHopOnKeeperTask(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "stash the credential",
		On:   config.KeeperTarget,
		Vars: map[string]any{"pw": "${ input.admin_password }"},
		Module: &config.ModuleTask{
			Module: "core.state.present",
			Params: map[string]any{
				"path":  "auth.password",
				"value": "${ vars.pw }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if !paths["value"] {
		t.Errorf("a keeper-side cell reading vars.pw is NOT sealed: %v", paths)
	}
	if paths["path"] {
		t.Errorf("a non-secret keeper-side cell was sealed: %v", paths)
	}
}

// destinyWith builds a destiny whose `input:` declares password secret, plus
// whatever tasks and locals the case needs.
func destinyWith(vars map[string]any, tasks ...config.Task) *ResolvedDestiny {
	return &ResolvedDestiny{
		Name: "redis",
		Input: config.InputSchemaMap{
			"password": {Type: "string", Required: true, Secret: true},
			"bind":     {Type: "string", Default: "127.0.0.1"},
		},
		Vars:  vars,
		Tasks: tasks,
	}
}

// applyHopScenario — a scenario that hands its own secret input to a destiny.
// `apply: input:` is the ONLY channel into a destiny (destiny isolation, ADR-009
// V2), so this is the shape the DSL steers a credential through.
func applyHopScenario() *config.ScenarioManifest {
	scn := hopScenario(config.Task{
		Name: "configure",
		Apply: &config.ApplyTask{
			Destiny: "redis",
			Input:   map[string]any{"password": "${ input.admin_password }"},
		},
	})
	return scn
}

// ★ NIM-812: `${ input.<secret> }` written INSIDE a destiny, against the
// destiny's own schema. Before the fix the destiny pass got a synthetic manifest
// with no Input at all, so secretInputNames returned nil and nothing about the
// cell was detectable.
func TestRender_ApplyDestinySealsItsOwnSecretInput(t *testing.T) {
	dst := destinyWith(nil, config.Task{
		Name: "write redis.conf",
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"path":    "/etc/redis/redis.conf",
				"content": "requirepass ${ input.password }",
				"listen":  "bind ${ input.bind }",
			},
		},
	})
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(applyHopScenario())
	in.Destiny = &stubDestinyResolver{resolved: dst}

	paths, params := renderHop(t, p, in, 0)

	if !paths["content"] {
		t.Errorf("a destiny cell reading its own secret input is NOT sealed: %v", paths)
	}
	if paths["listen"] || paths["path"] {
		t.Errorf("a non-secret destiny cell was sealed: %v", paths)
	}
	if got := params["content"]; got != "requirepass "+hopSecret {
		t.Fatalf("content = %v — the secret did not actually cross into the destiny", got)
	}
	masked := audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths})
	if masked["content"] == params["content"] {
		t.Errorf("content survived masking — the plaintext lands in apply_run_plan.params")
	}
}

// The same hop one layer down: a destiny local from `vars.yml` holding the
// destiny's secret input. It is the file layer of the same `vars.*` namespace as
// a task var, resolved once per pass, and leaving it out would reopen NIM-811
// inside a destiny.
func TestRender_ApplyDestinySealsVarsYmlHop(t *testing.T) {
	dst := destinyWith(
		map[string]any{"pw": "${ input.password }", "conf_dir": "/etc/redis"},
		config.Task{
			Name: "write redis.conf",
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{
					"path":    "${ vars.conf_dir }/redis.conf",
					"content": "requirepass ${ vars.pw }",
				},
			},
		})
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(applyHopScenario())
	in.Destiny = &stubDestinyResolver{resolved: dst}

	paths, _ := renderHop(t, p, in, 0)

	if !paths["content"] {
		t.Errorf("a destiny cell reading a vars.yml local that holds the secret input is NOT sealed: %v", paths)
	}
	if paths["path"] {
		t.Errorf("a cell reading a non-secret destiny local was sealed: %v", paths)
	}
}

// The task-var layer INSIDE a destiny pass: the same hop as NIM-811's, over the
// destiny's own input schema rather than the scenario's. It reaches params
// through the same collector, but only if both fixes are in — the schema has to
// be carried (NIM-812) for the var's value to be detectable at all.
//
// The task var is written against `input.` directly: the hop under test is the
// one from the destiny's own input schema.
func TestRender_ApplyDestinySealsTaskVarOverOwnInput(t *testing.T) {
	dst := destinyWith(
		nil,
		config.Task{
			Name: "write redis.conf",
			Vars: map[string]any{"line": "requirepass ${ input.password }"},
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"content": "${ vars.line }"},
			},
		})
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(applyHopScenario())
	in.Destiny = &stubDestinyResolver{resolved: dst}

	paths, _ := renderHop(t, p, in, 0)

	if !paths["content"] {
		t.Errorf("a destiny task var reading the destiny's own secret input did not carry the taint: %v", paths)
	}
}

// The schema a destiny is judged against is ITS OWN, never the caller's: it sees
// only what `apply: input:` handed it, under its own names, and judging it by the
// caller's would mean the isolation boundary leaked into the seal.
//
// The two schemas have to DISAGREE for that to be testable, so `admin_password` —
// the caller's secret name — is declared here as an ordinary non-secret destiny
// input. A cell reading it must come out unsealed while the destiny's own
// `password` is sealed, and only the destiny's schema satisfies both at once:
// wiring the caller's would seal `callers` too.
func TestRender_ApplyDestinyJudgedByItsOwnSchemaNotTheCallers(t *testing.T) {
	dst := destinyWith(nil, config.Task{
		Name: "record",
		Module: &config.ModuleTask{
			Module: "core.file.present",
			Params: map[string]any{
				"own":     "requirepass ${ input.password }",
				"callers": "audited-by ${ input.admin_password }",
			},
		},
	})
	dst.Input["admin_password"] = &config.InputSchema{Type: "string", Default: "audit"}

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(applyHopScenario())
	in.Destiny = &stubDestinyResolver{resolved: dst}

	paths, _ := renderHop(t, p, in, 0)

	if !paths["own"] {
		t.Errorf("the destiny's own secret input is not sealed: %v", paths)
	}
	if paths["callers"] {
		t.Errorf("sealed %v — `admin_password` is secret only in the CALLER's schema, and that schema does not reach inside a destiny", paths)
	}
}

// ★ GUARD (NIM-811, the ticket's ordering note): the sealed set must not move
// with the ROSTER.
//
// A task's `vars:` resolve per task per host, while the seal collection
// deliberately runs once per task, because a cell's provenance is host-invariant.
// Every other case in this file runs on a single host, so none of them can tell a
// once-per-task collection from a per-host one. Here the var expression IS
// host-variant (it reads `soulprint.self`) while its value is not — params must
// stay host-invariant or the pilot rejects the render outright — and the same
// scenario is rendered over one host and over two: same sealed set, or the seal
// depends on who is in the roster and a two-host run masks differently from a
// one-host run of the same plan.
func TestRender_SealedSetDoesNotMoveWithTheRoster(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	scenario := func() *config.ScenarioManifest {
		return hopScenario(config.Task{
			Name: "write redis.conf",
			Vars: map[string]any{
				"pw": "${ size(soulprint.self.sid) > 0 ? input.admin_password : '' }",
			},
			Module: &config.ModuleTask{
				Module: "core.file.present",
				Params: map[string]any{"content": "requirepass ${ vars.pw }"},
			},
		})
	}

	one := hopRenderInput(scenario())
	solo, _ := renderHop(t, p, one, 0)

	two := hopRenderInput(scenario())
	two.Hosts = []*topology.HostFacts{
		host("a.example.com", []string{"redis"}, nil),
		host("b.example.com", []string{"redis"}, nil),
	}
	pair, _ := renderHop(t, p, two, 0)

	if !solo["content"] {
		t.Fatalf("one host: the cell reading vars.pw is not sealed: %v", solo)
	}
	if !reflect.DeepEqual(solo, pair) {
		t.Errorf("sealed set moved with the roster: one host %v, two hosts %v", solo, pair)
	}
}

// ★ NIM-822: a `loop:` over a secret list, read through the DEFAULT bind name.
// `items:` puts the secret into the binding and every `${ item.<field> }` in
// params reads it back out — the same asymmetry `vars:` had, with `loop:` in its
// place: the identical value read without the loop was already sealed.
func TestRender_SealFollowsLoopItemsHop(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "set acl",
		Loop: &config.LoopSpec{Items: "${ input.acl_users }"},
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{
				"cmd":  "ACL SETUSER ${ item.name } >${ item.password }",
				"port": "${ input.port }",
			},
		},
	}))

	paths, params := renderHop(t, p, in, 0)

	if !paths["cmd"] {
		t.Errorf("the cell reading item.password is NOT sealed: %v — a loop over a secret renders plaintext into apply_run_plan", paths)
	}
	if paths["port"] {
		t.Errorf("a non-secret cell was sealed: %v", paths)
	}
	masked := audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths})
	if got := masked["cmd"]; got == params["cmd"] {
		t.Errorf("cmd survived masking as %v — the credential is served by GET .../runs/{apply_id}/tasks", got)
	}
}

// ★ NIM-823: the same binding under an author-chosen name. `item` is only the
// DEFAULT of `loop.as:`, so this is one mechanism with two spellings, not two
// defects — and a fix that hardcoded `item` would pass the test above and leave
// this one open.
//
// `index_as:` binds a position, never a value out of items, and must stay
// unsealed: sealing it would mask every `${ i }` in the run.
func TestRender_SealFollowsLoopAsHopAndSparesIndex(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "set acl",
		Loop: &config.LoopSpec{Items: "${ input.acl_users }", As: "cred", IndexAs: "i"},
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{
				"cmd":   "ACL SETUSER ${ cred.name } >${ cred.password }",
				"whole": "${ cred }",
				"label": "user ${ i }",
			},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if !paths["cmd"] {
		t.Errorf("the cell reading the `as:` bind is NOT sealed: %v", paths)
	}
	// A bare whole-binding read reaches no Select node, so nothing in the
	// detector visited it before the address reshape.
	if !paths["whole"] {
		t.Errorf("the bare `${ cred }` read is NOT sealed: %v — a whole-binding read carries the whole element", paths)
	}
	if paths["label"] {
		t.Errorf("the index bind was sealed: %v — over-seal costs every diagnostic naming the iteration", paths)
	}
}

// No secret in `items:`, no taint: the loop bind is only as sealed as the
// expression it was bound from. Without this the fix could be "seal every loop
// bind", which passes both tests above and masks the whole plan.
func TestRender_LoopOverPublicItemsIsNotSealed(t *testing.T) {
	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(hopScenario(config.Task{
		Name: "open ports",
		Loop: &config.LoopSpec{Items: []any{"6379", "16379"}},
		Module: &config.ModuleTask{
			Module: "core.exec.run",
			Params: map[string]any{"cmd": "ufw allow ${ item }"},
		},
	}))

	paths, _ := renderHop(t, p, in, 0)

	if len(paths) != 0 {
		t.Errorf("sealed %v — nothing in this task reads a secret", paths)
	}
}

// ★ `core.file.rendered` RELOCATES the vars cell, so the path collected off the
// raw params matches nothing in what is written.
//
// renderTaskIter pulls `params.vars` out and deletes the key; the values reappear
// under `render_context.vars`, resolved. A seal recorded as `vars.pw` therefore
// masks nothing, exactly the way `${ input.secret }` in a template masks nothing
// without the declarative S-1 mark beside it. This is the vars half of that
// mechanism, and it only became derivable once the vars taint existed at all.
func TestRender_SealMarksRelocatedRenderContextVars(t *testing.T) {
	const tmpl = "conf/redis.conf.tmpl"
	scn := hopScenario(config.Task{
		Name: "render redis.conf",
		Vars: map[string]any{"pw": "${ input.admin_password }", "bind": "127.0.0.1"},
		Module: &config.ModuleTask{
			Module: moduleFileRendered,
			Params: map[string]any{
				"path":     "/etc/redis/redis.conf",
				"template": tmpl,
				// The author-written passthrough: this whole key is extracted
				// and deleted from params before the task is written out.
				"vars": map[string]any{"pw": "${ vars.pw }", "bind": "${ vars.bind }"},
			},
		},
	})

	p := NewPipeline(nil, newEngine(t), nil, nil)
	in := hopRenderInput(scn)
	in.Templates = fakeReader{files: map[string][]byte{
		tmpl: []byte("requirepass {{ .vars.pw }}\nbind {{ .vars.bind }}\n"),
	}}

	paths, params := renderHop(t, p, in, 0)

	if paths["vars.pw"] && !paths["render_context.vars.pw"] {
		t.Errorf("the seal still names the PRE-relocation path: %v — params.vars is deleted, so this masks nothing", paths)
	}
	if !paths["render_context.vars.pw"] {
		t.Errorf("render_context.vars.pw is NOT sealed: %v — the resolved secret is written to apply_run_plan under that path", paths)
	}
	if paths["render_context.vars.bind"] {
		t.Errorf("a non-secret var was sealed: %v", paths)
	}

	// The cell really is there, holding the resolved secret, under that path.
	masked := audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths})
	rc, _ := masked[paramRenderContext].(map[string]any)
	rcVars, _ := rc[paramVars].(map[string]any)
	if rcVars == nil {
		t.Fatalf("render_context.vars is absent — the test no longer exercises the relocation: %v", masked)
	}
	if got := rcVars["pw"]; got == hopSecret {
		t.Errorf("render_context.vars.pw survived masking as %v", got)
	}
	if got := rcVars["bind"]; got != "127.0.0.1" {
		t.Errorf("a non-secret var was masked: %v — over-seal reaches the rendered config", got)
	}
}

// renderedRenderContextVars renders the one `core.file.rendered` task of in and returns
// its sealed paths with render_context.vars before and after masking.
func renderedRenderContextVars(t *testing.T, in RenderInput) (paths map[string]bool, plain, masked map[string]any) {
	t.Helper()
	paths, params := renderHop(t, NewPipeline(nil, newEngine(t), nil, nil), in, 0)
	rcVars := func(params map[string]any) map[string]any {
		rc, _ := params[paramRenderContext].(map[string]any)
		vars, _ := rc[paramVars].(map[string]any)
		if vars == nil {
			t.Fatalf("render_context.vars is absent: %v", params)
		}
		return vars
	}
	return paths, rcVars(params), rcVars(audit.MaskSecretsSealed(params, audit.SealOpts{Sealed: paths}))
}

// ★ GUARD (points 15 and 17). A template reads a service name directly, with no
// params.vars: the service definition reads a task override over a secret input, so
// the name is tainted in the task's view, and render_context.vars carries its
// resolved value — which only a declarative mark masks, since no raw params cell ever
// held it.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in renderTaskIter, hand
// sealRenderContextVars the names tainted in task.Vars alone —
// `own := scenarioSealSources(in); p.sealViewVars(own, []cel.VarLayer{p.taskRung(task,
// cel.Vars{})}); sealRenderContextVars(in.Sealed, sealedVarNamesOf(own))`. It goes red.
func TestRender_SealMarksAServiceNameTheTemplateReadsDirectly(t *testing.T) {
	const tmpl = "conf/redis.conf.tmpl"
	in := hopRenderInput(hopScenario(config.Task{
		Name: "render redis.conf",
		Vars: map[string]any{"password": "${ input.admin_password }"},
		Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
			"path": "/etc/redis/redis.conf", "template": tmpl,
		}},
	}))
	in.ServiceVars = serviceView(t, map[string]any{"dsn": "redis://:${ vars.password }@h", "port": "6379"})
	in.Templates = fakeReader{files: map[string][]byte{tmpl: []byte("dsn {{ .vars.dsn }}\nport {{ .vars.port }}\n")}}

	paths, plain, masked := renderedRenderContextVars(t, in)
	if plain["dsn"] != "redis://:"+hopSecret+"@h" {
		t.Fatalf("render_context.vars.dsn = %v — the test no longer carries the secret through the service name", plain["dsn"])
	}
	if !paths["render_context.vars.dsn"] {
		t.Errorf("render_context.vars.dsn is NOT sealed: %v", paths)
	}
	if masked["dsn"] == plain["dsn"] {
		t.Errorf("render_context.vars.dsn survived masking as %v — the plaintext lands in apply_run_plan", masked["dsn"])
	}
	if masked["port"] != "6379" {
		t.Errorf("a public service name was masked: %v", masked["port"])
	}
}

// ★ GUARD. A params.vars key that is no name of the task's view is relocated to
// render_context.vars like any other, so its seal has to be: the masker matches paths
// exactly, and `vars.k` — what the raw params walk records — names no cell of what is
// written.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: drop the collectSealed walk of
// params.vars under render_context in renderTaskIter. It goes red.
func TestRender_SealMarksAParamsVarsKeyThatNamesNoViewVar(t *testing.T) {
	const tmpl = "conf/redis.conf.tmpl"
	in := hopRenderInput(hopScenario(config.Task{
		Name: "render redis.conf",
		Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
			"path":     "/etc/redis/redis.conf",
			"template": tmpl,
			"vars": map[string]any{
				"k":      "${ input.admin_password }",
				"nested": map[string]any{"list": []any{"${ input.admin_password }"}},
				"bind":   "127.0.0.1",
			},
		}},
	}))
	in.Templates = fakeReader{files: map[string][]byte{tmpl: []byte("{{ .vars.k }} {{ .vars.nested }} {{ .vars.bind }}\n")}}

	paths, plain, masked := renderedRenderContextVars(t, in)
	if plain["k"] != hopSecret {
		t.Fatalf("render_context.vars.k = %v — the test no longer relocates the cell", plain["k"])
	}
	for _, path := range []string{"render_context.vars.k", "render_context.vars.nested.list[0]"} {
		if !paths[path] {
			t.Errorf("%s is NOT sealed: %v", path, paths)
		}
	}
	if masked["k"] == hopSecret {
		t.Errorf("render_context.vars.k survived masking")
	}
	if list := masked["nested"].(map[string]any)["list"].([]any); list[0] == hopSecret {
		t.Errorf("render_context.vars.nested.list[0] survived masking")
	}
	if masked["bind"] != "127.0.0.1" || paths["render_context.vars.bind"] {
		t.Errorf("a public params.vars key was sealed: %v (paths %v)", masked["bind"], paths)
	}
}

// ★ GUARD. A secret one level down in params.vars — a map's field, a list's element —
// is sealed under the path it is written to, and masked there: the walk follows
// params.vars to every depth, as it does any params cell.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in renderTaskIter, walk only
// the top-level strings of params.vars under render_context —
// `for k, v := range pv.(map[string]any) { if s, ok := v.(string); ok { collectSealed(p.cel, in.Sealed, map[string]any{paramVars: map[string]any{k: s}}, sources, paramRenderContext) } }`.
// It goes red.
func TestRender_SealMarksParamsVarsAtDepth(t *testing.T) {
	const tmpl = "conf/redis.conf.tmpl"
	in := hopRenderInput(hopScenario(config.Task{
		Name: "render redis.conf",
		Module: &config.ModuleTask{Module: moduleFileRendered, Params: map[string]any{
			"path":     "/etc/redis/redis.conf",
			"template": tmpl,
			"vars": map[string]any{
				"creds": map[string]any{"pw": "${ input.admin_password }", "user": "admin"},
				"k":     []any{"${ input.admin_password }", "public"},
			},
		}},
	}))
	in.Templates = fakeReader{files: map[string][]byte{tmpl: []byte("{{ .vars.creds.pw }} {{ index .vars.k 0 }}\n")}}

	paths, plain, masked := renderedRenderContextVars(t, in)
	if plain["creds"].(map[string]any)["pw"] != hopSecret || plain["k"].([]any)[0] != hopSecret {
		t.Fatalf("render_context.vars = %v — the test no longer relocates the secret cells", plain)
	}
	for _, path := range []string{"render_context.vars.creds.pw", "render_context.vars.k[0]"} {
		if !paths[path] {
			t.Errorf("%s is NOT sealed: %v", path, paths)
		}
	}
	creds, k := masked["creds"].(map[string]any), masked["k"].([]any)
	if creds["pw"] == hopSecret {
		t.Errorf("render_context.vars.creds.pw survived masking")
	}
	if k[0] == hopSecret {
		t.Errorf("render_context.vars.k[0] survived masking")
	}
	if creds["user"] != "admin" || k[1] != "public" || paths["render_context.vars.creds.user"] || paths["render_context.vars.k[1]"] {
		t.Errorf("a public cell beside a secret one was sealed: creds %v, k %v (paths %v)", creds, k, paths)
	}
}
