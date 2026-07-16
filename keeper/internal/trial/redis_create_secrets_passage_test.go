package trial

// Guard on carrying passage-invariant for generate-step of redis-create secrets (finding 2
// from review «generate-step of secrets»). create itself generates missing passwords in Vault
// via step `core.vault.kv-present` (on: keeper) BEFORE deploy-tasks that read those secrets
// through ${ vault(...) }. Invariant: generate MUST be in passage STRICTLY BEFORE any
// vault()-reading task — otherwise staged-render would run deploy render BEFORE secrets are
// written and fail on missing path (vault_resolve), and L0 does not catch this (fixtures
// pre-seed secrets).
//
// Why Go-guard and not L0 case.yml: L0 is render-only on pre-seeded fixtures, it does NOT
// express passage-order (case.yml does not see Stratify-plan). This guard works on REAL
// create-plan (LoadScenarioManifest + ExpandIncludes + Stratify) and checks exactly the
// relation passage(generate) < passage(each vault()-read). Loading example — repro_staged_test.go
// (same helper-set as trial).
//
// Order of generate→read in create/ is held by TWO independent passage-axes (ADR-056 +
// amendment Variant A): roster-axis (provision-refresh moves read-tasks to late passage)
// AND vault-axis (kv-present-emitter → vault()-read). Both point the same way
// (generate→passage 0, read→passage ≥1), combined via level=max — therefore create/
// Count did NOT grow from vault-axis (see TestRedisCreate_TwoAxesSamePassageCount).
//
// What catches regression (exactly the scenarios warned about in review):
//   1. generate-step deleted / renamed → fail «generate-step absent»;
//   2. BOTH axes broken (read-task stopped reading both roster and vault) — it would fall to
//      passage 0 WITH generate → fail «generate not before read».
// Sub-test TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis isolates vault-axis: removes
// provision-body (roster-axis is known inactive) and verifies that vault-axis ALONE holds
// the order — this is the fix for live-bug in create_from_souls (there is no roster-axis in principle).

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	createSentinelCase = "../../../examples/service/redis/scenario/create/tests/sentinel-create-1master-2replica/case.yml"
	createClusterCase  = "../../../examples/service/redis/scenario/create/tests/cluster-create-3shards/case.yml"
	generateModuleAddr = "core.vault.kv-present"
)

// loadCreatePlan loads the create-scenario by path of any of its L0-case and returns the
// flat plan (after ExpandIncludes) along with stratification. Plan is the same for all cases
// (cluster/sentinel branches are dropped by include-when LATER at render-phase — Stratify
// sees the full list), therefore passage-invariant is checked on the plan itself.
func loadCreatePlan(t *testing.T, caseFile string) ([]config.Task, config.Passage) {
	t.Helper()
	_, file, err := LoadCase(caseFile)
	if err != nil {
		t.Fatalf("LoadCase(%s): %v", caseFile, err)
	}
	scnPath := scenarioPathFor(file)
	scn, _, diags, err := config.LoadScenarioManifest(scnPath, config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest(%s): %v", scnPath, err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("scenario invalid: %s", formatDiags(diags))
	}
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if diag.HasErrors(iDiags) {
		t.Fatalf("expand includes: %s", formatDiags(iDiags))
	}
	passage, perr := config.Stratify(expanded)
	if perr != nil {
		t.Fatalf("Stratify: %v", perr)
	}
	return expanded, passage
}

// taskReadsVaultSecret — task reads secret through ${ vault(...) } in any keeper-
// rendered value (module.params / apply.input). vault() is the only CEL-builtin for reading
// secrets (ADR-010), therefore substring `vault(` is sufficient; string literals with
// `vault(` do not appear in redis-scenario data (secret paths are built by concatenation,
// do not contain tokens). The generate-task itself (core.vault.kv-present) does NOT call
// vault() (writes to targets), therefore does not fall into the read set — no intersection.
func taskReadsVaultSecret(t *config.Task) bool {
	found := false
	var scan func(v any)
	scan = func(v any) {
		switch x := v.(type) {
		case string:
			if strings.Contains(x, "vault(") {
				found = true
			}
		case map[string]any:
			for _, sub := range x {
				scan(sub)
			}
		case []any:
			for _, sub := range x {
				scan(sub)
			}
		}
	}
	if t.Module != nil {
		scan(map[string]any(t.Module.Params))
	}
	if t.Apply != nil {
		scan(map[string]any(t.Apply.Input))
	}
	return found
}

// taskIsSecretGenerate — secret-generator task: keeper-side core.vault.kv-present
// with targets addressing the main redis password (secret/redis/...#password). Checking targets
// (not only module-address) excludes false match with any other kv-present step.
func taskIsSecretGenerate(t *config.Task) bool {
	addr := ""
	if t.Module != nil {
		addr = t.Module.Module
	}
	if addr != generateModuleAddr {
		return false
	}
	targets, ok := t.Module.Params["targets"]
	if !ok {
		return false
	}
	s, ok := targets.(string)
	if !ok {
		return false
	}
	return strings.Contains(s, "secret/redis/") && strings.Contains(s, "'password'")
}

func assertGeneratePrecedesVaultReads(t *testing.T, caseFile string) {
	t.Helper()
	tasks, passage := loadCreatePlan(t, caseFile)

	// (1) generate-step IS PRESENT (finding 2, point «assert step present»).
	genIdx := -1
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			if genIdx != -1 {
				t.Fatalf("%s: found >1 generate step for secrets (idx %d and %d) — plan is ambiguous", caseFile, genIdx, i)
			}
			genIdx = i
		}
	}
	if genIdx == -1 {
		t.Fatalf("%s: generate step for secrets (core.vault.kv-present, targets secret/redis/...#password) IS ABSENT from plan — without it L0 passes on pre-seeded fixtures, but prod run on fresh Vault would fail (vault_resolve)", caseFile)
	}
	genPassage := passage.TaskPassage[genIdx]

	// (2) ALL vault()-reading tasks in passage STRICTLY AFTER generate (carrying invariant).
	readers := 0
	for i := range tasks {
		if i == genIdx || !taskReadsVaultSecret(&tasks[i]) {
			continue
		}
		readers++
		rp := passage.TaskPassage[i]
		if rp <= genPassage {
			name := tasks[i].Name
			t.Fatalf("%s: vault()-reading task %q in passage %d, generate in passage %d — generate MUST be STRICTLY BEFORE (otherwise deploy render would proceed before secrets are written)", caseFile, name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("%s: no vault()-reading task in plan — guard lost its subject (deploy body stopped reading secrets?)", caseFile)
	}
	t.Logf("%s: generate passage=%d < %d vault()-read tasks (all strictly later)", caseFile, genPassage, readers)
}

// TestRedisCreate_SecretGeneratePrecedesVaultReads_Sentinel — guard on sentinel-case.
func TestRedisCreate_SecretGeneratePrecedesVaultReads_Sentinel(t *testing.T) {
	assertGeneratePrecedesVaultReads(t, createSentinelCase)
}

// TestRedisCreate_SecretGeneratePrecedesVaultReads_Cluster — guard on cluster-case.
func TestRedisCreate_SecretGeneratePrecedesVaultReads_Cluster(t *testing.T) {
	assertGeneratePrecedesVaultReads(t, createClusterCase)
}

// TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis — reverse-guard on vault-axis
// (ADR-056 amendment, Variant A). BEFORE amendment edge generate→read in create/ was held
// ONLY by roster-axis (refresh-emitter of provision): without provision-body plan would
// collapse to one passage (this test historically fixed that exact degradation). After amendment
// the order is held by INDEPENDENT vault-axis (kv-present-emitter → vault()-read), therefore
// removing provision NO LONGER collapses generate→read. Test is inverted: now it verifies that
// even without refresh-emitter (provision-body removed) generate REMAINS strictly before each
// vault()-read. If vault-axis breaks — this test will fail (read will move back to generate
// passage), symmetric to the main guard in create_from_souls.
func TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis(t *testing.T) {
	tasks, _ := loadCreatePlan(t, createSentinelCase)

	var filtered []config.Task
	for i := range tasks {
		addr := ""
		if tasks[i].Module != nil {
			addr = tasks[i].Module.Module
		}
		// Provision body (cloud-create + bootstrap + soul.registered refresh) is the only
		// carrier of refresh-emitter in the plan. Remove it entirely so roster-axis does
		// NOT participate and order is checked EXCLUSIVELY by vault-axis.
		if strings.HasPrefix(addr, "core.cloud") || strings.HasPrefix(addr, "core.bootstrap") || strings.HasPrefix(addr, "core.soul") {
			continue
		}
		filtered = append(filtered, tasks[i])
	}

	passage, perr := config.Stratify(filtered)
	if perr != nil {
		t.Fatalf("Stratify (without provision): %v", perr)
	}

	genIdx := -1
	for i := range filtered {
		if taskIsSecretGenerate(&filtered[i]) {
			genIdx = i
			break
		}
	}
	if genIdx == -1 {
		t.Fatalf("generate step disappeared after provision filtering — filter is too broad")
	}
	genPassage := passage.TaskPassage[genIdx]

	// WITHOUT refresh-emitter there is no roster-axis — order is held by vault-axis. Each
	// vault()-read MUST be in passage STRICTLY AFTER generate (otherwise vault-axis is broken
	// and prod run create_from_souls — where there is no roster-axis in principle — would fail
	// with render_failed).
	readers := 0
	for i := range filtered {
		if i == genIdx || !taskReadsVaultSecret(&filtered[i]) {
			continue
		}
		readers++
		if passage.TaskPassage[i] <= genPassage {
			t.Fatalf("vault()-read task %q in passage %d, generate in passage %d — vault-axis MUST hold order without roster-axis (ADR-056 amendment), but read collapsed with generate", filtered[i].Name, passage.TaskPassage[i], genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("no vault()-read task after provision filtering — reverse-guard lost its subject")
	}
	t.Logf("without refresh-emitter: vault-axis holds generate (passage %d) strictly before %d vault()-read tasks", genPassage, readers)
}

// TestRedisCreate_TwoAxesSamePassageCount — vault-axis (ADR-056 amendment, Variant A)
// does NOT inflate Passage-plan of create/. In create/ provision-body provides roster-axis,
// and generate-step provides vault-axis; BOTH move read-tasks to the same late Passage
// (generate→0, read→≥1). Combining via level=max collapses matching axes, therefore adding
// vault-axis does NOT increase Count relative to the plan where only roster-axis works.
// Contract: Count(both axes) == Count(roster only). If vault-axis starts splitting the plan
// beyond roster-axis (extra Passage = extra dispatch-round) — test will fail. Commented on
// by guard comments (this file and redis_create_from_souls_secrets_passage_test.go) — it
// closes «Count did NOT grow».
func TestRedisCreate_TwoAxesSamePassageCount(t *testing.T) {
	tasks, both := loadCreatePlan(t, createSentinelCase)

	// Roster-only: rename generate-step to neutral module — vault-emitter disappears
	// (taskIsVaultEmitter checks address core.vault.kv-present), vault-axis is dead, roster-axis
	// (provision-refresh) remains. Task plan itself is the same, only the class of one task
	// changes — pure A/B on axis.
	var rosterOnly []config.Task
	renamed := false
	for i := range tasks {
		cp := tasks[i]
		if taskIsSecretGenerate(&cp) {
			m := *cp.Module
			m.Module = "core.noop.run"
			cp.Module = &m
			renamed = true
		}
		rosterOnly = append(rosterOnly, cp)
	}
	if !renamed {
		t.Fatalf("generate step not found in plan — A/B on vault-axis not possible (subject disappeared)")
	}

	rosterPlan, err := config.Stratify(rosterOnly)
	if err != nil {
		t.Fatalf("Stratify (roster-only): %v", err)
	}

	if both.Count != rosterPlan.Count {
		t.Fatalf("vault-axis inflated create/ plan: Count(both axes)=%d != Count(roster only)=%d — vault-axis should match roster-axis (level=max), not add Passage", both.Count, rosterPlan.Count)
	}
	t.Logf("create/ Count=%d same with both axes and roster-only — vault-axis did not inflate plan", both.Count)
}
