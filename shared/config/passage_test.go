package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// loadTasks парсит inline scenario-YAML в []Task (после общей валидации
// config-а), падая на любой error-диагностике. Фикстуры guard-тестов — inline, а
// НЕ загрузка из examples/**: examples — WIP-зона пользователя (uncommitted
// правки), guard-инвариант на silent-wrong-target обязан быть детерминирован и не
// зависеть от состояния примеров. Фикстуры ниже — синтетические, воспроизводящие
// 3-Passage re-probe-идиому (исторически redis-cluster restart).
func loadTasks(t *testing.T, src string) []Task {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("scenario diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	return m.Tasks
}

func stratify(t *testing.T, src string) Passage {
	t.Helper()
	p, err := Stratify(loadTasks(t, src))
	if err != nil {
		t.Fatalf("Stratify: %v", err)
	}
	return p
}

// --- синтетические фикстуры 3-Passage re-probe (исторически redis-cluster restart) ---

const redisUpdateACL = `
name: update_acl
input:
  changes:
    type: object
    required: true
    properties: {}
    additional_properties:
      type: object
      properties:
        acl: { type: string }
      required: [acl]
state_changes:
  modifies: [redis_users.*.acl]
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    on: ["${ incarnation.name }"]
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Diff and apply ACL changes on the current master
    on: ["${ incarnation.name }"]
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action:  update_acls
        changes: "${ input.changes }"
`

const redisAddUser = `
name: add_user
input:
  user: { type: string, required: true }
  acl:  { type: string, required: true }
  state: { type: string, required: true, enum: [on, off] }
state_changes:
  appends: [redis_users]
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    on: ["${ incarnation.name }"]
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Create the user on the current master
    on: ["${ incarnation.name }"]
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action: ensure_user
        user:   "${ input.user }"
  - name: Wait until the user is replicated to all replicas
    module: core.exec.run
    on: ["${ incarnation.name }"]
    where: register.redis_role.stdout == 'slave'
    changed_when: false
    retry:
      count: 15
      delay: 2s
      until: contains(register.self.stdout, input.user)
    failed_when: "!contains(register.self.stdout, input.user)"
    params:
      cmd: redis-cli
      args: ["ACL", "GETUSER", "${ input.user }"]
`

const redisRestart = `
name: restart
input:
  reason: { type: string, default: "manual restart" }
state_changes: {}
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    on: ["${ incarnation.name }"]
    register: redis_role
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Rolling-restart replicas one at a time
    on: ["${ incarnation.name }"]
    where: register.redis_role.stdout == 'slave'
    serial: 1
    block:
      - name: Restart redis-server
        module: core.service.restarted
        params:
          name: redis-server
      - name: Wait until replica is healthy again
        module: core.exec.run
        on: ["${ incarnation.name }"]
        changed_when: false
        retry:
          count: 12
          delay: 5s
          until: contains(register.self.stdout, 'master_link_status:up')
        failed_when: "!contains(register.self.stdout, 'master_link_status:up')"
        params:
          cmd: redis-cli
          args: ["INFO", "replication"]
  - name: Failover and restart the current master
    on: ["${ incarnation.name }"]
    where: register.redis_role.stdout == 'master'
    run_once: true
    apply:
      destiny: redis
      input:
        action: failover_and_restart
  - name: Re-detect redis role after failover
    module: core.cmd.shell
    on: ["${ incarnation.name }"]
    register: redis_role_after
    changed_when: false
    params:
      cmd: "redis-cli role | head -1"
  - name: Restart the former master (now a replica)
    on: ["${ incarnation.name }"]
    where: register.redis_role_after.stdout == 'slave' && register.redis_role.stdout == 'master'
    apply:
      destiny: redis
      input:
        action: restart
`

// TestStratify_RedisUpdateACL — реальный 2-Passage сценарий: probe (p0) → задача с
// where: register.redis_role (p1). Один probe-барьер.
func TestStratify_RedisUpdateACL(t *testing.T) {
	p := stratify(t, redisUpdateACL)
	if p.Count != 2 {
		t.Fatalf("update_acl: Count = %d, want 2", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("probe task #0 passage = %d, want 0", p.TaskPassage[0])
	}
	if p.TaskPassage[1] != 1 {
		t.Errorf("where-task #1 passage = %d, want 1 (СТРОГО после probe)", p.TaskPassage[1])
	}
}

// TestStratify_RedisAddUser — 2-Passage: probe (p0) → две задачи на p1 (создание
// на master + health-gate на slave, обе читают register.redis_role). Health-gate
// читает register.self в until/failed_when — это НЕ cross-task ребро, passage он
// не повышает.
func TestStratify_RedisAddUser(t *testing.T) {
	p := stratify(t, redisAddUser)
	if p.Count != 2 {
		t.Fatalf("add_user: Count = %d, want 2", p.Count)
	}
	want := []int{0, 1, 1}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("add_user task #%d passage = %d, want %d", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_RedisRestart — главный 3-Passage кейс (ADR-056 §«restart re-probe»):
// probe(p0) → where-задачи(p1) → re-probe(p1, перезамер ПОСЛЕ failover) →
// where: register.redis_role_after && register.redis_role (p2). Две probe-границы
// → три Passage. Это и есть «probe → действие → re-probe → действие».
func TestStratify_RedisRestart(t *testing.T) {
	p := stratify(t, redisRestart)
	if p.Count != 3 {
		t.Fatalf("restart: Count = %d, want 3 (две probe-границы)", p.Count)
	}
	// #0 probe; #1 rolling-restart(where slave); #2 failover(where master);
	// #3 re-probe; #4 restart former master(where redis_role_after && redis_role).
	want := []int{0, 1, 1, 1, 2}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("restart task #%d passage = %d, want %d", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_InvariantConsumerStrictlyAfterProbe — ГЛАВНЫЙ guard-инвариант
// (security-критично, ADR-056 silent-wrong-target). Для КАЖДОЙ задачи, читающей
// register.X, её passage ОБЯЗАН быть СТРОГО больше passage probe, эмитящего X.
// Регресс, отправивший потребителя в <= passage probe, означает резолв where: по
// пустому/устаревшему register → разрушительная операция на нерезолвнутом таргете
// МОЛЧА. Проверяется на всех синтетических фикстурах через прямой обход графа.
func TestStratify_InvariantConsumerStrictlyAfterProbe(t *testing.T) {
	fixtures := map[string]string{
		"update_acl": redisUpdateACL,
		"add_user":   redisAddUser,
		"restart":    redisRestart,
	}
	for name, src := range fixtures {
		t.Run(name, func(t *testing.T) {
			tasks := loadTasks(t, src)
			p, err := Stratify(tasks)
			if err != nil {
				t.Fatalf("Stratify: %v", err)
			}
			emitter := emitterIndex(tasks)
			for i := range tasks {
				for _, x := range taskRegisterReads(&tasks[i]) {
					src, ok := emitter[x]
					if !ok {
						t.Fatalf("task #%d reads register %q with no emitter — fixture broken", i, x)
					}
					if p.TaskPassage[i] <= p.TaskPassage[src] {
						t.Errorf("INVARIANT VIOLATED: task #%d (passage %d) reads register %q emitted by task #%d (passage %d) — consumer must be STRICTLY after probe, else silent-wrong-target",
							i, p.TaskPassage[i], x, src, p.TaskPassage[src])
					}
				}
			}
		})
	}
}

// TestStratify_BackwardCompatNoRegister — сценарий БЕЗ cross-task register:
// каждая задача читает только input/own register → все passage 0, Count==1
// (fast-path, идентично текущему up-front render).
func TestStratify_BackwardCompatNoRegister(t *testing.T) {
	const src = `
name: create
input:
  pkg: { type: string, required: true }
tasks:
  - name: Install package
    module: core.pkg.installed
    params:
      name: "${ input.pkg }"
  - name: Start service
    module: core.service.running
    params:
      name: redis-server
  - name: Probe without consumers
    module: core.cmd.shell
    register: anything
    changed_when: false
    failed_when: "register.self.rc != 0"
    params:
      cmd: "true"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("no-register scenario: Count = %d, want 1 (fast-path)", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (no cross-task register)", i, pass)
		}
	}
}

// TestStratify_Cycle — register-зависимость по кругу → явная ошибка StratifyCycle,
// НЕ молчаливая стратификация. probe_a читает register.b, probe_b читает register.a.
func TestStratify_Cycle(t *testing.T) {
	const src = `
name: cyclic
tasks:
  - name: Probe A reads B
    module: core.cmd.shell
    register: a
    changed_when: false
    where: register.b.stdout == 'x'
    params: { cmd: "true" }
  - name: Probe B reads A
    module: core.cmd.shell
    register: b
    changed_when: false
    where: register.a.stdout == 'y'
    params: { cmd: "true" }
`
	_, err := Stratify(loadTasks(t, src))
	if err == nil {
		t.Fatal("Stratify: expected cycle error, got nil")
	}
	var se *StratifyError
	if !errors.As(err, &se) || se.Code != StratifyCycle {
		t.Fatalf("Stratify: error code = %v, want %s", err, StratifyCycle)
	}
}

// TestStratify_UnknownRegister — задача читает register.X, но НИ ОДНА задача его
// не эмитит. Двойной рубеж:
//
//  1. ПОДТВЕРЖДЕНИЕ существующего валидатора: config-парс УЖЕ поднимает
//     unknown_register_reference (cross-ref-фаза task_refs.go) — первый рубеж.
//  2. Страховка render-слоя: даже если задачи дойдут до Stratify (например,
//     валидация отключена/пропущена), стратификация падает явно
//     StratifyUnknownRegister, а не идёт по неполному графу → silent-wrong-target.
func TestStratify_UnknownRegister(t *testing.T) {
	const src = `
name: dangling
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "true" }
  - name: Act on a register nobody emits
    on: ["${ incarnation.name }"]
    where: register.ghost_role.stdout == 'master'
    apply:
      destiny: redis
      input: { action: noop }
`
	// Рубеж 1: config-валидатор ловит висячую ссылку на парсе.
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	foundDiag := false
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			foundDiag = true
		}
	}
	if !foundDiag {
		t.Error("existing config-validator did NOT raise unknown_register_reference — render-side Stratify is now the only guard")
	}

	// Рубеж 2: Stratify падает явно, не молча.
	_, serr := Stratify(m.Tasks)
	if serr == nil {
		t.Fatal("Stratify: expected unknown-register error, got nil")
	}
	var se *StratifyError
	if !errors.As(serr, &se) || se.Code != StratifyUnknownRegister {
		t.Fatalf("Stratify: error code = %v, want %s", serr, StratifyUnknownRegister)
	}
}

// TestStratify_RegisterInParamsAndInput — cross-task register, протянутый через
// ${ … } в params: и apply:input: (не только where:), тоже двигает passage. Ловит
// регресс, где стратификация смотрит ТОЛЬКО where: и пропускает register в данных
// следующей задачи (тот тоже требует probe-барьер до render-а).
func TestStratify_RegisterInParamsAndInput(t *testing.T) {
	const src = `
name: chain
tasks:
  - name: Probe value
    module: core.cmd.shell
    register: probe
    changed_when: false
    params: { cmd: "true" }
  - name: Use register in params
    module: core.exec.run
    params:
      cmd: echo
      args: ["${ register.probe.stdout }"]
  - name: Use register in apply input
    apply:
      destiny: redis
      input:
        seed: "${ register.probe.stdout }"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 || p.TaskPassage[2] != 1 {
		t.Fatalf("passages = %v, want [0 1 1] (register в params/input двигает passage)", p.TaskPassage)
	}
}

// TestStratify_SelfRegisterNotCrossTask — задача с register: probe, читающая
// register.self.* И собственное именованное register (redis_role в failed_when
// своего probe), НЕ зависит сама от себя: остаётся passage 0. Ловит регресс, где
// own/self-ссылка ошибочно считается cross-task ребром (дала бы цикл/смещение).
func TestStratify_SelfRegisterNotCrossTask(t *testing.T) {
	const src = `
name: self
tasks:
  - name: Probe with self-referential predicate
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    failed_when: size(register.redis_role) < incarnation.host_count
    retry:
      count: 3
      delay: 1s
      until: contains(register.self.stdout, 'ok')
    params: { cmd: "true" }
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 (self/own register не cross-task)", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("probe passage = %d, want 0", p.TaskPassage[0])
	}
}

// TestStratify_RegisterInOutput — cross-task register, протянутый через ${ … } в
// `output:` (декларированный output destiny/scenario-задачи, читается потребителем
// через register:), тоже двигает passage. output — passage-определяющий источник
// (ADR-056-реестр); регресс, где collectTaskReads его не обходит, оставил бы
// потребителя output-register в том же Passage, что probe → silent-wrong-target.
func TestStratify_RegisterInOutput(t *testing.T) {
	const src = `
name: chain_output
tasks:
  - name: Probe value
    module: core.cmd.shell
    register: probe
    changed_when: false
    params: { cmd: "true" }
  - name: Expose probe result via output
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.probe.stdout }"
    params: { cmd: "true" }
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (register в output двигает passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (output-потребитель СТРОГО после probe)", p.TaskPassage)
	}
}

// registerSourceFields — канонический реестр passage-определяющих register-
// источников Task (ADR-056), КАЖДЫЙ как минимальная scenario-фикстура, где
// ghost-register встречается ТОЛЬКО в этом поле и НИКТО его не эмитит. Ключ —
// человекочитаемое имя поля; значение — YAML целого сценария.
//
// Это механизм reads==refs consistency: оба графа register-ссылок —
// стратификатора (render.collectTaskReads → Stratify) и config-валидатора
// (config.collectRefs → unknown_register_reference) — ОБЯЗАНЫ ловить ghost в
// каждом поле. Если кто-то добавит новый register-читающий source-field в один
// обходчик, но забудет в другой (или удалит из одного), соответствующий под-тест
// покраснеет: либо Stratify не вернёт StratifyUnknownRegister (стратификатор не
// видит поле → молча неполный граф → silent-wrong-target), либо config-валидатор
// не поднимет unknown_register_reference (дыра линтера → unknown доживает до
// рантайма). requisites (onchanges/onfail/require) и flow-control (when/...) сюда
// НЕ входят — они НЕ passage-определяющие (см. ADR-056 §реестр).
var registerSourceFields = map[string]string{
	"where": `
name: f_where
tasks:
  - name: consumer
    module: core.exec.run
    on: ["${ incarnation.name }"]
    where: register.ghost.stdout == 'x'
    changed_when: false
    params: { cmd: "true" }
`,
	"vars": `
name: f_vars
tasks:
  - name: consumer
    module: core.exec.run
    vars:
      v: "${ register.ghost.stdout }"
    changed_when: false
    params: { cmd: "true" }
`,
	"params": `
name: f_params
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    params:
      cmd: echo
      args: ["${ register.ghost.stdout }"]
`,
	"apply.input": `
name: f_apply_input
tasks:
  - name: consumer
    apply:
      destiny: redis
      input:
        seed: "${ register.ghost.stdout }"
`,
	"output": `
name: f_output
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.ghost.stdout }"
    params: { cmd: "true" }
`,
	"loop.items": `
name: f_loop_items
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    loop:
      items: "${ register.ghost.stdout }"
      as: item
    params: { cmd: "echo ${ item }" }
`,
	"block": `
name: f_block
tasks:
  - name: container
    block:
      - name: nested consumer
        module: core.exec.run
        where: register.ghost.stdout == 'x'
        changed_when: false
        params: { cmd: "true" }
`,
}

// TestStratify_ReadsEqRefsConsistency — ★ guard против молчаливого размывания
// register-графа: множество PASSAGE-ОПРЕДЕЛЯЮЩИХ source-полей, покрытых
// стратификатором (collectTaskReads), ОБЯЗАНО совпадать с покрытыми config-
// валидатором (collectRefs) в этом классе. Для каждого источникового поля (where /
// vars / params / apply.input / output / loop.items / block) ghost-register обязан
// быть пойман ОБОИМИ: Stratify → StratifyUnknownRegister И config-валидатор →
// unknown_register_reference.
//
// Flow-control (when/changed_when/failed_when) сюда НЕ входит — он ∈ collectRefs,
// но ∉ collectTaskReads (намеренная асимметрия после FC-5 narrow-fix; см.
// TestStratify_FlowControlInRefsNotPassageReads ниже). Поле passage-определяющего
// класса, добавленное в один обходчик, но не в другой, краснит ровно тот под-тест,
// который соответствует расхождению — это и есть инвариант сопровождения ADR-056.
func TestStratify_ReadsEqRefsConsistency(t *testing.T) {
	for field, src := range registerSourceFields {
		t.Run(field, func(t *testing.T) {
			// Сторона валидатора: unknown_register_reference на парсе.
			m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			// Никаких ДРУГИХ error-диагностик (фикстура структурно валидна, кроме
			// ожидаемого unknown_register) — иначе тест зелёный по ложной причине.
			foundUnknown := false
			for _, d := range diags {
				if d.Level != diag.LevelError {
					continue
				}
				if d.Code == "unknown_register_reference" {
					foundUnknown = true
					continue
				}
				t.Fatalf("unexpected error diagnostic (%s): %s — fixture for %q is structurally broken", d.Code, d.Message, field)
			}
			if !foundUnknown {
				t.Errorf("config-validator (collectRefs) did NOT raise unknown_register_reference for ghost in %q — source-field coverage diverged from stratifier", field)
			}

			// Сторона стратификатора: StratifyUnknownRegister на том же ghost.
			_, serr := Stratify(m.Tasks)
			if serr == nil {
				t.Fatalf("Stratify did NOT fail for ghost in %q — collectTaskReads does not cover this source-field (silent-wrong-target risk)", field)
			}
			var se *StratifyError
			if !errors.As(serr, &se) || se.Code != StratifyUnknownRegister {
				t.Fatalf("Stratify for %q: error code = %v, want %s", field, serr, StratifyUnknownRegister)
			}
		})
	}
}

// flowControlFields — flow-control source-поля (when / changed_when / failed_when),
// КАЖДОЕ как минимальная фикстура, где ghost-register встречается ТОЛЬКО в этом
// поле и НИКТО его не эмитит. После FC-5 narrow-fix flow-control НЕ passage-
// определяющий (ADR-056:85), но ОСТАЁТСЯ register-читающим (cross-ref-валидатор
// проверяет существование register). Это фиксирует асимметрию: refs ⊋ passage-reads.
var flowControlFields = map[string]string{
	"when": `
name: fc_when
tasks:
  - name: consumer
    module: core.exec.run
    when: register.ghost.stdout == 'x'
    changed_when: false
    params: { cmd: "true" }
`,
	"changed_when": `
name: fc_changed_when
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: register.ghost.rc == 0
    params: { cmd: "true" }
`,
	"failed_when": `
name: fc_failed_when
tasks:
  - name: consumer
    module: core.exec.run
    changed_when: false
    failed_when: register.ghost.rc != 0
    params: { cmd: "true" }
`,
}

// TestStratify_FlowControlInRefsNotPassageReads — ★ guard фиксирующий FC-5-асимметрию
// (ADR-056:85 amend): flow-control `when`/`changed_when`/`failed_when` — register-
// ЧИТАЮЩИЙ (∈ collectRefs → cross-ref-валидатор ловит ghost), но НЕ passage-
// ОПРЕДЕЛЯЮЩИЙ (∉ collectTaskReads → Stratify НЕ падает StratifyUnknownRegister и
// НЕ строит passage-ребро по нему).
//
// До narrow-fix flow-control был в collectTaskReads (conservative over-approx) и
// расщеплял probe↔same-passage-when-потребителя → Soul cross-passage register не
// видел → `no such key` (FC-5). Регресс «вернуть flow-control в collectTaskReads»
// этот тест краснит: Stratify начнёт падать на ghost-register в flow-control-поле.
func TestStratify_FlowControlInRefsNotPassageReads(t *testing.T) {
	for field, src := range flowControlFields {
		t.Run(field, func(t *testing.T) {
			m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
			if err != nil {
				t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
			}
			// (а) ∈ refs: cross-ref-валидатор ОБЯЗАН поймать ghost (register-читающее поле).
			foundUnknown := false
			for _, d := range diags {
				if d.Level != diag.LevelError {
					continue
				}
				if d.Code == "unknown_register_reference" {
					foundUnknown = true
					continue
				}
				t.Fatalf("unexpected error diagnostic (%s): %s — fixture for %q is structurally broken", d.Code, d.Message, field)
			}
			if !foundUnknown {
				t.Errorf("config-validator (collectRefs) did NOT raise unknown_register_reference for ghost in flow-control %q — flow-control dropped out of refs (must stay: register-reading)", field)
			}

			// (б) ∉ passage-reads: Stratify НЕ падает StratifyUnknownRegister (flow-control
			// не в collectTaskReads → не строит passage-граф по ghost-register).
			_, serr := Stratify(m.Tasks)
			if serr != nil {
				t.Fatalf("Stratify FAILED on ghost in flow-control %q (%v) — flow-control крадётся обратно в collectTaskReads (passage-определяющий), что вернуло бы FC-5 cross-passage-расщепление", field, serr)
			}
		})
	}
}

// TestStratify_RegisterDependentWhenDoesNotSplitPassage — ★ КЛЮЧЕВОЙ guard FC-5
// narrow-fix (ADR-056:85). probe эмитит register: redis_role (Passage 0), задача БЕЗ
// своего where:/vars/params-register-ребра, но с `when: register.redis_role...`,
// ОБЯЗАНА остаться в ТОМ ЖЕ Passage 0 — flow-control сам Passage НЕ расщепляет.
// Тогда Soul видит register same-passage → when работает (как задумано ADR-012(d)).
//
// До narrow-fix when загонял потребителя в Passage 1 (Count=2, [0 1]) → cross-passage
// no-such-key. После — Count=1, обе задачи Passage 0. Регресс краснит на Count!=1.
func TestStratify_RegisterDependentWhenDoesNotSplitPassage(t *testing.T) {
	const src = `
name: when_same_passage
tasks:
  - name: Detect actual redis role per host
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: Act on master only (when-gated)
    module: core.cmd.shell
    when: register.redis_role.stdout == 'master'
    changed_when: false
    params: { cmd: "touch /tmp/acted" }
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 — register-зависимый when НЕ должен расщеплять Passage (flow-control НЕ passage-определяющий, ADR-056:85); до narrow-fix было 2 → FC-5 cross-passage no-such-key", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (probe и when-потребитель в одном Passage → Soul видит register same-passage)", i, pass)
		}
	}
}

// TestCrossPassageWhenGating_Detect — ★ FC-5 fail-closed детект genuinely cross-
// passage when (ADR-056:85). probe `role` уезжает в Passage 0, потому что ДРУГАЯ
// задача таргетит `where: register.role` (passage-определяющий → role-эмиттер
// program-order остаётся в Passage 0, where-потребитель в Passage 1). when-задача
// тоже уехала в Passage 1 по СВОЕЙ register-зависимости (vars: register.other) — а
// `when: register.role` теперь genuinely cross-passage (role в Passage 0, потребитель
// в Passage 1). Детектор обязан reject с координатами.
func TestCrossPassageWhenGating_Detect(t *testing.T) {
	const src = `
name: cross_passage_when
state_changes: {}
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: role
    changed_when: false
    params: { cmd: "detect-role" }
  - name: Where-target on role (forces role into Passage 0)
    module: core.exec.run
    where: register.role.stdout == 'master'
    register: other
    changed_when: false
    params: { cmd: "true" }
  - name: When-gated on role across passages
    module: core.cmd.shell
    when: register.role.stdout == 'master'
    changed_when: false
    vars:
      seed: "${ register.other.stdout }"
    params: { cmd: "true" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count <= 1 {
		t.Fatalf("ожидался staged-план (Count>1), got Count=%d TaskPassage=%v", passage.Count, passage.TaskPassage)
	}
	info, bad := CrossPassageWhenGating(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageWhenGating НЕ задетектил genuinely cross-passage when (register.role из Passage 0, потребитель в Passage 1) — TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "when" || info.RegisterName != "role" {
		t.Errorf("info = %+v, want kind=when register=role", info)
	}
	if info.ConsumerPassage == info.SourcePassage {
		t.Errorf("consumer passage %d == source passage %d, ожидались разные", info.ConsumerPassage, info.SourcePassage)
	}
}

// TestCrossPassageWhenGating_SamePassageOK — when по register same-passage (probe и
// when-потребитель в одном Passage после narrow-fix) → НЕ reject. Это валидный
// FC-5-кейс: Soul видит register своего Passage.
func TestCrossPassageWhenGating_SamePassageOK(t *testing.T) {
	const src = `
name: when_same_passage_ok
tasks:
  - name: Probe role
    module: core.cmd.shell
    register: redis_role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: When-gated same passage
    module: core.cmd.shell
    when: register.redis_role.stdout == 'master'
    changed_when: false
    params: { cmd: "true" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if _, bad := CrossPassageWhenGating(tasks, passage); bad {
		t.Fatalf("CrossPassageWhenGating ложно зарепортил same-passage when как cross-passage — после narrow-fix when в одном Passage с probe (TaskPassage=%v)", passage.TaskPassage)
	}
}

// TestCrossPassageWhenGating_SelfRegisterOK — failed_when по register.self (same-task
// результат, идиома remove_replica) → НЕ reject. register.self отфильтрован
// ExtractRegisterRefs → детектор его не видит. Регресс «ловить register.self» сломал
// бы валидную защиту (например `failed_when: register.self.stdout == 'master'`).
func TestCrossPassageWhenGating_SelfRegisterOK(t *testing.T) {
	const src = `
name: failed_when_self
tasks:
  - name: Refuse to remove the current primary
    module: core.cmd.shell
    register: role
    changed_when: false
    failed_when: register.self.stdout == 'master'
    params: { cmd: "redis-cli role | head -1 | tr -d '\n'" }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if _, bad := CrossPassageWhenGating(tasks, passage); bad {
		t.Fatalf("CrossPassageWhenGating ложно зарепортил register.self (same-task) как cross-passage — сломал бы remove_replica-защиту (TaskPassage=%v)", passage.TaskPassage)
	}
}

// TestValidate_UnknownRegisterInOutput — закрытый ADR-056 S2 пробел: до S2
// cross-ref-валидатор НЕ обходил интерполяционные source-поля, и unknown-register
// в `output:` (как и в vars/params/apply.input/loop.items) доживал до рантайм-
// стратификатора. Теперь config-валидатор ловит его ОФЛАЙН.
func TestValidate_UnknownRegisterInOutput(t *testing.T) {
	const src = `
name: out_unknown
tasks:
  - name: Expose a register nobody emits
    module: core.exec.run
    changed_when: false
    output:
      role: "${ register.ghost.stdout }"
    params: { cmd: "true" }
`
	_, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	found := false
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			found = true
		}
	}
	if !found {
		t.Error("config-validator did NOT raise unknown_register_reference for ghost in output: — the validator gap (ADR-056 S2) is not closed")
	}
}

// TestStratify_Empty — пустой план задач → Count 1, без паники.
func TestStratify_Empty(t *testing.T) {
	p, err := Stratify(nil)
	if err != nil {
		t.Fatalf("Stratify(nil): %v", err)
	}
	if p.Count != 1 || p.TaskPassage != nil {
		t.Fatalf("Stratify(nil) = %+v, want {nil, 1}", p)
	}
}

// TestCrossPassageRequisite_Detect — ★ R2 ДЕТЕКТ (ADR-056 amend). Restart с
// onchanges:[cfg] вынужден в Passage 1 ОТДЕЛЬНОЙ register-зависимостью (where:
// register.role.*); requisite-источник cfg остаётся в Passage 0. consumer passage
// 1 ≠ источник passage 0 → CrossPassageRequisite ловит до dispatch. Без детекта
// Soul gating Passage-1 не видит register cfg (другой ApplyRequest) → restart молча
// SKIPPED.
func TestCrossPassageRequisite_Detect(t *testing.T) {
	const src = `
name: cross_passage
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Apply config
    module: core.file.present
    register: cfg
    params: { path: /etc/app.conf, content: x }
  - name: Restart on master after config change
    module: core.service.restarted
    where: "register.role.stdout == 'master'"
    onchanges: [cfg]
    params: { name: app }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count <= 1 {
		t.Fatalf("ожидался staged-план (Count>1), got Count=%d, TaskPassage=%v", passage.Count, passage.TaskPassage)
	}
	info, bad := CrossPassageRequisite(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageRequisite не задетектил cross-passage onchanges (consumer/source в разных Passage) — TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "onchanges" || info.RequisiteName != "cfg" {
		t.Errorf("info = %+v, want kind=onchanges requisite=cfg", info)
	}
	if info.ConsumerPassage == info.SourcePassage {
		t.Errorf("consumer passage %d == source passage %d, ожидались разные", info.ConsumerPassage, info.SourcePassage)
	}
}

// TestCrossPassageRequisite_SamePassageOK — same-passage onchanges (источник и
// потребитель в одном Passage, R1-remap их чинит) → НЕ reject. N=1 без where:
// (всё в Passage 0) — onchanges работает штатно после remap.
func TestCrossPassageRequisite_SamePassageOK(t *testing.T) {
	const src = `
name: same_passage
state_changes: {}
tasks:
  - name: Apply config
    module: core.file.present
    register: cfg
    params: { path: /etc/app.conf, content: x }
  - name: Restart on config change
    module: core.service.restarted
    onchanges: [cfg]
    params: { name: app }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	if passage.Count != 1 {
		t.Fatalf("ожидался N=1 (Count=1), got Count=%d", passage.Count)
	}
	if _, bad := CrossPassageRequisite(tasks, passage); bad {
		t.Fatalf("CrossPassageRequisite ложно зарепортил same-passage onchanges как cross-passage — R1-remap должен его чинить, не reject")
	}
}

// TestCrossPassageRequisite_OnFailDetect — onfail-зеркало детекта: onfail-источник
// в Passage 0, rescue-задача вынуждена в Passage 1 where-зависимостью → cross-passage
// reject (kind=onfail). Без детекта onfail-rescue молча не запустится на провал
// источника (Soul Passage-1 не видит register источника Passage 0).
func TestCrossPassageRequisite_OnFailDetect(t *testing.T) {
	const src = `
name: cross_passage_onfail
state_changes: {}
tasks:
  - name: Probe role
    module: core.exec.run
    register: role
    changed_when: "false"
    params: { cmd: detect-role }
  - name: Risky deploy
    module: core.exec.run
    register: deploy
    params: { cmd: deploy }
  - name: Rollback on master after deploy fail
    module: core.exec.run
    where: "register.role.stdout == 'master'"
    onfail: [deploy]
    params: { cmd: rollback }
`
	tasks := loadTasks(t, src)
	passage := stratify(t, src)
	info, bad := CrossPassageRequisite(tasks, passage)
	if !bad {
		t.Fatalf("CrossPassageRequisite не задетектил cross-passage onfail — TaskPassage=%v", passage.TaskPassage)
	}
	if info.Kind != "onfail" || info.RequisiteName != "deploy" {
		t.Errorf("info = %+v, want kind=onfail requisite=deploy", info)
	}
}

// --- within-block register-зависимость (silent-wrong-target) ---

// TestWithinBlock_PeerReject (guard #1) — ★ ОСНОВНОЙ КЕЙС. probe-потомок эмитит
// register: role ВНУТРИ блока, соседний потомок ТОГО ЖЕ блока читает
// where: register.role.* — peer-register недоступен на render (block атомарен,
// probe ещё не отработал). Детектор обязан reject с точными координатами.
func TestWithinBlock_PeerReject(t *testing.T) {
	const src = `
name: within_block_peer
state_changes: {}
tasks:
  - name: Rolling group
    on: ["${ incarnation.name }"]
    block:
      - name: Probe role inside block
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Restart on master
        module: core.service.restarted
        where: "register.role.stdout == 'master'"
        params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	info, bad := WithinBlockRegisterDependency(tasks)
	if !bad {
		t.Fatalf("WithinBlockRegisterDependency не задетектил peer-register внутри block — silent-wrong-target прошёл бы молча")
	}
	if info.RegisterName != "role" {
		t.Errorf("RegisterName = %q, want role", info.RegisterName)
	}
	if info.ReaderName != "Restart on master" {
		t.Errorf("ReaderName = %q, want \"Restart on master\"", info.ReaderName)
	}
	if info.EmitterName != "Probe role inside block" {
		t.Errorf("EmitterName = %q, want \"Probe role inside block\"", info.EmitterName)
	}
}

// TestWithinBlock_WhenPeerOK (guard #1b) — ★ FC-5 SIDE-EFFECT GUARD. probe-потомок
// эмитит register: role ВНУТРИ блока, соседний потомок ТОГО ЖЕ блока гейтит
// `when: register.role.stdout == 'master'` (flow-control, НЕ where). После FC-5
// narrow-fix `when` выпал из collectTaskReads (Soul-side per-task gating) → within-
// block `when: register.peer` ВАЛИДЕН: peer-probe исполняется тем же ApplyRequest
// ДО потребителя, Soul видит peer-register в накопленном срезе блока на момент eval.
// Детектор НЕ должен это отвергать → bad==false. Регресс «вернуть when в
// collectTaskReads» молча сломал бы within-block when:peer — этот тест краснит.
func TestWithinBlock_WhenPeerOK(t *testing.T) {
	const src = `
name: within_block_when_peer
state_changes: {}
tasks:
  - name: Rolling group
    on: ["${ incarnation.name }"]
    block:
      - name: Probe role inside block
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Act on master only (when-gated peer)
        module: core.cmd.shell
        when: "register.role.stdout == 'master'"
        changed_when: false
        params: { cmd: "touch /tmp/acted" }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency ложно зарепортил within-block when:peer как silent-wrong-target (%+v) — после FC-5 narrow-fix when выпал из collectTaskReads, when:peer валиден (Soul-side gating, peer-probe в том же ApplyRequest)", info)
	}
}

// TestWithinBlock_ExternalProbeOK (guard #2) — ★ ПРИЁМКА НЕ СЛОМАНА. probe эмитит
// register: role на TOP-LEVEL (отдельная задача ДО блока), block-потомок читает
// where: register.role.* — это ВАЛИДНО (probe — отдельный Passage, restart-кейс).
// Детектор сверяет ТОЛЬКО против register-ов, рождённых ВНУТРИ блока (не глобальный
// emitterIndex), поэтому внешний probe не ловится → ok==false. Регресс «сломать
// restart» этим тестом фиксируется.
func TestWithinBlock_ExternalProbeOK(t *testing.T) {
	const src = `
name: external_probe
state_changes: {}
tasks:
  - name: Probe role top-level
    module: core.cmd.shell
    on: ["${ incarnation.name }"]
    register: role
    changed_when: false
    params: { cmd: "redis-cli role | head -1" }
  - name: Rolling group
    on: ["${ incarnation.name }"]
    where: "register.role.stdout == 'slave'"
    block:
      - name: Restart replica
        module: core.service.restarted
        params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency ложно зарепортил внешний top-level probe как peer-зависимость (%+v) — сломал бы валидный restart-кейс", info)
	}
}

// TestWithinBlock_RegisterSelfOK (guard #3) — block-потомок читает register.self.*
// (собственный результат шага, не peer) в retry.until: → ВАЛИДНО. register.self
// уже отфильтрован ExtractRegisterRefs (collectTaskReads его не вернёт), поэтому
// детектор не должен на него реагировать → ok==false.
func TestWithinBlock_RegisterSelfOK(t *testing.T) {
	const src = `
name: register_self
state_changes: {}
tasks:
  - name: Rolling group
    on: ["${ incarnation.name }"]
    block:
      - name: Wait until healthy
        module: core.exec.run
        changed_when: false
        retry:
          count: 12
          delay: 5s
          until: "contains(register.self.stdout, 'master_link_status:up')"
        params: { cmd: redis-cli }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency ложно зарепортил register.self как peer-зависимость (%+v)", info)
	}
}

// TestWithinBlock_NestedPeerReject (guard #4) — вложенный block: внутренний
// потомок читает register, эмитнутый ВНЕШНИМ потомком того же (внешнего) блока →
// reject. peer внутри вложенного блока = peer снаружи (один Passage у всего блока).
func TestWithinBlock_NestedPeerReject(t *testing.T) {
	const src = `
name: nested_peer
state_changes: {}
tasks:
  - name: Outer group
    on: ["${ incarnation.name }"]
    block:
      - name: Probe role in outer
        module: core.cmd.shell
        register: role
        changed_when: false
        params: { cmd: "redis-cli role | head -1" }
      - name: Inner group
        block:
          - name: Restart on master deep
            module: core.service.restarted
            where: "register.role.stdout == 'master'"
            params: { name: redis-server }
`
	tasks := loadTasks(t, src)
	info, bad := WithinBlockRegisterDependency(tasks)
	if !bad {
		t.Fatalf("WithinBlockRegisterDependency не задетектил peer-register через вложенный block — silent-wrong-target")
	}
	if info.RegisterName != "role" {
		t.Errorf("RegisterName = %q, want role", info.RegisterName)
	}
}

// TestWithinBlock_NoRegisterOK (guard #5) — block без единого register: внутри →
// fast-path ok==false (нечего читать как peer). Ловит регресс, где пустой
// blockEmits даёт ложное срабатывание.
func TestWithinBlock_NoRegisterOK(t *testing.T) {
	const src = `
name: no_register
state_changes: {}
tasks:
  - name: Rolling group
    on: ["${ incarnation.name }"]
    where: "soulprint.self.sid != ''"
    block:
      - name: Restart redis
        module: core.service.restarted
        params: { name: redis-server }
      - name: Reload config
        module: core.exec.run
        params: { cmd: reload }
`
	tasks := loadTasks(t, src)
	if info, bad := WithinBlockRegisterDependency(tasks); bad {
		t.Fatalf("WithinBlockRegisterDependency ложно сработал на блок без register (%+v)", info)
	}
}

// TestWithinBlock_AcceptanceRestart (guard #6) — ★ ПРИЁМКА: реальный
// examples/service/redis/scenario/restart/main.yml (probe redis_role top-level,
// block с where на внешний register) ВАЛИДЕН → ok==false. Плюс регресс-цикл по
// ВСЕМ committed example-сценариям: ни один не должен ловиться детектором (иначе
// валидный пример молча перестал бы прогоняться).
func TestWithinBlock_AcceptanceRestart(t *testing.T) {
	restartPath := filepath.FromSlash("../../examples/service/redis/scenario/restart/main.yml")
	m, _, diags, err := LoadScenarioManifest(restartPath, ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifest(restart): %v", err)
	}
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Fatalf("restart diagnostic (%s): %s", d.Code, d.Message)
		}
	}
	if info, bad := WithinBlockRegisterDependency(m.Tasks); bad {
		t.Fatalf("ПРИЁМКА СЛОМАНА: restart/main.yml зарепортирован как within-block peer (%+v) — внешний probe redis_role валиден", info)
	}

	// Регресс приёмки: ни один committed example-сценарий не ловится детектором.
	all, gerr := filepath.Glob(filepath.FromSlash("../../examples/service/*/scenario/*/main.yml"))
	if gerr != nil {
		t.Fatalf("glob examples: %v", gerr)
	}
	if len(all) == 0 {
		t.Fatal("glob examples дал 0 сценариев — путь/раскладка сломаны")
	}
	for _, p := range all {
		em, _, ediags, eerr := LoadScenarioManifest(p, ValidateOptions{})
		if eerr != nil || em == nil {
			continue // невалидный/нерезолвимый офлайн пример — не зона этого детектора.
		}
		hardErr := false
		for _, d := range ediags {
			if d.Level == diag.LevelError {
				hardErr = true
				break
			}
		}
		if hardErr {
			continue
		}
		if info, bad := WithinBlockRegisterDependency(em.Tasks); bad {
			t.Errorf("регресс приёмки: example %s ловится within-block-детектором (%+v) — валидный пример перестал бы прогоняться", p, info)
		}
	}
}
