package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// countCode — сколько диагностик с данным кодом в наборе.
func countCode(diags []diag.Diagnostic, code string) int {
	n := 0
	for i := range diags {
		if diags[i].Code == code {
			n++
		}
	}
	return n
}

// --- destiny tasks (плоский top-level список) ---

func TestTaskRefs_DuplicateRegister_Destiny(t *testing.T) {
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: rewrite conf (copy-paste, register not renamed)
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis2.conf
    content: y
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !diag.HasErrors(diags) {
		t.Fatal("ожидалась ошибка duplicate_task_address")
	}
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Fatalf("duplicate_task_address count = %d, want 1 (на втором объявлении); diags=%v", got, diags)
	}
	// Диагностика — на ВТОРОМ объявлении (первое «основное»).
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].register" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].register", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownOnChanges_Destiny(t *testing.T) {
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart on typo'd ref
  module: core.service.restarted
  onchanges: [redis_cnf]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference count = %d, want 1; diags=%v", got, diags)
	}
	for _, d := range diags {
		if d.Code == "unknown_register_reference" && d.YAMLPath != "$[1].onchanges[0]" {
			t.Errorf("YAMLPath = %q, want $[1].onchanges[0]", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownOnFail_Destiny(t *testing.T) {
	src := `
- name: migrate
  module: core.exec.run
  register: migrate_db
  params:
    cmd: migrate
- name: rescue
  module: core.exec.run
  onfail: [migrate_dbb]
  params:
    cmd: rollback
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference (onfail) count = %d, want 1; diags=%v", got, diags)
	}
}

func TestTaskRefs_UnknownRequire_Destiny(t *testing.T) {
	src := `
- name: prepare
  module: core.exec.run
  register: prep
  params:
    cmd: prepare
- name: act
  module: core.exec.run
  require: [prepp]
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference (require) count = %d, want 1; diags=%v", got, diags)
	}
}

func TestTaskRefs_RequireAll_NotFlagged(t *testing.T) {
	// require: "all" — скаляр, не register-список; не должен ловиться cross-ref.
	src := `
- name: prepare
  module: core.exec.run
  register: prep
  params:
    cmd: prepare
- name: act
  module: core.exec.run
  require: all
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("require: all не должен давать unknown_register_reference; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_Valid_Destiny(t *testing.T) {
	// Уникальные register + существующие ссылки + CEL-обёртка в onchanges → OK.
	src := `
- name: write conf
  module: core.file.rendered
  register: redis_conf
  params:
    path: /etc/redis.conf
    template: redis.conf.tmpl
- name: harden
  module: core.file.present
  register: redis_hardening
  params:
    path: /etc/redis-hardening
    content: x
- name: restart
  module: core.service.restarted
  onchanges: [redis_conf, redis_hardening]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address") + countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("валидный план не должен давать cross-ref ошибок; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_CELRef_NotFlagged(t *testing.T) {
	// CEL-обёрнутый элемент в onchanges (динамический резолв) — статически
	// пропускается, не ловится как unknown.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart
  module: core.service.restarted
  onchanges: ["${ vars.dynamic_ref }"]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("CEL-обёртка в onchanges не должна ловиться; got=%d, diags=%v", got, diags)
	}
}

func TestTaskRefs_BlockNested_Destiny(t *testing.T) {
	// register внутри block виден из onchanges верхнего уровня (плоское
	// пространство имён плана), а дубль register между block и top-level — ошибка.
	src := `
- name: group
  block:
    - name: inner write
      module: core.file.present
      register: inner_conf
      params:
        path: /etc/inner
        content: x
- name: restart references inner block register
  module: core.service.restarted
  onchanges: [inner_conf]
  params:
    name: redis
- name: duplicate of inner block register at top-level
  module: core.file.present
  register: inner_conf
  params:
    path: /etc/other
    content: y
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	// onchanges:[inner_conf] резолвится (block register виден) → no unknown ref.
	if got := countCode(diags, "unknown_register_reference"); got != 0 {
		t.Errorf("block register должен быть виден из top-level onchanges; got unknown=%d, diags=%v", got, diags)
	}
	// top-level register inner_conf дублирует block-register → ошибка.
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Errorf("дубль register между block и top-level должен ловиться; got=%d, diags=%v", got, diags)
	}
}

// --- T2: уникальность адресного пространства register ∪ id (per-file) ---

func TestTaskAddress_DuplicateID_Destiny(t *testing.T) {
	// Два id с одним значением → дубль адреса подписки.
	src := `
- name: reload sysctl
  id: tuned
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: reload again
  id: tuned
  module: core.sysctl.present
  params:
    name: vm.dirty_ratio
    value: "20"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("дубль id должен ловиться; duplicate_task_address count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].id" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].id", d.YAMLPath)
		}
	}
}

func TestTaskAddress_IDCollidesRegister_Destiny(t *testing.T) {
	// id одной задачи == register другой → пересечение в едином пространстве.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: reload sysctl
  id: redis_conf
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("пересечение register/id должно ловиться; count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].id" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].id", d.YAMLPath)
		}
	}
}

func TestTaskAddress_RegisterCollidesID_Destiny(t *testing.T) {
	// Обратный порядок: id раньше register с тем же именем. Диагностика — на
	// втором объявлении (register).
	src := `
- name: reload sysctl
  id: shared_name
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: write conf
  module: core.file.present
  register: shared_name
  params:
    path: /etc/redis.conf
    content: x
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		dump(t, diags)
		t.Fatalf("пересечение id/register должно ловиться; count = %d, want 1", got)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$[1].register" {
			t.Errorf("duplicate_task_address YAMLPath = %q, want $[1].register", d.YAMLPath)
		}
	}
}

func TestTaskAddress_UniqueIDAndRegister_Destiny(t *testing.T) {
	// Уникальные register + id → OK.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: reload sysctl
  id: sysctl_reloaded
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 0 {
		dump(t, diags)
		t.Fatalf("уникальные register+id не должны давать дубль; count = %d, want 0", got)
	}
}

func TestTaskAddress_IDNotResolvableInRequisites_Destiny(t *testing.T) {
	// id адресует подписку, но НЕ создаёт register.<name>: ссылка onchanges на
	// id-задачу — unknown_register_reference (id не равноправен register в
	// cross-ref, хоть и делит пространство уникальности).
	src := `
- name: reload sysctl
  id: sysctl_reloaded
  module: core.sysctl.present
  params:
    name: vm.swappiness
    value: "10"
- name: restart referencing an id (not a register)
  module: core.service.restarted
  onchanges: [sysctl_reloaded]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		dump(t, diags)
		t.Fatalf("ссылка onchanges на id-задачу должна быть unknown_register_reference; count = %d, want 1", got)
	}
	// При этом дубля адреса тут нет (id и register-ссылка — разные имена).
	if got := countCode(diags, "duplicate_task_address"); got != 0 {
		t.Errorf("неожиданный duplicate_task_address; count = %d, want 0", got)
	}
}

// --- scenario (main.yml с обёрткой) ---

func TestTaskRefs_DuplicateRegister_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: a
    module: core.exec.run
    register: probe
    params:
      cmd: echo a
  - name: b
    module: core.exec.run
    register: probe
    params:
      cmd: echo b
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address"); got != 1 {
		t.Fatalf("duplicate_task_address count = %d, want 1; diags=%v", got, diags)
	}
	for _, d := range diags {
		if d.Code == "duplicate_task_address" && d.YAMLPath != "$.tasks[1].register" {
			t.Errorf("YAMLPath = %q, want $.tasks[1].register", d.YAMLPath)
		}
	}
}

func TestTaskRefs_UnknownReference_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: probe
    module: core.exec.run
    register: redis_role
    params:
      cmd: redis-cli role
  - name: restart
    module: core.service.restarted
    onchanges: [redis_rolee]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 1 {
		t.Fatalf("unknown_register_reference count = %d, want 1; diags=%v", got, diags)
	}
}

// --- O6: type-валидация onchanges/onfail/require ---

func TestTaskTypes_OnChangesScalar_Rejected(t *testing.T) {
	// Скаляр вместо списка — до O6 проходил молча (cross-ref смотрит только
	// sequence). Теперь type_mismatch.
	src := `
- name: write conf
  module: core.file.present
  register: redis_conf
  params:
    path: /etc/redis.conf
    content: x
- name: restart
  module: core.service.restarted
  onchanges: redis_conf
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].onchanges") {
		dump(t, diags)
		t.Fatalf("ожидался type_mismatch на $[1].onchanges (скаляр вместо списка)")
	}
}

func TestTaskTypes_OnFailScalar_Rejected(t *testing.T) {
	src := `
- name: migrate
  module: core.exec.run
  register: migrate_db
  params:
    cmd: migrate
- name: rescue
  module: core.exec.run
  onfail: migrate_db
  params:
    cmd: rollback
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].onfail") {
		dump(t, diags)
		t.Fatalf("ожидался type_mismatch на $[1].onfail (скаляр вместо списка)")
	}
}

func TestTaskTypes_RequireAll_Accepted(t *testing.T) {
	// require: all — единственная легитимная скалярная форма.
	src := `
- name: act
  module: core.exec.run
  require: all
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "type_mismatch") != 0 {
		dump(t, diags)
		t.Fatalf("require: all не должен давать type_mismatch")
	}
}

func TestTaskTypes_RequireScalarOther_Rejected(t *testing.T) {
	// require: <не-all скаляр> — ошибка (должен быть список или "all").
	src := `
- name: prep
  module: core.exec.run
  register: prep
  params:
    cmd: prep
- name: act
  module: core.exec.run
  require: prep
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[1].require") {
		dump(t, diags)
		t.Fatalf("ожидался type_mismatch на $[1].require (скаляр != all)")
	}
}

func TestTaskTypes_RequireList_Accepted(t *testing.T) {
	src := `
- name: prep
  module: core.exec.run
  register: prep
  params:
    cmd: prep
- name: act
  module: core.exec.run
  require: [prep]
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "type_mismatch") != 0 {
		dump(t, diags)
		t.Fatalf("require: [prep] не должен давать type_mismatch")
	}
}

func TestTaskTypes_OnChangesNonStringElem_Rejected(t *testing.T) {
	// Элемент-int в списке onchanges — type_mismatch на элементе.
	src := `
- name: restart
  module: core.service.restarted
  onchanges: [42]
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "type_mismatch", "$[0].onchanges[0]") {
		dump(t, diags)
		t.Fatalf("ожидался type_mismatch на $[0].onchanges[0] (int-элемент)")
	}
}

// --- O3: CEL cross-ref register в предикатах ---

func TestTaskRefs_CELWhenUnknown_Destiny(t *testing.T) {
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: redis-cli role
- name: restart
  module: core.service.restarted
  when: register.redis_rolee.changed
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if !hasCodeAt(diags, "unknown_register_reference", "$[1].when") {
		dump(t, diags)
		t.Fatalf("ожидался unknown_register_reference на $[1].when (опечатка register-имени в CEL)")
	}
}

func TestTaskRefs_CELWhenKnown_Destiny(t *testing.T) {
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: redis-cli role
- name: restart
  module: core.service.restarted
  when: register.redis_role.changed && register.redis_role.stdout == 'ok'
  params:
    name: redis
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("существующее register-имя в when не должно ловиться")
	}
}

func TestTaskRefs_CELSelf_NotFlagged(t *testing.T) {
	// register.self — текущая задача, не флагается (форвард — она же).
	src := `
- name: probe
  module: core.cmd.shell
  changed_when: false
  failed_when: register.self.stdout != 'up'
  retry:
    count: 3
    delay: 5s
    until: register.self.stdout == 'up'
  params:
    cmd: redis-cli ping
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("register.self не должен ловиться cross-ref-ом")
	}
}

func TestTaskRefs_CELStringLiteral_NotFalsePositive(t *testing.T) {
	// register.foo внутри строкового литерала CEL — данные, не идентификатор;
	// не должен давать ложное unknown_register_reference.
	src := `
- name: log
  module: core.exec.run
  failed_when: "register.self.stdout == 'register.ghost not found'"
  params:
    cmd: check
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("register.<x> внутри строкового литерала не должен ловиться")
	}
}

func TestTaskRefs_CELRetryUntilLoopWhere_Unknown(t *testing.T) {
	// retry.until, loop.when, where — все предикатные позиции покрыты.
	src := `
- name: act
  module: core.exec.run
  where: register.missing_a.changed
  loop:
    items: ${ input.xs }
    as: x
    when: register.missing_b.ok
  retry:
    count: 2
    delay: 1s
    until: register.missing_c.done
  params:
    cmd: act ${ x }
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "unknown_register_reference"); got != 3 {
		dump(t, diags)
		t.Fatalf("ожидалось 3 unknown_register_reference (where/loop.when/retry.until); got=%d", got)
	}
}

func TestTaskRefs_CELDynamicAccess_NotFlagged(t *testing.T) {
	// register["..."] — динамический доступ, формой не покрывается, не ловится.
	src := `
- name: probe
  module: core.exec.run
  register: redis_role
  params:
    cmd: probe
- name: act
  module: core.exec.run
  when: register["redis_role"].changed
  params:
    cmd: act
`
	_, diags, _ := LoadDestinyTasksFromBytes("tasks/main.yml", []byte(src), ValidateOptions{})
	if countCode(diags, "unknown_register_reference") != 0 {
		dump(t, diags)
		t.Fatalf("динамический register[...] не должен давать unknown_register_reference")
	}
}

func TestTaskRefs_Valid_Scenario(t *testing.T) {
	src := `
name: create
tasks:
  - name: probe
    module: core.exec.run
    register: redis_role
    params:
      cmd: redis-cli role
  - name: restart
    module: core.service.restarted
    onchanges: [redis_role]
    params:
      name: redis
`
	_, _, diags, _ := LoadScenarioManifestFromBytes("scenario/create/main.yml", []byte(src), ValidateOptions{})
	if got := countCode(diags, "duplicate_task_address") + countCode(diags, "unknown_register_reference"); got != 0 {
		t.Fatalf("валидный scenario не должен давать cross-ref ошибок; got=%d, diags=%v", got, diags)
	}
}
