package trial

// Drift-guard «генерим ≡ читаем» для секретов redis-create (qa-пробел 2026-06-28).
// Парный к redis_create_secrets_passage_test.go: тот следит за ПОРЯДКОМ
// (generate СТРОГО раньше любой vault()-read), этот — за ПОКРЫТИЕМ МНОЖЕСТВА путей.
//
// Инвариант: КАЖДЫЙ реально читаемый деплоем redis-секрет-путь
// (secret/redis/<inc> и secret/redis/<inc>/users/<name>, поле password) ОБЯЗАН
// быть в targets шага core.vault.kv-present, то есть read-set ⊆ generated-set.
// Иначе на свежем Vault render деплоя упадёт на vault_resolve ненайденного пути:
// generate его не создал. L0-кейсы пред-сеют ВСЕ секреты в fixtures.vault, поэтому
// сами по себе такой drift НЕ ловят (read проходит на пред-сеянном) — нужен этот
// guard, сверяющий фактически читаемые пути с тем, что шаг реально генерит.
//
// Как собирается read-set: render-план create прогоняется через ОДИН render-проход
// (как RunCase) с tracking-KVReader поверх fixtureVault — он перехватывает каждый
// ReadKV (= аргумент vault() без #field, ADR-010/shared.cel.splitVaultField). vault()
// в одном проходе резолвится для всех активных (non-group-drop) задач режима, так что
// перехватываются ВСЕ читаемые пути конкретного режима (sentinel ИЛИ cluster — ветви
// разводятся include-when). Из них берутся только redis-секрет-пути (см.
// isRedisSecretPath): TLS-PEM-пути живут под operator-конвенцией secret/ops/...
// (tls-essence-refs/case.yml) и под генерацию не подпадают.
//
// generated-set — пути targets отрендеренной kv-present-задачи (фактический план,
// не повторный CEL-резолв). Сверка по ПУТИ: и targets, и read используют поле
// password, дискриминатор пути достаточен (ReadKV всё равно field не видит).
//
// Что ловит регресс: новый системный/operator юзер, читающий
// vault('secret/redis/<inc>/users/<new>#password') в redis-deploy-*.yml, чей путь не
// попал в union targets kv-present (рассинхрон формулы generate и формулы read).

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	createClusterAclCase  = "../../../examples/service/redis/scenario/create/tests/cluster-acl-users/case.yml"
	createSentinelAclCase = "../../../examples/service/redis/scenario/create/tests/sentinel-acl-users/case.yml"
	kvPresentModuleAddr   = "core.vault.kv-present"
)

// trackingVault оборачивает fixtureVault и фиксирует множество путей, по которым
// деплой реально дёрнул ReadKV (= аргумент vault() без #field). Делегирует чтение
// базовому fixture-reader-у, поэтому render проходит ровно как в RunCase (значения
// не подменяются — только перехватывается факт обращения).
type trackingVault struct {
	base render.KVReader
	read map[string]struct{}
}

func newTrackingVault(base render.KVReader) *trackingVault {
	return &trackingVault{base: base, read: make(map[string]struct{})}
}

func (t *trackingVault) ReadKV(ctx context.Context, path string) (map[string]any, error) {
	t.read[path] = struct{}{}
	return t.base.ReadKV(ctx, path)
}

// renderCreateReadSet прогоняет create-кейс одним render-проходом (зеркало
// renderCase/RunCase) с tracking-reader и возвращает: множество фактически
// прочитанных redis-секрет-путей (read-set) и множество путей targets шага
// kv-present (generated-set). Оба нормализованы trim-ом дефолтного mount-префикса
// secret/ (как fixtureVault), чтобы сверка не зависела от logical/relative-формы.
func renderCreateReadSet(t *testing.T, caseFile string) (readSet, generatedSet map[string]struct{}) {
	t.Helper()
	ctx := context.Background()

	c, file, err := LoadCase(caseFile)
	if err != nil {
		t.Fatalf("LoadCase(%s): %v", caseFile, err)
	}

	// Единый load+covenant-резолв (harness.go::loadResolvedScenario) — тот же, что
	// renderCase: redis create — covenant-сценарий (compute.install/data_dir/
	// sentinel_directives в covenant.yml), без резолва CEL падает «no such key:
	// compute.install».
	scn, _, err := loadResolvedScenario(file)
	if err != nil {
		t.Fatalf("%v", err)
	}
	scnPath := scenarioPathFor(file)
	expanded, iDiags := config.ExpandIncludes(scn.Tasks, fixtureScenarioIncludeResolver(scnPath))
	if hasErrors(iDiags) {
		t.Fatalf("expand includes: %s", formatDiags(iDiags))
	}
	scn.Tasks = expanded

	tv := newTrackingVault(newFixtureVault(c.Fixtures.Vault))
	engine, err := cel.New(cel.WithVault(tv))
	if err != nil {
		t.Fatalf("cel.New(WithVault): %v", err)
	}
	pipeline := render.NewPipeline(tv, engine, nil, nil)

	deps, err := loadServiceDestinyDeps(file)
	if err != nil {
		t.Fatalf("destiny deps: %v", err)
	}
	destiny := newFixtureDestinyResolver(serviceRootFor(file), c.Fixtures.DefaultDestinySource, deps)

	effectiveInput, err := config.ResolveInputValues(scn.Input, c.Fixtures.Input)
	if err != nil {
		t.Fatalf("resolve input: %v", err)
	}
	if fail, evErr := config.EvalValidateRules(scn.Validate, effectiveInput); evErr != nil {
		t.Fatalf("validate err: %v", evErr)
	} else if fail != nil {
		t.Fatalf("validate fail: %s", fail.Error())
	}

	svcRoot := serviceRootFor(file)
	templates := render.NewSnapshotTemplateReader(
		func(rel string) ([]byte, error) { return readWithin(svcRoot, rel) },
		"scenario/"+scn.Name,
	)

	in := render.RenderInput{
		Scenario:    scn,
		Essence:     orEmptyMap(c.Fixtures.Essence),
		Input:       effectiveInput,
		Register:    orEmptyMap(c.Mocks.Register),
		Incarnation: render.IncarnationMeta{Name: scn.Name},
		Hosts:       fixtureHosts(scn.Name, c.Fixtures),
		Destiny:     destiny,
		Templates:   templates,
		State:       c.Fixtures.State,
		Ctx:         ctx,
	}

	tasks, _, rerr := pipeline.Render(ctx, in)
	if rerr != nil {
		t.Fatalf("%s: render failed: %v", caseFile, rerr)
	}

	generatedSet = kvPresentTargetPaths(t, caseFile, tasks)

	readSet = make(map[string]struct{})
	for p := range tv.read {
		if isRedisSecretPath(p) {
			readSet[normalizeVaultKey(p)] = struct{}{}
		}
	}
	return readSet, generatedSet
}

// kvPresentTargetPaths извлекает множество path из targets ЕДИНСТВЕННОЙ
// kv-present-задачи отрендеренного плана. Несколько таких задач или их отсутствие
// — Fatal: план неоднозначен / generate-шаг пропал (это бы и passage-guard поймал,
// но здесь предмет сверки именно targets, поэтому проверяем явно).
func kvPresentTargetPaths(t *testing.T, caseFile string, tasks []*render.RenderedTask) map[string]struct{} {
	t.Helper()
	out := make(map[string]struct{})
	found := 0
	for _, rt := range tasks {
		if rt.Module != kvPresentModuleAddr || rt.Params == nil {
			continue
		}
		found++
		raw, ok := rt.Params.AsMap()["targets"].([]any)
		if !ok {
			t.Fatalf("%s: kv-present targets не список: %T", caseFile, rt.Params.AsMap()["targets"])
		}
		for i, item := range raw {
			obj, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("%s: kv-present targets[%d] не объект: %T", caseFile, i, item)
			}
			path, ok := obj["path"].(string)
			if !ok || path == "" {
				t.Fatalf("%s: kv-present targets[%d].path пуст/не строка: %v", caseFile, i, obj["path"])
			}
			out[normalizeVaultKey(path)] = struct{}{}
		}
	}
	if found == 0 {
		t.Fatalf("%s: kv-present-задача (%s) ОТСУТСТВУЕТ в плане — generate-шаг секретов пропал", caseFile, kvPresentModuleAddr)
	}
	if found > 1 {
		t.Fatalf("%s: найдено %d kv-present-задач — план неоднозначен для сверки targets", caseFile, found)
	}
	return out
}

// isRedisSecretPath — путь относится к секретам redis-инкарнации под генерацию:
// всё под secret/redis/<inc> (главный пароль secret/redis/<inc> + per-user
// secret/redis/<inc>/users/...). TLS-PEM-пути под operator-конвенцией
// (secret/ops/..., см. tls-essence-refs/case.yml) сюда НЕ попадают — они не
// генерятся kv-present, оператор кладёт PEM сам. Нормализуем mount-префикс перед
// проверкой, чтобы logical ('secret/redis/...') и relative ('redis/...') совпали;
// имя инкарнации не хардкодим — префикса redis/ достаточно, прочих redis-путей
// (кроме паролей инкарнации) в плане нет.
func isRedisSecretPath(path string) bool {
	return strings.HasPrefix(normalizeVaultKey(path), "redis/")
}

// assertReadSubsetOfGenerated — несущая сверка: read-set ⊆ generated-set. Любой
// читаемый redis-секрет-путь вне targets kv-present — провал (на свежем Vault
// render деплоя упал бы на этом пути). Пустой read-set — тоже провал: guard потерял
// предмет (деплой перестал читать секреты?).
func assertReadSubsetOfGenerated(t *testing.T, caseFile string) {
	t.Helper()
	readSet, generatedSet := renderCreateReadSet(t, caseFile)

	if len(readSet) == 0 {
		t.Fatalf("%s: ни одного читаемого redis-секрет-пути — guard потерял предмет проверки (деплой перестал читать секреты?)", caseFile)
	}

	var missing []string
	for p := range readSet {
		if _, ok := generatedSet[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%s: читаемые секрет-пути НЕ покрыты targets kv-present (generate не создаст их → render деплоя упадёт на свежем Vault):\n  missing: %v\n  generated: %v\n  read: %v",
			caseFile, missing, sortedSetKeys(generatedSet), sortedSetKeys(readSet))
	}
	t.Logf("%s: read-set (%d путей) ⊆ generated-set (%d targets)", caseFile, len(readSet), len(generatedSet))
}

func sortedSetKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_Sentinel — sentinel-режим, без
// operator-extra: read-set ⊆ targets kv-present.
func TestRedisCreate_GeneratedSecretsCoverVaultReads_Sentinel(t *testing.T) {
	assertReadSubsetOfGenerated(t, createSentinelCase)
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_Cluster — cluster-режим с
// operator-extra (cluster-acl-users несёт input.users zeta/alpha).
func TestRedisCreate_GeneratedSecretsCoverVaultReads_Cluster(t *testing.T) {
	assertReadSubsetOfGenerated(t, createClusterAclCase)
}

// TestRedisCreate_GeneratedSecretsCoverVaultReads_SentinelOperatorExtra —
// sentinel-режим С operator-extra (sentinel-acl-users): закрывает drift sentinel +
// input.users постоянным кейсом.
func TestRedisCreate_GeneratedSecretsCoverVaultReads_SentinelOperatorExtra(t *testing.T) {
	assertReadSubsetOfGenerated(t, createSentinelAclCase)
}

// TestRedisCreate_ReadSetContainsExpectedPaths — sanity на сам перехват: в
// sentinel+operator-режиме read-set ОБЯЗАН содержать главный пароль и per-user путь
// (включая operator-extra). Без этого guard мог бы «проходить» на пустом read-set
// при сломанном перехвате (ложно-зелёный). Проверяем нижнюю границу множества.
func TestRedisCreate_ReadSetContainsExpectedPaths(t *testing.T) {
	readSet, _ := renderCreateReadSet(t, createSentinelAclCase)
	for _, want := range []string{
		"redis/create",             // главный пароль (requirepass / replica-auth / sentinel auth_pass)
		"redis/create/users/zeta",  // operator-extra
		"redis/create/users/alpha", // operator-extra
	} {
		if _, ok := readSet[want]; !ok {
			t.Errorf("ожидался читаемый путь %q в read-set, есть только: %v", want, sortedSetKeys(readSet))
		}
	}
}
