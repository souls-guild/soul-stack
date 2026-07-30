package config

import "testing"

// Two grammar elements arrived in this cycle without a registry row, so the
// floor cross-check of (i)–(k) could not see them (NIM-354).
//
// A missing row is not a missing warning — it is a `compat:` window that passes
// the check while promising a keeper that cannot render the definition. A
// service pinning `min: 0.1.0` beside a `name_template` create-scenario lints
// clean today, and the keeper it names does not compose the name at all: it
// expects `name` in the request. That surfaces on somebody else's cluster during
// an upgrade rather than on the author's lint, which is the exact failure mode
// ADR-0076 was written for.
//
// Both rows land as Unreleased, so InferKeeperFloor still skips them and nothing
// changes for existing definitions. They start carrying a floor when RELEASING.md
// step (c2) stamps the release — which is why they have to exist BEFORE the
// stamp, or the stamp freezes them as "these were always here".

// A scenario's own manifest grammar contributed no floor anywhere: soul-lint's
// scenario path and keeper's run path both walk only the TASK list. name_template
// is the first scenario-level key that needs one.
func TestKeeperFeaturesOfScenario_NameTemplate(t *testing.T) {
	if got := KeeperFeaturesOfScenario(&ScenarioManifest{}); len(got) != 0 {
		t.Fatalf("used = %+v, want nothing for a scenario without name_template", got)
	}

	got := KeeperFeaturesOfScenario(&ScenarioManifest{NameTemplate: "${ input.cluster }-${ input.shard }"})
	found := false
	for _, f := range got {
		if f.ID == FeatureScenarioNameTemplate {
			found = true
			if f.Where == "" {
				t.Error("the feature carries no location, so a diagnostic cannot point at it")
			}
		}
	}
	if !found {
		t.Errorf("used = %+v, want %s", got, FeatureScenarioNameTemplate)
	}
}

// The collector must also carry the task list, or wiring it into the callers
// would silently drop the floor the tasks contribute.
func TestKeeperFeaturesOfScenario_StillWalksTasks(t *testing.T) {
	async := moduleTask("core.exec.run", nil)
	async.Async = true

	got := KeeperFeaturesOfScenario(&ScenarioManifest{Tasks: []Task{async}})
	for _, f := range got {
		if f.ID == FeatureTaskAsync {
			return
		}
	}
	t.Errorf("used = %+v, want the task-level %s to survive the scenario collector", got, FeatureTaskAsync)
}

func TestKeeperFeaturesOfScenario_NilIsEmpty(t *testing.T) {
	if got := KeeperFeaturesOfScenario(nil); len(got) != 0 {
		t.Errorf("used = %+v, want nothing for a nil manifest", got)
	}
}

// `include:` at top level is baseline grammar. What this cycle added is
// expanding one INSIDE `block:` — so only the nested form carries a floor. An
// older keeper walks the block and silently produces no tasks for the include,
// which is the same "accepts and quietly does less" class as async:/require:.
func TestKeeperFeaturesOfTasks_IncludeInsideBlockIsTheNewGrammar(t *testing.T) {
	nested := Task{Name: "outer", Block: &BlockTask{Block: []Task{
		{Name: "inner", Include: &IncludeTask{Include: "install.yml"}},
	}}}

	got := map[string]bool{}
	for _, f := range keeperFeaturesOfTasks([]Task{nested}, stampedRegistry()) {
		got[f.ID] = true
	}
	if !got[FeatureTaskBlockInclude] {
		t.Errorf("an include: inside block: did not register %s", FeatureTaskBlockInclude)
	}

	topLevel := []Task{{Name: "plain", Include: &IncludeTask{Include: "install.yml"}}}
	for _, f := range keeperFeaturesOfTasks(topLevel, stampedRegistry()) {
		if f.ID == FeatureTaskBlockInclude {
			t.Error("a top-level include: registered the nested-include feature — that form is baseline grammar")
		}
	}
}

// A collector that emits an id the registry does not carry is silently dropped
// by dslFeatureUse, so the feature would look implemented and contribute
// nothing. Pin the rows themselves.
func TestKeeperDSLFeatures_CarriesTheNewRows(t *testing.T) {
	for _, id := range []string{FeatureScenarioNameTemplate, FeatureTaskBlockInclude} {
		if _, ok := keeperDSLFeatures[id]; !ok {
			t.Errorf("%s has no registry row, so the collector's emission is dropped on the floor", id)
		}
	}
}
