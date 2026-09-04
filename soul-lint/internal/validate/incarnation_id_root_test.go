package validate

// The static half of the [ADR-0085] CEL-root window (NIM-730). What these pin:
// that a scenario on the retired root PASSES and warns, that one on the new root
// passes SILENTLY, and that the warning carries a line — the three things the
// ticket's acceptance is written on.
//
// The runtime half is pinned in shared/cel (both roots evaluate) and in
// keeper/internal/render's label guard (the map carries `id`).
//
// [ADR-0085]: ../../../docs/adr/0085-entity-id-and-label.md

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// lintScenarioIn runs the scenario path over a real service tree, so `include:`
// resolves the two levels the engine resolves.
func lintScenarioIn(t *testing.T, root, scenario string) []diag.Diagnostic {
	t.Helper()
	p := filepath.Join(root, "scenario", scenario, "main.yml")
	src, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	diags, ok := diagnose(Options{Path: p, Kind: KindScenario}, src, nil)
	if !ok {
		t.Fatal("diagnose did not handle the scenario kind")
	}
	return diags
}

// lintScenario runs the whole scenario path over a source, so a finding here has
// travelled the same route a `soul-lint validate-scenario` finding does — parse,
// decode, every rule — rather than the one rule under test called directly.
func lintScenarioSource(t *testing.T, body string) []diag.Diagnostic {
	t.Helper()
	diags, ok := diagnose(Options{Path: "scenario/create/main.yml", Kind: KindScenario}, []byte(body), nil)
	if !ok {
		t.Fatal("diagnose did not handle the scenario kind")
	}
	return diags
}

func findCode(diags []diag.Diagnostic, code string) (diag.Diagnostic, bool) {
	for _, d := range diags {
		if d.Code == code {
			return d, true
		}
	}
	return diag.Diagnostic{}, false
}

// The retired root and the current one, in the SAME scenario shape, so the only
// difference between the two runs is the spelling.
const legacyRootScenario = `name: create
create: true
tasks:
  - name: probe
    module: core.exec.run
    where: "incarnation.%s == 'redis-prod'"
    params:
      cmd: "echo ${ incarnation.%s }"
`

// TestLegacyRoot_OldSpellingPassesAndWarns is the acceptance itself: a scenario
// written against `incarnation.name` still LINTS — the window is open, so this
// must not be an error — and it is told so, once per cell.
func TestLegacyRoot_OldSpellingPassesAndWarns(t *testing.T) {
	diags := lintScenarioSource(t, strings.ReplaceAll(legacyRootScenario, "%s", "name"))

	if diag.HasErrors(diags) {
		t.Fatalf("the retired root must not be an ERROR while the window is open; diags=%v", diags)
	}
	d, ok := findCode(diags, "incarnation_name_legacy_root")
	if !ok {
		t.Fatalf("no incarnation_name_legacy_root warning — the one static catcher for this rename is silent; diags=%v", diags)
	}
	if d.Level != diag.LevelWarning {
		t.Errorf("level = %v, want a WARNING: an error would close the window it exists to keep open", d.Level)
	}
	if !strings.Contains(d.Hint, "incarnation.id") {
		t.Errorf("hint = %q, want the replacement spelling in it", d.Hint)
	}
	// Both cells are reported: an author fixing only the one they were shown
	// would leave the other to fail on a host at the end of the window.
	n := 0
	for _, x := range diags {
		if x.Code == "incarnation_name_legacy_root" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("got %d findings, want one per cell (where: and params.cmd)", n)
	}
}

// TestLegacyRoot_NewSpellingIsSilent — the other half of the acceptance. The same
// scenario on `incarnation.id` produces no finding at all, so the warning cannot
// be something a migrated repository still has to read past.
func TestLegacyRoot_NewSpellingIsSilent(t *testing.T) {
	diags := lintScenarioSource(t, strings.ReplaceAll(legacyRootScenario, "%s", "id"))
	if d, ok := findCode(diags, "incarnation_name_legacy_root"); ok {
		t.Fatalf("the current spelling was flagged: %+v", d)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected errors on a migrated scenario: %v", diags)
	}
}

// TestLegacyRoot_WarningCarriesTheLine is the mutation guard the ticket names:
// the address is load-bearing, because the finding's whole job is to send an
// author to a place in a file they may not have written. A `YAMLPath` alone is
// not a place an editor jumps to.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
//
//	if line, col, ok := c.position(where); ok { d.Line, d.Column = line, col }
//
// block from legacyRootChecker.diag — the finding keeps its code, its message and
// its YAMLPath, and this test goes red on Line == 0.
func TestLegacyRoot_WarningCarriesTheLine(t *testing.T) {
	// `where:` is on line 6 of the source below; the address is the KEY's line,
	// which is what lookupPath resolves and what an author needs to open.
	const src = `name: create
create: true
tasks:
  - name: probe
    module: core.exec.run
    where: "incarnation.name == 'redis-prod'"
    params:
      cmd: "/usr/bin/true"
`
	d, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root")
	if !ok {
		t.Fatal("no finding to check an address on")
	}
	if d.Line != 6 {
		t.Errorf("Line = %d, want 6 (the `where:` key). A finding without an address "+
			"cannot be acted on across a repository of scenarios — it names a file and "+
			"leaves the reader to grep it.", d.Line)
	}
	if d.Column == 0 {
		t.Error("Column = 0: the address resolved to a line but lost its column")
	}
	if d.YAMLPath != "$.tasks[0].where" {
		t.Errorf("YAMLPath = %q, want $.tasks[0].where", d.YAMLPath)
	}
}

// TestLegacyRoot_AddressFallsBackOutward — a cell whose exact path the resolver
// cannot parse (a params key carrying a dot) must still get an address, from the
// nearest enclosing path. Losing the address on an awkward key would make the
// guard above true only for tidy files.
func TestLegacyRoot_AddressFallsBackOutward(t *testing.T) {
	const src = `name: create
create: true
tasks:
  - name: render
    module: core.file.rendered
    params:
      dest: /etc/redis/redis.conf
      vars.owner: "${ incarnation.name }"
`
	d, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root")
	if !ok {
		t.Fatal("no finding on a params cell with an awkward key")
	}
	if d.Line == 0 {
		t.Error("a cell whose exact YAML path does not resolve lost its address entirely; " +
			"an address one level out beats no address")
	}
}

// TestLegacyRoot_NotAReferenceIsNotFlagged — the rule asks shared/cel, not a
// regex, so the two places the word appears without being a reference stay quiet.
// A regex rule would flag both and teach authors to ignore the warning.
func TestLegacyRoot_NotAReferenceIsNotFlagged(t *testing.T) {
	const src = `name: create
create: true
description: "the incarnation.name spelling is retired; see ADR-0085"
tasks:
  - name: probe
    module: core.exec.run
    params:
      cmd: "redis-cli --eval 'incarnation.name'"
      note: "composed from incarnation.name at create"
`
	if d, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); ok {
		t.Errorf("flagged a non-reference: %+v", d)
	}
}

// TestLegacyRoot_IndexFormIsFlagged — `incarnation['name']` selects the same
// retired key by another syntax. Staying quiet on it would leave a way past the
// only static catcher this rename has.
func TestLegacyRoot_IndexFormIsFlagged(t *testing.T) {
	const src = `name: create
create: true
tasks:
  - name: probe
    module: core.exec.run
    when: "incarnation['name'] == 'redis-prod'"
    params:
      cmd: "/usr/bin/true"
`
	if _, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); !ok {
		t.Error("the index form of the retired root was not flagged")
	}
}

// TestLegacyRoot_ComputeBlockIsWalked — `compute:` resolves in the run-level
// context, which carries the incarnation like every other. It is a separate walk
// from the task list and would otherwise be a silent hole.
func TestLegacyRoot_ComputeBlockIsWalked(t *testing.T) {
	const src = `name: create
create: true
compute:
  prefix: "${ incarnation.name }-shard"
tasks:
  - name: probe
    module: core.exec.run
    params:
      cmd: "/usr/bin/true"
`
	d, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root")
	if !ok {
		t.Fatal("a compute: entry reading the retired root was not flagged")
	}
	if d.YAMLPath != "$.compute.prefix" {
		t.Errorf("YAMLPath = %q, want $.compute.prefix", d.YAMLPath)
	}
}

// TestLegacyRoot_NilDocumentStillReports — the address is a bonus, not a
// precondition. A caller with no Document must still get the finding, or a
// refactor that stops threading the document through would silently disable the
// rule rather than degrade it.
func TestLegacyRoot_NilDocumentStillReports(t *testing.T) {
	scn := &config.ScenarioManifest{
		Name: "create",
		Tasks: []config.Task{{
			Name:  "probe",
			Where: "incarnation.name == 'redis-prod'",
		}},
	}
	diags := legacyRootDiagnostics("scn.yml", scn, nil)
	if _, ok := findCode(diags, "incarnation_name_legacy_root"); !ok {
		t.Fatalf("no finding without a Document; diags=%v", diags)
	}
}

// TestLegacyRoot_IncludeBodyIsWalked is the coverage the rule exists for. A
// service is written as a thin `main.yml` over bodies in `_shared/`, and those
// bodies are where `incarnation.*` is actually written — a catcher that stopped
// at the entry file would be silent on the majority of real reference sites while
// looking green.
//
// The finding must cite the INCLUDED file and a line IN IT: an address into
// `main.yml`, or an index into a list that only exists after expansion, sends the
// author to the wrong place.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: delete the
// `for _, body := range includedTaskBodies(...)` loop from legacyRootDiagnostics.
// Every other test here stays green.
func TestLegacyRoot_IncludeBodyIsWalked(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"scenario/create/main.yml": `name: create
create: true
tasks:
  - include: _shared/provision.yml
`,
		"scenario/_shared/provision.yml": `- name: guard the composed name
  module: core.exec.run
  where: "incarnation.name.matches('^[a-z]')"
  params:
    cmd: "/usr/bin/true"
`,
	})

	var found *diag.Diagnostic
	for _, d := range lintScenarioIn(t, root, "create") {
		if d.Code == "incarnation_name_legacy_root" {
			d := d
			found = &d
		}
	}
	if found == nil {
		t.Fatal("an include: body reading the retired root was not flagged — the rule stops at main.yml, " +
			"which is where the references usually are not")
	}
	if !strings.HasSuffix(filepath.ToSlash(found.File), "scenario/_shared/provision.yml") {
		t.Errorf("File = %q, want the INCLUDED file — an address in main.yml sends the author to the wrong place", found.File)
	}
	if found.Line != 3 {
		t.Errorf("Line = %d, want 3 (the `where:` key inside the included body)", found.Line)
	}
	if found.YAMLPath != "$[0].where" {
		t.Errorf("YAMLPath = %q, want $[0].where — the body is a bare task list, addressed in its own file", found.YAMLPath)
	}
}

// TestLegacyRoot_EveryCELArmIsWalked covers the arms of the task walk one at a
// time. Without this, deleting any single line of legacyRootChecker.tasks leaves
// the suite green — the walk would be pinned only where a case happened to land.
func TestLegacyRoot_EveryCELArmIsWalked(t *testing.T) {
	const legacy = "incarnation.name"
	cases := map[string]string{
		"where":        "    where: \"" + legacy + " != ''\"\n",
		"when":         "    when: \"" + legacy + " != ''\"\n",
		"changed_when": "    changed_when: \"" + legacy + " != ''\"\n",
		"failed_when":  "    failed_when: \"" + legacy + " != ''\"\n",
		"retry.until":  "    retry:\n      attempts: 2\n      until: \"" + legacy + " != ''\"\n",
		"loop.when":    "    loop:\n      items: [1]\n      when: \"" + legacy + " != ''\"\n",
		"loop.items":   "    loop:\n      items: \"${ [" + legacy + "] }\"\n",
		"assert.that":  "", // assert is a task discriminator — its own shape below.
		"vars":         "    vars:\n      owner: \"${ " + legacy + " }\"\n",
		"on":           "    on: [\"env-${ " + legacy + " }\"]\n",
	}
	for name, extra := range cases {
		if extra == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			src := "name: create\ncreate: true\ntasks:\n  - name: t\n    module: core.exec.run\n" +
				extra + "    params:\n      cmd: \"/usr/bin/true\"\n"
			if _, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); !ok {
				t.Errorf("the %s arm of the walk is not checked; source:\n%s", name, src)
			}
		})
	}

	t.Run("assert.that", func(t *testing.T) {
		src := `name: create
create: true
tasks:
  - name: t
    assert:
      that:
        - "incarnation.name != ''"
      message: needs an id
`
		if _, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); !ok {
			t.Error("the assert.that arm of the walk is not checked")
		}
	})

	t.Run("apply.input", func(t *testing.T) {
		src := `name: create
create: true
tasks:
  - name: t
    apply:
      destiny: some-destiny
      input:
        owner: "${ incarnation.name }"
`
		if _, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); !ok {
			t.Error("the apply.input arm of the walk is not checked")
		}
	})

	t.Run("block child", func(t *testing.T) {
		src := `name: create
create: true
tasks:
  - name: group
    block:
      - name: child
        module: core.exec.run
        where: "incarnation.name != ''"
        params:
          cmd: "/usr/bin/true"
`
		if _, ok := findCode(lintScenarioSource(t, src), "incarnation_name_legacy_root"); !ok {
			t.Error("a block: child reading the retired root was not flagged")
		}
	})
}

// TestLegacyRoot_IncludeBodyReadingAnOuterRegisterIsStillWalked is the guard for
// the way this walk was first written wrong.
//
// An included body is loaded here on its OWN, without the
// [config.ValidateOptions.OuterRegisters] the expander threads into it — so a
// body that legally reads a register its INCLUDER declares ([ADR-0083] §4) comes
// back carrying error-level `unknown_register_reference`. Gating the walk on
// `diag.HasErrors` therefore dropped the whole body, silently, on the shape
// services are actually written in: the first version of this rule did exactly
// that and reported nothing for the repository's own `migrate_cluster` scenario.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: put the gate back —
// take the diagnostics from LoadDestinyTasksFromBytes in legacyRootInIncludedBody
// and `return nil` on diag.HasErrors. Every other test here stays green and this
// one goes red, which is the shape of the false green it prevents.
//
// [ADR-0083]: ../../../docs/adr/0083-declared-secret-state-fields.md
func TestLegacyRoot_IncludeBodyReadingAnOuterRegisterIsStillWalked(t *testing.T) {
	root := writeServiceTree(t, map[string]string{
		"scenario/create/main.yml": `name: create
create: true
tasks:
  - name: probe
    module: core.exec.run
    register: probe_out
    params:
      cmd: "/usr/bin/true"
  - include: _shared/follow.yml
`,
		"scenario/_shared/follow.yml": `- name: act on the includer's register
  module: core.exec.run
  when: "register.probe_out.rc == 0 && incarnation.name != ''"
  params:
    cmd: "/usr/bin/true"
`,
	})

	d, ok := findCode(lintScenarioIn(t, root, "create"), "incarnation_name_legacy_root")
	if !ok {
		t.Fatal("an included body reading an OUTER register was skipped whole — the body's own " +
			"unknown_register_reference is an artefact of loading it without its includer's context, " +
			"not a reason to stop looking")
	}
	if !strings.HasSuffix(filepath.ToSlash(d.File), "scenario/_shared/follow.yml") {
		t.Errorf("File = %q, want the included body", d.File)
	}
	if d.Line != 3 {
		t.Errorf("Line = %d, want 3 (the `when:` key in the body)", d.Line)
	}
}

// TestLegacyRoot_LooseFileIncludeIsWalked — outside a service tree the second
// resolve level is unavailable, but the LOCAL one is not: a loose `main.yml`
// beside its own body still has a body, and stage.go expands it. Stopping here
// would make this rule quieter than the sibling walking the same chain.
func TestLegacyRoot_LooseFileIncludeIsWalked(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	write("local.yml", `- name: probe
  module: core.exec.run
  where: "incarnation.name != ''"
  params:
    cmd: "/usr/bin/true"
`)
	main := write("main.yml", `name: create
create: true
tasks:
  - include: local.yml
`)

	src, err := os.ReadFile(main)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	diags, ok := diagnose(Options{Path: main, Kind: KindScenario}, src, nil)
	if !ok {
		t.Fatal("diagnose did not handle the scenario kind")
	}
	d, found := findCode(diags, "incarnation_name_legacy_root")
	if !found {
		t.Fatalf("a loose scenario's sibling include body was not walked; diags=%v", diags)
	}
	if !strings.HasSuffix(filepath.ToSlash(d.File), "local.yml") {
		t.Errorf("File = %q, want the included body", d.File)
	}
}
