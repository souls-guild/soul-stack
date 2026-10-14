package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// TestTaskSource_ScenarioAndInclude pins what a run failure points at (NIM-1000):
// the line a task is written on, the file it is written in, and the `include:`
// tasks that spliced it in, outermost first. A block child keeps its own line, and
// a task of an included body keeps the body's file after the splice.
func TestTaskSource_ScenarioAndInclude(t *testing.T) {
	const main = `name: demo
tasks:
  - name: probe
    module: core.exec.run
    params: { cmd: echo }

  - include: outer.yml

  - name: group
    block:
      - name: inside
        module: core.exec.run
        params: { cmd: echo }
`
	files := map[string]string{
		"scenario/demo/outer.yml": `- name: first
  module: core.exec.run
  params: { cmd: echo }
- include: inner.yml
`,
		"scenario/demo/inner.yml": `
- name: deep
  module: core.exec.run
  params: { cmd: echo }
`,
	}
	resolve := func(name string) ([]byte, string, error) {
		p := "scenario/demo/" + name
		return []byte(files[p]), p, nil
	}

	scn, _, diags, err := LoadScenarioManifestFromBytes("scenario/demo/main.yml", []byte(main), ValidateOptions{})
	if err != nil || diag.HasErrors(diags) {
		t.Fatalf("load: err=%v diags=%v", err, diags)
	}
	got, diags := ExpandIncludes(scn.Tasks, resolve)
	if diag.HasErrors(diags) {
		t.Fatalf("expand: %v", diags)
	}

	type at struct {
		name string
		src  SourcePos
		via  []SourcePos
	}
	mainPos := func(line int) SourcePos { return SourcePos{File: "scenario/demo/main.yml", Line: line} }
	want := []at{
		{"probe", mainPos(3), nil},
		{"first", SourcePos{File: "scenario/demo/outer.yml", Line: 1}, []SourcePos{mainPos(7)}},
		{"deep", SourcePos{File: "scenario/demo/inner.yml", Line: 2}, []SourcePos{mainPos(7), {File: "scenario/demo/outer.yml", Line: 4}}},
		{"group", mainPos(9), nil},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := at{got[i].Name, got[i].Source, got[i].IncludedFrom}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("task %d = %+v, want %+v", i, g, w)
		}
	}
	child := got[3].Block.Block[0]
	if child.Name != "inside" || child.Source != mainPos(11) {
		t.Errorf("block child = %q at %+v, want inside at %+v", child.Name, child.Source, mainPos(11))
	}
}
