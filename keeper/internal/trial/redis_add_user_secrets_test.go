package trial

// Guards on secret generation in redis add_user (NIM-172). Before the fix the operator had
// to `vault kv put` the new user's password BEFORE every add_user run, otherwise render
// aborted on vault_resolve. Now the scenario generates it itself
// (core.vault.kv-present, generate-if-absent) in a task preceding the render.
//
// Why Go-guards and not L0 case.yml: L0 is render-only on a PRE-SEEDED fixture vault and never
// applies the keeper-side generate step, so no case.yml can express either "no pre-seed was
// needed" or the passage order. These guards work on the REAL add_user plan (LoadScenarioManifest
// + ExpandIncludes + Stratify / one render pass) — the same split as the create/ guards
// (redis_create_secrets_passage_test.go + redis_create_secrets_coverage_test.go).
//
// Two invariants, one per axis of the ticket:
//
//  1. ORDER — generate strictly before every vault()-read. add_user has NO provision body →
//     no refresh emitter → the roster axis is INACTIVE; the order rests entirely on the vault
//     axis (ADR-056 amendment, shared/config/passage_vault.go), exactly like create_from_souls.
//     A regression there collapses the plan into one Passage → render_failed on a fresh Vault.
//  2. SCOPE — targets carry the new user's path and NOTHING else. Generating default_admin /
//     system users / already-deployed operator users would mint credentials the LIVE instance
//     does not know (community.redis.acl AUTHs as default_admin BEFORE ACL LOAD applies the new
//     file) or silently rotate a working client's password. This is also the re-run contract:
//     kv-present is generate-if-absent, so an add_user re-run on an existing user keeps their
//     password — nothing in the plan may reach past input.user.

import (
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// addUserSystemUsersCase is the richest add_user fixture: new user alice, already-deployed
// operator user bob in state.redis_users, and the full system set (default_admin/replica/
// monitoring/sentinel/haproxy) in the service's vars — i.e. every class of secret the render reads.
const addUserSystemUsersCase = "../../../examples/service/redis/scenario/add_user/tests/add-user-preserves-system-users/case.yml"

// TestRedisAddUser_SecretGeneratePrecedesVaultReads — the generate step exists in the add_user
// plan and lands in a Passage STRICTLY BEFORE every vault()-reading task. Without the step the
// operator is back to a manual pre-seed; without the order the run fails at vault_resolve on a
// fresh Vault (L0 would not notice — its fixtures are pre-seeded).
func TestRedisAddUser_SecretGeneratePrecedesVaultReads(t *testing.T) {
	tasks, passage := loadCreatePlan(t, addUserSystemUsersCase)

	genIdx := -1
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			if genIdx != -1 {
				t.Fatalf("found >1 secret generate steps (idx %d and %d) — add_user plan is ambiguous", genIdx, i)
			}
			genIdx = i
		}
	}
	if genIdx == -1 {
		t.Fatalf("secret generate step (core.vault.kv-present, targets secret/redis/...#password) MISSING from the add_user plan — the operator would have to pre-seed the password into Vault before every run (NIM-172)")
	}
	genPassage := passage.TaskPassage[genIdx]

	readers := 0
	for i := range tasks {
		if i == genIdx || !taskReadsVaultSecret(&tasks[i]) {
			continue
		}
		readers++
		if rp := passage.TaskPassage[i]; rp <= genPassage {
			t.Fatalf("vault()-reading task %q in passage %d, generate in passage %d — generate MUST be STRICTLY BEFORE (add_user has no refresh emitter, so only the vault axis holds this order)", tasks[i].Name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("no vault()-reading tasks in the add_user plan — guard lost its subject (the render stopped reading secrets?)")
	}
	t.Logf("add_user: generate passage=%d < %d vault()-read tasks", genPassage, readers)
}

// TestRedisAddUser_GeneratesOnlyNewUserSecret — the SCOPE contract, checked against the paths the
// render actually reads (a tracking Vault reader, like the create coverage guard):
//
//   - the new user's path IS read AND IS in targets → no pre-seed needed, and the generate
//     formula cannot drift away from the read convention;
//   - every OTHER read redis-secret path is NOT in targets → add_user never regenerates
//     default_admin / system users / already-deployed operator users, so a re-run cannot
//     silently rotate a password the live instance and its clients are already using.
func TestRedisAddUser_GeneratesOnlyNewUserSecret(t *testing.T) {
	readSet, generatedSet := renderCreateReadSet(t, addUserSystemUsersCase)

	// The fixture's input.user.name; the incarnation is the scenario name (renderCreateReadSet).
	const newUserPath = "redis/add_user/users/alice"

	if _, ok := readSet[newUserPath]; !ok {
		t.Fatalf("the new user's secret path %q is not read by the render — guard lost its subject; read: %v", newUserPath, sortedSetKeys(readSet))
	}
	if _, ok := generatedSet[newUserPath]; !ok {
		t.Fatalf("the new user's secret path %q is NOT in kv-present targets — add_user would still need a manual pre-seed (NIM-172); generated: %v", newUserPath, sortedSetKeys(generatedSet))
	}

	var overreach []string
	for p := range generatedSet {
		if p != newUserPath {
			overreach = append(overreach, p)
		}
	}
	if len(overreach) > 0 {
		sort.Strings(overreach)
		t.Fatalf("kv-present targets reach beyond the user being added: %v — add_user must NOT generate default_admin / system / already-deployed operator passwords (they are the credentials the live instance authenticates with, and a re-run would silently rotate them)", overreach)
	}

	// Sanity on the lower bound: the fixture really does read pre-existing secrets, so the
	// "only the new user" assertion above is meaningful rather than vacuous.
	for _, want := range []string{
		"redis/add_user/users/default_admin", // intra-cluster AUTH + ACL LOAD connect
		"redis/add_user/users/bob",           // already-deployed operator-extra
		"redis/add_user/users/replica",       // system user from the service's vars
	} {
		if _, ok := readSet[want]; !ok {
			t.Errorf("expected pre-existing path %q in read-set, only have: %v", want, sortedSetKeys(readSet))
		}
	}
	t.Logf("add_user: generated exactly %v out of %d read redis-secret paths", sortedSetKeys(generatedSet), len(readSet))
}

// TestRedisAddUser_GenerateStepLeaksNoSecret — the generate step itself carries no secret material
// (ADR-010): its params are paths + policy only, it reads no vault(), and it captures no
// register/output, so the generated value has no route into the run's register, events, logs or
// state. The module-side counterpart (the value never reaches output/audit) is pinned by
// keeper/internal/coremod/vault: TestPresent_SecurityNoLeak.
func TestRedisAddUser_GenerateStepLeaksNoSecret(t *testing.T) {
	tasks, _ := loadCreatePlan(t, addUserSystemUsersCase)

	var gen *config.Task
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			gen = &tasks[i]
			break
		}
	}
	if gen == nil {
		t.Fatalf("secret generate step missing from the add_user plan")
	}

	if taskReadsVaultSecret(gen) {
		t.Errorf("the generate step reads vault() — it must only WRITE, otherwise it becomes its own vault consumer and the passage edge degenerates")
	}
	if gen.Register != "" {
		t.Errorf("the generate step captures register %q — kv-present output would carry generated field names into the run register; add_user needs no such capture", gen.Register)
	}
	if len(gen.Output) != 0 {
		t.Errorf("the generate step declares output %v — no projection of a secret-generating step belongs in the run register", sortedKeys(gen.Output))
	}

	targets, _ := gen.Module.Params["targets"].(string)
	if !strings.Contains(targets, "input.user.name") {
		t.Errorf("targets %q does not derive the path from input.user.name — the generated path must follow the same convention the render reads", targets)
	}
}
