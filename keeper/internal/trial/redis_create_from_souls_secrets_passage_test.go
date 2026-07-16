package trial

// Guard on passage invariant of secret generate step in create_from_souls (Variant A,
// amendment ADR-056: vault-secrets-generated passage axis). create_from_souls rolls out
// deployment on ALREADY onboarded roster — no provision body, so NO
// refresh-emitter (core.soul.registered with refresh_soulprint), and with it no
// roster-passage axis (passage.go builds its edge ONLY if refresh-emitter present).
//
// ★ Difference from create/ (redis_create_secrets_passage_test.go): there order
// generate→read was held by roster axis (provision-refresh moved read-tasks to late
// passage). HERE it's gone — order MUST be held by NEW vault axis (kv-present-emitter
// → any vault()-read goes to passage strictly after, ADR-056 amendment). Without it plan
// collapsed to one passage and create_from_souls failed render_failed (vault_resolve
// read secret BEFORE write) — this is the live-bug for which guard exists.
//
// Invariant: generate (core.vault.kv-present) MUST be in passage STRICTLY BEFORE
// any vault()-reading task. Helpers taskIsSecretGenerate / taskReadsVaultSecret /
// loadCreatePlan are reused from redis_create_secrets_passage_test.go (same
// package trial) — single source of truth "what we generate / what we read".
//
// What catches regression:
//   1. generate step removed / renamed → fail "generate step missing";
//   2. vault axis broken (kv-present-emitter stopped splitting vault()-read) →
//      generate and read collapse to passage 0 → fail "generate not before read".
// Sub-test in redis_create_secrets_passage_test.go (inverted
// TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis) additionally fixes that
// vault axis holds order EVEN without roster axis.

import "testing"

// assertGeneratePrecedesVaultReadsCFS — same invariant as assertGeneratePrecedesVaultReads,
// but on create_from_souls plan (no refresh-emitter → order entirely on vault axis).
// Body identical to main helper; extracted with separate name so when failing
// in trace it's clear red came from create_from_souls (different order axis,
// different regression reason).
func assertGeneratePrecedesVaultReadsCFS(t *testing.T, caseFile string) {
	t.Helper()
	tasks, passage := loadCreatePlan(t, caseFile)

	genIdx := -1
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			if genIdx != -1 {
				t.Fatalf("%s: found >1 secret generate steps (idx %d and %d) — plan ambiguous", caseFile, genIdx, i)
			}
			genIdx = i
		}
	}
	if genIdx == -1 {
		t.Fatalf("%s: secret generate step (core.vault.kv-present, targets secret/redis/...#password) MISSING in plan — without it L0 passes on pre-seeded fixtures, but prod-run on fresh Vault would fail (vault_resolve)", caseFile)
	}
	genPassage := passage.TaskPassage[genIdx]

	readers := 0
	for i := range tasks {
		if i == genIdx || !taskReadsVaultSecret(&tasks[i]) {
			continue
		}
		readers++
		rp := passage.TaskPassage[i]
		if rp <= genPassage {
			t.Fatalf("%s: vault()-reading task %q in passage %d, generate in passage %d — generate MUST be STRICTLY BEFORE (vault axis ADR-056 amendment holds order without roster axis; no provision body here)", caseFile, tasks[i].Name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("%s: no vault()-reading tasks in plan — guard lost its subject matter (deploy body stopped reading secrets?)", caseFile)
	}
	t.Logf("%s: generate passage=%d < %d vault()-read tasks (vault axis holds order, no refresh-emitter)", caseFile, genPassage, readers)
}

const (
	cfsClusterCase  = "../../../examples/service/redis/scenario/create_from_souls/tests/cluster-from-souls-3shards/case.yml"
	cfsSentinelCase = "../../../examples/service/redis/scenario/create_from_souls/tests/sentinel-from-souls-1master-2replica/case.yml"
)

// TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Cluster — guard on
// cluster case of create_from_souls (no refresh-emitter, order on vault axis).
func TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Cluster(t *testing.T) {
	assertGeneratePrecedesVaultReadsCFS(t, cfsClusterCase)
}

// TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Sentinel — guard on
// sentinel case of create_from_souls.
func TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Sentinel(t *testing.T) {
	assertGeneratePrecedesVaultReadsCFS(t, cfsSentinelCase)
}
