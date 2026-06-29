package config

import (
	"testing"
)

// Guard-тесты S2 (ADR-0061 §S2): `refresh_soulprint: true` на core.soul.registered
// делает шаг PASSAGE-ОПРЕДЕЛЯЮЩИМ эмиттером «roster-refreshed». Любой последующий
// roster-потребитель (soulprint.hosts / on:[incarnation.name] / soulprint.self.* /
// опущенный on:) ОБЯЗАН уехать в Passage СТРОГО ПОСЛЕ refresh-шага — иначе он
// отрендерится по СТАРОМУ (до-роста) roster = silent-wrong-target на разрушительной
// операции (★ БЛОКЕР ADR-056 §риски: redis-apply на пустом/неполном наборе).

// --- фикстуры provision→refresh→роль (целевой сценарий ADR-0061) ---

// refreshThenSoulprintHosts — Passage 0: cloud-provision (keeper) + refresh-шаг;
// Passage 1: задача, читающая soulprint.hosts в assert. refresh-шаг загоняет
// потребителя soulprint.hosts в следующий Passage.
const refreshThenSoulprintHosts = `
name: create
state_changes: {}
tasks:
  - name: Register and await created hosts
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply redis role to grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`

// refreshThenOnIncarnation — refresh-шаг (keeper) + задача с on:[incarnation.name]
// (роль на весь выросший roster). on:[incarnation.name] — roster-таргетинг,
// разрешается из in.Hosts: после refresh обязан видеть новые SID → следующий Passage.
const refreshThenOnIncarnation = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to all incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server --start"
`

// refreshThenSoulprintSelf — refresh-шаг + задача, читающая soulprint.self.* в
// where:. soulprint.self host-вариативно (зависит от того, какие хосты в roster),
// поэтому это тоже roster-чтение → следующий Passage.
const refreshThenSoulprintSelf = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Configure each host by its facts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    where: "soulprint.self.os.family == 'debian'"
    changed_when: false
    params:
      cmd: "apt-get update"
`

// refreshThenOmittedOn — refresh-шаг + задача с ОПУЩЕННЫМ on: (= весь incarnation,
// весь roster). Опущенный on: — тоже roster-таргетинг (orchestration.md §3:
// опущенный on: = весь incarnation), значит после refresh обязан уехать в следующий
// Passage, иначе отработает на старом roster.
const refreshThenOmittedOn = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply baseline to whole incarnation
    module: core.exec.run
    changed_when: false
    params:
      cmd: "baseline"
`

// TestStratify_RefreshThenSoulprintHosts — ★ ОСНОВНОЙ КЕЙС S2 (ADR-0061). refresh-шаг
// (Passage 0) + потребитель soulprint.hosts → РАЗНЫЕ Passage (потребитель в Passage 1).
func TestStratify_RefreshThenSoulprintHosts(t *testing.T) {
	p := stratify(t, refreshThenSoulprintHosts)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh-граница расщепляет refresh-шаг и soulprint.hosts-потребителя)", p.Count)
	}
	if p.TaskPassage[0] != 0 {
		t.Errorf("refresh-шаг passage = %d, want 0", p.TaskPassage[0])
	}
	if p.TaskPassage[1] != 1 {
		t.Errorf("soulprint.hosts-потребитель passage = %d, want 1 (СТРОГО после refresh)", p.TaskPassage[1])
	}
}

// TestStratify_RefreshThenOnIncarnation — refresh-шаг + on:[incarnation.name]
// (roster-таргетинг) → разные Passage. Без этого redis-apply уехал бы в тот же
// Passage со старым (пустым) roster = silent-wrong-target.
func TestStratify_RefreshThenOnIncarnation(t *testing.T) {
	p := stratify(t, refreshThenOnIncarnation)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh-граница расщепляет refresh-шаг и on:[incarnation.name]-потребителя)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (on:[incarnation.name] СТРОГО после refresh)", p.TaskPassage)
	}
}

// TestStratify_RefreshThenSoulprintSelf — refresh-шаг + soulprint.self.* в where:
// (host-вариативное чтение) → разные Passage.
func TestStratify_RefreshThenSoulprintSelf(t *testing.T) {
	p := stratify(t, refreshThenSoulprintSelf)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (refresh-граница расщепляет refresh-шаг и soulprint.self-потребителя)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (soulprint.self СТРОГО после refresh)", p.TaskPassage)
	}
}

// TestStratify_RefreshThenOmittedOn — refresh-шаг + задача с ОПУЩЕННЫМ on: (весь
// incarnation = весь roster) → разные Passage. Опущенный on: — roster-таргетинг,
// активируется как refresh-потребитель ТОЛЬКО при наличии refresh-эмиттера в плане.
func TestStratify_RefreshThenOmittedOn(t *testing.T) {
	p := stratify(t, refreshThenOmittedOn)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (опущенный on: после refresh = весь выросший roster → следующий Passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (опущенный on: СТРОГО после refresh)", p.TaskPassage)
	}
}

// TestStratify_NoRefreshConsumerSamePassage — ★ КОНТРОЛЬ (опечатка/отсутствие
// refresh). Тот же план, но БЕЗ refresh_soulprint: true на регистрирующем шаге.
// roster-потребитель НЕ обязан расщепляться по roster-оси → оба в Passage 0
// (refresh-границы нет). Регресс «refresh-граница активна без флага» краснит этот
// тест: roster-потребитель ложно уехал бы в Passage 1.
func TestStratify_NoRefreshConsumerSamePassage(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register hosts WITHOUT refresh
    module: core.soul.registered
    on: keeper
    params:
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 — без refresh_soulprint roster-граница НЕ активна (опечатка/отсутствие флага → потребитель в том же Passage)", p.Count)
	}
	for i, pass := range p.TaskPassage {
		if pass != 0 {
			t.Errorf("task #%d passage = %d, want 0 (нет refresh-эмиттера → один Passage)", i, pass)
		}
	}
}

// TestStratify_RefreshFalseNotEmitter — refresh_soulprint: false (явный false) НЕ
// делает шаг refresh-эмиттером → roster-потребитель в том же Passage. Ловит регресс
// «любой refresh_soulprint-ключ = эмиттер» (надо именно true).
func TestStratify_RefreshFalseNotEmitter(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register hosts with refresh disabled
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: false
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to incarnation hosts
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "role"
`
	p := stratify(t, src)
	if p.Count != 1 {
		t.Fatalf("Count = %d, want 1 — refresh_soulprint: false НЕ эмиттер", p.Count)
	}
}

// TestStratify_RefreshBeforeAndAfterRoster — refresh-шаг МЕЖДУ двумя roster-
// потребителями: первый (ДО refresh) в Passage 0, второй (ПОСЛЕ refresh) в Passage 1.
// Доказывает, что refresh-граница работает по program-order: только ПОСЛЕДУЮЩИЕ
// потребители расщепляются, предшествующие — нет.
func TestStratify_RefreshBeforeAndAfterRoster(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Act on initial roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "initial"
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Act on grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "grown"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2", p.Count)
	}
	want := []int{0, 0, 1}
	for i, w := range want {
		if p.TaskPassage[i] != w {
			t.Errorf("task #%d passage = %d, want %d (refresh-граница: только ПОСЛЕДУЮЩИЕ roster-потребители расщепляются)", i, p.TaskPassage[i], w)
		}
	}
}

// TestStratify_RefreshThenAssertTopology — assert: топологии (soulprint.hosts в
// that[]) ПОСЛЕ refresh-шага → Passage 1. assert вычисляется Keeper-side на render
// (как where:), поэтому обязан видеть выросший roster (ADR-0061 §детерминизм).
func TestStratify_RefreshThenAssertTopology(t *testing.T) {
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register created hosts and refresh roster
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "host-new.example.com"
      coven: ["${ incarnation.name }"]
  - name: Assert the grown topology
    assert:
      that:
        - "size(soulprint.hosts) == 3"
      message: "expected 3 hosts after provisioning"
`
	p := stratify(t, src)
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (assert soulprint.hosts ПОСЛЕ refresh → следующий Passage)", p.Count)
	}
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (assert-топология видит выросший roster)", p.TaskPassage)
	}
}

// TestStratify_RefreshIsRosterAxisNotRegisterEdges — ★ ИНВАРИАНТ ОСИ (ADR-0061 §S2):
// refresh-граница — ОТДЕЛЬНАЯ ось от register, она НЕ вводит cross-task register-
// рёбер. Доказываем напрямую сравнением множеств: на refresh-фикстуре, где Passage-
// расщепление вызвано ИСКЛЮЧИТЕЛЬНО roster-границей (soulprint.hosts-потребитель
// после refresh-шага), множество passage-определяющих cross-task register-reads
// (taskRegisterReads/collectTaskReads) у КАЖДОЙ задачи ПУСТО — то есть Count==2 даёт
// именно roster-ось, а не register-граф. Регресс «refresh-логика протекла в
// register-reads» (например, refresh-эмиттер ошибочно стал источником register-
// ребра) краснит этот тест: появится непустой read-set / register-ребро.
//
// reads⊆refs ADR-056 при этом тривиально держится (∅ ⊆ refs): refresh не добавляет
// register-ссылок, поэтому существующий register-инвариант не затрагивается.
func TestStratify_RefreshIsRosterAxisNotRegisterEdges(t *testing.T) {
	// refresh-шаг (register: roster — собственный register, не cross-task) +
	// roster-потребитель (soulprint.hosts). Никаких cross-task register-ссылок:
	// расщепление обязано прийти ТОЛЬКО от roster-границы.
	const src = `
name: create
state_changes: {}
tasks:
  - name: Register a fixed host and refresh roster
    module: core.soul.registered
    on: keeper
    register: roster
    params:
      refresh_soulprint: true
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
  - name: Apply role to grown roster
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    vars:
      members: "${ soulprint.hosts }"
    params:
      cmd: "echo ${ members }"
`
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	for _, d := range diags {
		if d.Code == "unknown_register_reference" {
			t.Fatalf("ложный unknown_register_reference (%s): refresh-граница не должна вводить register-ссылок", d.Message)
		}
	}

	// ★ Прямое сравнение множеств: ни одна задача НЕ имеет cross-task register-reads.
	// Множество passage-определяющих register-reads ПУСТО на обеих задачах — значит
	// register-рёбер нет, и расщепление целиком на roster-оси.
	emitter := emitterIndex(m.Tasks)
	for i := range m.Tasks {
		reads := taskRegisterReads(&m.Tasks[i])
		if len(reads) != 0 {
			t.Errorf("task #%d имеет cross-task register-reads %v — refresh-фикстура должна расщепляться ТОЛЬКО roster-границей (register-рёбер быть не должно)", i, reads)
		}
		// reads⊆refs: каждый прочитанный register обязан иметь эмиттер (здесь reads
		// пуст, проверка тривиальна, но фиксирует инвариант на случай регресса).
		for _, name := range reads {
			if _, ok := emitter[name]; !ok {
				t.Errorf("task #%d читает register %q без эмиттера — reads⊄refs", i, name)
			}
		}
	}

	// Stratify не падает (register-граф чист) и даёт ровно roster-расщепление.
	p, serr := Stratify(m.Tasks)
	if serr != nil {
		t.Fatalf("Stratify: %v (refresh-граница не должна валить чистый register-граф)", serr)
	}
	if p.Count != 2 {
		t.Fatalf("Count = %d, want 2 (расщепление на roster-оси)", p.Count)
	}
	// Без refresh-границы (refreshEmitters пуст) тот же план не расщепился бы:
	// контрольно доказываем, что Count==2 — заслуга именно roster-оси.
	if p.TaskPassage[0] != 0 || p.TaskPassage[1] != 1 {
		t.Fatalf("passages = %v, want [0 1] (roster-потребитель строго после refresh)", p.TaskPassage)
	}
}

// TestHasRefreshEmitter — детектор «план провиженит roster mid-run» (ADR-0061
// amendment, no_hosts-bypass класс (б)). true ТОЛЬКО при core.soul.registered с
// литеральным refresh_soulprint: true; смежные формы (без флага / false / другой
// keeper-модуль / пустой план) → false. Чистая функция над плоским планом.
func TestHasRefreshEmitter(t *testing.T) {
	tasks := func(t *testing.T, src string) []Task {
		t.Helper()
		m, _, _, err := LoadScenarioManifestFromBytes("main.yml", []byte(src), ValidateOptions{})
		if err != nil {
			t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
		}
		return m.Tasks
	}

	// Эталон: refresh-эмиттер + host-задача деплоя (целевой mixed-план bypass-а).
	const mixedWithRefresh = `
name: create
state_changes: {}
tasks:
  - name: Provision and refresh
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: true
      sid: "${ register.provision.hosts }"
      coven: ["${ incarnation.name }"]
  - name: Deploy role
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server"
`
	const noFlag = `
name: create
state_changes: {}
tasks:
  - name: Register without refresh
    module: core.soul.registered
    on: keeper
    params:
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
`
	const refreshFalse = `
name: create
state_changes: {}
tasks:
  - name: Register with refresh disabled
    module: core.soul.registered
    on: keeper
    params:
      refresh_soulprint: false
      sid: "host-a.example.com"
      coven: ["${ incarnation.name }"]
`
	// Другой keeper-модуль с одноимённым param — НЕ эмиттер (модуль-носитель только
	// core.soul.registered).
	const otherKeeperModule = `
name: create
state_changes: {}
tasks:
  - name: Cloud provision
    module: core.cloud.created
    on: keeper
    params:
      refresh_soulprint: true
      profile: prod
`
	const hostOnly = `
name: create
state_changes: {}
tasks:
  - name: Deploy role
    module: core.exec.run
    on: ["${ incarnation.name }"]
    changed_when: false
    params:
      cmd: "redis-server"
`
	// refresh-эмиттер, вложенный в block: — рекурсивно распознаётся.
	const refreshInBlock = `
name: create
state_changes: {}
tasks:
  - name: Provision group
    block:
      - name: Register and refresh
        module: core.soul.registered
        on: keeper
        params:
          refresh_soulprint: true
          sid: "host-a.example.com"
          coven: ["${ incarnation.name }"]
`

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"mixed-with-refresh", mixedWithRefresh, true},
		{"refresh-in-block", refreshInBlock, true},
		{"no-flag", noFlag, false},
		{"refresh-false", refreshFalse, false},
		{"other-keeper-module", otherKeeperModule, false},
		{"host-only", hostOnly, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRefreshEmitter(tasks(t, tt.src)); got != tt.want {
				t.Errorf("HasRefreshEmitter = %v, want %v", got, tt.want)
			}
		})
	}

	// Пустой план — отдельно (LoadScenarioManifestFromBytes требует tasks непустым,
	// поэтому строим срез напрямую).
	if HasRefreshEmitter(nil) {
		t.Error("HasRefreshEmitter(nil) = true, want false")
	}
	if HasRefreshEmitter([]Task{}) {
		t.Error("HasRefreshEmitter([]) = true, want false")
	}
}
