//go:build e2e

// L3b smoke-test harness-расширения Vigil/Decree/Oracle (ADR-030):
// прокатывает все 3 новых helper-а end-to-end через настоящий Operator-API +
// настоящую PG-схему, БЕЗ реального mTLS-EventStream-emit-а от soul-stub-а.
//
// Что покрыто:
//   - Stack.CreateVigil — POST /v1/vigils → 201 → vigils-row в БД.
//   - Stack.CreateDecree — POST /v1/decrees → 201 → decrees-row в БД.
//   - Stack.EmitPortent — stub-INSERT в oracle_fires (отсутствие real
//     handlePortentEvent — задокументированное упрощение, см. harness/oracle.go).
//   - Stack.WaitForOracleFires — поллинг oracle_fires до появления N строк.
//
// Что НЕ покрыто (и не должно):
//   - full path Soul-stub → mTLS-EventStream → handlePortentEvent → SubjectMatches
//     → where-CEL → EnqueueScenario → RecordFire → audit `oracle.fired`. Это
//     `TestE2EOracleTypedPortent_FileChangedFireFlow` (Skip до полного
//     harness-расширения с soul-stub-emit). L1 покрытие — в
//     `keeper/internal/grpc/oracle_crosside_integration_test.go`.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/tests/e2e/harness"
)

func TestL3b_VigilDecreeOracleFlow_Smoke(t *testing.T) {
	stack := harness.NewStack(t, harness.Config{
		ExamplePath: "examples/service/service-noop",
		Souls:       1,
	})
	defer stack.Cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. CreateVigil — core-beacon file_changed с coven-субъектом. coven=web
	// произвольная; harness не сверяет принадлежность subject хоста.
	vigilName := stack.CreateVigil(ctx, t, harness.CreateVigilOpts{
		Name:     "l3b-vigil-smoke",
		Interval: "30s",
		Check:    "core.beacon.file_changed",
		Coven:    []string{"web"},
		Params:   map[string]any{"path": "/etc/nginx.conf"},
	})

	// 2. CreateDecree — реакция на этот Vigil → noop scenario. incarnation_name
	// произвольный (валидатор формата только; смоук не достигает enqueue-фазы,
	// где membership-проверка). where_cel пустой — субъектная привязка единственный
	// фильтр.
	decreeName := stack.CreateDecree(ctx, t, harness.CreateDecreeOpts{
		Name:            "l3b-decree-smoke",
		OnBeacon:        vigilName,
		Coven:           []string{"web"},
		IncarnationName: "web-app",
		ActionScenario:  "noop",
		Cooldown:        "5m",
	})

	// 3. EmitPortent — stub-fire в oracle_fires (см. шапку файла; реальный путь
	// через handlePortentEvent — отдельный slice).
	subjectSID := "soul-test-0.example.com" // SID stub-soul-а из NewStack (Souls:1).
	stack.EmitPortent(ctx, t, decreeName, subjectSID)

	// 4. WaitForOracleFires — поллинг до 1 строки (одна уникальная пара
	// (decree, subject)).
	fires := stack.WaitForOracleFires(ctx, t, decreeName, 1, 5*time.Second)

	if len(fires) != 1 {
		t.Fatalf("ожидали 1 fire-row, получили %d: %+v", len(fires), fires)
	}
	got := fires[0]
	if got.Decree != decreeName {
		t.Errorf("fires[0].Decree = %q, ожидали %q", got.Decree, decreeName)
	}
	if got.Subject != subjectSID {
		t.Errorf("fires[0].Subject = %q, ожидали %q", got.Subject, subjectSID)
	}
	if got.FiredAt.IsZero() {
		t.Error("fires[0].FiredAt пустой")
	}
}
