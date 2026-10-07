package artifact

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// NIM-941: the Keeper's own runtime parse refuses `async:`/`require:` in a scenario
// on the very fixtures soul-lint is tested against — service repositories are not
// linted, so a refusal living only in soul-lint would let the Keeper run them. The
// require fixture is the `existing-vm` example as it stood before its migration.
func TestLoadScenarioManifestResolved_DestinyOnlyKeysRefusedLikeSoulLint(t *testing.T) {
	for _, name := range []string{"scenario-require-outside-destiny", "scenario-async-outside-destiny"} {
		t.Run(name, func(t *testing.T) {
			p := lintFixtures + "/scenario-broken/" + name
			raw, err := os.ReadFile(p + ".expected.json")
			if err != nil {
				t.Fatal(err)
			}
			var exp struct{ Codes []string }
			if err := json.Unmarshal(raw, &exp); err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			diags := loadRuntimeScenario(t, p+".yml")
			for _, d := range diags {
				if d.Level == diag.LevelError {
					got[d.Code] = true
				}
			}
			for _, c := range exp.Codes {
				if !got[c] {
					t.Errorf("the Keeper did not refuse with %s; got %+v", c, diags)
				}
			}
		})
	}
}

// The Keeper's destiny loader keeps both keys: every production resolver loads a
// destiny through it, so a loader that lost the destiny flag would refuse them here.
func TestDestinyLoad_AsyncAndRequireAccepted(t *testing.T) {
	tr := &testRepo{t: t, dir: t.TempDir()}
	tr.initRepo()
	tr.writeFile("destiny.yml", destinyManifestYML)
	tr.writeFile("tasks/main.yml", "- name: warm\n  module: core.exec.run\n  register: warm\n  async: true\n  params:\n    cmd: /bin/true\n"+
		"- block:\n    - name: verify\n      module: core.exec.run\n      require: [warm]\n      params:\n        cmd: /bin/true\n"+
		"- include: body.yml\n")
	tr.writeFile("tasks/body.yml", "- name: report\n  module: core.exec.run\n  require: all\n  params:\n    cmd: /bin/true\n")
	tr.commit("destiny joining an async task")

	loader := NewDestinyLoader(t.TempDir(), nil)
	art, err := loader.Load(context.Background(), DestinyRef{Name: "pilot-flat", Git: tr.fileURL()})
	if err != nil {
		if strings.Contains(err.Error(), "allowed only on a task inside a destiny") {
			t.Fatalf("the destiny loader refused a destiny's async:/require: %v", err)
		}
		t.Fatalf("Load: %v", err)
	}
	if len(art.Tasks) != 3 || !art.Tasks[0].Async {
		t.Fatalf("tasks = %+v, want 3 with the first async", art.Tasks)
	}
	if all, _, ok := art.Tasks[2].RequireSpec(); !ok || !all {
		t.Errorf("the included body's require: all did not survive the load")
	}
	if _, names, _ := config.RequireSpecOf(art.Tasks[1].Block.Block[0].Require); len(names) != 1 || names[0] != "warm" {
		t.Errorf("the block child's require: = %v, want [warm]", names)
	}
}
