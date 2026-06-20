package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"
)

// writeScenarioTree создаёт временное дерево scenario/<name>/{main.yml,
// tests/<case>/case.yml} и возвращает путь к директории кейса.
func writeScenarioTree(t *testing.T, mainYML, caseYML string) string {
	t.Helper()
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	caseDir := filepath.Join(scnDir, "tests", "c1")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(mainYML), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, caseFileName), []byte(caseYML), 0o644); err != nil {
		t.Fatalf("write case.yml: %v", err)
	}
	return caseDir
}

// writeScenarioSibling кладёт sibling-файл в scenario/create/<name> (для
// include-целей). Возвращает абсолютный путь файла.
func writeScenarioSibling(t *testing.T, caseDir, name, content string) {
	t.Helper()
	scnDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/create
	if err := os.WriteFile(filepath.Join(scnDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write sibling %s: %v", name, err)
	}
}

// writeServiceLevelSibling кладёт файл на service-level (scenario/<name> — общий
// для всех сценариев каталог scenario/, родитель scenario/create/).
func writeServiceLevelSibling(t *testing.T, caseDir, name, content string) {
	t.Helper()
	scnDir := filepath.Dir(filepath.Dir(caseDir)) // .../scenario/create
	serviceScenarioDir := filepath.Dir(scnDir)    // .../scenario
	if err := os.WriteFile(filepath.Join(serviceScenarioDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write service-level sibling %s: %v", name, err)
	}
}

// TestRunCase_ScenarioIncludeShadowing — двухуровневый резолв (orchestration.md
// §6): локальный include-файл полностью перекрывает service-level одноимённый
// (shadowing, без merge).
func TestRunCase_ScenarioIncludeShadowing(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: greet.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: include shadowing
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/local
        content: hi
`)
	// service-level версия (должна быть перекрыта локальной).
	writeServiceLevelSibling(t, caseDir, "greet.yml", `- name: service-level
  module: core.file.present
  params:
    path: /tmp/service-level
    content: "${ input.greeting }"
`)
	// локальная версия — побеждает.
	writeScenarioSibling(t, caseDir, "greet.yml", `- name: local
  module: core.file.present
  params:
    path: /tmp/local
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (локальный перекрывает service-level), FAIL: %v", results[0].Failures)
	}
}

// TestRunCase_ScenarioIncludeServiceLevelFallback — при отсутствии локального
// файла резолв падает на service-level.
func TestRunCase_ScenarioIncludeServiceLevelFallback(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: shared.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: service-level fallback
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/service-level
        content: hi
`)
	writeServiceLevelSibling(t, caseDir, "shared.yml", `- name: service-level
  module: core.file.present
  params:
    path: /tmp/service-level
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (service-level fallback), FAIL: %v", results[0].Failures)
	}
}

func TestRunCase_ScenarioInclude(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: greet.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: scenario include splice
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)
	writeScenarioSibling(t, caseDir, "greet.yml", `- name: write greeting
  module: core.file.present
  params:
    path: /tmp/soul-stack-hello
    content: "${ input.greeting }"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS scenario-include splice, получили FAIL: %v", results[0].Failures)
	}
}

func TestRunCase_ScenarioIncludeCycle(t *testing.T) {
	mainYML := `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - include: a.yml
`
	caseDir := writeScenarioTree(t, mainYML, `name: include cycle
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	writeScenarioSibling(t, caseDir, "a.yml", "- include: b.yml\n")
	writeScenarioSibling(t, caseDir, "b.yml", "- include: a.yml\n")

	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("ожидалась ошибка include-цикла a→b→a, получили nil")
	}
	if !strings.Contains(err.Error(), "include_cycle") {
		t.Fatalf("ожидался include_cycle в ошибке, получили: %v", err)
	}
}

const helloMain = `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write greeting
    module: core.file.present
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
`

func TestRunCase_Pass(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: hello pass
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ожидали 1 результат, получили %d", len(results))
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS, получили FAIL: %v", results[0].Failures)
	}
	// `${ input.greeting }` — одна не-bool интерполяция, должна попасть в coverage.
	if got := len(results[0].Coverage.NonBranch) + len(results[0].Coverage.Branches); got == 0 {
		t.Errorf("ожидали ненулевой trial coverage, получили 0 выражений")
	}
}

func TestRunCase_FailOnParams(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: hello fail
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: WRONG
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("ожидали FAIL на расхождении content")
	}
	if len(results[0].Failures) == 0 {
		t.Fatalf("ожидали непустой список расхождений")
	}
}

// TestRunCase_FailOnModule — расхождение по module-адресу.
func TestRunCase_FailOnModule(t *testing.T) {
	caseDir := writeScenarioTree(t, helloMain, `name: wrong module
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.absent
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("ожидали FAIL на расхождении module")
	}
}

// whereMain — scenario с where:-предикатом по soulprint.self для проверки
// branch-coverage. Один хост → предикат вычисляется один раз.
const whereMain = `name: create
input:
  greeting:
    type: string
    required: true
tasks:
  - name: write only on linux
    module: core.file.present
    where: "soulprint.self.os.family == 'linux'"
    params:
      path: /tmp/soul-stack-hello
      content: "${ input.greeting }"
`

// TestRunCase_WhereBranchCovered — кейс с where:, попавший в truthy-ветку,
// учитывается в branch-coverage (одна ветка из двух).
func TestRunCase_WhereBranchCovered(t *testing.T) {
	caseDir := writeScenarioTree(t, whereMain, `name: where truthy
fixtures:
  input:
    greeting: hi
  soulprint:
    os:
      family: linux
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS, получили: %v", results[0].Failures)
	}
	covered, total := results[0].Coverage.CoveredBranches()
	if total != 1 {
		t.Fatalf("ожидали 1 bool-выражение (where:), получили %d", total)
	}
	if covered != 0 {
		t.Fatalf("одна truthy-ветка не покрывает выражение целиком: covered=%d, ожидали 0", covered)
	}
}

// optionalStateMain — scenario, читающий optional-без-default input в
// state_changes.sets БЕЗ has()-guard. На latest-пути (значение не передано)
// рендер sets обязан упасть «no such key» — класс багов, который harness ловит
// рендером state_changes.
const optionalStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
state_changes:
  sets:
    redis_version: "${ input.redis_version }"
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
`

// TestRunCase_StateChangesRenderError — незащищённый optional-без-default input в
// state_changes.sets даёт ошибку рендера sets (RunCase возвращает err), а не
// тихий PASS. Это слепая зона harness-а до расширения: tasks рендерились,
// state_changes — нет.
func TestRunCase_StateChangesRenderError(t *testing.T) {
	caseDir := writeScenarioTree(t, optionalStateMain, `name: state_changes render must fail
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
`)
	_, err := Run(context.Background(), caseDir)
	if err == nil {
		t.Fatal("ожидали ошибку рендера state_changes (no such key по незащищённому optional input), получили nil")
	}
	if !strings.Contains(err.Error(), "state_changes") {
		t.Fatalf("ошибка должна указывать на state_changes, получили: %v", err)
	}
}

// guardedStateMain — тот же optional input, но с каноническим has()-guard:
// рендер sets не падает, на latest в state пишется "".
const guardedStateMain = `name: create
input:
  redis_version:
    type: string
    required: false
state_changes:
  sets:
    redis_version: "${ has(input.redis_version) ? input.redis_version : '' }"
tasks:
  - name: noop
    module: core.file.present
    params:
      path: /tmp/noop
      content: x
`

// TestRunCase_StateChangesAssertPass — has()-guarded optional рендерится в ""
// и совпадает с assert.state_changes.
func TestRunCase_StateChangesAssertPass(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: state_changes assert pass
fixtures:
  input: {}
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_changes:
    redis_version: ""
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (guard → \"\"), получили: %v", results[0].Failures)
	}
}

// TestRunCase_StateChangesAssertFail — расхождение assert.state_changes с
// отрендеренным значением — фейл кейса.
func TestRunCase_StateChangesAssertFail(t *testing.T) {
	caseDir := writeScenarioTree(t, guardedStateMain, `name: state_changes assert fail
fixtures:
  input:
    redis_version: "7.2.4"
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
  state_changes:
    redis_version: "WRONG"
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("ожидали FAIL на расхождении state_changes")
	}
	if len(results[0].Failures) == 0 {
		t.Fatal("ожидали непустой список расхождений")
	}
}

// addUserStateMain — scenario, накапливающий state поверх существующего:
// state_changes.sets добавляет last_user из input, не трогая базовый users.
// Зеркало add_user-операций над incarnation.state (orchestration.md §7.1).
const addUserStateMain = `name: add_user
input:
  name:
    type: string
state_changes:
  sets:
    last_user: "${ input.name }"
tasks:
  - name: create user
    module: core.user.present
    params:
      name: "${ input.name }"
`

// TestRunCase_StateAfterPass — assert.state_after сверяет ПОЛНЫЙ итоговый state:
// базовый fixtures.state (users) + отрендеренный sets (last_user). Зеркало
// прод-коммита mergeStateChanges(stateBefore, renderedSets).
func TestRunCase_StateAfterPass(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user accumulates over base state
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    users:
      - alice
    last_user: bob
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (base.users + sets.last_user), получили: %v", results[0].Failures)
	}
}

// TestRunCase_StateAfterFail — расхождение ожидаемого итога с фактическим
// (last_user в state_after не совпадает с отрендеренным) — фейл кейса.
func TestRunCase_StateAfterFail(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after mismatch
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    users:
      - alice
    last_user: WRONG
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("ожидали FAIL на расхождении last_user в state_after")
	}
	if len(results[0].Failures) == 0 {
		t.Fatal("ожидали непустой список расхождений")
	}
}

// TestRunCase_StateAfterFullCompare — state_after сверяется ПОЛНОСТЬЮ (как L1):
// лишний ключ в фактическом итоге (базовый users не упомянут в ожидаемом
// state_after) — тоже расхождение. Отличие от частичной сверки state_changes.
func TestRunCase_StateAfterFullCompare(t *testing.T) {
	caseDir := writeScenarioTree(t, addUserStateMain, `name: add_user state_after must be complete
fixtures:
  input:
    name: bob
  state:
    users:
      - alice
assert:
  rendered_tasks:
    - index: 0
      module: core.user.present
  state_after:
    last_user: bob
`)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatal("ожидали FAIL: базовый users не упомянут в state_after, полная сверка ловит лишний ключ")
	}
}

// TestRun_MixedL0L2Tree — рекурсивный прогон дерева с L0- и L2-кейсами: L0
// исполняются, L2 (маркер stand:/verify:) пропускаются с пометкой Skipped, нет
// краша на strict-декоде L2-кейса. Регресс на mixed-дерево examples/.
func TestRun_MixedL0L2Tree(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")

	// L0-кейс: исполняется обычным L0-пайплайном.
	l0Dir := filepath.Join(scnDir, "tests", "l0")
	if err := os.MkdirAll(l0Dir, 0o755); err != nil {
		t.Fatalf("mkdir l0: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(helloMain), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l0Dir, caseFileName), []byte(`name: l0 pass
fixtures:
  input:
    greeting: hi
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-hello
        content: hi
`), 0o644); err != nil {
		t.Fatalf("write l0 case: %v", err)
	}

	// L2-кейс: несёт stand:/verify: + поля, на которых L0 strict-декод упал бы
	// (description, expect_idempotent). Должен быть распознан как L2 и пропущен.
	l2Dir := filepath.Join(scnDir, "tests", "l2")
	if err := os.MkdirAll(l2Dir, 0o755); err != nil {
		t.Fatalf("mkdir l2: %v", err)
	}
	if err := os.WriteFile(filepath.Join(l2Dir, caseFileName), []byte(`name: l2 stand
description: |
  Этот кейс исполняется на стенде, MVP-harness его не гоняет.
stand:
  driver: docker
  image: ubuntu:24.04
input:
  action: apply
expect_idempotent: true
verify:
  - name: ping
    expect:
      stdout: PONG
`), 0o644); err != nil {
		t.Fatalf("write l2 case: %v", err)
	}

	results, err := Run(context.Background(), root)
	if err != nil {
		t.Fatalf("Run на mixed-дереве не должен падать на L2-кейсе: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ожидали 2 результата (L0 + L2), получили %d", len(results))
	}

	var sawL0Pass, sawL2Skip bool
	for _, r := range results {
		switch {
		case r.Skipped:
			sawL2Skip = true
			if !strings.Contains(r.Case, "l2") {
				t.Errorf("пропущенным ожидался L2-кейс, получили %q", r.Case)
			}
		default:
			sawL0Pass = r.Pass
			if !r.Pass {
				t.Errorf("L0-кейс должен пройти, FAIL: %v", r.Failures)
			}
		}
	}
	if !sawL0Pass {
		t.Fatal("ожидали исполненный (не пропущенный) L0-кейс")
	}
	if !sawL2Skip {
		t.Fatal("ожидали пропущенный L2-кейс")
	}
}

// TestRun_L2OnlyVerifyMarker — кейс лишь с verify: (без stand:) тоже распознаётся
// как L2 и пропускается (любой из маркеров достаточен).
func TestRun_L2OnlyVerifyMarker(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: verify only
verify:
  - name: ping
    expect:
      stdout: PONG
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := Run(context.Background(), file)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 || !results[0].Skipped {
		t.Fatalf("ожидали 1 пропущенный L2-кейс, получили %+v", results)
	}
}

// TestRun_L0WithUnknownField_StillErrors — центральный инвариант L2-skip: L0-кейс
// с unknown-field (опечатка) и БЕЗ маркеров stand:/verify: проходит мягкий
// пред-парс isL2Case как НЕ-L2 и обязан упасть strict-декодом в LoadCase, а не
// тихо проскочить как «не-L2 → strict не сработал». Прогон строго через новый
// путь Run→isL2Case→LoadCase: ошибка проброшена, кейс НЕ помечен Skipped.
func TestRun_L0WithUnknownField_StillErrors(t *testing.T) {
	root := t.TempDir()
	scnDir := filepath.Join(root, "scenario", "create")
	caseDir := filepath.Join(scnDir, "tests", "l0")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scnDir, "main.yml"), []byte(helloMain), 0o644); err != nil {
		t.Fatalf("write main.yml: %v", err)
	}
	// Опечатка верхнего уровня (assertt вместо assert) + нет stand:/verify:.
	if err := os.WriteFile(filepath.Join(caseDir, caseFileName), []byte(`name: l0 typo
fixtures:
  input:
    greeting: hi
assertt:
  rendered_tasks:
    - index: 0
      module: core.file.present
`), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}

	results, err := Run(context.Background(), root)
	if err == nil {
		t.Fatal("ожидали ошибку strict-декода на unknown-field L0-кейса (без stand/verify), получили nil")
	}
	for _, r := range results {
		if r.Skipped {
			t.Fatalf("L0-кейс с опечаткой не должен пропускаться как L2: %q", r.Case)
		}
	}
}

// TestRun_NonMapTopLevel_Errors — case.yml с non-map top-level (скаляр/список):
// мягкий пред-парс isL2Case не может декодировать его в map → ошибка, Run её
// пробрасывает, а не молча скипает кейс.
func TestRun_NonMapTopLevel_Errors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte("- just a list, not a case map\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	results, err := Run(context.Background(), file)
	if err == nil {
		t.Fatal("ожидали ошибку пред-парса на non-map top-level, получили nil")
	}
	for _, r := range results {
		if r.Skipped {
			t.Fatalf("non-map кейс не должен пропускаться: %q", r.Case)
		}
	}
}

func TestLoadCase_RejectsUnknownSection(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: x
fixtures:
  input: {}
assert:
  dispatch:
    - task: 0
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadCase(file); err == nil {
		t.Fatalf("ожидали ошибку strict-decode на assert.dispatch (TODO-секция)")
	}
}

func TestLoadCase_RejectsEmptyAssert(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: x
fixtures:
  input: {}
assert:
  rendered_tasks: []
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := LoadCase(file); err == nil {
		t.Fatalf("ожидали ошибку на пустой assert.rendered_tasks")
	}
}

// vaultMain — scenario с vault:-ref в params для проверки, что fixtures.vault
// feeds render-пайплайн герметично (без поднятия Vault).
const vaultMain = `name: create
input: {}
tasks:
  - name: write secret-derived content
    module: core.file.present
    params:
      path: /tmp/soul-stack-secret
      content: "vault:secret/app/cfg#token"
`

// TestRunCase_VaultRef — fixtures.vault резолвится в params через
// fixture-backed KVReader (герметично, без Vault).
func TestRunCase_VaultRef(t *testing.T) {
	caseDir := writeScenarioTree(t, vaultMain, `name: vault ref
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: abc123
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (vault-ref резолвлен в abc123), получили: %v", results[0].Failures)
	}
}

// celVaultMain — scenario с CEL-функцией vault() (НЕ vault:-ref) в params.
// vault() резолвится keeper-side в CEL-render-фазе через fixtureVault (тот же
// герметичный reader, что для vault:-ref). Закрывает qa-пробел: L0 покрывал
// только vault:-ref, не CEL vault().
const celVaultMain = `name: create
input: {}
tasks:
  - name: write secret via CEL vault()
    module: core.file.present
    params:
      path: /tmp/soul-stack-secret
      content: "${ vault('secret/app/cfg#token') }"
`

// TestRunCase_CELVaultFunc — fixtures.vault резолвится через CEL-функцию vault()
// (#field-форма) детерминированно в реальное значение секрета в L0 (soul-trial).
func TestRunCase_CELVaultFunc(t *testing.T) {
	caseDir := writeScenarioTree(t, celVaultMain, `name: cel vault()
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: abc123
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (CEL vault() резолвлен в abc123), получили: %v", results[0].Failures)
	}
}

// vaultNoLogMain — scenario с vault-ref в params задачи, помеченной no_log:
// при FAIL значение не должно утечь в diff.
const vaultNoLogMain = `name: create
input: {}
tasks:
  - name: write secret-derived content
    module: core.file.present
    no_log: true
    params:
      path: /tmp/soul-stack-secret
      content: "vault:secret/app/cfg#token"
`

// TestRunCase_FailNoLogMasksSecret — задача с no_log: true и FAIL по params:
// diff маскирует значения (печатает только ключи), сырой секрет abc123 в
// Failures не появляется.
func TestRunCase_FailNoLogMasksSecret(t *testing.T) {
	caseDir := writeScenarioTree(t, vaultNoLogMain, `name: vault no_log fail
fixtures:
  vault:
    "secret/app/cfg":
      token: abc123
assert:
  rendered_tasks:
    - index: 0
      module: core.file.present
      params:
        path: /tmp/soul-stack-secret
        content: WRONG
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results[0].Pass {
		t.Fatalf("ожидали FAIL на расхождении content")
	}
	if len(results[0].Failures) == 0 {
		t.Fatalf("ожидали непустой список расхождений")
	}
	for _, f := range results[0].Failures {
		if strings.Contains(f, "abc123") {
			t.Fatalf("секрет abc123 утёк в diff no_log-задачи: %q", f)
		}
	}
}

// TestCompareParams_NoLogMask — unit на маскировку: при noLog=true diff не
// содержит значений, только ключи (и наоборот при noLog=false).
func TestCompareParams_NoLogMask(t *testing.T) {
	got, err := structpb.NewStruct(map[string]any{"content": "abc123"})
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	want := map[string]any{"content": "WRONG"}

	masked := compareParams(0, want, got, true)
	if masked == "" {
		t.Fatalf("ожидали diff (значения расходятся)")
	}
	if strings.Contains(masked, "abc123") || strings.Contains(masked, "WRONG") {
		t.Fatalf("no_log diff не должен содержать значений: %q", masked)
	}
	if !strings.Contains(masked, "content") {
		t.Fatalf("no_log diff должен печатать ключи: %q", masked)
	}

	open := compareParams(0, want, got, false)
	if !strings.Contains(open, "abc123") {
		t.Fatalf("без no_log diff должен показывать значения: %q", open)
	}
}

// writeApplyDestinyTree строит герметичное дерево для L0-кейса с apply:destiny:
//
//	<root>/service.yml                       — declares destiny[] dep
//	<root>/scenario/create/main.yml          — scenario с apply: { destiny: <dst> }
//	<root>/scenario/create/tests/c1/case.yml — кейс с fixtures.soulprint + default_destiny_source
//	<root>/destiny-<dst>/{destiny.yml,tasks/main.yml} — destiny, читающая soulprint.self
//
// Возвращает путь к каталогу кейса (для Run). serviceRootFor(case.yml) == <root>,
// поэтому service.yml и destiny-каталог резолвятся относительно него.
func writeApplyDestinyTree(t *testing.T, dst, mainYML, caseYML, destinyYML, destinyTasks string) string {
	t.Helper()
	root := t.TempDir()
	caseDir := filepath.Join(root, "scenario", "create", "tests", "c1")
	dstTasksDir := filepath.Join(root, "destiny-"+dst, "tasks")
	for _, d := range []string{caseDir, dstTasksDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "service.yml"):                    "name: arch-svc\nstate_schema_version: 1\nstate_schema:\n  type: object\n  properties: {}\ndestiny:\n  - { name: " + dst + ", ref: v1.0.0 }\n",
		filepath.Join(root, "scenario", "create", "main.yml"): mainYML,
		filepath.Join(caseDir, caseFileName):                  caseYML,
		filepath.Join(root, "destiny-"+dst, "destiny.yml"):    destinyYML,
		filepath.Join(dstTasksDir, "main.yml"):                destinyTasks,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return caseDir
}

// TestRunCase_ApplyDestinySelfArch — L0-guard ослабления инварианта (ADR-009/010
// amendment): destiny, рендерящаяся через scenario-обёртку apply:destiny, видит
// инжектированный fixtures.soulprint.self.os.arch (синтетический arm64). Доказывает,
// что self целевого хоста течёт в destiny-проход и в L0 (тот же renderApplyDestiny,
// что в проде) — soulprint.self в destiny-CEL рендерится с fixtures.soulprint.
func TestRunCase_ApplyDestinySelfArch(t *testing.T) {
	caseDir := writeApplyDestinyTree(t, "arch-aware",
		`name: create
tasks:
  - name: apply arch-aware destiny
    apply:
      destiny: arch-aware
      input: {}
`,
		`name: apply destiny self.arch
fixtures:
  default_destiny_source: file://destiny-{name}
  soulprint:
    os:
      family: debian
      arch: arm64
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "install --arch arm64"
`,
		"name: arch-aware\n",
		`- name: fetch by arch
  module: core.exec.run
  params:
    cmd: "install --arch ${ soulprint.self.os.arch }"
`,
	)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("ожидали 1 результат, получили %d", len(results))
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (soulprint.self.os.arch=arm64 в destiny-проходе L0), получили: %v", results[0].Failures)
	}
}

// TestFixtureVault — fixture-vault резолвит vault-ref герметично и нормализует
// logical/relative формы пути.
func TestFixtureVault(t *testing.T) {
	fv := newFixtureVault(map[string]map[string]any{
		"secret/db/cred": {"password": "s3cret"},
	})
	for _, path := range []string{"secret/db/cred", "db/cred", "/secret/db/cred"} {
		got, err := fv.ReadKV(context.Background(), path)
		if err != nil {
			t.Fatalf("ReadKV(%q): %v", path, err)
		}
		if got["password"] != "s3cret" {
			t.Errorf("ReadKV(%q) password = %v", path, got["password"])
		}
	}
	if _, err := fv.ReadKV(context.Background(), "secret/missing"); err == nil {
		t.Fatalf("ожидали ErrVaultKVNotFound на отсутствующем секрете")
	}
}
