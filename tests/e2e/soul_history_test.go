//go:build e2e

// Per-section E2E: GET /v1/souls/{sid}/history агрегирует per-host timeline из
// двух источников — scenario-прогоны (apply_runs) и ad-hoc exec (errands). Тест
// гоняет на ОДНОМ connected-SID сначала scenario-apply (incarnation create),
// затем single-Errand /exec (ADR-033), после чего ассертит, что /history отдаёт
// обе записи с корректной дискриминацией (type=scenario несёт
// incarnation/scenario, type=errand несёт module), сортировкой started_at DESC и
// работающим query-фильтром ?type=.
//
// Почему ловит регрессии:
//   - merge-запрос soul.SelectHistory сломан / не объединяет источники → total<2
//     или один из типов отсутствует;
//   - фильтр ?type= игнорируется → лишние/недостающие записи;
//   - сортировка started_at DESC потеряна → порядок нарушен.
//
// Ограничение (как у scenario_apply): soul-stub не исполняет реальные модули
// (SetApplyDefaultSuccess + errand SUCCESS-эхо) — проверяем keeper-side
// агрегацию history, не реализм apply/exec (L3a-контракт).
package e2e_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestSoulHistory_AggregatesScenarioAndErrand(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	stack.RegisterService(t, "noop", "examples/service/noop")

	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	sid := stack.SoulSID(0)

	stack.AddSoulToCoven(t, 0, "test-history")

	// Источник №1 — scenario: incarnation create авто-запускает scenario `create`
	// → apply_runs-строка под этим SID-ом.
	_, applyID := stack.CreateIncarnationWithApply(t, "test-history", "noop@main", nil)
	stack.WaitApplySuccess(t, applyID, 60)

	// Источник №2 — single-Errand: ad-hoc /exec на том же SID-е → errands-строка.
	res := stack.ExecErrand(t, sid, "core.cmd.shell", map[string]any{"cmd": "echo ok"})
	if res.Status != "success" {
		t.Fatalf("ExecErrand: status=%q, ожидался success", res.Status)
	}

	// /history без фильтра — обе записи.
	reply := stack.SoulHistory(t, sid, "")
	if reply.SID != sid {
		t.Fatalf("/history: sid echo=%q, ожидался %q", reply.SID, sid)
	}
	if reply.Total < 2 {
		t.Fatalf("/history: total=%d, ожидалось >=2 (scenario+errand); items=%+v", reply.Total, reply.Items)
	}

	var scen, errItem *harness.SoulHistoryItem
	for i := range reply.Items {
		it := &reply.Items[i]
		switch it.Type {
		case "scenario":
			scen = it
		case "errand":
			errItem = it
		default:
			t.Fatalf("/history: неизвестный type=%q в item=%+v", it.Type, it)
		}
	}
	if scen == nil {
		t.Fatalf("/history: нет записи type=scenario; items=%+v", reply.Items)
	}
	if errItem == nil {
		t.Fatalf("/history: нет записи type=errand; items=%+v", reply.Items)
	}

	// type=scenario несёт incarnation/scenario, НЕ несёт module.
	if scen.Incarnation != "test-history" {
		t.Fatalf("scenario-item.incarnation=%q, ожидался test-history (%+v)", scen.Incarnation, scen)
	}
	if scen.Scenario == "" {
		t.Fatalf("scenario-item.scenario пуст (%+v)", scen)
	}
	if scen.Module != "" {
		t.Fatalf("scenario-item несёт errand-поле module=%q (%+v)", scen.Module, scen)
	}

	// type=errand несёт module, НЕ несёт incarnation.
	if errItem.Module != "core.cmd.shell" {
		t.Fatalf("errand-item.module=%q, ожидался core.cmd.shell (%+v)", errItem.Module, errItem)
	}
	if errItem.Incarnation != "" {
		t.Fatalf("errand-item несёт scenario-поле incarnation=%q (%+v)", errItem.Incarnation, errItem)
	}

	// Сортировка started_at DESC: errand стартовал ПОСЛЕ scenario → должен идти
	// первым (или раньше) в items. Сравниваем по RFC3339-строкам (лексикографика
	// = хронология для UTC RFC3339).
	for i := 1; i < len(reply.Items); i++ {
		if reply.Items[i-1].StartedAt < reply.Items[i].StartedAt {
			t.Fatalf("/history: сортировка не DESC по started_at: items[%d]=%q < items[%d]=%q",
				i-1, reply.Items[i-1].StartedAt, i, reply.Items[i].StartedAt)
		}
	}

	// Фильтр ?type=errand — только errand.
	onlyErrand := stack.SoulHistory(t, sid, "errand")
	if len(onlyErrand.Items) == 0 {
		t.Fatalf("/history?type=errand: пусто, ожидалась >=1 errand-запись")
	}
	for _, it := range onlyErrand.Items {
		if it.Type != "errand" {
			t.Fatalf("/history?type=errand вернул type=%q (%+v)", it.Type, it)
		}
	}

	// Фильтр ?type=scenario — только scenario.
	onlyScenario := stack.SoulHistory(t, sid, "scenario")
	if len(onlyScenario.Items) == 0 {
		t.Fatalf("/history?type=scenario: пусто, ожидалась >=1 scenario-запись")
	}
	for _, it := range onlyScenario.Items {
		if it.Type != "scenario" {
			t.Fatalf("/history?type=scenario вернул type=%q (%+v)", it.Type, it)
		}
	}
}
