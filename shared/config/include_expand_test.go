package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// mapResolver — IncludeResolver поверх in-memory map имя→содержимое. display-путь
// = имя файла (для тестов этого достаточно: разные имена = разные источники).
func mapResolver(files map[string]string) IncludeResolver {
	return func(name string) ([]byte, string, error) {
		data, ok := files[name]
		if !ok {
			return nil, "", fmt.Errorf("файл %q не найден", name)
		}
		return []byte(data), name, nil
	}
}

func moduleNames(tasks []Task) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.Module != nil {
			out = append(out, t.Module.Module)
		} else {
			out = append(out, "<non-module>")
		}
	}
	return out
}

func TestExpandIncludes_FlatSplice(t *testing.T) {
	root := []Task{
		{Module: &ModuleTask{Module: "core.pkg.installed", Params: map[string]any{"name": "x"}}},
		{Include: &IncludeTask{Include: "sub.yml"}},
		{Module: &ModuleTask{Module: "core.service.running", Params: map[string]any{"name": "x"}}},
	}
	files := map[string]string{
		// Лист-модули несут валидные required-params: после тиража H2 все core
		// в coremanifest-реестре, и ExpandIncludes прогоняет params-валидацию.
		// Тест проверяет include-splice, params минимальны но валидны.
		"sub.yml": `
- name: a
  module: core.git.cloned
  params: { repo: "https://x", path: "/y" }
- name: b
  module: core.cmd.shell
  params: { cmd: "true" }
`,
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []string{"core.pkg.installed", "core.git.cloned", "core.cmd.shell", "core.service.running"}
	if names := moduleNames(got); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("modules = %v, want %v", names, want)
	}
}

func TestExpandIncludes_Nested(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml": "- include: b.yml\n",
		"b.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
		t.Fatalf("modules = %v, want [core.cmd.shell]", names)
	}
}

func TestExpandIncludes_DirectCycle(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "self.yml"}}}
	files := map[string]string{
		"self.yml": "- include: self.yml\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "include_cycle") {
		t.Fatalf("ожидался include_cycle, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_MutualCycle(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml": "- include: b.yml\n",
		"b.yml": "- include: a.yml\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !hasCode(diags, "include_cycle") {
		t.Fatalf("ожидался include_cycle для a→b→a, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ResolveError(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "missing.yml"}}}
	_, diags := ExpandIncludes(root, mapResolver(map[string]string{}))
	if !hasCode(diags, "include_resolve_failed") {
		t.Fatalf("ожидался include_resolve_failed, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ParseErrorInSub(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "bad.yml"}}}
	files := map[string]string{
		// Задача без дискриминатора — schema-error парсера задач.
		"bad.yml": "- name: orphan\n  params: {}\n",
	}
	_, diags := ExpandIncludes(root, mapResolver(files))
	if !diag.HasErrors(diags) {
		t.Fatalf("ожидалась диагностика парсинга подключённого файла, diagnostics: %v", diags)
	}
}

func TestExpandIncludes_ModifierUnsupported(t *testing.T) {
	cases := map[string]Task{
		"vars":     {Include: &IncludeTask{Include: "x.yml"}, Vars: map[string]any{"a": 1}},
		"loop":     {Include: &IncludeTask{Include: "x.yml"}, Loop: &LoopSpec{Items: "${ input.users }"}},
		"when":     {Include: &IncludeTask{Include: "x.yml"}, When: "input.go"},
		"on":       {Include: &IncludeTask{Include: "x.yml"}, On: []any{"coven"}},
		"serial":   {Include: &IncludeTask{Include: "x.yml"}, Serial: 2},
		"run_once": {Include: &IncludeTask{Include: "x.yml"}, RunOnce: true},
		"register": {Include: &IncludeTask{Include: "x.yml"}, Register: "r"},
	}
	files := map[string]string{"x.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	for name, task := range cases {
		t.Run(name, func(t *testing.T) {
			_, diags := ExpandIncludes([]Task{task}, mapResolver(files))
			if !hasCode(diags, "include_modifier_unsupported") {
				t.Fatalf("ожидался include_modifier_unsupported, diagnostics: %v", diags)
			}
		})
	}
}

func TestExpandIncludes_DepthExceeded(t *testing.T) {
	// Линейная НЕциклическая цепочка include глубже потолка: каждый файл
	// `level-<n>.yml` подключает `level-<n+1>.yml`, последний — лист-модуль.
	// Резолвер генерирует содержимое на лету (без 33 фикстур); все display-пути
	// различны, так что cycle-detection не сработает — упрётся именно в depth.
	depth := maxIncludeDepth + 1
	resolve := func(name string) ([]byte, string, error) {
		var n int
		if _, err := fmt.Sscanf(name, "level-%d.yml", &n); err != nil {
			return nil, "", fmt.Errorf("неожиданное имя %q", name)
		}
		if n >= depth {
			return []byte("- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"), name, nil
		}
		body := fmt.Sprintf("- include: level-%d.yml\n", n+1)
		return []byte(body), name, nil
	}
	root := []Task{{Include: &IncludeTask{Include: "level-0.yml"}}}
	_, diags := ExpandIncludes(root, resolve)
	if !hasCode(diags, "include_depth_exceeded") {
		t.Fatalf("ожидался include_depth_exceeded для цепочки глубиной %d, diagnostics: %v", depth, diags)
	}
}

// levelChainResolver генерирует линейную НЕциклическую цепочку include
// `level-<n>.yml` → `level-<n+1>.yml` на лету (без N фикстур). Файл
// `level-<leafLevel>.yml` — лист-модуль (конец цепочки), всё что глубже не
// генерируется. Все display-пути различны — cycle-detection не сработает, в
// потолок упирается только depth-проверка.
func levelChainResolver(leafLevel int) IncludeResolver {
	return func(name string) ([]byte, string, error) {
		var n int
		if _, err := fmt.Sscanf(name, "level-%d.yml", &n); err != nil {
			return nil, "", fmt.Errorf("неожиданное имя %q", name)
		}
		if n >= leafLevel {
			return []byte("- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"), name, nil
		}
		return []byte(fmt.Sprintf("- include: level-%d.yml\n", n+1)), name, nil
	}
}

// TestExpandIncludes_DepthBoundary стережёт off-by-one в `len(stack) >=
// maxIncludeDepth`. Самая глубокая допустимая цепочка должна пройти, на единицу
// глубже — упереться в include_depth_exceeded.
//
// expandOne для `level-<n>.yml` вызывается с stack длиной ровно n (root-include
// идёт на nil-stack: level-0 при len 0, level-1 при len 1, …). Depth-проверка
// `len(stack) >= maxIncludeDepth` стреляет на первом include, у которого stack
// дорос до maxIncludeDepth, т.е. на `level-<maxIncludeDepth>.yml`. Значит самый
// глубокий лист, который ещё резолвится, — `level-<maxIncludeDepth-1>.yml`
// (раскрывается при len(stack) == maxIncludeDepth-1 < maxIncludeDepth), а
// `level-<maxIncludeDepth>.yml` — первый отвергаемый. Сдвиг проверки на `>`
// (off-by-one) пропустил бы maxIncludeDepth — этот тест поймает.
func TestExpandIncludes_DepthBoundary(t *testing.T) {
	cases := []struct {
		name      string
		leafLevel int
		wantExc   bool
	}{
		{"deepest-allowed", maxIncludeDepth - 1, false},
		{"one-over", maxIncludeDepth, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := []Task{{Include: &IncludeTask{Include: "level-0.yml"}}}
			got, diags := ExpandIncludes(root, levelChainResolver(tc.leafLevel))
			gotExc := hasCode(diags, "include_depth_exceeded")
			if gotExc != tc.wantExc {
				t.Fatalf("include_depth_exceeded = %v, want %v (leafLevel=%d), diagnostics: %v",
					gotExc, tc.wantExc, tc.leafLevel, diags)
			}
			if !tc.wantExc {
				// На границе цепочка раскрывается до единственного листа.
				if diag.HasErrors(diags) {
					t.Fatalf("на границе depth=%d unexpected diagnostics: %v", tc.leafLevel, diags)
				}
				if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
					t.Fatalf("modules = %v, want [core.cmd.shell]", names)
				}
			}
		})
	}
}

// TestExpandIncludes_Diamond фиксирует, что один и тот же файл, подключённый в
// ДВУХ независимых ветках (a→b→leaf и a→c→leaf), — НЕ цикл: cycle-detection
// смотрит активную цепочку (visited-stack), а не глобальный seen-set. leaf
// раскрывается ДВАЖДЫ. Регресс-страховка на случай рефактора cycle-detection в
// глобальный set, который молча схлопнул бы diamond.
func TestExpandIncludes_Diamond(t *testing.T) {
	root := []Task{{Include: &IncludeTask{Include: "a.yml"}}}
	files := map[string]string{
		"a.yml":    "- include: b.yml\n- include: c.yml\n",
		"b.yml":    "- include: leaf.yml\n",
		"c.yml":    "- include: leaf.yml\n",
		"leaf.yml": "- name: leaf\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n",
	}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if diag.HasErrors(diags) {
		t.Fatalf("diamond не должен давать ошибок (это не цикл), diagnostics: %v", diags)
	}
	names := moduleNames(got)
	count := 0
	for _, n := range names {
		if n == "core.cmd.shell" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("leaf-задача встречается %d раз(а), want 2 (diamond раскрывается дважды); modules = %v", count, names)
	}
}

// TestExpandIncludes_NameAllowed — позитивный whitelist: include-задача с
// заполненным `name:` (и больше ничем) НЕ должна давать
// include_modifier_unsupported и должна нормально раскрыться. Стережёт регрессию
// «случайно добавили Name в запрещённые поля» (Name намеренно вне
// includeModifierReason: имя include-узла — легитимный label, не scope/контроль).
func TestExpandIncludes_NameAllowed(t *testing.T) {
	root := []Task{{Name: "подключаю install", Include: &IncludeTask{Include: "x.yml"}}}
	files := map[string]string{"x.yml": "- name: a\n  module: core.cmd.shell\n  params: { cmd: 'true' }\n"}
	got, diags := ExpandIncludes(root, mapResolver(files))
	if hasCode(diags, "include_modifier_unsupported") {
		t.Fatalf("name: на include-задаче не должен отвергаться, diagnostics: %v", diags)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if names := moduleNames(got); len(names) != 1 || names[0] != "core.cmd.shell" {
		t.Fatalf("modules = %v, want [core.cmd.shell]", names)
	}
}

// --- T2: уникальность адресного пространства register ∪ id через include ---

// TestExpandIncludes_DuplicateAddressAcrossInclude — register основного файла и
// register/id подключённого совпали: per-file validateTaskRefs этот дубль НЕ
// видит (разные scope), ловит только пост-flatten validateFlatTaskAddresses.
func TestExpandIncludes_DuplicateAddressAcrossInclude(t *testing.T) {
	cases := map[string]struct {
		root string // адрес на root-задаче (register/id)
		sub  string // адрес на задаче подключённого файла
	}{
		"register-vs-register": {
			root: "register: shared",
			sub:  "register: shared",
		},
		"register-vs-id": {
			root: "register: shared",
			sub:  "id: shared",
		},
		"id-vs-id": {
			root: "id: shared",
			sub:  "id: shared",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rootSrc := fmt.Sprintf(`
- name: root task
  module: core.file.present
  %s
  params: { path: /etc/x, content: a }
- include: sub.yml
`, tc.root)
			subSrc := fmt.Sprintf(`
- name: sub task
  module: core.sysctl.present
  %s
  params: { name: vm.swappiness, value: "10" }
`, tc.sub)
			rootTasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(rootSrc), ValidateOptions{})
			if diag.HasErrors(diags) {
				t.Fatalf("root-файл не должен иметь per-file ошибок (дубль cross-file): %v", diags)
			}
			files := map[string]string{"sub.yml": subSrc}
			_, idiags := ExpandIncludes(rootTasks, mapResolver(files))
			if !hasCode(idiags, "duplicate_task_address") {
				t.Fatalf("ожидался duplicate_task_address через include, diagnostics: %v", idiags)
			}
		})
	}
}

// TestExpandIncludes_UniqueAddressAcrossInclude — разные адреса в основном и
// подключённом файлах раскрываются без дубля.
func TestExpandIncludes_UniqueAddressAcrossInclude(t *testing.T) {
	rootSrc := `
- name: root task
  module: core.file.present
  register: root_conf
  params: { path: /etc/x, content: a }
- include: sub.yml
`
	files := map[string]string{
		"sub.yml": `
- name: sub task
  module: core.sysctl.present
  id: sub_tuned
  params: { name: vm.swappiness, value: "10" }
`,
	}
	rootTasks, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(rootSrc), ValidateOptions{})
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected per-file diagnostics: %v", diags)
	}
	_, idiags := ExpandIncludes(rootTasks, mapResolver(files))
	if diag.HasErrors(idiags) {
		t.Fatalf("уникальные адреса через include не должны давать ошибок: %v", idiags)
	}
}

func TestExpandIncludes_NoInclude(t *testing.T) {
	root := []Task{{Module: &ModuleTask{Module: "core.cmd.shell", Params: map[string]any{"cmd": "true"}}}}
	got, diags := ExpandIncludes(root, mapResolver(map[string]string{}))
	if diag.HasErrors(diags) {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (passthrough без include)", len(got))
	}
}
