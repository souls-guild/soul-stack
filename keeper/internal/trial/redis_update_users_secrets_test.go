package trial

// Guards on secret generation in redis update_users (NIM-172, symmetric with
// redis_add_user_secrets_test.go). Before the fix the operator had to `vault kv put` a password
// for every user newly appearing in the set; now the scenario generates the missing ones itself
// (core.vault.kv-present, generate-if-absent) in a task preceding the render.
//
// Difference from add_user: the operator-extra layer here is the WHOLE input.users array
// (bulk-replace), so targets fan out over the array rather than a single user. That brings one
// extra failure mode of its own — an EMPTY set (`input.users: []`, the legitimate "remove ALL
// operator-extra" run) would hit kv-present's "targets: empty list" rejection. The scenario gates
// the step with a static `when`, and the group-drop is asserted L0-natively via task_absent in
// scenario/update_users/tests/empty-removes-all/case.yml.

import (
	"sort"
	"testing"
)

// updateUsersSystemUsersCase carries a two-user new set plus the full system set in essence —
// every class of secret the render reads.
const updateUsersSystemUsersCase = "../../../examples/service/redis/scenario/update_users/tests/preserves-system-users/case.yml"

// TestRedisUpdateUsers_SecretGeneratePrecedesVaultReads — the generate step exists and lands in a
// Passage STRICTLY BEFORE every vault()-reading task. update_users has no provision body, so (as
// in add_user and create_from_souls) only the vault axis holds this order.
func TestRedisUpdateUsers_SecretGeneratePrecedesVaultReads(t *testing.T) {
	tasks, passage := loadCreatePlan(t, updateUsersSystemUsersCase)

	genIdx := -1
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			if genIdx != -1 {
				t.Fatalf("found >1 secret generate steps (idx %d and %d) — update_users plan is ambiguous", genIdx, i)
			}
			genIdx = i
		}
	}
	if genIdx == -1 {
		t.Fatalf("secret generate step (core.vault.kv-present) MISSING from the update_users plan — a user newly added to the set would need a manual Vault pre-seed (NIM-172)")
	}
	genPassage := passage.TaskPassage[genIdx]

	readers := 0
	for i := range tasks {
		if i == genIdx || !taskReadsVaultSecret(&tasks[i]) {
			continue
		}
		readers++
		if rp := passage.TaskPassage[i]; rp <= genPassage {
			t.Fatalf("vault()-reading task %q in passage %d, generate in passage %d — generate MUST be STRICTLY BEFORE (only the vault axis holds this order here)", tasks[i].Name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("no vault()-reading tasks in the update_users plan — guard lost its subject")
	}
	t.Logf("update_users: generate passage=%d < %d vault()-read tasks", genPassage, readers)
}

// TestRedisUpdateUsers_GeneratesOnlyOperatorSetSecrets — the SCOPE contract against the paths the
// render actually reads: targets cover EXACTLY the new operator set (input.users) and nothing
// else. default_admin and the system users are read but must NOT be generated here — they are the
// credentials the live instance authenticates with, and a bulk-replace run must never rotate them.
func TestRedisUpdateUsers_GeneratesOnlyOperatorSetSecrets(t *testing.T) {
	readSet, generatedSet := renderCreateReadSet(t, updateUsersSystemUsersCase)

	// The fixture's input.users; the incarnation is the scenario name (renderCreateReadSet).
	want := []string{
		"redis/update_users/users/alice",
		"redis/update_users/users/carol",
	}
	for _, p := range want {
		if _, ok := readSet[p]; !ok {
			t.Fatalf("operator-set path %q is not read by the render — guard lost its subject; read: %v", p, sortedSetKeys(readSet))
		}
		if _, ok := generatedSet[p]; !ok {
			t.Fatalf("operator-set path %q is NOT in kv-present targets — update_users would still need a manual pre-seed for it (NIM-172); generated: %v", p, sortedSetKeys(generatedSet))
		}
	}

	wanted := map[string]bool{}
	for _, p := range want {
		wanted[p] = true
	}
	var overreach []string
	for p := range generatedSet {
		if !wanted[p] {
			overreach = append(overreach, p)
		}
	}
	if len(overreach) > 0 {
		sort.Strings(overreach)
		t.Fatalf("kv-present targets reach beyond input.users: %v — update_users must NOT generate default_admin / system passwords (the live instance authenticates with them; a bulk-replace run would silently rotate them)", overreach)
	}

	// Lower bound: the fixture really does read credentials outside the operator set, so the
	// assertion above is meaningful rather than vacuous.
	for _, p := range []string{
		"redis/update_users/users/default_admin", // intra-cluster AUTH + ACL LOAD connect
		"redis/update_users/users/replica",       // system user from essence
	} {
		if _, ok := readSet[p]; !ok {
			t.Errorf("expected pre-existing path %q in read-set, only have: %v", p, sortedSetKeys(readSet))
		}
	}
	t.Logf("update_users: generated exactly %v out of %d read redis-secret paths", sortedSetKeys(generatedSet), len(readSet))
}
