package trial

// Guard на passage-инвариант generate-шага секретов в create_from_souls (Вариант A,
// amendment ADR-056: vault-secrets-generated passage-ось). create_from_souls катит
// деплой по УЖЕ онбордившемуся roster-у — provision-тела НЕТ, значит НЕТ
// refresh-эмиттера (core.soul.registered с refresh_soulprint), а с ним нет и
// roster-passage-оси (passage.go строит её ребро ТОЛЬКО при наличии refresh-эмиттера).
//
// ★ Отличие от create/ (redis_create_secrets_passage_test.go): там порядок
// generate→read держала roster-ось (provision-refresh уводил read-задачи в поздний
// passage). ЗДЕСЬ её нет — порядок ОБЯЗАНА держать НОВАЯ vault-ось (kv-present-эмиттер
// → любая vault()-read едет в passage строго после, ADR-056 amendment). Без неё план
// схлопывался в один passage и create_from_souls падал render_failed (vault_resolve
// читал секрет ДО записи) — это и есть live-баг, ради которого guard существует.
//
// Инвариант: generate (core.vault.kv-present) ОБЯЗАН быть в passage СТРОГО РАНЬШЕ
// любой vault()-читающей задачи. Helper-ы taskIsSecretGenerate / taskReadsVaultSecret /
// loadCreatePlan переиспользуются из redis_create_secrets_passage_test.go (тот же
// пакет trial) — один источник правды «что генерим / что читаем».
//
// Что ловит регресс:
//   1. generate-шаг удалён / переименован → fail «generate-шаг отсутствует»;
//   2. vault-ось сломана (kv-present-эмиттер перестал расщеплять vault()-read) →
//      generate и read схлопываются в passage 0 → fail «generate не раньше read».
// Под-тест в redis_create_secrets_passage_test.go (инвертированный
// TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis) дополнительно фиксирует, что
// vault-ось держит порядок ДАЖЕ без roster-оси.

import "testing"

// assertGeneratePrecedesVaultReadsCFS — тот же инвариант, что assertGeneratePrecedes-
// VaultReads, но на плане create_from_souls (refresh-эмиттера нет → порядок целиком на
// vault-оси). Тело идентично основному helper-у; вынесено отдельным именем, чтобы при
// падении в трейсе было видно, что красный пришёл от create_from_souls (другая ось
// порядка, другая причина регресса).
func assertGeneratePrecedesVaultReadsCFS(t *testing.T, caseFile string) {
	t.Helper()
	tasks, passage := loadCreatePlan(t, caseFile)

	genIdx := -1
	for i := range tasks {
		if taskIsSecretGenerate(&tasks[i]) {
			if genIdx != -1 {
				t.Fatalf("%s: найдено >1 generate-шага секретов (idx %d и %d) — план неоднозначен", caseFile, genIdx, i)
			}
			genIdx = i
		}
	}
	if genIdx == -1 {
		t.Fatalf("%s: generate-шаг секретов (core.vault.kv-present, targets secret/redis/...#password) ОТСУТСТВУЕТ в плане — без него L0 проходит на пред-сеянных фикстурах, но прод-прогон на свежем Vault упал бы (vault_resolve)", caseFile)
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
			t.Fatalf("%s: vault()-читающая задача %q в passage %d, generate в passage %d — generate ОБЯЗАН быть СТРОГО РАНЬШЕ (vault-ось ADR-056 amendment держит порядок без roster-оси; provision-тела тут нет)", caseFile, tasks[i].Name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("%s: ни одной vault()-читающей задачи в плане — guard потерял предмет проверки (деплой-тело перестало читать секреты?)", caseFile)
	}
	t.Logf("%s: generate passage=%d < %d vault()-read задач (vault-ось держит порядок, refresh-эмиттера нет)", caseFile, genPassage, readers)
}

const (
	cfsClusterCase  = "../../../examples/service/redis/scenario/create_from_souls/tests/cluster-from-souls-3shards/case.yml"
	cfsSentinelCase = "../../../examples/service/redis/scenario/create_from_souls/tests/sentinel-from-souls-1master-2replica/case.yml"
)

// TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Cluster — guard на
// cluster-кейсе create_from_souls (нет refresh-эмиттера, порядок на vault-оси).
func TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Cluster(t *testing.T) {
	assertGeneratePrecedesVaultReadsCFS(t, cfsClusterCase)
}

// TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Sentinel — guard на
// sentinel-кейсе create_from_souls.
func TestRedisCreateFromSouls_SecretGeneratePrecedesVaultReads_Sentinel(t *testing.T) {
	assertGeneratePrecedesVaultReadsCFS(t, cfsSentinelCase)
}
