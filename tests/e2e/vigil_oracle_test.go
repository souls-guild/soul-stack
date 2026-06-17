//go:build e2e

// L3a E2E: full Vigil/Oracle/Decree execution-loop (ADR-030, beacons reactor) на
// реальном стеке. Доводит skeleton TestE2EOracleTypedPortent_FileChangedFireFlow
// (t.Skip) до рабочего теста: soul-stub.SendPortent (V5-1) уже умеет слать
// PortentEvent по mTLS-EventStream-у, поэтому реальный путь покрывается без
// PG-stub-обхода EmitPortent.
//
// Поток (реальный, без моков keeper-side):
//   1. RegisterService + CreateIncarnationWithApply (incarnation существует —
//      Oracle-enqueuer резолвит ServiceRef из incarnation.service).
//   2. AddSoulToCoven(incarnation.name) — субъект-match Decree + membership-check
//      (incarnation_name ∈ covens субъекта, ADR-030(b)) проходят.
//   3. CreateVigil (core.beacon.file_changed) + CreateDecree (typed-payload
//      where-CEL `event.file_changed.path.startsWith("/etc/")`, action_scenario
//      ОТЛИЧНЫЙ от auto-create — `converge`, чтобы реактор-прогон отличался от
//      авто-create-прогона по scenario+started_by_aid).
//   4. soul-stub.SendPortent(FileChangedPortent{path:/etc/...}) через живой стрим.
//   5. ASSERT прямыми PG-queries (real DB, real flows):
//      - WaitForOracleFires — oracle_fires cooldown-state (decree, subject);
//      - audit_log `oracle.fired` (decree + scenario + sid);
//      - WaitForOracleReaction — apply_runs(scenario=converge, started_by_aid=NULL)
//        поставлен реактором (EnqueueScenario → InsertPlanned).
//
// Ограничение (документированное): soul-stub не запускает реальный beacon-
// scheduler — caller вручную собирает PortentEvent (L3a-контракт: проверяем
// keeper-side reactor-pipeline match/where/membership/cooldown/enqueue, а не
// реализм Soul-side Check-а). Реальный inotify/scheduler — L3b territory.
package e2e_test

import (
	"context"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestOracle_FileChanged_FiresScenario(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const incName = "oracle-fire-target"

	stack.RegisterService(t, "noop", "examples/service/service-noop")

	// Live EventStream: Redis SID-lease → dispatch маршрутизируется локально;
	// SetApplyDefaultSuccess — SUCCESS на любую задачу (важен lifecycle apply_runs,
	// не реализм per-task, см. scenario_apply_test.go).
	stub := stack.ConnectSoulStub(t, 0)
	stub.SetApplyDefaultSuccess(true)
	sid := stack.SoulSID(0)

	// Coven=incarnation.name: roster авто-create + субъект-match Decree +
	// membership-check (incarnation_name ∈ covens, ADR-030(b)) — все по одной метке.
	stack.AddSoulToCoven(t, 0, incName)

	// Incarnation должна существовать ДО Portent-а: enqueuer резолвит ServiceRef
	// из incarnation.service (oracle_enqueuer.go). Авто-create катит scenario
	// `create` — дожидаемся success, чтобы incarnation вышла из applying и не
	// конфликтовала с реактор-прогоном.
	_, createApplyID := stack.CreateIncarnationWithApply(t, incName, "noop@main", nil)
	stack.WaitApplySuccess(t, createApplyID, 60)

	vigilName := stack.CreateVigil(ctx, t, harness.CreateVigilOpts{
		Name:     "oracle-fire-vigil",
		Interval: "30s",
		Check:    "core.beacon.file_changed",
		Coven:    []string{incName},
		Params:   map[string]any{"path": "/etc/nginx.conf"},
	})

	// action_scenario = converge (существует в service-noop, отличен от create):
	// реактор-прогон отличим от авто-create по (scenario, started_by_aid IS NULL).
	// where-CEL — typed-field-access V5-1 над FileChangedPortent.
	decreeName := stack.CreateDecree(ctx, t, harness.CreateDecreeOpts{
		Name:            "oracle-fire-decree",
		OnBeacon:        vigilName,
		WhereCEL:        `event.file_changed.path.startsWith("/etc/")`,
		Coven:           []string{incName},
		IncarnationName: incName,
		ActionScenario:  "converge",
		Cooldown:        "5m",
	})

	// Момент отсечки авто-create-прогона: реактор-прогон стартует позже.
	beforeFire := time.Now().UTC()

	// Реальный emit: soul-stub шлёт typed FileChangedPortent по mTLS-EventStream-у.
	// path под /etc/ → where-CEL true → реактор срабатывает.
	if err := stub.SendPortent(&keeperv1.PortentEvent{
		BeaconName: vigilName,
		Payload: &keeperv1.PortentEvent_FileChanged{
			FileChanged: &keeperv1.FileChangedPortent{
				Path:   "/etc/nginx.conf",
				Sha256: "deadbeef",
			},
		},
	}); err != nil {
		t.Fatalf("SendPortent: %v", err)
	}

	// 1. oracle_fires cooldown-state: одна строка (decree, subject=sid).
	fires := stack.WaitForOracleFires(ctx, t, decreeName, 1, 15*time.Second)
	if got := fires[0].Subject; got != sid {
		t.Fatalf("oracle_fires.subject = %q, ожидался авторитетный SID %q (mTLS peer cert)", got, sid)
	}

	// 2. audit `oracle.fired` с decree/scenario/sid (payload-subset).
	stack.AssertAuditEvent(t, "oracle.fired", map[string]any{
		"decree":   decreeName,
		"scenario": "converge",
		"sid":      sid,
	})

	// 3. Реактор поставил scenario в work-queue: planned apply_run scenario=converge,
	// started_by_aid IS NULL (Soul-инициированная реакция без identity Архонта).
	reactionApplyID := stack.WaitForOracleReaction(ctx, t, incName, "converge", beforeFire, 15*time.Second)
	if reactionApplyID == createApplyID {
		t.Fatalf("реактор-прогон совпал с авто-create apply_id %q — фильтр не различил прогоны", createApplyID)
	}
}
