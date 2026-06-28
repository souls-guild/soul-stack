package trial

// Guard на несущий passage-инвариант generate-шага секретов redis-create (находка 2
// review «generate-шаг секретов»). create САМ генерит недостающие пароли в Vault шагом
// `core.vault.kv-present` (on: keeper) ДО деплой-задач, читающих те же секреты через
// ${ vault(...) }. Инвариант: generate ОБЯЗАН быть в passage СТРОГО РАНЬШЕ любой
// vault()-читающей задачи — иначе staged-render запустил бы render деплоя ДО записи
// секретов и упал бы на отсутствующем пути (vault_resolve), а L0 этого не ловит (фикстуры
// пред-сеют секреты).
//
// Почему Go-guard, а не L0 case.yml: L0 — render-only на пред-сеянных фикстурах, он НЕ
// выражает passage-порядок (case.yml не видит Stratify-план). Этот guard работает на
// РЕАЛЬНОМ create-плане (LoadScenarioManifest + ExpandIncludes + Stratify) и проверяет
// именно отношение passage(generate) < passage(каждой vault()-read). Образец загрузки —
// repro_staged_test.go (тот же helper-набор trial).
//
// Что ловит регресс (ровно сценарии, от которых предостерёг review):
//   1. generate-шаг удалён / переименован → fail «generate-шаг отсутствует»;
//   2. read-задача стала host-инвариантной (перестала читать roster) — она села бы в
//      passage 0 К generate → fail «generate не раньше read»;
//   3. refresh-эмиттер (provision core.soul.registered refresh_soulprint:true) исчез из
//      плана → roster-ребро пропадает, generate и read схлопываются в passage 0 → fail.
// Под-тест TestRedisCreate_NoRefreshEmitter_CollapsesPassage документирует механизм (3)
// явно: без refresh-эмиттера план вырождается в один passage — это и есть хрупкость, ради
// которой guard и существует.

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

// loadCreatePlan грузит create-сценарий по пути любого его L0-кейса и возвращает плоский
// план (после ExpandIncludes) вместе со стратификацией. План одинаков для всех кейсов
// (ветви cluster/sentinel дропаются по include-when ПОЗЖЕ, на render-фазе — Stratify
// видит полный список), поэтому passage-инвариант проверяется на самом плане.
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

// taskReadsVaultSecret — задача читает секрет через ${ vault(...) } в любом keeper-
// рендеримом значении (module.params / apply.input). vault() — единственный CEL-builtin
// чтения секрета (ADR-010), поэтому подстроки `vault(` достаточно; строковые литералы с
// `vault(` в данных redis-сценария не встречаются (пути секретов строятся конкатенацией,
// не содержат токена). Сама generate-задача (core.vault.kv-present) vault() НЕ зовёт
// (пишет по targets), поэтому в множество read не попадает — пересечения нет.
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

// taskIsSecretGenerate — задача-генератор секретов: keeper-side core.vault.kv-present
// с targets, адресующими главный пароль redis (secret/redis/...#password). Сверка targets
// (а не только module-адреса) исключает ложное совпадение с любым другим kv-present шагом.
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

	// (1) generate-шаг ПРИСУТСТВУЕТ (находка 2, пункт «assert шаг присутствует»).
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

	// (2) ВСЕ vault()-читающие задачи в passage СТРОГО ПОЗЖЕ generate (несущий инвариант).
	readers := 0
	for i := range tasks {
		if i == genIdx || !taskReadsVaultSecret(&tasks[i]) {
			continue
		}
		readers++
		rp := passage.TaskPassage[i]
		if rp <= genPassage {
			name := tasks[i].Name
			t.Fatalf("%s: vault()-читающая задача %q в passage %d, generate в passage %d — generate ОБЯЗАН быть СТРОГО РАНЬШЕ (иначе render деплоя пошёл бы до записи секретов)", caseFile, name, rp, genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("%s: ни одной vault()-читающей задачи в плане — guard потерял предмет проверки (деплой-тело перестало читать секреты?)", caseFile)
	}
	t.Logf("%s: generate passage=%d < %d vault()-read задач (все строго позже)", caseFile, genPassage, readers)
}

// TestRedisCreate_SecretGeneratePrecedesVaultReads_Sentinel — guard на sentinel-кейсе.
func TestRedisCreate_SecretGeneratePrecedesVaultReads_Sentinel(t *testing.T) {
	assertGeneratePrecedesVaultReads(t, createSentinelCase)
}

// TestRedisCreate_SecretGeneratePrecedesVaultReads_Cluster — guard на cluster-кейсе.
func TestRedisCreate_SecretGeneratePrecedesVaultReads_Cluster(t *testing.T) {
	assertGeneratePrecedesVaultReads(t, createClusterCase)
}

// TestRedisCreate_NoRefreshEmitter_CollapsesPassage документирует ФАКТИЧЕСКИЙ механизм
// порядка: ребро generate→read держит roster-ось (refresh-эмиттер provision +
// roster-потребление деплоя), а НЕ register. Убираем provision-задачи из плана (имитируем
// исчезновение refresh-эмиттера) и фиксируем, что generate и vault()-read СХЛОПЫВАЮТСЯ в
// один passage. Это контроль-точка хрупкости: если кто-то решит, что provision можно убрать
// «безопасно», основной guard выше упадёт — а этот тест объясняет ПОЧЕМУ.
func TestRedisCreate_NoRefreshEmitter_CollapsesPassage(t *testing.T) {
	tasks, _ := loadCreatePlan(t, createSentinelCase)

	var filtered []config.Task
	for i := range tasks {
		addr := ""
		if tasks[i].Module != nil {
			addr = tasks[i].Module.Module
		}
		// Provision-тело (cloud-create + bootstrap + soul.registered refresh) — единственный
		// носитель refresh-эмиттера в плане. Убираем его целиком.
		if strings.HasPrefix(addr, "core.cloud") || strings.HasPrefix(addr, "core.bootstrap") || strings.HasPrefix(addr, "core.soul") {
			continue
		}
		filtered = append(filtered, tasks[i])
	}

	passage, perr := config.Stratify(filtered)
	if perr != nil {
		t.Fatalf("Stratify (без provision): %v", perr)
	}

	genIdx := -1
	for i := range filtered {
		if taskIsSecretGenerate(&filtered[i]) {
			genIdx = i
			break
		}
	}
	if genIdx == -1 {
		t.Fatalf("generate-шаг исчез после фильтрации provision — фильтр слишком широкий")
	}
	genPassage := passage.TaskPassage[genIdx]

	// БЕЗ refresh-эмиттера ребро roster-оси пропадает → generate и read в одном passage.
	// Документируем это как ожидаемое вырождение (НЕ как корректное поведение прогона):
	// именно поэтому provision-include обязан оставаться в плане, а основной guard следит,
	// что в реальном (полном) плане порядок соблюдён.
	collapsed := true
	for i := range filtered {
		if i == genIdx || !taskReadsVaultSecret(&filtered[i]) {
			continue
		}
		if passage.TaskPassage[i] > genPassage {
			collapsed = false
		}
	}
	if !collapsed {
		t.Fatalf("ожидалось вырождение в один passage без refresh-эмиттера, но read-задача уехала позже generate (passage %d) — механизм порядка изменился, обнови комментарий main.yml и этот guard", genPassage)
	}
	t.Logf("без refresh-эмиттера: generate и vault()-read схлопнулись в passage %d (хрупкость задокументирована — provision несёт ребро)", genPassage)
}
