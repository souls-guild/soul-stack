package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostsSizeMain — scenario, печатающий size(soulprint.hosts) в params команды.
// Render-наблюдаемый аналог size-guard-а: размер roster-а проявляется прямо в
// отрендеренном плане (а не во flow-control `failed_when:`, который Soul-side и
// не несёт soulprint.hosts во flow_context — см. buildFlowContext).
const hostsSizeMain = `name: create
input: {}
tasks:
  - name: report roster size
    module: core.exec.run
    params:
      cmd: "echo ${ soulprint.hosts.size() }"
`

// hostsWhereMain — scenario, фильтрующий roster по declared-роли: cross-host
// проекция soulprint.hosts.where(...) в params (primary-discovery идиома).
const hostsWhereMain = `name: create
input: {}
tasks:
  - name: replicaof primary
    module: core.exec.run
    params:
      cmd: "replicaof ${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip } 6379"
`

// TestRunCase_MultiHostRoster_Size3 — fixtures.hosts из 3 хостов даёт
// size(soulprint.hosts)==3 в отрендеренном плане: multi-host roster реально
// поднимает потолок single-host harness-а.
func TestRunCase_MultiHostRoster_Size3(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsSizeMain, `name: roster of three
fixtures:
  hosts:
    - sid: node-3.example.com
      covens: [create, redis]
    - sid: node-1.example.com
      covens: [create, redis]
    - sid: node-2.example.com
      covens: [create, redis]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "echo 3"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (size==3 на 3-хостовом roster), получили: %v", results[0].Failures)
	}
}

// TestRunCase_MultiHostRoster_WhereProjection — soulprint.hosts.where(...) на
// roster-е резолвит cross-host факт (primary_ip хоста с declared-ролью primary).
func TestRunCase_MultiHostRoster_WhereProjection(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsWhereMain, `name: where over roster
fixtures:
  hosts:
    - sid: replica-1.example.com
      covens: [create]
      role: replica
      soulprint:
        network: { primary_ip: 10.0.0.2 }
    - sid: primary-1.example.com
      covens: [create]
      role: primary
      soulprint:
        network: { primary_ip: 10.0.0.1 }
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "replicaof 10.0.0.1 6379"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (where role==primary → 10.0.0.1), получили: %v", results[0].Failures)
	}
}

// TestRunCase_MultiHostRoster_DeterministicOrder — порядок soulprint.hosts
// детерминирован сортировкой по SID независимо от порядка записей в YAML.
// [0]-элемент после where("role!=”) — лексикографически первый SID.
func TestRunCase_MultiHostRoster_DeterministicOrder(t *testing.T) {
	main := `name: create
input: {}
tasks:
  - name: first host by sid
    module: core.exec.run
    params:
      cmd: "first=${ soulprint.hosts[0].sid }"
`
	// YAML-порядок (c, a, b) намеренно не отсортирован: harness обязан
	// отсортировать по SID → первый = a.example.com.
	caseYML := `name: order by sid
fixtures:
  hosts:
    - sid: c.example.com
      covens: [create]
    - sid: a.example.com
      covens: [create]
    - sid: b.example.com
      covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "first=a.example.com"
`
	caseDir := writeScenarioTree(t, main, caseYML)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (детерминированный порядок: первый по SID = a.example.com), получили: %v", results[0].Failures)
	}
}

// TestRunCase_SingleHostSugar_Size1 — back-compat: fixtures.soulprint
// (single-host сахар) даёт size(soulprint.hosts)==1 бит-в-бит, как до правки.
func TestRunCase_SingleHostSugar_Size1(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsSizeMain, `name: single host sugar
fixtures:
  soulprint:
    os:
      family: debian
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "echo 1"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("ожидали PASS (single-host сахар → size==1), получили: %v", results[0].Failures)
	}
}

// TestLoadCase_RejectsSoulprintAndHosts — fixtures.soulprint (single) и
// fixtures.hosts (multi) в одном кейсе → strict-ошибка (взаимоисключение,
// в духе strict-декода harness).
func TestLoadCase_RejectsSoulprintAndHosts(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: both
fixtures:
  soulprint:
    os: { family: debian }
  hosts:
    - sid: node-1.example.com
      covens: [both]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("ожидали ошибку взаимоисключения soulprint+hosts")
	}
	if !strings.Contains(err.Error(), "взаимоисключены") {
		t.Fatalf("ожидали сообщение про взаимоисключение, получили: %v", err)
	}
}

// TestLoadCase_RejectsHostWithoutSID — host-запись roster-а без sid →
// strict-ошибка (sid обязателен).
func TestLoadCase_RejectsHostWithoutSID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: no sid
fixtures:
  hosts:
    - covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("ожидали ошибку на host без sid")
	}
	if !strings.Contains(err.Error(), "sid") {
		t.Fatalf("ожидали сообщение про обязательный sid, получили: %v", err)
	}
}

// TestLoadCase_RejectsDuplicateSID — две host-записи roster-а с одинаковым sid →
// strict-ошибка. Дубль схлопывает RegisterByHost (карта по SID) и делает порядок
// soulprint.hosts недетерминированным → недопустим.
func TestLoadCase_RejectsDuplicateSID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: dup sid
fixtures:
  hosts:
    - sid: node-1.example.com
      covens: [create]
    - sid: node-1.example.com
      covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("ожидали ошибку на дублирующийся sid")
	}
	if !strings.Contains(err.Error(), "дублирующийся sid") {
		t.Fatalf("ожидали сообщение про дублирующийся sid, получили: %v", err)
	}
}

// TestRunCase_MultiHostSizeGuard_PassAndFail — demo size-guard на render-уровне:
// param сверяет size(soulprint.hosts) с ожидаемым N. Pass-ветка (roster ровно
// N) и fail-ветка (roster != N) — обе наблюдаемы в отрендеренном плане.
//
// L0-форма guard-а — render-наблюдаемая (size в params), НЕ flow-control
// `failed_when:`: flow_context Soul-side не несёт soulprint.hosts
// (buildFlowContext), поэтому реальный failed_when-size-guard сервиса — это
// scenario-слой следующего слайса, а не L0-render.
func TestRunCase_MultiHostSizeGuard_PassAndFail(t *testing.T) {
	main := `name: create
input: {}
tasks:
  - name: size guard
    module: core.exec.run
    params:
      cmd: "size=${ soulprint.hosts.size() }"
`
	threeHosts := `fixtures:
  hosts:
    - sid: node-1.example.com
      covens: [create]
    - sid: node-2.example.com
      covens: [create]
    - sid: node-3.example.com
      covens: [create]
`

	// Pass: ассерт ждёт ровно size==3 на 3-хостовом roster.
	passDir := writeScenarioTree(t, main, "name: guard pass\n"+threeHosts+`assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "size=3"
`)
	passRes, err := Run(context.Background(), passDir)
	if err != nil {
		t.Fatalf("Run pass: %v", err)
	}
	if !passRes[0].Pass {
		t.Fatalf("pass-ветка: ожидали PASS (size==3), получили: %v", passRes[0].Failures)
	}

	// Fail: ассерт ждёт size==4, но roster — 3 хоста → расхождение.
	failDir := writeScenarioTree(t, main, "name: guard fail\n"+threeHosts+`assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "size=4"
`)
	failRes, err := Run(context.Background(), failDir)
	if err != nil {
		t.Fatalf("Run fail: %v", err)
	}
	if failRes[0].Pass {
		t.Fatalf("fail-ветка: ожидали FAIL (roster=3, ассерт ждал size=4)")
	}
}
