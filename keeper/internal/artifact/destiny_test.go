package artifact

import (
	"context"
	"testing"
)

// destinyRepoFiles — a minimal flat destiny: destiny.yml + tasks/main.yml.
const (
	destinyManifestYML = `name: pilot-flat
description: flat pilot destiny for loader test
input:
  marker_file:
    type: string
    required: true
`
	destinyTasksYML = `- name: Lay down the marker file
  module: core.file.present
  params:
    path: "${ input.marker_file }"
    content: "ok"
`
)

func newDestinyTestRepo(t *testing.T) *testRepo {
	t.Helper()
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	tr.writeFile("tasks/main.yml", destinyTasksYML)
	tr.commit("init destiny")
	return tr
}

func TestDestinyLoad_ManifestAndTasks(t *testing.T) {
	tr := newDestinyTestRepo(t)
	loader := NewDestinyLoader(t.TempDir(), nil)

	art, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if art.Manifest == nil || art.Manifest.Name != "pilot-flat" {
		t.Fatalf("manifest = %+v", art.Manifest)
	}
	if len(art.Tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(art.Tasks))
	}
	if art.Tasks[0].Module == nil || art.Tasks[0].Module.Module != "core.file.present" {
		t.Errorf("task0 module = %+v", art.Tasks[0].Module)
	}
}

func TestDestinyLoad_InvalidTasksRejected(t *testing.T) {
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	// tasks/main.yml — a mapping instead of a sequence → load error.
	tr.writeFile("tasks/main.yml", "name: not-a-list\n")
	tr.commit("break tasks")

	loader := NewDestinyLoader(t.TempDir(), nil)
	if _, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()}); err == nil {
		t.Fatal("want error for invalid tasks/main.yml")
	}
}

func TestDestinyLoad_MissingTasksFile(t *testing.T) {
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	tr.commit("manifest only")

	loader := NewDestinyLoader(t.TempDir(), nil)
	if _, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()}); err == nil {
		t.Fatal("want error for missing tasks/main.yml")
	}
}

// TestDestinyLoad_WithinInclude — within-destiny include (tasks/<sub>.yml)
// expands into a flat list at load time (destiny/tasks.md §4).
func TestDestinyLoad_WithinInclude(t *testing.T) {
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	tr.writeFile("tasks/main.yml", "- include: place.yml\n- include: record.yml\n")
	tr.writeFile("tasks/place.yml", destinyTasksYML)
	tr.writeFile("tasks/record.yml", "- name: record\n  module: core.exec.run\n  changed_when: \"false\"\n  params:\n    cmd: echo\n    args: [\"done\"]\n")
	tr.commit("destiny with include")

	loader := NewDestinyLoader(t.TempDir(), nil)
	art, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(art.Tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (expanded flat list)", len(art.Tasks))
	}
	if art.Tasks[0].Module == nil || art.Tasks[0].Module.Module != "core.file.present" {
		t.Errorf("task0 = %+v, want core.file.present", art.Tasks[0].Module)
	}
	if art.Tasks[1].Module == nil || art.Tasks[1].Module.Module != "core.exec.run" {
		t.Errorf("task1 = %+v, want core.exec.run", art.Tasks[1].Module)
	}
}

// TestDestinyLoad_IncludeCycle — a within-destiny include cycle (a→b→a)
// is detected, it does not hang the load.
func TestDestinyLoad_IncludeCycle(t *testing.T) {
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	tr.writeFile("tasks/main.yml", "- include: a.yml\n")
	tr.writeFile("tasks/a.yml", "- include: b.yml\n")
	tr.writeFile("tasks/b.yml", "- include: a.yml\n")
	tr.commit("destiny include cycle")

	loader := NewDestinyLoader(t.TempDir(), nil)
	_, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()})
	if err == nil {
		t.Fatal("want error for include cycle")
	}
}
