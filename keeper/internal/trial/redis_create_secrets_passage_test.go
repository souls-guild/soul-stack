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
// Порядок generate→read в create/ держат ДВЕ независимые passage-оси (ADR-056 +
// amendment Вариант A): roster-ось (provision-refresh уводит read-задачи в поздний
// passage) И vault-ось (kv-present-эмиттер → vault()-read). Обе указывают одинаково
// (generate→passage 0, read→passage ≥1), объединяются через level=max — поэтому create/
// Count от vault-оси НЕ вырос (см. TestRedisCreate_TwoAxesSamePassageCount).
//
// Что ловит регресс (ровно сценарии, от которых предостерёг review):
//   1. generate-шаг удалён / переименован → fail «generate-шаг отсутствует»;
//   2. ОБЕ оси сломаны (read-задача перестала читать и roster, и vault) — она села бы в
//      passage 0 К generate → fail «generate не раньше read».
// Под-тест TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis изолирует vault-ось: вырезает
// provision-тело (roster-ось заведомо неактивна) и фиксирует, что vault-ось ОДНА держит
// порядок — это и есть фикс live-бага create_from_souls (там roster-оси нет в принципе).

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

// TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis — реверс-guard на vault-ось
// (ADR-056 amendment, Вариант A). ДО amendment ребро generate→read в create/ держала
// ТОЛЬКО roster-ось (refresh-эмиттер provision): без provision-тела план схлопывался в
// один passage (этот тест исторически фиксировал ИМЕННО вырождение). После amendment
// порядок держит НЕЗАВИСИМАЯ vault-ось (kv-present-эмиттер → vault()-read), поэтому
// удаление provision БОЛЬШЕ НЕ схлопывает generate→read. Тест инвертирован: теперь он
// фиксирует, что даже без refresh-эмиттера (provision-тело вырезано) generate ОСТАЁТСЯ
// строго раньше каждой vault()-read. Если vault-ось сломают — этот тест покраснеет
// (read уедет обратно в passage generate), симметрично основному guard create_from_souls.
func TestRedisCreate_NoRefreshEmitter_HeldByVaultAxis(t *testing.T) {
	tasks, _ := loadCreatePlan(t, createSentinelCase)

	var filtered []config.Task
	for i := range tasks {
		addr := ""
		if tasks[i].Module != nil {
			addr = tasks[i].Module.Module
		}
		// Provision-тело (cloud-create + bootstrap + soul.registered refresh) — единственный
		// носитель refresh-эмиттера в плане. Убираем его целиком, чтобы roster-ось
		// заведомо НЕ участвовала и порядок проверялся ИСКЛЮЧИТЕЛЬНО vault-осью.
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

	// БЕЗ refresh-эмиттера roster-оси нет — порядок держит vault-ось. Каждая vault()-read
	// ОБЯЗАНА быть в passage СТРОГО ПОЗЖЕ generate (иначе vault-ось сломана и прод-прогон
	// create_from_souls — где roster-оси нет в принципе — упал бы render_failed).
	readers := 0
	for i := range filtered {
		if i == genIdx || !taskReadsVaultSecret(&filtered[i]) {
			continue
		}
		readers++
		if passage.TaskPassage[i] <= genPassage {
			t.Fatalf("vault()-read задача %q в passage %d, generate в passage %d — vault-ось ДОЛЖНА держать порядок без roster-оси (ADR-056 amendment), но read схлопнулась с generate", filtered[i].Name, passage.TaskPassage[i], genPassage)
		}
	}
	if readers == 0 {
		t.Fatalf("ни одной vault()-read задачи после фильтрации provision — реверс-guard потерял предмет проверки")
	}
	t.Logf("без refresh-эмиттера: vault-ось держит generate (passage %d) строго раньше %d vault()-read задач", genPassage, readers)
}

// TestRedisCreate_TwoAxesSamePassageCount — vault-ось (ADR-056 amendment, Вариант A)
// НЕ раздувает Passage-план create/. В create/ provision-тело даёт roster-ось, а
// generate-шаг — vault-ось; ОБЕ уводят read-задачи в один и тот же поздний Passage
// (generate→0, read→≥1). Объединение через level=max схлопывает совпадающие оси,
// поэтому добавление vault-оси НЕ увеличивает Count относительно плана, где работает
// только roster-ось. Контракт: Count(обе оси) == Count(только roster). Если vault-ось
// начнёт расщеплять план сверх roster-оси (лишний Passage = лишний dispatch-round) —
// тест покраснеет. На него ссылаются комментарии guard-ов (этот файл и
// redis_create_from_souls_secrets_passage_test.go) — он закрывает «Count НЕ вырос».
func TestRedisCreate_TwoAxesSamePassageCount(t *testing.T) {
	tasks, both := loadCreatePlan(t, createSentinelCase)

	// Roster-only: переименовываем generate-шаг в нейтральный модуль — vault-эмиттер
	// исчезает (taskIsVaultEmitter сверяет адрес core.vault.kv-present), vault-ось
	// мертва, roster-ось (provision-refresh) остаётся. Сам план задач тот же, меняется
	// только класс одной задачи — чистое A/B на ось.
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
		t.Fatalf("generate-шаг не найден в плане — A/B на vault-ось невозможно (предмет проверки исчез)")
	}

	rosterPlan, err := config.Stratify(rosterOnly)
	if err != nil {
		t.Fatalf("Stratify (roster-only): %v", err)
	}

	if both.Count != rosterPlan.Count {
		t.Fatalf("vault-ось раздула план create/: Count(обе оси)=%d != Count(только roster)=%d — vault-ось должна совпадать с roster-осью (level=max), а не добавлять Passage", both.Count, rosterPlan.Count)
	}
	t.Logf("create/ Count=%d одинаков с обеими осями и только с roster — vault-ось план не раздула", both.Count)
}
