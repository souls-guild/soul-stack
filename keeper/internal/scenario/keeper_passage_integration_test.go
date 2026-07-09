//go:build integration

// Guard-набор Слайса 2 (keeper-side dispatch per-Passage, ADR-056). До Слайса 2
// dispatchKeeperTasks звался ОДИН раз ДО stage-loop на tasks первого render
// (ActivePassage=0), где keeper-задачи Passage>0 — placeholder-ы без Params → не
// диспатчились. Слайс 2 перенёс вызов ВНУТРЬ stage-loop, per-Passage, на ПЕРЕ-
// рендеренных при ActivePassage=p tasks.
//
// Прогоны идут сквозь run()+PG (Start → waitRunDone) с keeper-Registry-заглушкой,
// реальным auditpg и stubPassageCap (staged-гейт ADR-056 §S5 требует passage-aware
// хостов; для all-keeper roster пуст, но при nil passageCap гейт fail-closed-
// отвергает staged — прод всегда с Redis, поэтому stub отражает прод).

package scenario

import (
	"context"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/module"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Guard-ы Слайса 2 переиспользуют newRunnerKeeperStaged (keeper-Registry +
// stubPassageCap) из midrun_reresolve_integration_test.go — то же сочетание
// «keeper-задача + staged-стратификация».

// capturingKeeperModule — keeper-side core-модуль, ЗАХВАТЫВАЮЩИЙ полученные Params
// (для доказательства keeper→keeper register-chaining: задача Passage 1 должна
// увидеть в Params значение, отрендеренное из register.<prev>.* keeper-задачи
// Passage 0) и эхающий заранее заданный output. failOnState — state-суффикс, на
// котором модуль возвращает failed (для keeper-fail-теста).
type capturingKeeperModule struct {
	module.BaseModule
	mu          sync.Mutex
	output      map[string]any            // output, эхаемый в ApplyEvent (на success)
	failOnState string                    // state-суффикс адреса, на котором вернуть failed
	echoParams  []string                  // ключи полученных Params, протягиваемые в output (транзитивная цепочка)
	gotParams   map[string]any            // Params последнего Apply (по state)
	gotStates   []string                  // порядок исполненных state-ов
	gotByState  map[string]map[string]any // Params per-state (для multi-passage цепочек с одним модулем)
}

func (m *capturingKeeperModule) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	m.mu.Lock()
	m.gotStates = append(m.gotStates, req.GetState())
	var params map[string]any
	if p := req.GetParams(); p != nil {
		params = p.AsMap()
		m.gotParams = params
		if m.gotByState == nil {
			m.gotByState = map[string]map[string]any{}
		}
		m.gotByState[req.GetState()] = params
	}
	fail := req.GetState() == m.failOnState
	// out: статический output + протянутые из Params ключи (echoParams). Протяжка
	// нужна для транзитивных цепочек (P2 видит значение register P0, прошедшее
	// через P1 → P1 кладёт полученный из register.P0.* params-ключ в свой output).
	out := map[string]any{}
	for k, v := range m.output {
		out[k] = v
	}
	for _, k := range m.echoParams {
		if v, ok := params[k]; ok {
			out[k] = v
		}
	}
	m.mu.Unlock()

	if fail {
		return stream.Send(&pluginv1.ApplyEvent{Failed: true, Message: "keeper task failed at " + req.GetState()})
	}
	ev := &pluginv1.ApplyEvent{Changed: true}
	if len(out) > 0 {
		ev.Output = mustStructAny(out)
	}
	return stream.Send(ev)
}

// paramsForState возвращает Params, полученные модулём на конкретном state
// (для цепочек, где один модуль исполняется в нескольких Passage под разными
// state — например core.cloud.created на P0 и core.cloud.updated на P1).
func (m *capturingKeeperModule) paramsForState(state string) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotByState[state]
}

func (m *capturingKeeperModule) params() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gotParams
}

func (m *capturingKeeperModule) states() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.gotStates...)
}

// keeperChainServiceRepo — 2-Passage all-keeper цепочка (ADR-056, Слайс 2):
//
//	#0 provision (core.cloud.created, register: provision) → Passage 0
//	#1 deliver   (core.bootstrap.delivered, params читает register.provision.ip) → Passage 1
//
// Stratify разводит по Passage (deliver читает register provision в params).
// all-keeper → no_hosts bypass. Это end-to-end доказательство keeper→keeper
// register-chaining: deliver видит ip, эмитнутый provision Passage 0.
func keeperChainServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: 2-passage all-keeper chain (cloud.created -> bootstrap.delivered)
state_changes: {}
tasks:
  - name: provision vm
    module: core.cloud.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    params:
      target_ip: "${ register.provision.ip }"
`)
}

// keeperChain3ServiceRepo — 3-Passage all-keeper цепочка (ADR-056, Слайс 2),
// ★ зеркало целевого live-потока create redis-кластера (provision→deliver→register):
//
//	#0 provision (core.bootstrap.created,   register: provision, output ip)          → Passage 0
//	#1 deliver   (core.bootstrap.delivered, params target_ip=register.provision.ip,
//	             register: deliver, echoParams проброс target_ip в output)            → Passage 1
//	#2 finalize  (core.bootstrap.finalized, params origin=register.deliver.target_ip) → Passage 2
//
// Все звенья — base core.bootstrap (НЕ в coremanifest → params не валидируются
// scenario-load-ом, register-выражения проходят свободно), различаются state-ом —
// один capturingKeeperModule обслуживает все три, paramsForState(state) различает
// per-Passage Params. Каждое звено читает register ПРЕДЫДУЩЕГО → Stratify разводит
// на 3 Passage. Транзитивность: значение provision.ip (P0) протягивается через
// deliver (P1 кладёт полученный target_ip в свой register-output) и доходит до
// finalize (P2) как origin. all-keeper → no_hosts bypass.
func keeperChain3ServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: 3-passage all-keeper chain (bootstrap.created -> delivered -> finalized)
state_changes: {}
tasks:
  - name: provision vm
    module: core.bootstrap.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    register: deliver
    params:
      target_ip: "${ register.provision.ip }"
  - name: finalize
    module: core.bootstrap.finalized
    on: keeper
    params:
      origin: "${ register.deliver.target_ip }"
`)
}

// TestIntegration_KeeperChain_3Passage_TransitiveRegister — ★ №1 (КРИТИЧНЫЙ,
// целевой live-поток create redis-кластера). 3-звенная keeper-цепочка
// cloud.created(P0)→bootstrap.delivered(P1)→vault.kv-read(P2), каждое звено
// читает register предыдущего. ASSERT:
//   - три apply_runs(apply_id, keeper, passage) для passage 0/1/2, ВСЕ success;
//   - Params последней (P2) содержит значение, протянутое ТРАНЗИТИВНО от register
//     ПЕРВОЙ (P0): provision.ip → deliver.target_ip (P1 проброс) → finalize.origin (P2);
//   - host-fan-out не было (all-keeper).
//
// Расширяет 2-Passage-кейс до 3 Passage — guard на то, что register-chaining
// keeper→keeper держит цепочку длиннее одного звена (transitive accumulation).
func TestIntegration_KeeperChain_3Passage_TransitiveRegister(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Хостов НЕ сидим: all-keeper → no_hosts bypass (provision-from-zero).

	// Один модуль core.bootstrap обслуживает все три state (created/delivered/finalized):
	// эхает ip в каждом register-output; echoParams протягивает target_ip из params
	// в output (P1 кладёт полученный из register.provision.ip target_ip в свой
	// register-output → finalize прочитает register.deliver.target_ip).
	bootstrap := &capturingKeeperModule{
		output:     map[string]any{"ip": "10.0.0.7"},
		echoParams: []string{"target_ip"},
	}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperChain3ServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Все три keeper-state исполнены (по одному разу, в своём Passage), в порядке цепочки.
	if got := bootstrap.states(); len(got) != 3 || got[0] != "created" || got[1] != "delivered" || got[2] != "finalized" {
		t.Errorf("bootstrap states = %v, want [created delivered finalized]", got)
	}

	// Промежуточное звено (P1, state delivered) получило ip от P0 (register.provision.ip).
	if got := bootstrap.paramsForState("delivered"); got == nil || got["target_ip"] != "10.0.0.7" {
		t.Fatalf("P1 (delivered) Params.target_ip = %v, want '10.0.0.7' (register.provision.ip P0 не прокинут)", got["target_ip"])
	}

	// ★ ТРАНЗИТИВНАЯ протяжка: finalize (P2, state finalized) получил origin == ip
	// ПЕРВОЙ задачи (P0), прошедшее через deliver (P1). register-chaining держит
	// цепочку длиннее одного звена.
	got := bootstrap.paramsForState("finalized")
	if got == nil || got["origin"] != "10.0.0.7" {
		t.Fatalf("★ P2 (finalized) Params.origin = %v, want '10.0.0.7' — значение register ПЕРВОЙ задачи (P0) НЕ протянуто транзитивно через P1 до P2 (keeper→keeper chaining оборвался на 2-м звене)", got["origin"])
	}

	// host-fan-out не было (all-keeper).
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (all-keeper)", disp.calls)
	}

	// apply_runs = ровно 3 keeper-строки (apply_id, keeper, 0/1/2), все success. Ни одной host-строки.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if len(keeperPassages) != 3 {
		t.Fatalf("★ keeper apply_runs passages = %v, want ровно {0,1,2} (по строке на Passage с keeper-задачами)", keeperPassages)
	}
	for _, p := range []int{0, 1, 2} {
		if keeperPassages[p] != applyrun.StatusSuccess {
			t.Errorf("keeper apply_runs[passage=%d] = %s, want success", p, keeperPassages[p])
		}
	}
}

// TestIntegration_KeeperChain_Rerun_NoPKConflict — ★ №5 (операционный rerun). Два
// последовательных прогона одной staged keeper-цепочки с РАЗНЫМИ apply_id.
// ASSERT:
//   - второй прогон НЕ ловит PK-конфликт на apply_runs(apply_id, keeper, passage)
//     (тройной PK миграции 078 различает прогоны по apply_id) — цепочка
//     ПЕРЕИСПОЛНЯЕТСЯ целиком (оба Passage заново);
//   - apply_runs-строки прогона #1 (apply_id_1) НЕ затёрты строками #2 — каждый
//     прогон несёт собственный набор keeper-строк под своим apply_id;
//   - register-chaining работает на ОБОИХ прогонах (deliver получил ip на #2 тоже).
func TestIntegration_KeeperChain_Rerun_NoPKConflict(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	bootstrap := &capturingKeeperModule{output: map[string]any{"delivered": true}}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	run := func(applyID string) {
		t.Helper()
		if err := r.Start(context.Background(), RunSpec{
			ApplyID:         applyID,
			IncarnationName: "noop-prod",
			ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
			ScenarioName:    "create",
			StartedByAID:    "archon-alice",
		}); err != nil {
			t.Fatalf("Start(%s): %v", applyID, err)
		}
		waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)
	}

	// keeperPassages — keeper-строки конкретного прогона (по apply_id), все success.
	keeperPassages := func(applyID string) map[int]applyrun.Status {
		t.Helper()
		statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
		if err != nil {
			t.Fatalf("SelectStatusesByApplyID(%s): %v", applyID, err)
		}
		out := map[int]applyrun.Status{}
		for _, st := range statuses {
			if st.SID != render.KeeperTargetSID {
				t.Errorf("прогон %s: не-keeper строка sid=%s passage=%d", applyID, st.SID, st.Passage)
				continue
			}
			out[st.Passage] = st.Status
		}
		return out
	}

	applyID1 := audit.NewULID()
	run(applyID1)
	if got := keeperPassages(applyID1); len(got) != 2 || got[0] != applyrun.StatusSuccess || got[1] != applyrun.StatusSuccess {
		t.Fatalf("прогон #1 keeper passages = %v, want {0:success,1:success}", got)
	}

	// Второй прогон — другой apply_id. Если бы Passage не входил в PK или прогон не
	// различался по apply_id, второй Insert(running) на (apply_id, keeper, passage)
	// упал бы дубликатом → keeper_dispatch_failed → error_locked (waitRunDone в run()
	// не дождался бы Ready).
	applyID2 := audit.NewULID()
	run(applyID2)
	if got := keeperPassages(applyID2); len(got) != 2 || got[0] != applyrun.StatusSuccess || got[1] != applyrun.StatusSuccess {
		t.Fatalf("★ прогон #2 keeper passages = %v, want {0:success,1:success} (rerun staged keeper-цепочки переисполнился без PK-конфликта)", got)
	}

	// ★ Строки прогона #1 НЕ затёрты прогоном #2 — каждый apply_id несёт свой набор.
	if got := keeperPassages(applyID1); len(got) != 2 {
		t.Fatalf("★ прогон #1 keeper passages ПОСЛЕ rerun = %v, want по-прежнему {0,1} (строки #1 затёрты прогоном #2 — apply_id не изолирует)", got)
	}

	// register-chaining отработал на ВТОРОМ прогоне тоже (deliver видит ip).
	if got := bootstrap.params(); got == nil || got["target_ip"] != "10.0.0.7" {
		t.Errorf("после rerun bootstrap.delivered Params.target_ip = %v, want '10.0.0.7'", got["target_ip"])
	}
	// Каждое звено исполнено по 2 раза (два прогона).
	if got := cloud.states(); len(got) != 2 {
		t.Errorf("cloud states = %v, want 2 исполнения (два прогона)", got)
	}
	if got := bootstrap.states(); len(got) != 2 {
		t.Errorf("bootstrap states = %v, want 2 исполнения (два прогона)", got)
	}
}

// keeperForwardAccumServiceRepo — №2 forward-accumulation: P2 читает register от
// P0 И P1 ОДНОВРЕМЕННО. Все звенья — base core.bootstrap (свободные params),
// различаются state.
//
//	#0 provision (core.bootstrap.created,   register: provision, output ip+token)    → Passage 0
//	#1 deliver   (core.bootstrap.delivered, register: deliver,   output ip+token)    → Passage 1 (читает register.provision.ip)
//	#2 finalize  (core.bootstrap.finalized, params from_p0=register.provision.ip
//	                                              + from_p1=register.deliver.token)   → Passage 2
//
// finalize (P2) читает register ДВУХ предыдущих Passage сразу — register-bucket
// keeperVars на P2 несёт accumulated provision (P0) И deliver (P1).
func keeperForwardAccumServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: P2 reads register of both P0 and P1 (forward-accumulation)
state_changes: {}
tasks:
  - name: provision vm
    module: core.bootstrap.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver bootstrap
    module: core.bootstrap.delivered
    on: keeper
    register: deliver
    params:
      target_ip: "${ register.provision.ip }"
  - name: finalize reads both
    module: core.bootstrap.finalized
    on: keeper
    params:
      from_p0: "${ register.provision.ip }"
      from_p1: "${ register.deliver.token }"
`)
}

// TestIntegration_KeeperChain_ForwardAccumulation — №2: keeper-задача Passage 2
// читает register ОБОИХ предыдущих Passage (P0 provision И P1 deliver) в одном
// рендере. ASSERT: finalize (P2) получил from_p0 == ip (register P0) И from_p1 ==
// token (register P1) — KeeperRegister-bucket на P2 несёт накопленный register всех
// прошлых Passage (loadRegisterByHostUpToPassage(P2) = register Passage<2), не только
// непосредственно предыдущего.
func TestIntegration_KeeperChain_ForwardAccumulation(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	// Каждый register-output несёт И ip, И token (output общий на модуль) — P0
	// register provision.ip и P1 register deliver.token оба доступны финализатору.
	bootstrap := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7", "token": "tok-abc"}}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperForwardAccumServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	got := bootstrap.paramsForState("finalized")
	if got == nil {
		t.Fatal("P2 (finalized) Params == nil")
	}
	if got["from_p0"] != "10.0.0.7" {
		t.Errorf("from_p0 = %v, want '10.0.0.7' (register.provision.ip P0 не виден на P2)", got["from_p0"])
	}
	if got["from_p1"] != "tok-abc" {
		t.Errorf("from_p1 = %v, want 'tok-abc' (register.deliver.token P1 не виден на P2)", got["from_p1"])
	}
}

// TestIntegration_KeeperChain_FailPassage2_EarlyPassagesSucceed — №3: keeper-fail
// на ПОСЛЕДНЕМ звене 3-Passage цепочки (P2), после двух успешных Passage. ASSERT:
// incarnation ERROR_LOCKED (reason keeper_dispatch_failed); keeper apply_runs:
// passage 0 = success, passage 1 = success, passage 2 = failed; state НЕ закоммичен
// (commit только после ПОСЛЕДНЕГО Passage). Доказывает, что abort на самом позднем
// keeper-Passage не теряет терминалы ранних успешных Passage.
func TestIntegration_KeeperChain_FailPassage2_EarlyPassagesSucceed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	// Единый core.bootstrap, падающий на ПОСЛЕДНЕМ state (finalized = P2).
	bootstrap := &capturingKeeperModule{
		output:      map[string]any{"ip": "10.0.0.7"},
		echoParams:  []string{"target_ip"},
		failOnState: "finalized",
	}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := keeperChain3ServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "keeper_dispatch_failed" {
		t.Errorf("reason = %v, want keeper_dispatch_failed (keeper-fail Passage 2)", inc.StatusDetails)
	}
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (фейл до финального commit)", inc.State)
	}

	// Ранние Passage отработали (P0 created, P1 delivered), P2 finalized упал до конца.
	if got := bootstrap.states(); len(got) != 3 || got[0] != "created" || got[1] != "delivered" || got[2] != "finalized" {
		t.Errorf("bootstrap states = %v, want [created delivered finalized] (ранние Passage успели, P2 дошёл до Apply и упал)", got)
	}

	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if keeperPassages[0] != applyrun.StatusSuccess || keeperPassages[1] != applyrun.StatusSuccess {
		t.Errorf("keeper passages 0/1 = %s/%s, want success/success (ранние Passage отработали ДО фейла P2)", keeperPassages[0], keeperPassages[1])
	}
	if keeperPassages[2] != applyrun.StatusFailed {
		t.Fatalf("★ keeper apply_runs[passage=2] = %s, want failed (keeper-fail на последнем звене записал failed-строку именно P2)", keeperPassages[2])
	}
}

// crossChannelServiceRepo — keeper-задача Passage 1, читающая HOST-register
// (НЕ keeper-register) в params. Структура:
//
//	#0 host probe (core.exec.run, register: hostprobe) → Passage 0 (host-задача)
//	#1 keeper read (core.vault.kv-read, params data=register.hostprobe.stdout) → Passage 1
//
// keeper-задача читает register.hostprobe.* — это HOST-register, эмитнутый
// host-задачей Passage 0. Он живёт в RegisterByHost[<hostSID>], а keeperVars
// видит ТОЛЬКО KeeperRegister (keeper-bucket). Значит на render Passage 1
// keeper-задача получит no-such-key → render_failed → error_locked.
func crossChannelServiceRepo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: keeper task reads HOST register (cross-channel, must fail-closed)
state_changes: {}
tasks:
  - name: host probe
    module: core.exec.run
    register: hostprobe
    params:
      cmd: echo
      args: ["ok"]
    changed_when: "false"
  - name: keeper reads host register
    module: core.bootstrap.read
    on: keeper
    params:
      data: "${ register.hostprobe.stdout }"
`)
}

// TestIntegration_KeeperChain_CrossChannel_FailClosed — ★ №6 (БЕЗОПАСНОСТЬ
// ДАННЫХ, fail-closed). keeper-задача (Passage 1) пытается прочитать HOST-register
// (register.hostprobe.*, эмитнутый host-задачей Passage 0) в params. host-register
// живёт в per-host RegisterByHost[<hostSID>]; keeperVars читает ТОЛЬКО изолированный
// KeeperRegister-канал (keeper-bucket). ASSERT: keeper-задача НЕ видит host-register
// → CEL no-such-key → render_failed → incarnation ERROR_LOCKED; keeper-задача (P1)
// НЕ исполнена (vault.Apply не вызван — фейл на render ДО dispatch).
//
// Guard на изоляцию каналов: если кто-то «починит» host-fallback так, что
// host-register протечёт в keeperVars (keeperVars увидит register.hostprobe), этот
// тест ПОКРАСНЕЕТ — прогон дойдёт до Ready вместо error_locked. Зеркало unit-теста
// TestKeeperRegisterChannel_Isolated (render-пакет), но через полный run()+PG.
func TestIntegration_KeeperChain_CrossChannel_FailClosed(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	bootstrap := &capturingKeeperModule{output: map[string]any{"ok": true}}
	keepers := fakeKeeperRegistry{"core.bootstrap": bootstrap}
	gitURL := crossChannelServiceRepo(t)

	// host probe (Passage 0) терминалится success — Passage 0 сходится, прогон
	// доходит до re-render Passage 1, где keeper-задача и падает на render.
	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	// render_failed — keeper-задача не смогла отрендерить params (host-register не
	// виден в keeperVars). Reason должен быть render_failed (не keeper_dispatch_failed:
	// фейл на render ДО исполнения модуля).
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "render_failed" {
		t.Errorf("reason = %v, want render_failed (host-register не виден keeper-задаче на render Passage 1)", inc.StatusDetails)
	}

	// ★ keeper-задача НЕ исполнена — фейл на render ДО dispatch модуля. Если бы
	// host-register протёк в keeperVars, render прошёл бы и bootstrap.Apply вызвался.
	if got := bootstrap.states(); len(got) != 0 {
		t.Fatalf("★ keeper-модуль states = %v, want пусто — keeper-задача читает HOST-register, должна упасть на render (no-such-key) ДО исполнения; непустой states ⇒ host-register протёк в keeperVars (канал НЕ изолирован)", got)
	}
}

// TestIntegration_KeeperChain_2Passage_RegisterChained — ★ END-TO-END ДОКАЗАТЕЛЬСТВО
// ЭПИКА (Слайс 2). 2-Passage all-keeper цепочка: cloud.created (Passage 0) эмитит
// register provision{ip}, bootstrap.delivered (Passage 1) читает register.provision.ip
// в params. ASSERT: ОБА keeper-Passage исполнены, deliver получил Params.target_ip ==
// ip от provision (register прокинут end-to-end), apply_runs = 2 keeper-строки
// (apply_id, keeper, 0) и (apply_id, keeper, 1), incarnation READY.
func TestIntegration_KeeperChain_2Passage_RegisterChained(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	// Хостов НЕ сидим: all-keeper → no_hosts bypass (provision-from-zero).

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	bootstrap := &capturingKeeperModule{output: map[string]any{"delivered": true}}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// Оба keeper-state исполнены.
	if got := cloud.states(); len(got) != 1 || got[0] != "created" {
		t.Errorf("cloud states = %v, want [created]", got)
	}
	if got := bootstrap.states(); len(got) != 1 || got[0] != "delivered" {
		t.Errorf("bootstrap states = %v, want [delivered]", got)
	}

	// ★ register прокинут end-to-end: deliver получил target_ip == ip от provision.
	got := bootstrap.params()
	if got == nil || got["target_ip"] != "10.0.0.7" {
		t.Fatalf("★ bootstrap.delivered Params.target_ip = %v, want '10.0.0.7' (register.provision.ip Passage 0 НЕ прокинут в keeper-задачу Passage 1)", got["target_ip"])
	}

	// host-fan-out не было (all-keeper).
	if disp.calls != 0 {
		t.Errorf("SendApply calls = %d, want 0 (all-keeper, host-fan-out нет)", disp.calls)
	}

	// apply_runs = ровно 2 keeper-строки (apply_id, keeper, 0) и (apply_id, keeper, 1),
	// обе success. Ни одной host-строки.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if len(keeperPassages) != 2 {
		t.Fatalf("★ keeper apply_runs passages = %v, want ровно {0,1} (по строке на Passage с keeper-задачами)", keeperPassages)
	}
	for _, p := range []int{0, 1} {
		if keeperPassages[p] != applyrun.StatusSuccess {
			t.Errorf("keeper apply_runs[passage=%d] = %s, want success", p, keeperPassages[p])
		}
	}
}

// TestIntegration_KeeperChain_FailPassage1_ErrorLocked — ★ keeper-FAIL на Passage>0
// (Слайс 2). cloud.created (Passage 0) успешен → host-dispatch Passage 0 (none) →
// barrier 0 → bootstrap.delivered (Passage 1) FAILED. ASSERT: incarnation
// ERROR_LOCKED, reason keeper_dispatch_failed, state НЕ закоммичен; apply_runs:
// keeper passage 0 = success (отработал ДО фейла), keeper passage 1 = failed;
// никакого host-dispatch (all-keeper). Доказывает abort на Passage>0 ПОСЛЕ
// успешного раннего Passage + наблюдаемость (error_locked корректен, state last-
// known-good).
func TestIntegration_KeeperChain_FailPassage1_ErrorLocked(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")

	cloud := &capturingKeeperModule{output: map[string]any{"ip": "10.0.0.7"}}
	// bootstrap.delivered падает (failOnState="delivered").
	bootstrap := &capturingKeeperModule{failOnState: "delivered"}
	keepers := fakeKeeperRegistry{
		"core.cloud":     cloud,
		"core.bootstrap": bootstrap,
	}
	gitURL := keeperChainServiceRepo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inc := waitRunDone(t, "noop-prod", applyID, incarnation.StatusErrorLocked)
	if inc.StatusDetails == nil || inc.StatusDetails["reason"] != "keeper_dispatch_failed" {
		t.Errorf("reason = %v, want keeper_dispatch_failed (keeper-fail Passage 1)", inc.StatusDetails)
	}
	// state НЕ закоммичен (commit только после ПОСЛЕДНЕГО Passage; фейл Passage 1 до него).
	if len(inc.State) != 0 {
		t.Errorf("incarnation.state = %v, want пустой (keeper-fail Passage 1 НЕ коммитит state)", inc.State)
	}

	// Passage 0 keeper-задача успела (cloud исполнен), Passage 1 — упала до конца.
	if got := cloud.states(); len(got) != 1 {
		t.Errorf("cloud states = %v, want [created] (Passage 0 успел до фейла Passage 1)", got)
	}

	// apply_runs: keeper passage 0 = success, keeper passage 1 = failed. Никаких host-строк.
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	keeperPassages := map[int]applyrun.Status{}
	for _, st := range statuses {
		if st.SID != render.KeeperTargetSID {
			t.Errorf("неожиданная не-keeper apply_runs-строка sid=%s passage=%d (host-dispatch не должен был стартовать)", st.SID, st.Passage)
			continue
		}
		keeperPassages[st.Passage] = st.Status
	}
	if keeperPassages[0] != applyrun.StatusSuccess {
		t.Errorf("keeper apply_runs[passage=0] = %s, want success (отработал ДО фейла Passage 1)", keeperPassages[0])
	}
	if keeperPassages[1] != applyrun.StatusFailed {
		t.Fatalf("★ keeper apply_runs[passage=1] = %s, want failed (keeper-fail Passage 1 записал failed-строку)", keeperPassages[1])
	}
}

// mixedKeeperHostPassage0Repo — keeper-задача + host-задача В ОДНОМ Passage 0
// (mixed keeper-passage-0 + host-passage-0). Нет register-зависимости между ними →
// Stratify даёт обоим Passage 0 (N=1). host-задача делает roster непустым (no_hosts
// не отсекает; keeper-задача исполняется первой, host-fan-out — следом).
func mixedKeeperHostPassage0Repo(t *testing.T) string {
	t.Helper()
	return writeServiceRepo(t, `name: create
description: mixed keeper + host in Passage 0
state_changes: {}
tasks:
  - name: vault read
    module: core.vault.kv-read
    on: keeper
    register: secret
    params:
      path: secret/data/db
  - name: host echo
    module: core.exec.run
    params:
      cmd: echo
      args: ["hi"]
    changed_when: "false"
`)
}

// TestIntegration_MixedKeeperHostPassage0 — ★ MIXED keeper+host Passage 0 (Слайс 2).
// keeper-задача (core.vault.kv-read, on: keeper) + host-задача (core.exec.run) В
// ОДНОМ Passage 0 (нет register-зависимости → N=1). ASSERT: keeper-задача исполнена
// ДО host-dispatch (host-fan-out стартовал — 1 SendApply), barrier Passage 0 НЕ
// раздулся keeper-строкой (classify скипает keeper-target → не зависает на «лишнем»
// терминале), incarnation READY. apply_runs: keeper passage 0 + host passage 0.
func TestIntegration_MixedKeeperHostPassage0(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "noop-prod")
	seedConnectedSoul(t, "host-a.example.com", []string{"noop-prod"})

	vault := &capturingKeeperModule{output: map[string]any{"value": "s3cr3t"}}
	keepers := fakeKeeperRegistry{"core.vault": vault}
	gitURL := mixedKeeperHostPassage0Repo(t)

	disp := &mockDispatcher{t: t, result: applyrun.StatusSuccess}
	r := newRunnerKeeperStaged(t, disp, keepers)

	applyID := audit.NewULID()
	if err := r.Start(context.Background(), RunSpec{
		ApplyID:         applyID,
		IncarnationName: "noop-prod",
		ServiceRef:      artifact.ServiceRef{Name: "noop", Git: gitURL, Ref: "master"},
		ScenarioName:    "create",
		StartedByAID:    "archon-alice",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Прогон НЕ виснет (если бы classify считал keeper-строку host-терминалом и
	// раздул terminal, либо наоборот ждал бы keeper-строку как хоста — barrier
	// завис бы до RunTimeout, waitRunDone упал бы). READY = barrier сошёлся ровно
	// по host-строкам Passage 0.
	waitRunDone(t, "noop-prod", applyID, incarnation.StatusReady)

	// keeper-задача исполнена.
	if got := vault.states(); len(got) != 1 || got[0] != "kv-read" {
		t.Errorf("vault states = %v, want [kv-read]", got)
	}
	// host-fan-out стартовал (host-задача ушла одному хосту).
	if disp.calls != 1 {
		t.Errorf("SendApply calls = %d, want 1 (host echo на одном хосте)", disp.calls)
	}

	// apply_runs: keeper passage 0 (success) + host passage 0 (success).
	statuses, err := applyrun.SelectStatusesByApplyID(context.Background(), integrationPool, applyID)
	if err != nil {
		t.Fatalf("SelectStatusesByApplyID: %v", err)
	}
	var keeperOK, hostOK bool
	for _, st := range statuses {
		if st.Passage != 0 {
			t.Errorf("apply_runs[%s] passage = %d, want 0 (N=1, всё в Passage 0)", st.SID, st.Passage)
		}
		switch st.SID {
		case render.KeeperTargetSID:
			keeperOK = st.Status == applyrun.StatusSuccess
		case "host-a.example.com":
			hostOK = st.Status == applyrun.StatusSuccess
		}
	}
	if !keeperOK {
		t.Errorf("нет keeper apply_runs success passage 0: %+v", statuses)
	}
	if !hostOK {
		t.Errorf("нет host apply_runs success passage 0: %+v", statuses)
	}
}

// --- Слайс-2 unit-guard над Stratify keeper-цепочки (без PG) -----------------

// TestStratify_KeeperChain_TwoPassages — keeper→keeper цепочка стратифицируется по
// register-зависимости в params: provision (register: provision) → Passage 0,
// deliver (params читает register.provision) → Passage 1. Доказывает, что Stratify
// видит register-ребро через params keeper-задачи (у keeper-задач нет where:, ребро
// идёт ИМЕННО через params) — фундамент per-passage keeper-dispatch.
func TestStratify_KeeperChain_TwoPassages(t *testing.T) {
	scn, _, diags, err := config.LoadScenarioManifestFromBytes("main.yml", []byte(`name: create
description: keeper chain stratify
state_changes: {}
tasks:
  - name: provision
    module: core.cloud.created
    on: keeper
    register: provision
    params:
      provider: fake
  - name: deliver
    module: core.bootstrap.delivered
    on: keeper
    params:
      target_ip: "${ register.provision.ip }"
`), config.ValidateOptions{})
	if err != nil {
		t.Fatalf("LoadScenarioManifestFromBytes: %v", err)
	}
	if diag.HasErrors(diags) {
		t.Fatalf("parse diags: %v", diags)
	}

	passage, perr := render.Stratify(scn.Tasks)
	if perr != nil {
		t.Fatalf("Stratify: %v", perr)
	}
	if passage.Count != 2 {
		t.Fatalf("passage.Count = %d, want 2 (provision P0, deliver P1)", passage.Count)
	}
	if passage.TaskPassage[0] != 0 || passage.TaskPassage[1] != 1 {
		t.Fatalf("TaskPassage = %v, want [0 1] (deliver читает register.provision в params)", passage.TaskPassage)
	}
}
