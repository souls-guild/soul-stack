package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadScenarioManifest_Golden(t *testing.T) {
	// Local patched copy of the golden (soul-lint/testdata/scenario-golden/): a
	// self-contained redis-create fixture, historically derived from the
	// redis-cluster create scenario. The derivation is intentional — the original
	// had a deviation from input.md (`type: object` without `properties:`); here
	// the form is fixed to the normative schema.
	path := filepath.FromSlash("../../soul-lint/testdata/scenario-golden/redis-create.yml")
	cfg, doc, diags, err := LoadScenarioManifest(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if cfg == nil || doc == nil {
		t.Fatalf("cfg/doc must be non-nil")
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("[%s] %s:%d:%d %s %s", d.Code, d.File, d.Line, d.Column, d.Message, d.YAMLPath)
		}
		t.Fatalf("expected 0 errors on golden scenario, got %d diagnostics", len(diags))
	}
	if cfg.Name != "create" {
		t.Errorf("name: got %q want create", cfg.Name)
	}
	// The golden writes state through explicit `core.state.set` tasks ([ADR-0084]);
	// there is no top-level block left to parse.
	captures := 0
	for i := range cfg.Tasks {
		if m := cfg.Tasks[i].Module; m != nil && m.Module == "core.state.set" {
			captures++
		}
	}
	if captures != 4 {
		t.Errorf("core.state.set captures: got %d want 4", captures)
	}
	if len(cfg.Tasks) == 0 {
		t.Errorf("tasks must be parsed")
	}
	// Discriminator round-trip smoke: the first task is module:.
	if cfg.Tasks[0].Module == nil {
		t.Errorf("tasks[0].Module must be set (provision uses core.cloud.created)")
	}
	if cfg.Tasks[0].Module != nil && cfg.Tasks[0].Module.Module != "core.cloud.created" {
		t.Errorf("tasks[0].Module.Module: got %q", cfg.Tasks[0].Module.Module)
	}
}

func TestLoadScenarioManifest_MissingName(t *testing.T) {
	src := `description: no name
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for absent name")
	}
}

func TestLoadScenarioManifest_BadName(t *testing.T) {
	src := `name: Create
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format")
	}
}

func TestLoadScenarioManifest_MissingTasks(t *testing.T) {
	src := `name: noop
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.tasks" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on $.tasks")
	}
}

func TestLoadScenarioManifest_EmptyTasksOK(t *testing.T) {
	// Empty tasks: [] is valid (no-op scenario).
	src := `name: noop
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for empty tasks: []")
	}
}

func TestLoadScenarioManifest_DeprecatedKeys(t *testing.T) {
	for _, key := range []string{"wait", "filter", "version", "state_changes"} {
		key := key
		t.Run(key, func(t *testing.T) {
			src := "name: x\ntasks: []\n" + key + ": foo\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "unknown_key" && d.YAMLPath == "$."+key && d.Hint != "" {
					found = true
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected unknown_key with hint for deprecated %q", key)
			}
			// There must be no duplicate unknown_key on the same path.
			count := 0
			for _, d := range diags {
				if d.Code == "unknown_key" && d.YAMLPath == "$."+key {
					count++
				}
			}
			if count != 1 {
				dump(t, diags)
				t.Fatalf("expected exactly 1 unknown_key for $.%s, got %d", key, count)
			}
		})
	}
}

func TestLoadScenarioManifest_TaskNoDiscriminator(t *testing.T) {
	src := `name: x
tasks:
  - name: bare task
    when: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_missing") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_missing")
	}
}

func TestLoadScenarioManifest_TaskMultiDiscriminator(t *testing.T) {
	src := `name: x
tasks:
  - name: both
    module: core.exec.run
    params: { cmd: "true" }
    apply:
      destiny: redis
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_multiple") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_multiple")
	}
}

func TestLoadScenarioManifest_AllFourDiscriminators(t *testing.T) {
	// Edge case: one valid task of each kind.
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
  - apply:
      destiny: redis
      input: { action: apply }
  - include: install.yml
  - block:
      - module: core.exec.run
        params: { cmd: "id" }
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors")
	}
	if cfg.Tasks[0].Module == nil || cfg.Tasks[1].Apply == nil ||
		cfg.Tasks[2].Include == nil || cfg.Tasks[3].Block == nil {
		t.Fatalf("discriminator fields not populated: %#v", cfg.Tasks)
	}
	if cfg.Tasks[2].Include.Include != "install.yml" {
		t.Errorf("include name not captured: %q", cfg.Tasks[2].Include.Include)
	}
	if len(cfg.Tasks[3].Block.Block) != 1 {
		t.Errorf("block content not captured: %#v", cfg.Tasks[3].Block.Block)
	}
}

// TestLoadScenarioManifest_AssertTask — an assert task (ADR-009 amendment
// 2026-06-23): a valid form parses into AssertSpec (assert discriminator).
func TestLoadScenarioManifest_AssertTask(t *testing.T) {
	src := `name: x
tasks:
  - name: topology guard
    when: "input.redis_type == 'cluster'"
    assert:
      that:
        - "size(soulprint.hosts) == int(input.shards)"
      message: "topology mismatch"
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors on valid assert task")
	}
	if cfg.Tasks[0].Assert == nil {
		t.Fatalf("Assert field not populated: %#v", cfg.Tasks[0])
	}
	if len(cfg.Tasks[0].Assert.That) != 1 {
		t.Errorf("Assert.That = %#v, want 1 predicate", cfg.Tasks[0].Assert.That)
	}
	if cfg.Tasks[0].Assert.Message != "topology mismatch" {
		t.Errorf("Assert.Message = %q", cfg.Tasks[0].Assert.Message)
	}
}

// TestLoadScenarioManifest_AssertEmptyThat — empty that[] → error
// (assert requires at least one predicate).
func TestLoadScenarioManifest_AssertEmptyThat(t *testing.T) {
	src := `name: x
tasks:
  - assert:
      that: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on empty assert.that")
	}
}

// TestLoadScenarioManifest_AssertWithModuleConflict — assert ⊕ module:
// (assert is a discriminator, mutually exclusive with module/apply/include/block).
func TestLoadScenarioManifest_AssertWithModuleConflict(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    assert:
      that: [ "true" ]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_multiple") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_multiple for assert + module")
	}
}

func TestLoadScenarioManifest_BadModuleFormat(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "module_format_invalid") {
		dump(t, diags)
		t.Fatalf("expected module_format_invalid for 2-level form")
	}
}

func TestLoadScenarioManifest_SerialRunOnceConflict(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    serial: 1
    run_once: true
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "serial_run_once_conflict") {
		dump(t, diags)
		t.Fatalf("expected serial_run_once_conflict")
	}
}

func TestLoadScenarioManifest_RegisterOnBlock(t *testing.T) {
	src := `name: x
tasks:
  - register: r
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "register_on_block_invalid") {
		dump(t, diags)
		t.Fatalf("expected register_on_block_invalid")
	}
}

// TestLoadScenarioManifest_BlockForbiddenKeys (guard #8) — module-specific keys
// on a block task are cut fail-closed with code <key>_on_block_invalid
// (destiny/tasks.md §6.5 does not mention them on block). async: is also rejected.
//
// ★ `output:` is still cut on a block, but by `output_unsupported` (NIM-334) — it
// is unimplemented on EVERY kind, so it left the _on_block_invalid family rather
// than claiming to be meaningless only here.
func TestLoadScenarioManifest_BlockForbiddenKeys(t *testing.T) {
	cases := map[string]string{
		"changed_when_on_block_invalid": "changed_when: \"true\"",
		"failed_when_on_block_invalid":  "failed_when: \"false\"",
		"retry_on_block_invalid":        "retry: { count: 3 }",
		"timeout_on_block_invalid":      "timeout: 30s",
		"output_unsupported":            "output: { x: \"y\" }",
		"params_on_block_invalid":       "params: { a: 1 }",
		"async_on_block_invalid":        "async: true",
	}
	for wantCode, line := range cases {
		t.Run(wantCode, func(t *testing.T) {
			src := "name: x\ntasks:\n  - " + line + "\n    block:\n      - module: core.exec.run\n        params: { cmd: \"true\" }\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, wantCode) {
				dump(t, diags)
				t.Fatalf("expected %s", wantCode)
			}
		})
	}
}

// TestLoadScenarioManifest_ParallelIsReserved — `parallel:` is no longer a task
// key (ADR-0075): it is held for a future concurrent group with a join, which is
// a different construct from asynchrony rather than a synonym. Writing it is the
// ordinary fail-closed `unknown_key` of the strict manifest walker — NOT a
// silently ignored key, which is what would let a plan claim concurrency it
// never gets.
func TestLoadScenarioManifest_ParallelIsReserved(t *testing.T) {
	src := "name: x\ntasks:\n  - module: core.exec.run\n    parallel: true\n    params: { cmd: \"true\" }\n"
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for the reserved parallel: key")
	}
}

// TestLoadScenarioManifest_AsyncOnApply — `async:` on an apply: task is the
// other construct that expands into a group, and ADR-0075 defers group
// asynchrony. Render drops the flag there (renderApplyDestiny never sees the
// applier task), so the group would run sequentially with nothing said about
// it — fail-closed instead, exactly like async_on_block_invalid.
func TestLoadScenarioManifest_AsyncOnApply(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      destiny: redis
      input: {}
    async: true
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "async_on_apply_invalid", "$.tasks[0].async") {
		dump(t, diags)
		t.Fatalf("expected async_on_apply_invalid")
	}
}

// The negative half: the gate is about the applier, not about `async:`. An
// ordinary module task next to an apply: one keeps the key.
func TestLoadScenarioManifest_AsyncOnApply_ModuleTaskUnaffected(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    async: true
    params: { cmd: "true" }
  - apply:
      destiny: redis
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "async_on_apply_invalid") {
		dump(t, diags)
		t.Fatalf("async: on a module task must not be flagged")
	}
}

// TestLoadScenarioManifest_AsyncOnKeeper — the third and last construct where
// `async:` has no meaning. Render already refused it; this moves the refusal to
// where the author is looking (NIM-247).
func TestLoadScenarioManifest_AsyncOnKeeper(t *testing.T) {
	src := `name: x
tasks:
  - module: core.soul.registered
    async: true
    params: { sid: a.example.com }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "async_on_keeper_invalid", "$.tasks[0].async") {
		dump(t, diags)
		t.Fatalf("expected async_on_keeper_invalid")
	}
}

// Negative — `require:` on a keeper task stays legal (redundant, since the
// keeper executor runs its tasks in plan order), and `async:` on an ordinary
// Soul-side task is untouched by the keeper gate.
func TestLoadScenarioManifest_AsyncOnKeeper_NeighboursUnaffected(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    async: true
    register: probe
    params: { cmd: "true" }
  - module: core.soul.registered
    require: [probe]
    params: { sid: a.example.com }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "async_on_keeper_invalid") {
		dump(t, diags)
		t.Fatalf("require: on a keeper task and async: on a Soul-side task must both pass")
	}
}

// TestLoadScenarioManifest_WhenOnKeeperDynamic — ★ [ADR-0084] F-D. `when:` is a
// Soul-side predicate; a keeper task never reaches a Soul, so a register-/
// soulprint-reading one was accepted and dropped, and the step ran every time.
// Since [ADR-0084] the step that runs every time is the step that writes
// incarnation state, which is why the offline half is an ERROR.
func TestLoadScenarioManifest_WhenOnKeeperDynamic(t *testing.T) {
	for name, when := range map[string]string{
		"register":  "register.probe.changed",
		"soulprint": "soulprint.self.os.family == 'debian'",
		"mixed":     "input.provision && register.probe.changed",
	} {
		t.Run(name, func(t *testing.T) {
			// The producing task keeps the fixture free of
			// unknown_register_reference, so the file's only defect is the one
			// under test. No `on:` key: since NIM-749 the capture's ADDRESS is what
			// makes it keeper-side, and writing the key here would both add a
			// second defect (`on_keeper_redundant`) and let this test pass with the
			// derivation reverted.
			src := "name: x\ntasks:\n  - module: core.exec.run\n    register: probe\n    params: { cmd: \"true\" }\n" +
				"  - module: core.state.set\n    when: \"" + when + "\"\n    params: { field: provisioned, value: yes }\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCodeAt(diags, "when_on_keeper_dynamic_unsupported", "$.tasks[1].when") {
				dump(t, diags)
				t.Fatalf("expected when_on_keeper_dynamic_unsupported for when: %q", when)
			}
		})
	}
}

// The negative half, and the one a too-wide rule would break: a STATIC when: on
// a keeper task is the working form — the keeper settles it at render, before the
// task is routed keeper-side. A dynamic when: on an ordinary Soul-side task is
// legal gating and must stay untouched.
func TestLoadScenarioManifest_WhenOnKeeperStaticAccepted(t *testing.T) {
	src := `name: x
tasks:
  - module: core.state.set
    when: input.provision
    params: { field: provisioned, value: yes }
  - module: core.exec.run
    when: register.probe.changed
    params: { cmd: "true" }
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "when_on_keeper_dynamic_unsupported") {
		dump(t, diags)
		t.Fatalf("a static when: on a keeper task and a dynamic when: on a Soul-side task must both pass")
	}
}

// TestLoadScenarioManifest_SideFollowsTheModuleAddress — NIM-749: the side of a
// task is read off its MODULE, and the task does not restate it.
//
// The core module sets are disjoint (`core.state`/`core.cloud`/`core.soul`/… on
// the keeper, the other twenty-one on the Soul side), so the address decides on
// its own. What used to be here was the opposite rule — `state_capture_not_on_keeper`,
// which refused a capture that did NOT carry `on: keeper` — and it existed only
// to force the author to write what the engine already knew. Both halves are
// asserted together because they are one rule seen from two sides: the keyless
// form is accepted, and the key is refused.
func TestLoadScenarioManifest_SideFollowsTheModuleAddress(t *testing.T) {
	capture := func(on string) string {
		return `name: x
tasks:
  - name: record the owner
    module: core.state.set
` + on + `    params:
      field: owner
      value: alice
`
	}

	t.Run("a capture without on: is accepted", func(t *testing.T) {
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(capture("")), ValidateOptions{})
		if diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("a keeper-side module needs no on: key — the address routes it")
		}
	})

	t.Run("on: keeper on a core keeper-side address is redundant", func(t *testing.T) {
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(capture("    on: keeper\n")), ValidateOptions{})
		if !hasCodeAt(diags, "on_keeper_redundant", "$.tasks[0].on") {
			dump(t, diags)
			t.Fatalf("expected on_keeper_redundant at the on: key")
		}
	})

	// A coven list on a keeper-side module is a DIFFERENT mistake, and refusing it
	// is not optional: render never reads `task.On` on a keeper task, so the labels
	// select nothing and vanish without a word — the silent-wrong-target class. It
	// gets its own code because "redundant" would be a lie about it: the literal
	// restates something true, a list states something false.
	t.Run("a coven list is refused under its own code", func(t *testing.T) {
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(capture("    on: [\"primary\"]\n")), ValidateOptions{})
		if !hasCodeAt(diags, "on_covens_on_keeper_module", "$.tasks[0].on") {
			dump(t, diags)
			t.Fatalf("expected on_covens_on_keeper_module at the on: key")
		}
		if hasCode(diags, "on_keeper_redundant") {
			dump(t, diags)
			t.Fatalf("a coven list is not the redundant-literal case")
		}
	})

	// And the same list on a Soul-side module is ordinary targeting.
	t.Run("a coven list on a Soul-side module is untouched", func(t *testing.T) {
		src := `name: x
tasks:
  - name: restart
    module: core.service.restarted
    on: ["primary"]
    params: { name: redis }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("on: [coven] is what the key is FOR on a Soul-side task")
		}
	})

	// The boundary the epic draws: a PLUGIN address keeps the literal, and keeps
	// it permanently. The keeper DOES execute such a plugin now (NIM-758), but by
	// reading `side:` in the artifact's schema document — which lives in its
	// plugin cache, not in the service repo this package parses. Nothing
	// available offline declares a plugin's side, so refusing the key would leave
	// those scenarios with no spelling at all.
	t.Run("a plugin address keeps on: keeper", func(t *testing.T) {
		src := `name: x
tasks:
  - name: create the VM
    module: wb-cloud.vm.created
    on: keeper
    params: { flavor: small }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "on_keeper_redundant") {
			dump(t, diags)
			t.Fatalf("on: keeper on a plugin address is still the only way to route it")
		}
		if diag.HasErrors(diags) {
			dump(t, diags)
			t.Fatalf("a plugin address with on: keeper must still validate")
		}
	})
}

// TestLoadScenarioManifest_KeeperChecksWithoutTheKey — the four constructs that
// are meaningless keeper-side are judged by the task's SIDE, not by a written
// `on: keeper` (NIM-749). Each case carries no `on:` at all, which is the form
// every keeper-side task takes from now on.
//
// ★ The `when:` case is the defect this closes. It used to require the key, so a
// dynamic predicate on a capture written without one went through untouched: the
// file said the step was conditional, the step wrote state every time, and
// nothing anywhere said so.
func TestLoadScenarioManifest_KeeperChecksWithoutTheKey(t *testing.T) {
	for name, tc := range map[string]struct {
		key  string
		code string
		path string
	}{
		"dynamic when:": {
			key:  "    when: \"register.probe.stdout == 'yes'\"\n",
			code: "when_on_keeper_dynamic_unsupported",
			path: "$.tasks[0].when",
		},
		"async:": {
			key:  "    async: true\n",
			code: "async_on_keeper_invalid",
			path: "$.tasks[0].async",
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := `name: x
tasks:
  - name: record the owner
    module: core.state.set
` + tc.key + `    params:
      field: owner
      value: alice
`
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCodeAt(diags, tc.code, tc.path) {
				dump(t, diags)
				t.Fatalf("expected %s at %s with no on: key written", tc.code, tc.path)
			}
		})
	}

	// The negative half: the same keys on a Soul-side module stay legal. Without
	// it the checks could fire on everything and still look correct.
	t.Run("a Soul-side module is untouched", func(t *testing.T) {
		src := `name: x
tasks:
  - name: probe
    module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - name: restart
    module: core.service.restarted
    when: "register.probe.stdout == 'yes'"
    async: true
    params: { name: redis }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "when_on_keeper_dynamic_unsupported") || hasCode(diags, "async_on_keeper_invalid") {
			dump(t, diags)
			t.Fatalf("a Soul-side task must keep when:/async:")
		}
	})
}

// TestLoadScenarioManifest_BlockOnKeeper — ★ regression for a PANIC. A block
// carrying `on: keeper` used to pass validation and then crash the render: the
// Render loop tests IsKeeperTask before it tests task.Block, so the task went
// into renderKeeperTask, which dereferenced its nil Module.
//
// Both levels are refused, because neither works. `on: keeper` on the block is
// the crash; a keeper-side CHILD is fanned out through the Soul-side roster
// resolve, which rejects it as a routing bug. Blocks and keeper tasks are
// disjoint - the diagnostic says so at whichever level the author wrote it.
//
// ★ Since NIM-749 the child needs no `on:` to be caught: the module decides, so
// the diagnostic anchors on `module:`. That is load-bearing rather than cosmetic
// — [render.IsKeeperTask] is consulted on top-level tasks only, so a keeper-side
// module reached through a block is dispatched to a host either way, and
// dropping the now-redundant key would otherwise have removed the only check.
func TestLoadScenarioManifest_BlockOnKeeper(t *testing.T) {
	t.Run("on the block", func(t *testing.T) {
		src := `name: x
tasks:
  - name: record the topology
    on: keeper
    block:
      - module: core.state.set
        params: { field: mode, value: sentinel }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCodeAt(diags, "block_on_keeper_invalid", "$.tasks[0].block") {
			dump(t, diags)
			t.Fatalf("expected block_on_keeper_invalid at the block key")
		}
	})

	t.Run("on a block child", func(t *testing.T) {
		src := `name: x
tasks:
  - name: record the topology
    block:
      - module: core.state.set
        params: { field: mode, value: sentinel }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCodeAt(diags, "block_on_keeper_invalid", "$.tasks[0].block[0].module") {
			dump(t, diags)
			t.Fatalf("expected block_on_keeper_invalid at the child's module: key")
		}
	})

	t.Run("a plain block is untouched", func(t *testing.T) {
		src := `name: x
tasks:
  - name: configure
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "block_on_keeper_invalid") {
			dump(t, diags)
			t.Fatalf("a block with no keeper task must pass")
		}
	})
}

// TestLoadScenarioManifest_ApplyWhenDynamic — an applier's condition is decided
// before its destiny is rendered, so a `when:` reading register/soulprint has
// nowhere to be evaluated. It used to be dropped and the destiny applied
// everywhere, the author's gate included (NIM-245).
func TestLoadScenarioManifest_ApplyWhenDynamic(t *testing.T) {
	for name, when := range map[string]string{
		"register":  "register.probe.changed",
		"soulprint": "soulprint.self.os.family == 'debian'",
	} {
		t.Run(name, func(t *testing.T) {
			src := "name: x\ntasks:\n  - apply:\n      destiny: redis\n      input: {}\n    when: \"" + when + "\"\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCodeAt(diags, "apply_when_dynamic_unsupported", "$.tasks[0].when") {
				dump(t, diags)
				t.Fatalf("expected apply_when_dynamic_unsupported for when: %q", when)
			}
		})
	}
}

// The negative half, and the one that matters most: a static `when:` on an
// applier is the working form (render decides it) and must not be flagged.
// Nor is a dynamic when: on an ordinary module task — that is Soul-side gating
// and entirely legal.
func TestLoadScenarioManifest_ApplyWhenStaticAccepted(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      destiny: redis
      input: {}
    when: input.action == 'apply'
  - module: core.exec.run
    when: register.probe.changed
    params: { cmd: "true" }
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "apply_when_dynamic_unsupported") {
		dump(t, diags)
		t.Fatalf("a static when: on an applier and a dynamic when: on a module task must both pass")
	}
}

// TestLoadScenarioManifest_ApplyForbiddenKeys — module-specific keys on an
// apply: task are cut fail-closed with code <key>_on_apply_invalid, the apply:
// half of the <key>_on_block_invalid family (NIM-286). Each of these WORKS on a
// module task and reaches no RenderedTask on an applier: render reads only the
// applier's Apply/Register/requisites and its When/Where/On/RunOnce/Serial.
func TestLoadScenarioManifest_ApplyForbiddenKeys(t *testing.T) {
	cases := map[string]string{
		"changed_when_on_apply_invalid": "changed_when: \"true\"",
		"failed_when_on_apply_invalid":  "failed_when: \"false\"",
		"retry_on_apply_invalid":        "retry: { count: 3 }",
		"timeout_on_apply_invalid":      "timeout: 30s",
		"params_on_apply_invalid":       "params: { a: 1 }",
	}
	for wantCode, line := range cases {
		t.Run(wantCode, func(t *testing.T) {
			key := strings.TrimSuffix(wantCode, "_on_apply_invalid")
			src := "name: x\ntasks:\n  - apply:\n      destiny: redis\n      input: {}\n    " + line + "\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCodeAt(diags, wantCode, "$.tasks[0]."+key) {
				dump(t, diags)
				t.Fatalf("expected %s at $.tasks[0].%s", wantCode, key)
			}
		})
	}
}

// The negative half, and the one that decides whether the gate is about the
// applier or about the keys: every one of them on an ordinary module task is
// legal and stays legal.
func TestLoadScenarioManifest_ApplyForbiddenKeys_ModuleTaskUnaffected(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    changed_when: "true"
    failed_when: "false"
    retry: { count: 3 }
    timeout: 30s
    vars: { v: "x" }
    params: { cmd: "true" }
  - apply:
      destiny: redis
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("module-task key wrongly rejected: %s", d.Code)
		}
	}
}

// TestLoadScenarioManifest_ApplyAllowedKeysOK — the keys an applier DOES answer
// stay valid: the requisites and register: that render merges into the group,
// the targeting delta it resolves before the destiny pass, and a static when:.
//
// ★ `output:` LEFT this fixture in NIM-334. It stayed here while the argument was
// "unread everywhere, so refusing it only on an applier would misreport an
// unimplemented key as a meaningless one" — that argument survived; the answer
// changed. It is now refused on every kind at once (`output_unsupported`), so an
// applier does not answer it either.
//
// ★ `vars:` is here since NIM-336: it is resolved into the env that renders
// `apply.input`, so it is an answered key, not a lost one.
func TestLoadScenarioManifest_ApplyAllowedKeysOK(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - name: roll the redis destiny over the drifted hosts
    apply:
      destiny: redis
      input: {}
    vars: { pause: 5 }
    when: input.action == 'apply'
    where: "register.probe.changed"
    on: [redis]
    serial: 1
    onchanges: [probe]
    require: [probe]
    register: rolled
  - apply:
      destiny: redis
      input: {}
    run_once: true
    onfail: [probe]
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("key an applier answers wrongly rejected: %s", d.Code)
		}
	}
}

// TestLoadScenarioManifest_IDOnApply pins the one member of the family that
// needed no new code: `id:` is already refused on every non-module
// discriminator, applier included, so the gate is complete without a second
// rule for the same key.
func TestLoadScenarioManifest_IDOnApply(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      destiny: redis
      input: {}
    id: rolled
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "id_unsupported_target", "$.tasks[0].id") {
		dump(t, diags)
		t.Fatalf("expected id_unsupported_target")
	}
}

// TestLoadScenarioManifest_AsyncTaskKeyAccepted — the other half of the rename:
// `async: true` on an ordinary task parses clean and lands on Task.Async.
func TestLoadScenarioManifest_AsyncTaskKeyAccepted(t *testing.T) {
	src := "name: x\ntasks:\n  - module: core.exec.run\n    async: true\n    register: probe\n    params: { cmd: \"true\" }\n  - module: core.exec.run\n    require: [probe]\n    params: { cmd: \"true\" }\n"
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("async:/require: wrongly rejected: %s", d.Code)
		}
	}
	if !m.Tasks[0].Async {
		t.Error("Tasks[0].Async = false, want true")
	}
	all, names, ok := m.Tasks[1].RequireSpec()
	if !ok || all || len(names) != 1 || names[0] != "probe" {
		t.Errorf("RequireSpec() = %v/%v/%v, want false/[probe]/true", all, names, ok)
	}
}

// TestRequireSpec_ScalarAll — the scalar form decodes to the all-barrier and
// names no source.
func TestRequireSpec_ScalarAll(t *testing.T) {
	all, names, ok := Task{Require: RequireAll}.RequireSpec()
	if !ok || !all || names != nil {
		t.Errorf("RequireSpec() = %v/%v/%v, want true/nil/true", all, names, ok)
	}
	if _, _, unset := (Task{}).RequireSpec(); unset {
		t.Error("an unset require: must decode to ok=false")
	}
}

// TestLoadScenarioManifest_BlockInheritedKeysOK — inherited keys (when/
// where/vars/onchanges/onfail/serial/run_once/name) on a block task are VALID (§6.5
// explicitly allows them) — must not yield <key>_on_block_invalid.
func TestLoadScenarioManifest_BlockInheritedKeysOK(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
  - name: grp
    when: "input.go"
    where: "register.probe.changed"
    serial: 1
    vars: { v: "x" }
    onchanges: [probe]
    block:
      - module: core.service.restarted
        params: { name: redis }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError && strings.HasSuffix(d.Code, "_on_block_invalid") {
			dump(t, diags)
			t.Fatalf("inherited key wrongly rejected: %s", d.Code)
		}
	}
}

func TestLoadScenarioManifest_BadOn(t *testing.T) {
	src := `name: x
tasks:
  - on: 42
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("expected type_mismatch on on:")
	}
}

func TestLoadScenarioManifest_ChangedWhenBoolLiteral(t *testing.T) {
	// changed_when:/failed_when: accept a bool literal (force-shortcut) and a
	// CEL string; invalid types (number/list) → type_mismatch.
	ok := []string{"false", "true", `"register.self.exit_code != 0"`}
	for _, v := range ok {
		src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { cmd: \"true\" }\n    changed_when: " + v + "\n    failed_when: " + v + "\n"
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		for _, d := range diags {
			if d.Level == diag.LevelError {
				dump(t, diags)
				t.Fatalf("changed_when/failed_when: %s - unexpected validation error", v)
			}
		}
	}

	bad := []string{"42", "[a, b]"}
	for _, v := range bad {
		src := "name: x\ntasks:\n  - module: core.exec.run\n    params: { cmd: \"true\" }\n    changed_when: " + v + "\n"
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCodeAt(diags, "type_mismatch", "$.tasks[0].changed_when") {
			dump(t, diags)
			t.Fatalf("changed_when: %s - expected type_mismatch", v)
		}
	}
}

// TestLoadScenarioManifest_KeeperSideCoreAddressOK — a keeper-side core module
// validates with no `on:` at all. Since NIM-749 that is the whole spelling: the
// address is what routes it, and the key that used to sit here is now
// `on_keeper_redundant` (see TestLoadScenarioManifest_SideFollowsTheModuleAddress).
func TestLoadScenarioManifest_KeeperSideCoreAddressOK(t *testing.T) {
	src := `name: x
tasks:
  - module: core.cloud.created
    params: { provider: aws }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for a keeper-side core address")
	}
}

func TestLoadScenarioManifest_OnAsBadScalar(t *testing.T) {
	// on: arbitrary-scalar (not keeper) — must be enum_invalid.
	src := `name: x
tasks:
  - on: random
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "enum_invalid") {
		dump(t, diags)
		t.Fatalf("expected enum_invalid on on: random (only 'keeper' as scalar)")
	}
}

func TestLoadScenarioManifest_OnCovenList(t *testing.T) {
	// A CEL element + a coven literal in on: parse fine. The CEL element is a
	// plain input ref (NOT ${ incarnation.name }, which is a render-time
	// validation error post-NIM-124 — see resolveCovenList).
	src := `name: x
tasks:
  - on: ["${ input.zone }", baremetal]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for on: [CEL, coven]")
	}
}

func TestLoadScenarioManifest_BadCovenInOnList(t *testing.T) {
	src := `name: x
tasks:
  - on: [BAD_NAME]
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "name_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for non-kebab coven in on[]")
	}
}

func TestLoadScenarioManifest_SerialPercent(t *testing.T) {
	src := `name: x
tasks:
  - serial: "25%"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for serial: \"25%%\"")
	}
}

func TestLoadScenarioManifest_SerialBadPercent(t *testing.T) {
	src := `name: x
tasks:
  - serial: "0%"
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for serial: \"0%%\"")
	}
}

func TestLoadScenarioManifest_SerialZero(t *testing.T) {
	src := `name: x
tasks:
  - serial: 0
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for serial: 0")
	}
}

func TestLoadScenarioManifest_RegisterIdentifierInvalid(t *testing.T) {
	src := `name: x
tasks:
  - register: BAD-ID
    module: core.exec.run
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "register_identifier_invalid") {
		dump(t, diags)
		t.Fatalf("expected register_identifier_invalid")
	}
}

func TestLoadScenarioManifest_IDValidOnModule(t *testing.T) {
	// id: on a module task without register — valid.
	src := `name: x
tasks:
  - id: redis_config
    module: core.file.present
    params: { path: /etc/redis/redis.conf, content: "..." }
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for valid id on module task")
	}
	if cfg.Tasks[0].ID != "redis_config" {
		t.Errorf("id not captured: %q", cfg.Tasks[0].ID)
	}
}

func TestLoadScenarioManifest_IDInvalidFormat(t *testing.T) {
	// Invalid id format: kebab / CamelCase / digit-first / empty string.
	// `redis-config` is the explicit "dash in the middle = invalid" case (id is
	// snake_case, not kebab), separate from BAD-ID where invalidity is also from caps.
	for _, v := range []string{"BAD-ID", "redis-config", "RedisConfig", "9config", `""`} {
		v := v
		t.Run(v, func(t *testing.T) {
			src := "name: x\ntasks:\n  - id: " + v + "\n    module: core.exec.run\n    params: { cmd: \"true\" }\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if !hasCode(diags, "id_identifier_invalid") {
				dump(t, diags)
				t.Fatalf("expected id_identifier_invalid for id: %s", v)
			}
		})
	}
}

func TestLoadScenarioManifest_IDWithRegisterConflict(t *testing.T) {
	// Guard invariant: id together with register is always an error (a task with
	// register already has an address; id is redundant and ambiguous).
	src := `name: x
tasks:
  - id: redis_config
    register: redis_config
    module: core.file.present
    params: { path: /etc/redis/redis.conf, content: "..." }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_register_conflict") {
		dump(t, diags)
		t.Fatalf("expected id_register_conflict when both id and register set")
	}
}

func TestLoadScenarioManifest_IDOnBlockRejected(t *testing.T) {
	// pilot: id on a block task is not supported.
	src := `name: x
tasks:
  - id: grp
    block:
      - module: core.exec.run
        params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected id_unsupported_target for id on block task")
	}
}

func TestLoadScenarioManifest_IDOnIncludeRejected(t *testing.T) {
	// pilot: id on an include task is not supported.
	src := `name: x
tasks:
  - id: inc
    include: install.yml
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "id_unsupported_target") {
		dump(t, diags)
		t.Fatalf("expected id_unsupported_target for id on include task")
	}
}

func TestLoadScenarioManifest_NoIDOK(t *testing.T) {
	// Regression: a task without id (like everything existing) is valid — id is optional.
	src := `name: x
tasks:
  - module: core.exec.run
    register: probe
    params: { cmd: "true" }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected no errors for task without id")
	}
}

func TestLoadScenarioManifest_RetryCountMissing(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      delay: 5s
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "missing_required_field") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for retry.count")
	}
}

func TestLoadScenarioManifest_RetryCountZero(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      count: 0
      delay: 5s
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "value_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected value_out_of_range for retry.count: 0")
	}
}

func TestLoadScenarioManifest_RetryBadDelay(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    retry:
      count: 3
      delay: "not-a-duration"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for retry.delay")
	}
}

func TestLoadScenarioManifest_TimeoutBad(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "forever"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected duration_invalid for timeout")
	}
}

// TestLoadScenarioManifest_DurationDaysSuffix — destiny duration fields accept the
// `<N>d` suffix per the `duration` convention (config.ParseDuration), unified with
// keeper.yml validation. Previously bare time.ParseDuration rejected `30d`.
func TestLoadScenarioManifest_DurationDaysSuffix(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "30d"
    retry:
      count: 3
      delay: "1d"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("did not expect duration_invalid for <N>d-suffixed durations")
	}
}

// TestLoadScenarioManifest_DurationGoSyntaxStillValid — backward-compat: forms
// accepted by time.ParseDuration (`30s`/`5m`) remain valid.
func TestLoadScenarioManifest_DurationGoSyntaxStillValid(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    timeout: "5m"
    retry:
      count: 3
      delay: "30s"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("did not expect duration_invalid for Go-syntax durations")
	}
}

func TestLoadScenarioManifest_ApplyMissingDestiny(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      input: {}
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.tasks[0].apply.destiny" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field on apply.destiny")
	}
}

func TestLoadScenarioManifest_ApplyUnknownKey(t *testing.T) {
	src := `name: x
tasks:
  - apply:
      destiny: redis
      input: {}
      mystery: 42
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks[0].apply.mystery" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected unknown_key on apply.mystery")
	}
}

func TestLoadScenarioManifest_IncludeBadName(t *testing.T) {
	// The include target is clamped by the GRAMMAR, not by a later path check:
	// `.` is outside the segment alphabet, so `..`, hidden names and an absolute
	// path cannot be written at all; the subdirectory is capped at ONE level
	// (NIM-694) so a service cannot grow a tree the two-level resolve would have
	// to walk.
	// Each row pins the rule that must reject it, not merely THAT something did.
	// Asserting the code alone made every row interchangeable: the two depth-cap
	// rows stayed green when the segment alphabet was what rejected them, so the
	// cap the ticket asked for had no guard of its own.
	for _, tc := range []struct{ name, wantReason string }{
		{"../escape.yml", "must not contain a `.` or `..` path segment"},
		{"_create/../../escape.yml", "must not contain a `.` or `..` path segment"},
		{"a/b/c.yml", "at most one subdirectory level"},
		{"_create/nested/deploy.yml", "at most one subdirectory level"},
		{"/abs.yml", "must not be an absolute path"},
		{".hidden.yml", "each path segment must match"},
		{"_create/.hidden.yml", "each path segment must match"},
		{"deploy.yaml", "must end in `.yml`"},
		{"deploy", "must end in `.yml`"},
		{"_create/", "must end in `.yml`"},
		{"UPPER.yml", "each path segment must match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "name: x\ntasks:\n  - include: \"" + tc.name + "\"\n"
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			d := diagWithCode(diags, "name_invalid_format")
			if d == nil {
				dump(t, diags)
				t.Fatalf("expected name_invalid_format for include %q", tc.name)
			}
			if !strings.Contains(d.Message, tc.wantReason) {
				t.Fatalf("include %q rejected for the wrong reason:\n got: %s\nwant it to name: %s",
					tc.name, d.Message, tc.wantReason)
			}
		})
	}
}

// TestLoadScenarioManifest_IncludeSubdirectory is the NIM-694 grammar guard for
// the shared-bodies layout: the common pieces of a scenario family live in a
// `_`-prefixed directory and are included by a one-level relative path.
func TestLoadScenarioManifest_IncludeSubdirectory(t *testing.T) {
	for _, name := range []string{"provision.yml", "_create/deploy.yml", "_create/sentinel-tls.yml", "shared/body_2.yml"} {
		t.Run(name, func(t *testing.T) {
			src := "name: x\ntasks:\n  - include: \"" + name + "\"\n"
			m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if err != nil || diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("include %q must be accepted, err=%v", name, err)
			}
			if len(m.Tasks) != 1 || m.Tasks[0].Include == nil || m.Tasks[0].Include.Include != name {
				t.Fatalf("include %q did not decode: %#v", name, m.Tasks)
			}
		})
	}
}

func TestLoadScenarioManifest_BlockRecursive(t *testing.T) {
	// Recursive block: block inside block — an error in the nested task must bubble up.
	src := `name: x
tasks:
  - block:
      - block:
          - name: deep
            when: "true"
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "task_discriminator_missing") {
		dump(t, diags)
		t.Fatalf("expected task_discriminator_missing bubbled up from nested block")
	}
}

func TestLoadScenarioManifest_TaskWaitDeprecated(t *testing.T) {
	src := `name: x
tasks:
  - module: core.exec.run
    params: { cmd: "true" }
    wait: { condition: "true", timeout: 30s }
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "unknown_key" && d.YAMLPath == "$.tasks[0].wait" && d.Hint != "" {
			found = true
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected task-level unknown_key for deprecated wait:")
	}
}

// TestLoadScenarioManifest_TaskNoLogRemoved — `no_log:` is gone ([ADR-0083] §8):
// a module declares `secret: true` on the output fields it returns, and the
// platform masks exactly those. A surviving key must FAIL rather than be ignored —
// a scenario that still writes it was written expecting suppression, and silently
// accepting it would promise a masking the platform no longer performs from that
// side. It reaches the author as unknown_key WITH the replacement hint, on the
// task path, both at the top level and inside a block.
func TestLoadScenarioManifest_TaskNoLogRemoved(t *testing.T) {
	cases := map[string]string{
		"$.tasks[0].no_log":          "name: x\ntasks:\n  - module: core.exec.run\n    no_log: true\n    params: { cmd: \"true\" }\n",
		"$.tasks[0].block[0].no_log": "name: x\ntasks:\n  - block:\n      - module: core.exec.run\n        no_log: true\n        params: { cmd: \"true\" }\n",
	}
	for path, src := range cases {
		path, src := path, src
		t.Run(path, func(t *testing.T) {
			_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "unknown_key" && d.YAMLPath == path && strings.Contains(d.Hint, "ADR-0083") {
					found = true
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected unknown_key with an ADR-0083 hint at %s", path)
			}
		})
	}
}

// --- create: top-level flag of a starter scenario (multiple-create mechanism) ---

// TestLoadScenarioManifest_CreateFlagTrue — `create: true` parses into *bool
// (the scenario is declared as a starter/bootstrap-capable one; reading the flag
// is a layer above, keeper resolution of the create-set by artifact.Scenario.Create).
func TestLoadScenarioManifest_CreateFlagTrue(t *testing.T) {
	src := `name: create_cluster
create: true
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("create: true should be a valid top-level key")
	}
	if cfg.Create == nil || *cfg.Create != true {
		t.Fatalf("cfg.Create = %v, want *true", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagFalse — `create: false` is distinguishable from
// "unset" (an explicit opt-out from the create-set).
func TestLoadScenarioManifest_CreateFlagFalse(t *testing.T) {
	src := `name: add_user
create: false
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("create: false should be valid")
	}
	if cfg.Create == nil || *cfg.Create != false {
		t.Fatalf("cfg.Create = %v, want *false", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagAbsent — a missing key: Create==nil
// (back-compat: an ordinary operational scenario does not silently become a create-starter).
func TestLoadScenarioManifest_CreateFlagAbsent(t *testing.T) {
	src := `name: restart
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("absence of create: - valid")
	}
	if cfg.Create != nil {
		t.Fatalf("cfg.Create = %v, want nil", cfg.Create)
	}
}

// TestLoadScenarioManifest_CreateFlagBadType — a non-bool `create:` value → type_mismatch
// (the key is known to a struct field, the decode phase catches the type mismatch).
func TestLoadScenarioManifest_CreateFlagBadType(t *testing.T) {
	src := `name: x
create: "yes"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf("create: \"yes\" should yield type_mismatch (boolean expected)")
	}
	// `create:` must NOT be caught as unknown_key (it is known to a struct field).
	if hasCodeAt(diags, "unknown_key", "$.create") {
		dump(t, diags)
		t.Fatalf("create: should not be unknown_key (a known field)")
	}
}

// --- from: list of source versions of an upgrade scenario (ADR-0068) ---

// TestLoadScenarioManifest_FromVersions — the top-level `from:` (a self-describing
// list of an upgrade scenario's source versions) parses with the strict walker
// WITHOUT unknown_key and fills ScenarioManifest.FromVersions (ADR-0068 §3). Without
// the FromVersions field the walker would reject `from` as unknown_key — that is the
// red without the fix.
func TestLoadScenarioManifest_FromVersions(t *testing.T) {
	src := `name: v2
from: ["v1.0.0", "v1.2.0"]
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("top-level from: should be a valid key (ADR-0068)")
	}
	if hasCodeAt(diags, "unknown_key", "$.from") {
		dump(t, diags)
		t.Fatalf("from: should not be unknown_key (a known field)")
	}
	want := []string{"v1.0.0", "v1.2.0"}
	if len(cfg.FromVersions) != len(want) {
		t.Fatalf("cfg.FromVersions = %v, want %v", cfg.FromVersions, want)
	}
	for i, v := range want {
		if cfg.FromVersions[i] != v {
			t.Fatalf("cfg.FromVersions[%d] = %q, want %q", i, cfg.FromVersions[i], v)
		}
	}
}

// TestLoadScenarioManifest_FromVersionsAbsent — a missing key: FromVersions==nil
// (an ordinary scenario without an upgrade role does not silently become one).
func TestLoadScenarioManifest_FromVersionsAbsent(t *testing.T) {
	src := `name: restart
tasks: []
`
	cfg, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("absence of from: - valid")
	}
	if cfg.FromVersions != nil {
		t.Fatalf("cfg.FromVersions = %v, want nil", cfg.FromVersions)
	}
}

// TestLoadScenarioManifest_FromVersionsBadType — a scalar `from:` value instead of
// a list → type_mismatch (the key is known to a struct field, the decode phase catches the type).
func TestLoadScenarioManifest_FromVersionsBadType(t *testing.T) {
	src := `name: x
from: "v1.0.0"
tasks: []
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if !hasCode(diags, "type_mismatch") {
		dump(t, diags)
		t.Fatalf(`from: "v1.0.0" (scalar instead of a list) should yield type_mismatch`)
	}
	if hasCodeAt(diags, "unknown_key", "$.from") {
		dump(t, diags)
		t.Fatalf("from: should not be unknown_key (a known field)")
	}
}

// TestLoadScenarioManifest_OnKeeperOnSoulModule — the reverse of
// `on_keeper_redundant`, and a hole rather than a symmetry exercise (NIM-749).
//
// [render.IsKeeperTask] honours the `on: keeper` literal whatever the address —
// it has to, because that is still how a keeper-side PLUGIN is routed — so the
// literal on a Soul-side core module sends a host module to the keeper, whose
// registry has no such module, and the run dies there. docs/keeper/modules.md has
// called this "a validation error" since ADR-017 with nothing enforcing it.
func TestLoadScenarioManifest_OnKeeperOnSoulModule(t *testing.T) {
	t.Run("a Soul-side core module is refused", func(t *testing.T) {
		src := `name: x
tasks:
  - name: install
    module: core.pkg.installed
    on: keeper
    params: { name: redis }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if !hasCodeAt(diags, "on_keeper_on_soul_module", "$.tasks[0].on") {
			dump(t, diags)
			t.Fatalf("expected on_keeper_on_soul_module at the on: key")
		}
	})

	// The scope is DECLARED core modules, not the whole `core` namespace, and the
	// difference is load-bearing: the keeper's registry is built from its Deps, so a
	// `core.*` address with no declaration here can still be a registered keeper-side
	// module — the integration harness registers `core.probe.*` exactly that way.
	// Refusing it broke a dozen keeper-chain cases.
	t.Run("an undeclared core address is left alone", func(t *testing.T) {
		src := `name: x
tasks:
  - name: probe
    module: core.probe.created
    on: keeper
    params: {}
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "on_keeper_on_soul_module") {
			dump(t, diags)
			t.Fatalf("a core address this binary has no declaration for may still be a registered keeper-side module")
		}
	})

	// And a plugin MUST keep the literal.
	t.Run("a plugin address keeps the literal", func(t *testing.T) {
		src := `name: x
tasks:
  - name: create the VM
    module: wb-cloud.vm.created
    on: keeper
    params: { flavor: small }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "on_keeper_on_soul_module") {
			dump(t, diags)
			t.Fatalf("a plugin address has no other spelling — the literal must stay legal")
		}
	})

	// And the keeper-side core case is the OTHER code: the two must not collapse
	// into one, since the remedies read differently.
	t.Run("a keeper-side core module is the redundancy case", func(t *testing.T) {
		src := `name: x
tasks:
  - name: capture
    module: core.state.set
    on: keeper
    params: { field: owner, value: alice }
`
		_, _, diags, _ := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if hasCode(diags, "on_keeper_on_soul_module") {
			dump(t, diags)
			t.Fatalf("core.state is keeper-side — this is on_keeper_redundant, not the Soul-module case")
		}
		if !hasCode(diags, "on_keeper_redundant") {
			dump(t, diags)
			t.Fatalf("expected on_keeper_redundant")
		}
	})
}
